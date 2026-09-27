// Package d1store keeps the daemon's runtime state in Cloudflare D1 and serves
// the phone's history endpoints from it (REQ-046, CON-012).
//
// D1 is the authoritative copy; the local files stay exactly what the rest of
// the runtime already reads and writes (config/*.json and one SQLite file per
// session under data/sessions/). Hydrate pulls the cloud copy down, Push sends
// local changes up. A daemon that loses its whole state directory can still
// come back, as long as one phone connects and hands over a Cloudflare token.
//
// The daemon owns no credential. The token a phone presents in a verified
// Authorization header is kept in memory by the transport and is empty until
// the first successful connection (CON-012). That token is an ordinary Cloudflare
// API token, so the daemon discovers the account and the database from it and
// talks to D1 through the documented REST API — no Worker, no second secret.
package d1store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrTokenRejected marks a wrong credential, as opposed to a transport or
// configuration problem. Callers count only the former as failed logins.
var ErrTokenRejected = errors.New("d1store: token rejected")

// ErrNoToken means nobody has connected yet: the daemon holds no credential of
// its own, so D1 is unreachable until a phone presents one.
var ErrNoToken = errors.New("d1store: no Cloudflare token yet (waiting for a phone)")

const (
	// DefaultAPIBase is Cloudflare's REST root; the whole daemon config is one
	// base URL plus the database name to pick.
	DefaultAPIBase = "https://api.cloudflare.com/client/v4"
	// DefaultDatabaseName is used when the account holds exactly one D1
	// database, so a fresh install needs no ids typed by hand.
	DefaultDatabaseName = "aixodia"
	defaultTimeout      = 30 * time.Second
	// D1 refuses a single bound value above 1 MB; stay well under it so a
	// session blob never gets halfway through and fails the whole batch.
	maxValueBytes = 900_000
	// SessionKeyPrefix is the state-key namespace for session databases.
	SessionKeyPrefix = "sessions/"
)

// Target is the account and database the daemon resolved from a token.
type Target struct {
	AccountID  string
	DatabaseID string
	Name       string
}

// Client talks to Cloudflare's REST API with the token a phone handed over. It
// is dependency-free so the runtime keeps its own HTTP conventions.
type Client struct {
	apiBase  string
	database string
	http     *http.Client
	tokenFn  func() string

	mu       sync.RWMutex
	target   Target
	owner    string // token the cached target was resolved with
	resolved bool
}

// NewClient returns nil when no API base is configured, which is the daemon's
// way of saying "this deployment does not talk to D1".
func NewClient(apiBase string, tokenFn func() string) *Client {
	apiBase = strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if apiBase == "" {
		return nil
	}
	if tokenFn == nil {
		tokenFn = func() string { return "" }
	}
	return &Client{apiBase: apiBase, http: &http.Client{Timeout: defaultTimeout}, tokenFn: tokenFn}
}

// SetDatabaseName pins which D1 database to use when the account has several.
// Empty keeps the default: prefer DefaultDatabaseName, else the only database.
func (c *Client) SetDatabaseName(name string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.database = strings.TrimSpace(name)
	c.resolved = false
	c.target = Target{}
	c.owner = ""
	c.mu.Unlock()
}

func (c *Client) do(ctx context.Context, token, method, path string, body []byte, out any) error {
	if c == nil {
		return errors.New("d1store: client is not configured")
	}
	if strings.TrimSpace(token) == "" {
		return ErrNoToken
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "ai")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w (HTTP %d)", ErrTokenRejected, resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return fmt.Errorf("d1store: %s %s -> HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

type cfEnvelope struct {
	Success bool `json:"success"`
	Result  []struct {
		Success bool              `json:"success"`
		Results []json.RawMessage `json:"results"`
		Meta    struct {
			Changes   int   `json:"changes"`
			LastRowID int64 `json:"last_row_id"`
		} `json:"meta"`
	} `json:"result"`
	Errors []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

// queryResult is what one D1 statement returned: rows, rows changed, and the
// rowid of the last inserted row (the only handle a caller has on a turn's id).
type queryResult struct {
	rows      []json.RawMessage
	changes   int
	lastRowID int64
}

// query runs one SQL statement against the resolved database.
func (c *Client) query(ctx context.Context, sql string, params []string) (queryResult, error) {
	token := c.CurrentToken()
	target, err := c.ensureTarget(ctx, token)
	if err != nil {
		return queryResult{}, err
	}
	if params == nil {
		params = []string{}
	}
	body, err := json.Marshal(map[string]any{"sql": sql, "params": params})
	if err != nil {
		return queryResult{}, err
	}
	var env cfEnvelope
	path := fmt.Sprintf("/accounts/%s/d1/database/%s/query", target.AccountID, target.DatabaseID)
	if err := c.do(ctx, token, http.MethodPost, path, body, &env); err != nil {
		return queryResult{}, err
	}
	if !env.Success {
		return queryResult{}, cfFailure(env.Errors)
	}
	if len(env.Result) == 0 {
		return queryResult{}, nil
	}
	return queryResult{
		rows:      env.Result[0].Results,
		changes:   env.Result[0].Meta.Changes,
		lastRowID: env.Result[0].Meta.LastRowID,
	}, nil
}

func cfFailure(errs []struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}) error {
	if len(errs) == 0 {
		return errors.New("d1store: cloudflare reported a failure without a message")
	}
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		parts = append(parts, fmt.Sprintf("%d %s", e.Code, e.Message))
	}
	return errors.New("d1store: " + strings.Join(parts, "; "))
}

func queryInto[T any](rows []json.RawMessage) ([]T, error) {
	out := make([]T, 0, len(rows))
	for _, row := range rows {
		var item T
		if err := json.Unmarshal(row, &item); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

// VerifyToken checks a candidate Cloudflare API token a phone presented and
// resolves the account and database it can reach. Only a bad credential returns
// ErrTokenRejected; a valid token that cannot see a database is reported as a
// plain error, so the gate treats it as "cannot check" instead of a wrong guess.
func (c *Client) VerifyToken(ctx context.Context, token string) error {
	if c == nil {
		return errors.New("d1store: client is not configured")
	}
	if strings.TrimSpace(token) == "" {
		return ErrTokenRejected
	}
	var out struct {
		Success bool `json:"success"`
		Result  struct {
			Status string `json:"status"`
		} `json:"result"`
	}
	if err := c.do(ctx, token, http.MethodGet, "/user/tokens/verify", nil, &out); err != nil {
		return err
	}
	if !out.Success || (out.Result.Status != "" && out.Result.Status != "active") {
		return fmt.Errorf("%w (status %q)", ErrTokenRejected, out.Result.Status)
	}
	_, err := c.resolve(ctx, token)
	return err
}

// CurrentToken reports the token the transport currently holds ("" = none).
func (c *Client) CurrentToken() string {
	if c == nil || c.tokenFn == nil {
		return ""
	}
	return c.tokenFn()
}

type accountRow struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type databaseRow struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

// resolve turns a token into the account and database to use, and caches the
// answer per token: a rotated token must be resolved again.
func (c *Client) resolve(ctx context.Context, token string) (Target, error) {
	if strings.TrimSpace(token) == "" {
		return Target{}, ErrNoToken
	}
	c.mu.RLock()
	cached, owner, ok := c.target, c.owner, c.resolved
	c.mu.RUnlock()
	if ok && owner == token {
		return cached, nil
	}

	var accounts struct {
		Success bool         `json:"success"`
		Result  []accountRow `json:"result"`
	}
	if err := c.do(ctx, token, http.MethodGet, "/accounts", nil, &accounts); err != nil {
		return Target{}, err
	}
	if !accounts.Success || len(accounts.Result) == 0 {
		return Target{}, errors.New("d1store: the token cannot see any Cloudflare account")
	}
	account := accounts.Result[0].ID
	if len(accounts.Result) > 1 {
		c.mu.RLock()
		wanted := c.database
		c.mu.RUnlock()
		for _, a := range accounts.Result {
			if wanted != "" && a.Name == wanted {
				account = a.ID
			}
		}
	}

	var databases struct {
		Success bool          `json:"success"`
		Result  []databaseRow `json:"result"`
	}
	if err := c.do(ctx, token, http.MethodGet, "/accounts/"+account+"/d1/database", nil, &databases); err != nil {
		return Target{}, err
	}
	if !databases.Success || len(databases.Result) == 0 {
		return Target{}, fmt.Errorf("d1store: account %s has no D1 database the token can see", account)
	}
	c.mu.RLock()
	wanted := c.database
	if wanted == "" {
		wanted = DefaultDatabaseName
	}
	c.mu.RUnlock()
	chosen := databases.Result[0]
	if len(databases.Result) > 1 {
		found := false
		for _, db := range databases.Result {
			if db.Name == wanted {
				chosen, found = db, true
			}
		}
		if !found {
			names := make([]string, 0, len(databases.Result))
			for _, db := range databases.Result {
				names = append(names, db.Name)
			}
			return Target{}, fmt.Errorf("d1store: set d1_database in entry.json; this account has %s", strings.Join(names, ", "))
		}
	}
	target := Target{AccountID: account, DatabaseID: chosen.UUID, Name: chosen.Name}
	c.mu.Lock()
	c.target, c.owner, c.resolved = target, token, true
	c.mu.Unlock()
	return target, nil
}

func (c *Client) ensureTarget(ctx context.Context, token string) (Target, error) {
	if c == nil {
		return Target{}, errors.New("d1store: client is not configured")
	}
	return c.resolve(ctx, token)
}

// ResolvedTarget reports the account and database currently in use, for the log
// line that says which D1 this daemon ended up on.
func (c *Client) ResolvedTarget() (Target, bool) {
	if c == nil {
		return Target{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.target, c.resolved
}

// Get returns one state value. found=false means the key does not exist.
func (c *Client) Get(ctx context.Context, key string) (value string, found bool, err error) {
	res, err := c.query(ctx, "SELECT value FROM state WHERE key = ?", []string{key})
	if err != nil {
		return "", false, err
	}
	if len(res.rows) == 0 {
		return "", false, nil
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(res.rows[0], &out); err != nil {
		return "", false, err
	}
	return out.Value, true, nil
}

// Put stores a state value, refusing oversized payloads instead of letting D1
// reject them mid-turn (CON-012).
func (c *Client) Put(ctx context.Context, key, value string) error {
	if len(value) > maxValueBytes {
		return fmt.Errorf("d1store: %q is %d bytes, over the %d byte limit", key, len(value), maxValueBytes)
	}
	_, err := c.query(ctx,
		`INSERT INTO state(key, value, updated_at) VALUES(?, ?, unixepoch())
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = unixepoch()`,
		[]string{key, value})
	return err
}

// List returns keys under a prefix, e.g. SessionKeyPrefix.
func (c *Client) List(ctx context.Context, prefix string) ([]string, error) {
	res, err := c.query(ctx,
		"SELECT key FROM state WHERE key >= ? AND key < ? ORDER BY key LIMIT 200",
		[]string{prefix, prefix + string(rune(0x10FFFF))})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(res.rows))
	for _, row := range res.rows {
		var item struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(row, &item); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(item.Key, prefix) {
			continue
		}
		out = append(out, item.Key)
	}
	return out, nil
}

// DeleteState removes one state key, used when a chat is deleted.
func (c *Client) DeleteState(ctx context.Context, key string) error {
	_, err := c.query(ctx, "DELETE FROM state WHERE key = ?", []string{key})
	return err
}

// Session is one row of the history table the phone renders.
type Session struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	SubProvider string `json:"sub_provider"`
	SubModel    string `json:"sub_model"`
	// SubEnabled is -1 when the session does not override the agent's global
	// sub-agent switch, 0 off, 1 on.
	SubEnabled int   `json:"sub_enabled"`
	CreatedAt  int64 `json:"created_at"`
	UpdatedAt  int64 `json:"updated_at"`
}

// Turn is one history row, ordered by seq within a session.
type Turn struct {
	Seq       int64  `json:"seq"`
	Role      string `json:"role"`
	Agent     string `json:"agent"`
	JobID     string `json:"job_id"`
	Text      string `json:"text"`
	CreatedAt int64  `json:"created_at"`
	// Footer of a model turn: which model answered, what it cost and how long
	// it took, so the phone can draw it under that message and keep it after a
	// restart (AX-095).
	Model        string `json:"model"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	CacheRead    int    `json:"cache_read_tokens"`
	CacheWrite   int    `json:"cache_write_tokens"`
	DurationMs   int64  `json:"duration_ms"`
}

// Node is the quick-tunnel announcement the phone reads to find the daemon.
type Node struct {
	TunnelURL string `json:"tunnel_url"`
	Version   string `json:"version"`
	Heartbeat int64  `json:"heartbeat"`
}

// truncateRunes cuts a title on a character boundary: slicing bytes would leave
// a half-written rune in the chat title, which every client renders as ���.
func truncateRunes(text string, limit int) string {
	count := 0
	for i := range text {
		if count == limit {
			return text[:i]
		}
		count++
	}
	return text
}

const sessionColumns = "id, title, provider, model, sub_provider, sub_model, sub_enabled, created_at, updated_at"

// ListSessions returns the newest chats, the order the phone shows them in.
func (c *Client) ListSessions(ctx context.Context, limit int) ([]Session, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	res, err := c.query(ctx, "SELECT "+sessionColumns+" FROM sessions ORDER BY updated_at DESC LIMIT ?", []string{fmt.Sprint(limit)})
	if err != nil {
		return nil, err
	}
	return queryInto[Session](res.rows)
}

// GetSession returns one chat.
func (c *Client) GetSession(ctx context.Context, id string) (Session, bool, error) {
	res, err := c.query(ctx, "SELECT "+sessionColumns+" FROM sessions WHERE id = ?", []string{id})
	if err != nil {
		return Session{}, false, err
	}
	list, err := queryInto[Session](res.rows)
	if err != nil || len(list) == 0 {
		return Session{}, false, err
	}
	return list[0], true, nil
}

// CreateSession inserts a chat if it is new and returns the stored row.
func (c *Client) CreateSession(ctx context.Context, id, title, model string) (Session, error) {
	if strings.TrimSpace(id) == "" {
		return Session{}, errors.New("d1store: session id is required")
	}
	if title == "" {
		title = id
	}
	_, err := c.query(ctx,
		`INSERT OR IGNORE INTO sessions(id, title, model, created_at, updated_at)
		 VALUES(?, ?, ?, unixepoch(), unixepoch())`,
		[]string{id, title, model})
	if err != nil {
		return Session{}, err
	}
	session, found, err := c.GetSession(ctx, id)
	if err != nil {
		return Session{}, err
	}
	if !found {
		return Session{ID: id, Title: title, Model: model}, nil
	}
	return session, nil
}

// RenameSession sets a chat title. found=false means no such chat.
func (c *Client) RenameSession(ctx context.Context, id, title string) (Session, bool, error) {
	res, err := c.query(ctx,
		"UPDATE sessions SET title = ?, updated_at = unixepoch() WHERE id = ?", []string{title, id})
	if err != nil {
		return Session{}, false, err
	}
	if res.changes == 0 {
		return Session{}, false, nil
	}
	session, found, err := c.GetSession(ctx, id)
	return session, found, err
}

// SetSessionSubAgent stores the per-session sub-agent override. Empty fields
// clear the pin so the session follows the global agent defaults again.
// subEnabled == nil leaves the stored flag untouched.
// SetSessionSubAgent stores the per-session sub-agent override. writeRoute is
// false for a request that only flips subEnabled: the route columns then keep
// whatever the session already had (an empty route means "use the agent's
// global sub-agent"), so enabling the sub-agent does not erase a stored route.
func (c *Client) SetSessionSubAgent(ctx context.Context, sessionID, subProvider, subModel string, subEnabled *bool, writeRoute bool) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("d1store: session id is required")
	}
	if _, err := c.query(ctx,
		"INSERT OR IGNORE INTO sessions(id, title, sub_enabled, created_at, updated_at) VALUES(?, ?, -1, unixepoch(), unixepoch())",
		[]string{sessionID, sessionID}); err != nil {
		return err
	}
	if writeRoute {
		if _, err := c.query(ctx,
			"UPDATE sessions SET sub_provider = ?, sub_model = ?, updated_at = unixepoch() WHERE id = ?",
			[]string{subProvider, subModel, sessionID}); err != nil {
			return err
		}
	} else if subEnabled == nil {
		return nil
	}
	if subEnabled != nil {
		enabled := 0
		if *subEnabled {
			enabled = 1
		}
		if _, err := c.query(ctx,
			"UPDATE sessions SET sub_enabled = ?, updated_at = unixepoch() WHERE id = ?",
			[]string{strconv.Itoa(enabled), sessionID}); err != nil {
			return err
		}
	}
	return nil
}

// SetSessionRoute remembers which provider and model a chat runs on, so the
// phone's choice survives a restart and a second device. The chat is created if
// the daemon sees it before the phone does.
func (c *Client) SetSessionRoute(ctx context.Context, sessionID, provider, model string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("d1store: session id is required")
	}
	if _, err := c.query(ctx,
		"INSERT OR IGNORE INTO sessions(id, title, sub_enabled, created_at, updated_at) VALUES(?, ?, -1, unixepoch(), unixepoch())",
		[]string{sessionID, sessionID}); err != nil {
		return err
	}
	_, err := c.query(ctx,
		"UPDATE sessions SET provider = ?, model = ?, updated_at = unixepoch() WHERE id = ?",
		[]string{provider, model, sessionID})
	return err
}

// DeleteSession removes a chat with its turns and its D1 session blob.
func (c *Client) DeleteSession(ctx context.Context, id string) (bool, error) {
	if _, err := c.query(ctx, "DELETE FROM turns WHERE session_id = ?", []string{id}); err != nil {
		return false, err
	}
	if err := c.DeleteState(ctx, SessionKeyPrefix+id); err != nil {
		return false, err
	}
	res, err := c.query(ctx, "DELETE FROM sessions WHERE id = ?", []string{id})
	if err != nil {
		return false, err
	}
	return res.changes > 0, nil
}

// Turns returns history rows in ascending seq order; beforeSeq == 0 means "from
// the newest backwards", which is what the phone's paged list asks for.
func (c *Client) Turns(ctx context.Context, sessionID string, beforeSeq int64, limit int) ([]Turn, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	if beforeSeq <= 0 {
		beforeSeq = 1<<62 - 1
	}
	res, err := c.query(ctx,
		`SELECT seq, role, agent, job_id, text, created_at, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, duration_ms FROM turns
		 WHERE session_id = ? AND seq < ? ORDER BY seq DESC LIMIT ?`,
		[]string{sessionID, fmt.Sprint(beforeSeq), fmt.Sprint(limit)})
	if err != nil {
		return nil, err
	}
	turns, err := queryInto[Turn](res.rows)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(turns)-1; i < j; i, j = i+1, j-1 {
		turns[i], turns[j] = turns[j], turns[i]
	}
	return turns, nil
}

// AppendTurn mirrors one finished turn into the history table the phone reads.
// order=0 means "append at the end"; positive values keep FIFO ingestion when
// several turns are flushed together.
func (c *Client) AppendTurn(ctx context.Context, sessionID, role, agent, jobID, text string) error {
	_, err := c.AppendTurnAt(ctx, sessionID, role, agent, jobID, text)
	return err
}

// TurnMeta is the part of a turn the phone shows in the message footer.
type TurnMeta struct {
	Model        string
	InputTokens  int
	OutputTokens int
	CacheRead    int
	CacheWrite   int
	DurationMs   int64
}

// AppendModelTurn mirrors one answered turn together with the footer data, and
// returns its seq. A user turn carries none of it.
func (c *Client) AppendModelTurn(ctx context.Context, sessionID, agent, jobID, text string, meta TurnMeta) (int64, error) {
	return c.appendTurn(ctx, sessionID, "model", agent, jobID, text, meta)
}

// AppendTurnAt appends one turn and returns its seq. A user turn that is the
// first message names the session, the way every messenger does.
func (c *Client) AppendTurnAt(ctx context.Context, sessionID, role, agent, jobID, text string) (int64, error) {
	return c.appendTurn(ctx, sessionID, role, agent, jobID, text, TurnMeta{})
}

func (c *Client) appendTurn(ctx context.Context, sessionID, role, agent, jobID, text string, meta TurnMeta) (int64, error) {
	if strings.TrimSpace(sessionID) == "" {
		return 0, errors.New("d1store: session id is required")
	}
	if _, err := c.query(ctx,
		"INSERT OR IGNORE INTO sessions(id, title, sub_enabled, created_at, updated_at) VALUES(?, ?, -1, unixepoch(), unixepoch())",
		[]string{sessionID, sessionID}); err != nil {
		return 0, err
	}
	if role == "user" {
		res, err := c.query(ctx, "SELECT title FROM sessions WHERE id = ?", []string{sessionID})
		if err != nil {
			return 0, err
		}
		titles, err := queryInto[struct {
			Title string `json:"title"`
		}](res.rows)
		if err != nil {
			return 0, err
		}
		if len(titles) == 1 && (titles[0].Title == "" || titles[0].Title == sessionID) {
			name := truncateRunes(text, 42)
			if name == "" {
				name = sessionID
			}
			if _, err := c.query(ctx, "UPDATE sessions SET title = ? WHERE id = ?", []string{name, sessionID}); err != nil {
				return 0, err
			}
		}
	}
	// The next seq is read inside the same statement so a turn can never land on
	// a duplicate (session_id, seq) when two phones finish at the same moment.
	inserted, err := c.query(ctx,
		`INSERT INTO turns(session_id, seq, role, agent, job_id, text, created_at, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, duration_ms)
		 SELECT ?, COALESCE((SELECT MAX(seq) + 1 FROM turns WHERE session_id = ?), 1), ?, ?, ?, ?, unixepoch(), ?, ?, ?, ?, ?, ?`,
		[]string{sessionID, sessionID, role, agent, jobID, text, meta.Model,
			strconv.Itoa(meta.InputTokens), strconv.Itoa(meta.OutputTokens),
			strconv.Itoa(meta.CacheRead), strconv.Itoa(meta.CacheWrite), strconv.FormatInt(meta.DurationMs, 10)})
	if err != nil {
		return 0, err
	}
	if _, err := c.query(ctx, "UPDATE sessions SET updated_at = unixepoch() WHERE id = ?", []string{sessionID}); err != nil {
		return 0, err
	}
	if inserted.lastRowID == 0 {
		return 0, nil
	}
	rows, err := c.query(ctx, "SELECT seq FROM turns WHERE id = ?", []string{fmt.Sprint(inserted.lastRowID)})
	if err != nil || len(rows.rows) == 0 {
		return 0, err
	}
	var seq struct {
		Seq int64 `json:"seq"`
	}
	if err := json.Unmarshal(rows.rows[0], &seq); err != nil {
		return 0, err
	}
	return seq.Seq, nil
}

// Heartbeat announces the public quick-tunnel URL so the phone can discover
// this daemon through the same D1 it already has a token for.
func (c *Client) Heartbeat(ctx context.Context, tunnelURL, version string) error {
	_, err := c.query(ctx,
		`INSERT INTO nodes(id, tunnel_url, version, heartbeat) VALUES('ai', ?, ?, unixepoch())
		 ON CONFLICT(id) DO UPDATE SET tunnel_url = excluded.tunnel_url,
		   version = excluded.version, heartbeat = unixepoch()`,
		[]string{tunnelURL, version})
	return err
}

// Node returns the current announcement, or found=false when the daemon has
// never reported in.
func (c *Client) Node(ctx context.Context) (Node, bool, error) {
	res, err := c.query(ctx, "SELECT tunnel_url, version, heartbeat FROM nodes WHERE id = 'ai'", nil)
	if err != nil {
		return Node{}, false, err
	}
	list, err := queryInto[Node](res.rows)
	if err != nil || len(list) == 0 {
		return Node{}, false, err
	}
	return list[0], true, nil
}

// MemoryToken stores the Cloudflare token in RAM. It is the daemon's only
// credential and it is intentionally volatile: a restart requires a phone to
// hand it over again (REQ-046(3), CON-012).
type MemoryToken struct{ value atomic.Value }

func NewMemoryToken() *MemoryToken {
	t := &MemoryToken{}
	t.value.Store("")
	return t
}

func (m *MemoryToken) Get() string {
	if m == nil {
		return ""
	}
	v, _ := m.value.Load().(string)
	return v
}

// Adopt records a token only after the caller verified it with Cloudflare.
func (m *MemoryToken) Adopt(token string) {
	if m == nil || strings.TrimSpace(token) == "" {
		return
	}
	m.value.Store(strings.TrimSpace(token))
}

func (m *MemoryToken) Clear() {
	if m != nil {
		m.value.Store("")
	}
}

// turnFooterColumns are the columns an answered turn needs for the footer the
// phone draws under that message (AX-095).
var turnFooterColumns = []struct{ name, ddl string }{
	{"model", "ALTER TABLE turns ADD COLUMN model TEXT NOT NULL DEFAULT ''"},
	{"input_tokens", "ALTER TABLE turns ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0"},
	{"output_tokens", "ALTER TABLE turns ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0"},
	{"cache_read_tokens", "ALTER TABLE turns ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0"},
	{"cache_write_tokens", "ALTER TABLE turns ADD COLUMN cache_write_tokens INTEGER NOT NULL DEFAULT 0"},
	{"duration_ms", "ALTER TABLE turns ADD COLUMN duration_ms INTEGER NOT NULL DEFAULT 0"},
}

// subAgentColumns are the per-session sub-agent columns (CHANGE-085). The
// default is -1, "not set", so a row that predates them, or a fresh one, falls
// back to the agent's global sub-agent instead of reading as disabled.
var subAgentColumns = []struct{ name, ddl string }{
	{"sub_provider", "ALTER TABLE sessions ADD COLUMN sub_provider TEXT NOT NULL DEFAULT ''"},
	{"sub_model", "ALTER TABLE sessions ADD COLUMN sub_model TEXT NOT NULL DEFAULT ''"},
	{"sub_enabled", "ALTER TABLE sessions ADD COLUMN sub_enabled INTEGER NOT NULL DEFAULT -1"},
}

// EnsureSubAgentColumns adds the per-session sub-agent columns when they are
// missing, and repairs a column an earlier manual migration created with a
// default that reads as "off": sub_enabled is a three-state value, so an empty
// text default would fail the int scan. It runs once, next to
// EnsureTurnFooter, after the token that reaches the database is verified.
func (c *Client) EnsureSubAgentColumns(ctx context.Context) error {
	if c == nil {
		return errors.New("d1store: client is not configured")
	}
	have := map[string]string{}
	res, err := c.query(ctx, "PRAGMA table_info(sessions)", nil)
	if err != nil {
		return err
	}
	rows, err := queryInto[struct {
		Name         string `json:"name"`
		DefaultValue string `json:"dflt_value"`
	}](res.rows)
	if err != nil {
		return err
	}
	for _, row := range rows {
		have[row.Name] = row.DefaultValue
	}
	for _, column := range subAgentColumns {
		if _, ok := have[column.name]; !ok {
			if _, err := c.query(ctx, column.ddl, nil); err != nil {
				return fmt.Errorf("d1store: add sessions.%s: %w", column.name, err)
			}
			continue
		}
		// Present but with a default that cannot be read as the three-state
		// value: normalise the stored rows and rebuild the column default.
		if column.name == "sub_enabled" && !isNumericSQLDefault(have[column.name]) {
			if _, err := c.query(ctx, "UPDATE sessions SET sub_enabled = -1 WHERE sub_enabled IS NULL OR CAST(sub_enabled AS TEXT) = ''", nil); err != nil {
				return fmt.Errorf("d1store: normalise sessions.sub_enabled: %w", err)
			}
			if _, err := c.query(ctx, "UPDATE sessions SET sub_enabled = -1 WHERE sub_enabled > 1 OR sub_enabled < -1", nil); err != nil {
				return fmt.Errorf("d1store: clamp sessions.sub_enabled: %w", err)
			}
		}
	}
	return nil
}

// isNumericSQLDefault reports whether a D1 dflt_value literal is an integer, so
// a column default of -1/0/1 is left alone and anything else is repaired.
func isNumericSQLDefault(value string) bool {
	value = strings.TrimSpace(strings.Trim(strings.TrimSpace(value), "'"))
	if value == "" {
		return false
	}
	if _, err := strconv.Atoi(value); err != nil {
		return false
	}
	return true
}

// EnsureTurnFooter adds the footer columns when they are missing. The daemon
// writes them on every answer, so a database created before AX-095 would fail
// every insert until it is brought up to date; the phone's Worker schema lists
// the same columns for a fresh database, and worker/migrations/0002_turn_footer.sql
// is the same change written for wrangler. It runs once, after the token that
// reaches the database has been verified.
func (c *Client) EnsureTurnFooter(ctx context.Context) error {
	if c == nil {
		return errors.New("d1store: client is not configured")
	}
	res, err := c.query(ctx, "PRAGMA table_info(turns)", nil)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	rows, err := queryInto[struct {
		Name string `json:"name"`
	}](res.rows)
	if err != nil {
		return err
	}
	for _, row := range rows {
		have[row.Name] = true
	}
	for _, column := range turnFooterColumns {
		if have[column.name] {
			continue
		}
		if _, err := c.query(ctx, column.ddl, nil); err != nil {
			return fmt.Errorf("d1store: add turns.%s: %w", column.name, err)
		}
	}
	return nil
}
