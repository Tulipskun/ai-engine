package d1store

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func testClient(t *testing.T, fake *fakeCloudflare) (*Client, *MemoryToken) {
	t.Helper()
	tokens := NewMemoryToken()
	tokens.Adopt(fake.token)
	server := fake.start(t)
	client := NewClient(server.URL, tokens.Get)
	if client == nil {
		t.Fatal("NewClient returned nil for a configured base")
	}
	return client, tokens
}

func TestVerifyTokenAcceptsActiveTokenAndResolvesDatabase(t *testing.T) {
	fake := newFakeCloudflare("cf-token")
	client, _ := testClient(t, fake)

	if err := client.VerifyToken(context.Background(), "cf-token"); err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	target, ok := client.ResolvedTarget()
	if !ok {
		t.Fatal("target was not resolved")
	}
	if target.AccountID != "acct-1" || target.DatabaseID != "db-1" || target.Name != DefaultDatabaseName {
		t.Fatalf("target = %+v, want acct-1/db-1/%s", target, DefaultDatabaseName)
	}
	if got := fake.pathCount("/user/tokens/verify"); got != 1 {
		t.Fatalf("token verify calls = %d, want 1", got)
	}
}

func TestVerifyTokenRejectsWrongTokenAndKeepsDownDistinct(t *testing.T) {
	fake := newFakeCloudflare("cf-token")
	client, _ := testClient(t, fake)

	if err := client.VerifyToken(context.Background(), "not-the-token"); !errors.Is(err, ErrTokenRejected) {
		t.Fatalf("wrong token error = %v, want ErrTokenRejected", err)
	}
	fake.mu.Lock()
	fake.down = true
	fake.mu.Unlock()
	err := client.VerifyToken(context.Background(), "cf-token")
	if err == nil || errors.Is(err, ErrTokenRejected) {
		t.Fatalf("Cloudflare down error = %v, want a transport error that is not a rejection", err)
	}
}

func TestVerifyTokenRefusesEmptyTokenWithoutCallingCloudflare(t *testing.T) {
	fake := newFakeCloudflare("cf-token")
	client, _ := testClient(t, fake)
	if err := client.VerifyToken(context.Background(), "  "); !errors.Is(err, ErrTokenRejected) {
		t.Fatalf("empty token error = %v, want ErrTokenRejected", err)
	}
	if got := len(fake.paths); got != 0 {
		t.Fatalf("requests = %d, want none for an empty token", got)
	}
}

func TestCallsWithoutTokenFailWithErrNoToken(t *testing.T) {
	fake := newFakeCloudflare("cf-token")
	server := fake.start(t)
	client := NewClient(server.URL, func() string { return "" })
	if client == nil {
		t.Fatal("NewClient returned nil")
	}
	if err := client.VerifyToken(context.Background(), "cf-token"); err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if _, _, err := client.Get(context.Background(), "config:provider"); !errors.Is(err, ErrNoToken) {
		t.Fatalf("Get without a token = %v, want ErrNoToken", err)
	}
	if err := client.Heartbeat(context.Background(), "https://x.trycloudflare.com", "v1"); !errors.Is(err, ErrNoToken) {
		t.Fatalf("Heartbeat without a token = %v, want ErrNoToken", err)
	}
}

func TestMemoryTokenAdoptIsVolatile(t *testing.T) {
	tokens := NewMemoryToken()
	if tokens.Get() != "" {
		t.Fatal("a fresh cache must be empty")
	}
	tokens.Adopt("  cf-token  ")
	if tokens.Get() != "cf-token" {
		t.Fatalf("cached token = %q, want the trimmed value", tokens.Get())
	}
	tokens.Clear()
	if tokens.Get() != "" {
		t.Fatal("Clear must leave the daemon with no credential")
	}
}

func TestHydrateAndPushConfig(t *testing.T) {
	fake := newFakeCloudflare("cf-token")
	client, _ := testClient(t, fake)
	state := t.TempDir()

	remote := "{\"providers\":[\"nous\"]}"
	if err := client.Put(context.Background(), "config:provider", remote); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := fake.state["config:provider"]; got != remote {
		t.Fatalf("stored value = %q, want %q", got, remote)
	}
	report, err := client.HydrateConfig(context.Background(), DefaultConfigFiles(state))
	if err != nil {
		t.Fatalf("HydrateConfig: %v", err)
	}
	if len(report.PulledConfig) != 1 || report.PulledConfig[0] != "config:provider" {
		t.Fatalf("pulled = %v, want config:provider", report.PulledConfig)
	}
	local, err := os.ReadFile(filepath.Join(state, "config", "provider.json"))
	if err != nil {
		t.Fatalf("read hydrated file: %v", err)
	}
	if string(local) != remote {
		t.Fatalf("hydrated file = %q, want %q", local, remote)
	}

	if err := os.WriteFile(filepath.Join(state, "config", "system.json"), []byte("{\"x\":1}"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err = client.PushConfig(context.Background(), DefaultConfigFiles(state))
	if err != nil {
		t.Fatalf("PushConfig: %v", err)
	}
	if len(report.PushedConfig) != 2 {
		t.Fatalf("pushed = %v, want provider + system", report.PushedConfig)
	}
	if got := fake.state["config:system"]; got != "{\"x\":1}" {
		t.Fatalf("pushed system = %q", got)
	}
}

func TestEntryConfigIsNeverSynced(t *testing.T) {
	files := DefaultConfigFiles(t.TempDir())
	for _, f := range files {
		if f.Key == "config:entry" {
			t.Fatal("config/entry.json must stay local: it is the gateway bootstrap")
		}
	}
	if len(files) == 0 {
		t.Fatal("no config files to sync")
	}
}

func TestHydrateKeepsLocalFileWhenD1HasNoCopy(t *testing.T) {
	fake := newFakeCloudflare("cf-token")
	client, _ := testClient(t, fake)
	state := t.TempDir()
	path := filepath.Join(state, "config", "system.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{\"enabled\":true}"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := client.HydrateConfig(context.Background(), DefaultConfigFiles(state))
	if err != nil {
		t.Fatalf("HydrateConfig: %v", err)
	}
	if len(report.PulledConfig) != 0 {
		t.Fatalf("pulled = %v, want nothing", report.PulledConfig)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "{\"enabled\":true}" {
		t.Fatalf("local file = %q (err %v), want it untouched", raw, err)
	}
}

func TestSessionRoundTrip(t *testing.T) {
	fake := newFakeCloudflare("cf-token")
	client, _ := testClient(t, fake)
	dir := t.TempDir()
	ctx := context.Background()

	db := filepath.Join(dir, base64.RawURLEncoding.EncodeToString([]byte("work-1"))+".db")
	if err := os.WriteFile(db, []byte("SQLite format 3\x00off"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := client.PushSession(ctx, "work-1", db)
	if err != nil {
		t.Fatalf("PushSession: %v", err)
	}
	if len(report.PushedSession) != 1 {
		t.Fatalf("pushed = %v, want work-1", report.PushedSession)
	}
	if _, ok := fake.state[SessionKeyPrefix+"work-1"]; !ok {
		t.Fatalf("no blob in state; keys = %v", fake.state)
	}

	target := t.TempDir()
	report, err = client.HydrateSessions(ctx, target, func(id string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(id)) + ".db"
	})
	if err != nil {
		t.Fatalf("HydrateSessions: %v", err)
	}
	if len(report.PulledSession) != 1 || report.PulledSession[0] != "work-1" {
		t.Fatalf("pulled = %v, want work-1", report.PulledSession)
	}
	raw, err := os.ReadFile(filepath.Join(target, base64.RawURLEncoding.EncodeToString([]byte("work-1"))+".db"))
	if err != nil {
		t.Fatalf("read restored db: %v", err)
	}
	if string(raw) != "SQLite format 3\x00off" {
		t.Fatalf("restored db = %q", raw)
	}
}

func TestOversizedValueIsSkippedNotTruncated(t *testing.T) {
	fake := newFakeCloudflare("cf-token")
	client, _ := testClient(t, fake)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, maxValueBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	if err := os.WriteFile(filepath.Join(dir, "config", "provider.json"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := client.PushConfig(context.Background(), DefaultConfigFiles(dir))
	if err != nil {
		t.Fatalf("PushConfig: %v", err)
	}
	if len(report.SkippedOversize) != 1 {
		t.Fatalf("skipped = %v, want the oversize file reported", report.SkippedOversize)
	}
	if len(fake.state) != 0 {
		t.Fatalf("state = %v, want nothing written", fake.state)
	}
}

func TestAppendTurnKeepsFIFOOrderAndNamesTheChat(t *testing.T) {
	fake := newFakeCloudflare("cf-token")
	client, _ := testClient(t, fake)
	ctx := context.Background()

	if err := client.AppendTurn(ctx, "work-9", "user", "user", "", "สวัสดี daemon"); err != nil {
		t.Fatalf("AppendTurn user: %v", err)
	}
	if err := client.AppendTurn(ctx, "work-9", "model", "main", "job-1", "ตอบแล้ว"); err != nil {
		t.Fatalf("AppendTurn model: %v", err)
	}
	turns, err := client.Turns(ctx, "work-9", 0, 50)
	if err != nil {
		t.Fatalf("Turns: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(turns))
	}
	if turns[0].Role != "user" || turns[1].Role != "model" {
		t.Fatalf("order = %+v, want user then model", turns)
	}
	if turns[0].Seq >= turns[1].Seq {
		t.Fatalf("seq = %d,%d, want ascending", turns[0].Seq, turns[1].Seq)
	}
	session, found, err := client.GetSession(ctx, "work-9")
	if err != nil || !found {
		t.Fatalf("GetSession: found=%v err=%v", found, err)
	}
	if session.Title != "สวัสดี daemon" {
		t.Fatalf("title = %q, want the first user message", session.Title)
	}
}

func TestRenameAndDeleteSession(t *testing.T) {
	fake := newFakeCloudflare("cf-token")
	client, _ := testClient(t, fake)
	ctx := context.Background()
	if _, err := client.CreateSession(ctx, "work-2", "เดิม", ""); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	row, found, err := client.RenameSession(ctx, "work-2", "ใหม่")
	if err != nil || !found {
		t.Fatalf("RenameSession: found=%v err=%v", found, err)
	}
	if row.Title != "ใหม่" {
		t.Fatalf("title = %q, want ใหม่", row.Title)
	}
	if err := client.AppendTurn(ctx, "work-2", "user", "user", "", "หัวข้อเดิม"); err != nil {
		t.Fatal(err)
	}
	removed, err := client.DeleteSession(ctx, "work-2")
	if err != nil || !removed {
		t.Fatalf("DeleteSession: removed=%v err=%v", removed, err)
	}
	if _, found, _ := client.GetSession(ctx, "work-2"); found {
		t.Fatal("session survived the delete")
	}
	turns, err := client.Turns(ctx, "work-2", 0, 50)
	if err != nil {
		t.Fatalf("Turns: %v", err)
	}
	if len(turns) != 0 {
		t.Fatalf("turns = %d, want them gone with the chat", len(turns))
	}
}

func TestHeartbeatWritesTheNodeRow(t *testing.T) {
	fake := newFakeCloudflare("cf-token")
	client, _ := testClient(t, fake)
	ctx := context.Background()
	url := "https://quiet-fog-lands.trycloudflare.com"
	if err := client.Heartbeat(ctx, url, "one-url"); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	node, found, err := client.Node(ctx)
	if err != nil || !found {
		t.Fatalf("Node: found=%v err=%v", found, err)
	}
	if node.TunnelURL != url || node.Version != "one-url" {
		t.Fatalf("node = %+v, want %s", node, url)
	}
}

func TestListOnlyReturnsKeysUnderThePrefix(t *testing.T) {
	fake := newFakeCloudflare("cf-token")
	client, _ := testClient(t, fake)
	ctx := context.Background()
	for key, value := range map[string]string{
		"config:provider":   "{}",
		"sessions/work-1":   "blob-1",
		"sessions/work-10":  "blob-10",
		"sessions/other-id": "blob-2",
	} {
		if err := client.Put(ctx, key, value); err != nil {
			t.Fatal(err)
		}
	}
	keys, err := client.List(ctx, SessionKeyPrefix)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("keys = %v, want the three session blobs", keys)
	}
	for _, key := range keys {
		if key[:len(SessionKeyPrefix)] != SessionKeyPrefix {
			t.Fatalf("key %q escaped the prefix", key)
		}
	}
}

func TestSetDatabaseNamePicksTheNamedDatabase(t *testing.T) {
	fake := newFakeCloudflare("cf-token")
	fake.databases = append(fake.databases, map[string]string{"uuid": "db-2", "name": "other"})
	client, _ := testClient(t, fake)
	client.SetDatabaseName("other")
	if err := client.VerifyToken(context.Background(), "cf-token"); err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	target, _ := client.ResolvedTarget()
	if target.DatabaseID != "db-2" {
		t.Fatalf("database = %q, want db-2 (the configured name)", target.DatabaseID)
	}
}

func TestQueriesCarryTheBoundParameters(t *testing.T) {
	fake := newFakeCloudflare("cf-token")
	client, _ := testClient(t, fake)
	if err := client.Put(context.Background(), "config:system", "{}"); err != nil {
		t.Fatal(err)
	}
	q, ok := fake.sqlContaining("INSERT INTO state(key, value")
	if !ok {
		t.Fatal("no INSERT INTO state was sent")
	}
	if len(q.params) != 2 || q.params[0] != "config:system" || q.params[1] != "{}" {
		t.Fatalf("params = %v, want the key and value bound", q.params)
	}
	if q.path != "/accounts/acct-1/d1/database/db-1/query" {
		t.Fatalf("path = %q, want the resolved account and database", q.path)
	}
}

func TestFirstUserMessageNamesTheChatWithoutBreakingThaiText(t *testing.T) {
	fake := newFakeCloudflare("cf-token")
	client, _ := testClient(t, fake)
	ctx := context.Background()
	long := strings.Repeat("ก", 60) // three bytes each
	if err := client.AppendTurn(ctx, "work-utf8", "user", "user", "", long); err != nil {
		t.Fatal(err)
	}
	session, found, err := client.GetSession(ctx, "work-utf8")
	if err != nil || !found {
		t.Fatalf("GetSession: found=%v err=%v", found, err)
	}
	if !utf8.ValidString(session.Title) {
		t.Fatalf("title %q is not valid UTF-8 (byte slicing cut a rune)", session.Title)
	}
	if n := utf8.RuneCountInString(session.Title); n != 42 {
		t.Fatalf("title has %d runes, want 42", n)
	}
}

// A model turn keeps the footer data the phone draws under that message, and
// history hands it back (AX-095).
func TestModelTurnKeepsItsFooter(t *testing.T) {
	fake := newFakeCloudflare("cf-token")
	c, _ := testClient(t, fake)
	ctx := context.Background()
	if _, err := c.AppendTurnAt(ctx, "s1", "user", "user", "", "งาน"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AppendModelTurn(ctx, "s1", "main", "", "pong", TurnMeta{
		Model: "nemotron-3-ultra-free", InputTokens: 2269, OutputTokens: 51,
		CacheRead: 1800, CacheWrite: 12, DurationMs: 4000,
	}); err != nil {
		t.Fatal(err)
	}
	turns, err := c.Turns(ctx, "s1", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2", len(turns))
	}
	answer := turns[1]
	if answer.Model != "nemotron-3-ultra-free" || answer.InputTokens != 2269 ||
		answer.OutputTokens != 51 || answer.CacheRead != 1800 || answer.CacheWrite != 12 ||
		answer.DurationMs != 4000 {
		t.Fatalf("footer lost on the way back: %+v", answer)
	}
	if turns[0].Model != "" {
		t.Errorf("a user turn grew a model: %q", turns[0].Model)
	}
}

// The footer columns are added once and only when they are missing, so a
// database that already has them is left alone (AX-095).
func TestEnsureTurnFooterAddsColumnsOnce(t *testing.T) {
	fake := newFakeCloudflare("cf-token")
	c, _ := testClient(t, fake)
	ctx := context.Background()
	if err := c.EnsureTurnFooter(ctx); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if len(fake.extraTurnColumns) != 8 {
		t.Fatalf("added %v, want the eight footer columns", fake.extraTurnColumns)
	}
	if err := c.EnsureTurnFooter(ctx); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(fake.extraTurnColumns) != 8 {
		t.Fatalf("a second run added %v again", fake.extraTurnColumns)
	}
}

func TestSetSessionSubAgentRoundTrip(t *testing.T) {
	fake := newFakeCloudflare("cf-token")
	client, _ := testClient(t, fake)
	ctx := context.Background()

	if err := client.SetSessionRoute(ctx, "chat-1", "Opencode", "mimo-v2.5-free"); err != nil {
		t.Fatal(err)
	}
	if err := client.SetSessionSubAgent(ctx, "chat-1", "AgentRouter", "mimo-v2.5-free", boolPtr(true), true); err != nil {
		t.Fatal(err)
	}
	row, found, err := client.GetSession(ctx, "chat-1")
	if err != nil || !found {
		t.Fatalf("GetSession: found=%v err=%v", found, err)
	}
	if row.SubProvider != "AgentRouter" || row.SubModel != "mimo-v2.5-free" || row.SubEnabled != 1 {
		t.Fatalf("sub = %s/%s/%d, want AgentRouter/mimo-v2.5-free/1", row.SubProvider, row.SubModel, row.SubEnabled)
	}

	// Turning the flag off without a route must keep the stored route: the
	// session falls back to the agent's global sub-agent only when the route
	// is empty, and the flag is the per-session switch on top of it.
	if err := client.SetSessionSubAgent(ctx, "chat-1", "", "", boolPtr(false), false); err != nil {
		t.Fatal(err)
	}
	row, _, err = client.GetSession(ctx, "chat-1")
	if err != nil {
		t.Fatal(err)
	}
	if row.SubProvider != "AgentRouter" || row.SubModel != "mimo-v2.5-free" || row.SubEnabled != 0 {
		t.Fatalf("flag-only sub = %s/%s/%d, want the route kept and 0", row.SubProvider, row.SubModel, row.SubEnabled)
	}

	if err := client.SetSessionSubAgent(ctx, "chat-1", "", "", boolPtr(false), true); err != nil {
		t.Fatal(err)
	}
	row, _, err = client.GetSession(ctx, "chat-1")
	if err != nil {
		t.Fatal(err)
	}
	if row.SubProvider != "" || row.SubModel != "" || row.SubEnabled != 0 {
		t.Fatalf("cleared sub = %s/%s/%d, want empty/0", row.SubProvider, row.SubModel, row.SubEnabled)
	}
}

func boolPtr(b bool) *bool { return &b }
