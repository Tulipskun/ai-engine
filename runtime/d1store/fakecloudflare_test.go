package d1store

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeCloudflare is a small stand-in for the parts of Cloudflare's REST API the
// client uses: token verification, account and database discovery, and the D1
// query endpoint with just enough SQL understanding for the statements this
// package sends. It keeps the tests honest about the request shape (bearer
// token, account/database path, bound params) instead of only the mapping.
type fakeCloudflare struct {
	// extraTurnColumns are the footer columns a migration has added so far, so a
	// test can check that EnsureTurnFooter is idempotent.
	extraTurnColumns []string
	mu               sync.Mutex

	token     string
	accounts  []map[string]string
	databases []map[string]string
	down      bool

	state    map[string]string
	sessions map[string]map[string]any
	turns    []map[string]any
	nodes    map[string]any
	nextTurn int64

	queries []recordedQuery
	paths   []string
}

type recordedQuery struct {
	path   string
	sql    string
	params []string
}

func newFakeCloudflare(token string) *fakeCloudflare {
	return &fakeCloudflare{
		token:     token,
		accounts:  []map[string]string{{"id": "acct-1", "name": "Test Account"}},
		databases: []map[string]string{{"uuid": "db-1", "name": DefaultDatabaseName}},
		state:     map[string]string{},
		sessions:  map[string]map[string]any{},
		nextTurn:  1,
	}
}

func (f *fakeCloudflare) start(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	return server
}

// fakeInt parses a bound parameter that stands for a number.
func fakeInt(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func (f *fakeCloudflare) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	down := f.down
	f.paths = append(f.paths, r.URL.Path)
	f.mu.Unlock()
	if down {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		writeCF(w, map[string]any{"success": false, "errors": []map[string]any{{"code": 10000, "message": "bad token"}}})
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.URL.Path == "/user/tokens/verify":
		writeCF(w, map[string]any{"success": true, "result": map[string]any{"id": "tok-1", "status": "active"}})
	case r.URL.Path == "/accounts":
		writeCF(w, map[string]any{"success": true, "result": f.accounts})
	case strings.HasPrefix(r.URL.Path, "/accounts/") && strings.HasSuffix(r.URL.Path, "/d1/database"):
		writeCF(w, map[string]any{"success": true, "result": f.databases})
	case strings.HasSuffix(r.URL.Path, "/query"):
		var body struct {
			SQL    string   `json:"sql"`
			Params []string `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.queries = append(f.queries, recordedQuery{path: r.URL.Path, sql: body.SQL, params: body.Params})
		rows, changes, lastID, queryErr := f.exec(body.SQL, body.Params)
		f.mu.Unlock()
		if queryErr != nil {
			writeCF(w, map[string]any{"success": false, "errors": []map[string]any{{"code": 1000, "message": queryErr.Error()}}})
			return
		}
		results := make([]any, 0, len(rows))
		for _, row := range rows {
			results = append(results, row)
		}
		writeCF(w, map[string]any{"success": true, "result": []any{map[string]any{
			"success": true,
			"results": results,
			"meta":    map[string]any{"changes": changes, "last_row_id": lastID},
		}}})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func writeCF(w http.ResponseWriter, body map[string]any) {
	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func norm(sql string) string { return strings.Join(strings.Fields(sql), " ") }

func (f *fakeCloudflare) exec(sql string, params []string) ([]map[string]any, int, int64, error) {
	s := norm(sql)
	switch {
	case strings.HasPrefix(s, "SELECT value FROM state WHERE key ="):
		value, ok := f.state[params[0]]
		if !ok {
			return nil, 0, 0, nil
		}
		return []map[string]any{{"value": value}}, 0, 0, nil
	case strings.HasPrefix(s, "SELECT key FROM state WHERE key >="):
		prefix := params[0]
		keys := make([]string, 0, len(f.state))
		for key := range f.state {
			if strings.HasPrefix(key, prefix) {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		rows := make([]map[string]any, 0, len(keys))
		for _, key := range keys {
			rows = append(rows, map[string]any{"key": key})
		}
		return rows, 0, 0, nil
	case strings.HasPrefix(s, "INSERT INTO state(key, value, updated_at)"):
		f.state[params[0]] = params[1]
		return nil, 1, 0, nil
	case s == "DELETE FROM state WHERE key = ?":
		_, existed := f.state[params[0]]
		delete(f.state, params[0])
		changes := 0
		if existed {
			changes = 1
		}
		return nil, changes, 0, nil
	case strings.HasPrefix(s, "INSERT OR IGNORE INTO sessions(id, title, sub_enabled"):
		f.ensureSession(params[0], params[1])
		return nil, 0, 0, nil
	case strings.HasPrefix(s, "INSERT OR IGNORE INTO sessions(id, title, created_at"):
		f.ensureSession(params[0], params[1])
		return nil, 0, 0, nil
	case strings.HasPrefix(s, "INSERT OR IGNORE INTO sessions(id, title, model"):
		if _, ok := f.sessions[params[0]]; !ok {
			f.sessions[params[0]] = sessionRow(params[0], params[1], params[2])
		}
		return nil, 0, 0, nil
	case s == "SELECT title FROM sessions WHERE id = ?":
		row, ok := f.sessions[params[0]]
		if !ok {
			return nil, 0, 0, nil
		}
		return []map[string]any{{"title": row["title"]}}, 0, 0, nil
	case strings.HasPrefix(s, "UPDATE sessions SET title = ?, updated_at = unixepoch()"):
		row, ok := f.sessions[params[1]]
		if !ok {
			return nil, 0, 0, nil
		}
		row["title"] = params[0]
		return nil, 1, 0, nil
	case s == "UPDATE sessions SET title = ? WHERE id = ?":
		row, ok := f.sessions[params[1]]
		if !ok {
			return nil, 0, 0, nil
		}
		row["title"] = params[0]
		return nil, 1, 0, nil
	case s == "UPDATE sessions SET updated_at = unixepoch() WHERE id = ?":
		return nil, 0, 0, nil
	case strings.HasPrefix(s, "UPDATE sessions SET provider = ?, model = ?"):
		row, ok := f.sessions[params[2]]
		if !ok {
			return nil, 0, 0, nil
		}
		row["provider"] = params[0]
		row["model"] = params[1]
		return nil, 1, 0, nil
	case strings.HasPrefix(s, "UPDATE sessions SET sub_provider = ?, sub_model = ?"):
		row, ok := f.sessions[params[2]]
		if !ok {
			return nil, 0, 0, nil
		}
		row["sub_provider"] = params[0]
		row["sub_model"] = params[1]
		return nil, 1, 0, nil
	case s == "UPDATE sessions SET sub_enabled = ?, updated_at = unixepoch() WHERE id = ?":
		row, ok := f.sessions[params[1]]
		if !ok {
			return nil, 0, 0, nil
		}
		if v, err := strconv.Atoi(params[0]); err == nil {
			row["sub_enabled"] = v
		}
		return nil, 1, 0, nil
	case strings.HasPrefix(s, "SELECT id, title, provider, model, created_at, updated_at FROM sessions ORDER BY"):
		ids := make([]string, 0, len(f.sessions))
		for id := range f.sessions {
			ids = append(ids, id)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(ids)))
		rows := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			rows = append(rows, f.sessions[id])
		}
		return rows, 0, 0, nil
	case strings.HasPrefix(s, "SELECT id, title, provider, model, sub_provider, sub_model, sub_enabled, created_at, updated_at FROM sessions WHERE"):
		row, ok := f.sessions[params[0]]
		if !ok {
			return nil, 0, 0, nil
		}
		return []map[string]any{row}, 0, 0, nil
	case s == "DELETE FROM turns WHERE session_id = ?":
		kept := f.turns[:0]
		for _, turn := range f.turns {
			if turn["session_id"] != params[0] {
				kept = append(kept, turn)
			}
		}
		f.turns = kept
		return nil, 0, 0, nil
	case s == "DELETE FROM sessions WHERE id = ?":
		_, existed := f.sessions[params[0]]
		delete(f.sessions, params[0])
		changes := 0
		if existed {
			changes = 1
		}
		return nil, changes, 0, nil
	case s == "PRAGMA table_info(turns)":
		rows := []map[string]any{}
		for _, name := range []string{"id", "session_id", "seq", "role", "agent", "job_id", "text", "created_at"} {
			rows = append(rows, map[string]any{"name": name})
		}
		for _, name := range f.extraTurnColumns {
			rows = append(rows, map[string]any{"name": name})
		}
		return rows, 0, 0, nil
	case strings.HasPrefix(s, "ALTER TABLE turns ADD COLUMN "):
		name := strings.TrimSuffix(strings.TrimPrefix(s, "ALTER TABLE turns ADD COLUMN "), " ")
		name = strings.Fields(name)[0]
		f.extraTurnColumns = append(f.extraTurnColumns, name)
		return nil, 1, 0, nil
	case strings.HasPrefix(s, "INSERT INTO turns(session_id, seq, role, agent, job_id, text, created_at, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens, input_includes_cache, duration_ms) SELECT"):
		sessionID, role, agent, jobID, text := params[0], params[2], params[3], params[4], params[5]
		seq := f.nextTurn
		f.nextTurn++
		id := seq
		f.turns = append(f.turns, map[string]any{
			"id": id, "session_id": sessionID, "seq": seq, "role": role,
			"agent": agent, "job_id": jobID, "text": text, "created_at": seq,
			"model": params[6], "input_tokens": fakeInt(params[7]), "output_tokens": fakeInt(params[8]),
			"cache_read_tokens": fakeInt(params[9]), "cache_write_tokens": fakeInt(params[10]),
			"reasoning_tokens": fakeInt(params[11]), "input_includes_cache": fakeInt(params[12]),
			"duration_ms": fakeInt(params[13]),
		})
		return nil, 1, id, nil
	case strings.HasPrefix(s, "SELECT seq, role, agent, job_id, text, created_at, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens, input_includes_cache, duration_ms FROM turns"):
		before := params[1]
		var limit int
		fmt.Sscan(params[2], &limit)
		rows := make([]map[string]any, 0, len(f.turns))
		for i := len(f.turns) - 1; i >= 0; i-- {
			turn := f.turns[i]
			if turn["session_id"] != params[0] {
				continue
			}
			if fmt.Sprint(turn["seq"]) >= before {
				continue
			}
			rows = append(rows, map[string]any{
				"seq": turn["seq"], "role": turn["role"], "agent": turn["agent"],
				"job_id": turn["job_id"], "text": turn["text"], "created_at": turn["created_at"],
				"model": turn["model"], "input_tokens": turn["input_tokens"],
				"output_tokens": turn["output_tokens"], "cache_read_tokens": turn["cache_read_tokens"],
				"cache_write_tokens": turn["cache_write_tokens"], "reasoning_tokens": turn["reasoning_tokens"],
				"input_includes_cache": turn["input_includes_cache"], "duration_ms": turn["duration_ms"],
			})
			if len(rows) == limit {
				break
			}
		}
		return rows, 0, 0, nil
	case s == "SELECT seq FROM turns WHERE id = ?":
		for _, turn := range f.turns {
			if fmt.Sprint(turn["id"]) == params[0] {
				return []map[string]any{{"seq": turn["seq"]}}, 0, 0, nil
			}
		}
		return nil, 0, 0, nil
	case strings.HasPrefix(s, "INSERT INTO nodes(id, tunnel_url, version, heartbeat)"):
		f.nodes = map[string]any{"tunnel_url": params[0], "version": params[1], "heartbeat": 1234}
		return nil, 1, 0, nil
	case s == "SELECT tunnel_url, version, heartbeat FROM nodes WHERE id = 'ai'":
		if f.nodes == nil {
			return nil, 0, 0, nil
		}
		return []map[string]any{f.nodes}, 0, 0, nil
	}
	return nil, 0, 0, fmt.Errorf("fake: unsupported SQL %q", s)
}

func sessionRow(id, title, model string) map[string]any {
	return map[string]any{
		"id": id, "title": title, "provider": "", "model": model,
		// -1 is the "not set" state a real row starts in, so a fresh session
		// follows the agent's global sub-agent instead of reading as off.
		"sub_provider": "", "sub_model": "", "sub_enabled": -1,
		"created_at": 1, "updated_at": 1,
	}
}

func (f *fakeCloudflare) ensureSession(id, title string) {
	if _, ok := f.sessions[id]; !ok {
		f.sessions[id] = sessionRow(id, title, "")
	}
}

func (f *fakeCloudflare) pathCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, seen := range f.paths {
		if seen == path {
			count++
		}
	}
	return count
}

func (f *fakeCloudflare) sqlContaining(needle string) (recordedQuery, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, q := range f.queries {
		if strings.Contains(q.sql, needle) {
			return q, true
		}
	}
	return recordedQuery{}, false
}
