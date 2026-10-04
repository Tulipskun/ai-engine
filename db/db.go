package db

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---- from db/client.go ----
// ErrTokenRejected marks a wrong credential, as opposed to a transport or
// configuration problem. Callers count only the former as failed logins.
var ErrTokenRejected = errors.New("db: token rejected")

// ErrNoToken means nobody has connected yet: the daemon holds no credential
// of its own, so D1 is unreachable until a token is adopted.
var ErrNoToken = errors.New("db: no Cloudflare token yet")

const (
	// DefaultAPIBase is Cloudflare's REST root; the whole daemon config is
	// one base URL plus the database name to pick.
	DefaultAPIBase = "https://api.cloudflare.com/client/v4"
	// DefaultDatabaseName is used when the account holds exactly one D1
	// database, so a fresh install needs no ids typed by hand.
	DefaultDatabaseName = "aixodia"
	defaultTimeout      = 30 * time.Second
	// maxValueBytes caps one state value: D1 refuses a single bound value
	// above 1 MB, so oversized payloads are skipped with a report entry
	// instead of failing a whole batch halfway through (CON-012).
	maxValueBytes = 900_000
	// SessionKeyPrefix is the state-key namespace for session databases.
	SessionKeyPrefix = "sessions/"
	// HandoverKey is where a running daemon says it is serving, so a second
	// daemon can tell that it is the newer one and stand itself down. It is
	// deliberately not the nodes row: nodes holds the single address the
	// phone reads, so two daemons writing it would make the address
	// flicker between them.
	HandoverKey = "handover/ready"
)

// Target is the account and database a token resolved to.
type Target struct {
	AccountID  string
	DatabaseID string
	Name       string
}

// Client talks to Cloudflare's REST API with the adopted token. It is
// dependency-free so the runtime keeps its own HTTP conventions.
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

// NewClient returns nil when no API base is configured, which is the
// daemon's way of saying "this deployment does not talk to D1".
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

// SetDatabaseName pins which D1 database to use when the account has
// several. Empty keeps the default: prefer DefaultDatabaseName, else the
// only database.
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

// CurrentToken reports the token currently held ("" = none).
func (c *Client) CurrentToken() string {
	if c == nil || c.tokenFn == nil {
		return ""
	}
	return c.tokenFn()
}

// ResolvedTarget reports the account and database currently in use, for the
// log line that says which D1 this daemon ended up on.
func (c *Client) ResolvedTarget() (Target, bool) {
	if c == nil {
		return Target{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.target, c.resolved
}

// do sends one authenticated request. A 401/403 is a wrong credential
// (ErrTokenRejected); any other failure is a transport problem the caller
// must not count as a wrong guess.
func (c *Client) do(ctx context.Context, token, method, path string, body []byte, out any) error {
	if c == nil {
		return errors.New("db: client is not configured")
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
		return fmt.Errorf("db: %s %s -> HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// envelope is Cloudflare's wrapper around one query call: per-statement
// rows plus the shared success flag and error list.
type envelope struct {
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

// queryResult is what one D1 statement returned: rows, rows changed, and
// the rowid of the last inserted row (the only handle a caller has on a
// turn's id).
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
	var env envelope
	path := fmt.Sprintf("/accounts/%s/d1/database/%s/query", target.AccountID, target.DatabaseID)
	if err := c.do(ctx, token, http.MethodPost, path, body, &env); err != nil {
		return queryResult{}, err
	}
	if !env.Success {
		return queryResult{}, cloudFailure(env.Errors)
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

func cloudFailure(errs []struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}) error {
	if len(errs) == 0 {
		return errors.New("db: cloudflare reported a failure without a message")
	}
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		parts = append(parts, fmt.Sprintf("%d %s", e.Code, e.Message))
	}
	return errors.New("db: " + strings.Join(parts, "; "))
}

// queryInto decodes every row into T.
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

// VerifyToken checks a candidate Cloudflare API token and resolves the
// account and database it can reach. Only a bad credential returns
// ErrTokenRejected; a valid token that cannot see a database is a plain
// error, so the gate treats it as "cannot check" instead of a wrong guess.
func (c *Client) VerifyToken(ctx context.Context, token string) error {
	if c == nil {
		return errors.New("db: client is not configured")
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

type accountRow struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type databaseRow struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

// resolve turns a token into the account and database to use, and caches
// the answer per token: a rotated token must be resolved again.
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
		return Target{}, errors.New("db: the token cannot see any Cloudflare account")
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
		return Target{}, fmt.Errorf("db: account %s has no D1 database the token can see", account)
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
			return Target{}, fmt.Errorf("db: set d1_database in entry.json; this account has %s", strings.Join(names, ", "))
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
		return Target{}, errors.New("db: client is not configured")
	}
	return c.resolve(ctx, token)
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

// Put stores a state value, refusing oversized payloads instead of letting
// D1 reject them mid-turn (CON-012).
func (c *Client) Put(ctx context.Context, key, value string) error {
	if len(value) > maxValueBytes {
		return fmt.Errorf("db: %q is %d bytes, over the %d byte limit", key, len(value), maxValueBytes)
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
	// it took, so the phone can draw it under that message and keep it after
	// a restart (AX-095).
	Model           string `json:"model"`
	InputTokens     int    `json:"input_tokens"`
	OutputTokens    int    `json:"output_tokens"`
	CacheRead       int    `json:"cache_read_tokens"`
	CacheWrite      int    `json:"cache_write_tokens"`
	ReasoningTokens int    `json:"reasoning_tokens"`
	DurationMs      int64  `json:"duration_ms"`
	// InputIncludesCache says whether the provider's input count already
	// contains the cache parts. D1 stores it as INTEGER, so it arrives as
	// 0/1 rather than a JSON boolean, and unmarshalling a number into a
	// bool is an error.
	InputIncludesCache int `json:"input_includes_cache"`
}

// Node is the quick-tunnel announcement the phone reads to find the daemon.
type Node struct {
	TunnelURL string `json:"tunnel_url"`
	Version   string `json:"version"`
	Heartbeat int64  `json:"heartbeat"`
}

// ProviderRow is one provider in the D1 providers table. keys travels as
// a JSON array because D1 has no array type; free is 0/1 for the same
// reason. Index is the running number (0, 1, 2, ...) the daemon assigns
// in provider order.
type ProviderRow struct {
	Index    int
	Name     string
	Adapter  string
	Endpoint string
	APIKeys  []string
	Free     bool
}

// TurnMeta is the part of a turn the phone shows in the message footer.
type TurnMeta struct {
	Model        string
	InputTokens  int
	OutputTokens int
	CacheRead    int
	CacheWrite   int
	// ReasoningTokens counts the output a model spent thinking, and
	// InputIncludesCache records whether InputTokens already contains the
	// cache parts, so the phone can label the two instead of printing them
	// as rivals.
	ReasoningTokens    int
	InputIncludesCache bool
	DurationMs         int64
}

// Handover is one daemon's claim to be the one currently serving.
type Handover struct {
	Instance string `json:"instance"`
	Tunnel   string `json:"tunnel"`
	Version  string `json:"version,omitempty"`
	At       int64  `json:"at"`
}

// truncateRunes cuts a title on a character boundary: slicing bytes would
// leave a half-written rune in the chat title, which every client renders
// as replacement characters.
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

// boolInt renders a flag for the SQL driver, which has no boolean.
func boolInt(v bool) string {
	if v {
		return "1"
	}
	return "0"
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
		return Session{}, errors.New("db: session id is required")
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
// subEnabled == nil leaves the stored flag untouched, and writeRoute false
// leaves the route columns alone, so flipping only the flag cannot erase a
// stored route.
func (c *Client) SetSessionSubAgent(ctx context.Context, sessionID, subProvider, subModel string, subEnabled *bool, writeRoute bool) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("db: session id is required")
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
// phone's choice survives a restart and a second device. The chat is created
// if the daemon sees it before the phone does.
func (c *Client) SetSessionRoute(ctx context.Context, sessionID, provider, model string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("db: session id is required")
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

// Turns returns history rows in ascending seq order; beforeSeq == 0 means
// "from the newest backwards", which is what the phone's paged list asks for.
func (c *Client) Turns(ctx context.Context, sessionID string, beforeSeq int64, limit int) ([]Turn, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	if beforeSeq <= 0 {
		beforeSeq = 1<<62 - 1
	}
	res, err := c.query(ctx,
		`SELECT seq, role, agent, job_id, text, created_at, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens, input_includes_cache, duration_ms FROM turns
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

// AppendTurnAt appends one turn and returns its seq. A user turn that is the
// first message names the session, the way every messenger does.
func (c *Client) AppendTurnAt(ctx context.Context, sessionID, role, agent, jobID, text string) (int64, error) {
	return c.appendTurn(ctx, sessionID, role, agent, jobID, text, TurnMeta{})
}

// AppendModelTurn mirrors one answered turn together with the footer data,
// and returns its seq. A user turn carries none of it.
func (c *Client) AppendModelTurn(ctx context.Context, sessionID, agent, jobID, text string, meta TurnMeta) (int64, error) {
	return c.appendTurn(ctx, sessionID, "model", agent, jobID, text, meta)
}

func (c *Client) appendTurn(ctx context.Context, sessionID, role, agent, jobID, text string, meta TurnMeta) (int64, error) {
	if strings.TrimSpace(sessionID) == "" {
		return 0, errors.New("db: session id is required")
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
	// The next seq is read inside the same statement so a turn can never land
	// on a duplicate (session_id, seq) when two phones finish at the same
	// moment.
	inserted, err := c.query(ctx,
		`INSERT INTO turns(session_id, seq, role, agent, job_id, text, created_at, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens, input_includes_cache, duration_ms)
		 SELECT ?, COALESCE((SELECT MAX(seq) + 1 FROM turns WHERE session_id = ?), 1), ?, ?, ?, ?, unixepoch(), ?, ?, ?, ?, ?, ?, ?, ?`,
		[]string{sessionID, sessionID, role, agent, jobID, text, meta.Model,
			strconv.Itoa(meta.InputTokens), strconv.Itoa(meta.OutputTokens),
			strconv.Itoa(meta.CacheRead), strconv.Itoa(meta.CacheWrite),
			strconv.Itoa(meta.ReasoningTokens), boolInt(meta.InputIncludesCache),
			strconv.FormatInt(meta.DurationMs, 10)})
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

// AnnounceTunnelURL records the daemon's public quick-tunnel URL in the
// tunnel table, replacing the previous row. The phone reads this table
// directly to find the daemon.
func (c *Client) AnnounceTunnelURL(ctx context.Context, tunnelURL string) error {
	if strings.TrimSpace(tunnelURL) == "" {
		return errors.New("db: tunnel URL is required")
	}
	if _, err := c.query(ctx, "DELETE FROM tunnel", nil); err != nil {
		return err
	}
	_, err := c.query(ctx, "INSERT INTO tunnel (url) VALUES (?)", []string{tunnelURL})
	return err
}

// Node returns the current announcement, or found=false when the daemon
// has never reported in.
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

// ClaimHandover records that this daemon is up and where it is reachable.
// It is written by the daemon rather than by the thing hosting it, because
// the daemon is what holds the Cloudflare token — the host has none, and
// CON-012 says the system must not grow a second credential for this.
func (c *Client) ClaimHandover(ctx context.Context, instance, tunnel, version string, at int64) error {
	raw, err := json.Marshal(Handover{Instance: instance, Tunnel: tunnel, Version: version, At: at})
	if err != nil {
		return err
	}
	_, err = c.query(ctx,
		`INSERT INTO state(key, value, updated_at) VALUES(?, ?, unixepoch())
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = unixepoch()`,
		[]string{HandoverKey, string(raw)})
	return err
}

// Successor returns the daemon that took over, if it started after us.
// Comparing the start time is what keeps a fresh instance from reading the
// record of the one it replaced and standing itself down on the spot.
func (c *Client) Successor(ctx context.Context, startedAt int64) (Handover, bool, error) {
	res, err := c.query(ctx, "SELECT value FROM state WHERE key = ? LIMIT 1", []string{HandoverKey})
	if err != nil {
		return Handover{}, false, err
	}
	list, err := queryInto[struct{ Value string }](res.rows)
	if err != nil || len(list) == 0 || list[0].Value == "" {
		return Handover{}, false, err
	}
	var claim Handover
	if err := json.Unmarshal([]byte(list[0].Value), &claim); err != nil {
		return Handover{}, false, nil
	}
	if claim.At <= startedAt {
		return Handover{}, false, nil
	}
	return claim, true, nil
}

// MemoryToken stores the Cloudflare token in RAM. It is the daemon's only
// credential and it is intentionally volatile: a restart requires a phone
// to hand it over again (REQ-046(3), CON-012).
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

// turnFooterColumns are the columns an answered turn needs for the footer
// the phone draws under that message (AX-095).
var turnFooterColumns = []struct{ name, ddl string }{
	{"model", "ALTER TABLE turns ADD COLUMN model TEXT NOT NULL DEFAULT ''"},
	{"input_tokens", "ALTER TABLE turns ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0"},
	{"output_tokens", "ALTER TABLE turns ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0"},
	{"cache_read_tokens", "ALTER TABLE turns ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0"},
	{"reasoning_tokens", "ALTER TABLE turns ADD COLUMN reasoning_tokens INTEGER NOT NULL DEFAULT 0"},
	{"input_includes_cache", "ALTER TABLE turns ADD COLUMN input_includes_cache INTEGER NOT NULL DEFAULT 0"},
	{"cache_write_tokens", "ALTER TABLE turns ADD COLUMN cache_write_tokens INTEGER NOT NULL DEFAULT 0"},
	{"duration_ms", "ALTER TABLE turns ADD COLUMN duration_ms INTEGER NOT NULL DEFAULT 0"},
}

// subAgentColumns are the per-session sub-agent columns (CHANGE-085). The
// default is -1, "not set", so a row that predates them, or a fresh one,
// falls back to the agent's global sub-agent instead of reading as disabled.
var subAgentColumns = []struct{ name, ddl string }{
	{"sub_provider", "ALTER TABLE sessions ADD COLUMN sub_provider TEXT NOT NULL DEFAULT ''"},
	{"sub_model", "ALTER TABLE sessions ADD COLUMN sub_model TEXT NOT NULL DEFAULT ''"},
	{"sub_enabled", "ALTER TABLE sessions ADD COLUMN sub_enabled INTEGER NOT NULL DEFAULT -1"},
}

// EnsureTurnFooter adds the footer columns when they are missing. The daemon
// writes them on every answer, so a database created before AX-095 would
// fail every insert until it is brought up to date. It runs once, after the
// token that reaches the database is verified.
func (c *Client) EnsureTurnFooter(ctx context.Context) error {
	if c == nil {
		return errors.New("db: client is not configured")
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
			return fmt.Errorf("db: add turns.%s: %w", column.name, err)
		}
	}
	return nil
}

// EnsureSubAgentColumns adds the per-session sub-agent columns when they are
// missing, and repairs a column an earlier manual migration created with a
// default that reads as "off": sub_enabled is a three-state value, so an
// empty text default would fail the int scan. It runs once, next to
// EnsureTurnFooter, after the token that reaches the database is verified.
func (c *Client) EnsureSubAgentColumns(ctx context.Context) error {
	if c == nil {
		return errors.New("db: client is not configured")
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
				return fmt.Errorf("db: add sessions.%s: %w", column.name, err)
			}
			continue
		}
		// Present but with a default that cannot be read as the three-state
		// value: normalise the stored rows and clamp the out-of-range ones.
		if column.name == "sub_enabled" && !isNumericSQLDefault(have[column.name]) {
			if _, err := c.query(ctx, "UPDATE sessions SET sub_enabled = -1 WHERE sub_enabled IS NULL OR CAST(sub_enabled AS TEXT) = ''", nil); err != nil {
				return fmt.Errorf("db: normalise sessions.sub_enabled: %w", err)
			}
			if _, err := c.query(ctx, "UPDATE sessions SET sub_enabled = -1 WHERE sub_enabled > 1 OR sub_enabled < -1", nil); err != nil {
				return fmt.Errorf("db: clamp sessions.sub_enabled: %w", err)
			}
		}
	}
	return nil
}

// isNumericSQLDefault reports whether a D1 dflt_value literal is an integer,
// so a column default of -1/0/1 is left alone and anything else is repaired.
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

// EnsureProvidersTable creates the providers table when it is missing, so
// a database created before the table existed still works. "index" is
// quoted: it is a reserved word in SQLite. It runs once at boot, after the
// bootstrap token is verified.
func (c *Client) EnsureProvidersTable(ctx context.Context) error {
	if c == nil {
		return errors.New("db: client is not configured")
	}
	_, err := c.query(ctx, `CREATE TABLE IF NOT EXISTS providers(
		"index" INTEGER PRIMARY KEY,
		provider TEXT NOT NULL DEFAULT '',
		adapter TEXT NOT NULL DEFAULT '',
		endpoint TEXT NOT NULL DEFAULT '',
		keys TEXT NOT NULL DEFAULT '[]',
		free INTEGER NOT NULL DEFAULT 0
	)`, nil)
	return err
}

// providerRowRaw is the wire shape of one providers row: the JSON column
// arrives as text and is parsed into ProviderRow by ListProviders.
type providerRowRaw struct {
	Index    int    `json:"index"`
	Name     string `json:"provider"`
	Adapter  string `json:"adapter"`
	Endpoint string `json:"endpoint"`
	Keys     string `json:"keys"`
	Free     int    `json:"free"`
}

// ListProviders returns every provider in index order. A row whose JSON
// does not parse fails the whole call: a half-read provider set would
// route turns to the wrong keys.
func (c *Client) ListProviders(ctx context.Context) ([]ProviderRow, error) {
	res, err := c.query(ctx,
		`SELECT "index", provider, adapter, endpoint, keys, free FROM providers ORDER BY "index"`,
		nil)
	if err != nil {
		return nil, err
	}
	raws, err := queryInto[providerRowRaw](res.rows)
	if err != nil {
		return nil, err
	}
	out := make([]ProviderRow, 0, len(raws))
	for _, raw := range raws {
		var keys []string
		if strings.TrimSpace(raw.Keys) != "" {
			if err := json.Unmarshal([]byte(raw.Keys), &keys); err != nil {
				return nil, fmt.Errorf("db: provider %q keys: %w", raw.Name, err)
			}
		}
		out = append(out, ProviderRow{
			Index: raw.Index, Name: raw.Name, Adapter: raw.Adapter,
			Endpoint: raw.Endpoint, APIKeys: keys,
			Free: raw.Free != 0,
		})
	}
	return out, nil
}

// PutProviders replaces the whole providers table with rows, numbered
// 0, 1, 2, ... in the given order. The table is tiny (a handful of rows),
// so a full replace keeps the daemon from ever disagreeing with D1 about
// a provider the phone just deleted.
func (c *Client) PutProviders(ctx context.Context, rows []ProviderRow) error {
	if _, err := c.query(ctx, "DELETE FROM providers", nil); err != nil {
		return err
	}
	for i, row := range rows {
		keys, err := json.Marshal(row.APIKeys)
		if err != nil {
			return fmt.Errorf("db: provider %q keys: %w", row.Name, err)
		}
		free := "0"
		if row.Free {
			free = "1"
		}
		_, err = c.query(ctx,
			`INSERT INTO providers("index", provider, adapter, endpoint, keys, free)
			 VALUES(?, ?, ?, ?, ?, ?)`,
			[]string{strconv.Itoa(i), row.Name, row.Adapter, row.Endpoint, string(keys), free})
		if err != nil {
			return err
		}
	}
	return nil
}

// ---- from db/sync.go ----
// ConfigFile is one runtime config file that syncs with D1: the D1 state
// key and the local path it materializes to. Providers are deliberately
// not here — they live in the providers table, not in a state blob.
type ConfigFile struct {
	Key  string // D1 state key, e.g. config/system
	Path string // local path, e.g. <state>/config/system.json
}

// DefaultConfigFiles lists every config file the daemon syncs with D1.
// config/entry.json is deliberately NOT here: it is the gateway bootstrap
// (where the tunnel points, whether the tunnel runs at all), so it must
// exist locally before D1 is reachable. Syncing it would let a stale cloud
// copy disable the gateway that is supposed to fetch the cloud copy.
func DefaultConfigFiles(stateRoot string) []ConfigFile {
	join := func(name string) string { return filepath.Join(stateRoot, "config", name) }
	return []ConfigFile{
		{Key: "config:system", Path: join("system.json")},
	}
}

// SyncReport records what a hydrate/push pass actually moved, so the daemon
// can log the outcome instead of failing silently.
type SyncReport struct {
	PulledConfig    []string
	PushedConfig    []string
	PulledSession   []string
	PushedSession   []string
	SkippedOversize []string
}

func (r *SyncReport) merge(other SyncReport) {
	r.PulledConfig = append(r.PulledConfig, other.PulledConfig...)
	r.PushedConfig = append(r.PushedConfig, other.PushedConfig...)
	r.PulledSession = append(r.PulledSession, other.PulledSession...)
	r.PushedSession = append(r.PushedSession, other.PushedSession...)
	r.SkippedOversize = append(r.SkippedOversize, other.SkippedOversize...)
}

// HydrateConfig writes the D1 copies of the config files into the state
// root. A missing key leaves the local file untouched, so a first run on
// an empty D1 still boots from whatever the operator has locally.
func (c *Client) HydrateConfig(ctx context.Context, files []ConfigFile) (SyncReport, error) {
	var report SyncReport
	for _, f := range files {
		value, found, err := c.Get(ctx, f.Key)
		if err != nil {
			return report, err
		}
		if !found {
			continue
		}
		if len(value) > maxValueBytes {
			report.SkippedOversize = append(report.SkippedOversize, f.Key)
			continue
		}
		if err := writeFileAtomic(f.Path, []byte(value), 0o600); err != nil {
			return report, err
		}
		report.PulledConfig = append(report.PulledConfig, f.Key)
	}
	return report, nil
}

// PushConfig uploads the local config files that exist. Missing files are
// skipped; the D1 copy stays untouched.
func (c *Client) PushConfig(ctx context.Context, files []ConfigFile) (SyncReport, error) {
	var report SyncReport
	for _, f := range files {
		raw, err := os.ReadFile(f.Path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return report, err
		}
		if len(raw) > maxValueBytes {
			report.SkippedOversize = append(report.SkippedOversize, f.Key)
			continue
		}
		if err := c.Put(ctx, f.Key, string(raw)); err != nil {
			return report, err
		}
		report.PushedConfig = append(report.PushedConfig, f.Key)
	}
	return report, nil
}

// SessionBlob is the D1 representation of one session database: base64 of
// the SQLite file plus a flag telling HydrateSessions whether it came from
// a clean close. WAL contents are checkpointed by the caller before
// PushSession.
type SessionBlob struct {
	Version int    `json:"version"`
	Name    string `json:"name"`
	Data    string `json:"data"`
}

// encodeBlob renders a value as JSON text: D1 stores whatever string it is
// given, so keeping the payload structured lets Hydrate tell a session
// blob apart from a raw config file.
func encodeBlob(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// decodeBlob accepts either our JSON object or a raw string written by an
// older build, so a D1 that already holds plain config still hydrates.
func decodeBlob(value string, out *SessionBlob) error {
	trimmed := strings.TrimSpace(value)
	if strings.HasPrefix(trimmed, "{") {
		return json.Unmarshal([]byte(trimmed), out)
	}
	var s string
	if err := json.Unmarshal([]byte(trimmed), &s); err == nil {
		out.Version = 0
		out.Data = s
		return nil
	}
	*out = SessionBlob{Version: 0, Data: value}
	return nil
}

// HydrateSessions restores session databases from D1 into dir, using the
// same file naming the session layer already uses
// (base64url(sessionID) + ".db"). Existing files are replaced only when D1
// actually has a copy, so an offline daemon keeps working from local state.
func (c *Client) HydrateSessions(ctx context.Context, dir string, encodeName func(sessionID string) string) (SyncReport, error) {
	var report SyncReport
	keys, err := c.List(ctx, SessionKeyPrefix)
	if err != nil {
		return report, err
	}
	sort.Strings(keys)
	for _, key := range keys {
		sessionID := strings.TrimPrefix(key, SessionKeyPrefix)
		if sessionID == "" {
			continue
		}
		value, found, err := c.Get(ctx, key)
		if err != nil {
			return report, err
		}
		if !found {
			continue
		}
		var blob SessionBlob
		if err := decodeBlob(value, &blob); err != nil {
			return report, fmt.Errorf("db: decode session %q: %w", sessionID, err)
		}
		raw, err := base64.StdEncoding.DecodeString(blob.Data)
		if err != nil {
			return report, fmt.Errorf("db: session %q payload: %w", sessionID, err)
		}
		if err := writeFileAtomic(filepath.Join(dir, encodeName(sessionID)), raw, 0o600); err != nil {
			return report, err
		}
		report.PulledSession = append(report.PulledSession, sessionID)
	}
	return report, nil
}

// PushSession uploads one session database. Oversized files are skipped
// with a report entry rather than truncated (CON-012).
func (c *Client) PushSession(ctx context.Context, sessionID, path string) (SyncReport, error) {
	var report SyncReport
	raw, err := os.ReadFile(path)
	if err != nil {
		return report, err
	}
	blob := SessionBlob{Version: 1, Name: filepath.Base(path), Data: base64.StdEncoding.EncodeToString(raw)}
	if len(raw) > maxValueBytes/2 { // base64 inflates by ~4/3
		report.SkippedOversize = append(report.SkippedOversize, SessionKeyPrefix+sessionID)
		return report, nil
	}
	encoded, err := encodeBlob(blob)
	if err != nil {
		return report, err
	}
	if err := c.Put(ctx, SessionKeyPrefix+sessionID, encoded); err != nil {
		return report, err
	}
	report.PushedSession = append(report.PushedSession, sessionID)
	return report, nil
}

// PushSessions uploads several session databases, continuing past
// per-session failures so one oversized DB cannot block the rest.
func (c *Client) PushSessions(ctx context.Context, dir string, sessionIDs []string) (SyncReport, error) {
	var total SyncReport
	var firstErr error
	for _, id := range sessionIDs {
		name := base64.RawURLEncoding.EncodeToString([]byte(id)) + ".db"
		report, err := c.PushSession(ctx, id, filepath.Join(dir, name))
		total.merge(report)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return total, firstErr
}

// writeFileAtomic replaces a file through a temp file in the same
// directory, so a crash mid-write never leaves a half-written config.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".db-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
