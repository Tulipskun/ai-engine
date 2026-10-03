package session

import (
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/Tulipskun/ai-engine/provider"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type SessionDB struct {
	mu   sync.Mutex
	db   *sql.DB
	path string
}

func OpenSessionDB(path string) (*SessionDB, error) {
	if path == "" {
		return nil, errors.New("sdk: session database path is required")
	}
	if filepath.Clean(path) == filepath.Join(".data", "sessions.db") {
		path = filepath.Join(".ai", "sessions.db")
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &SessionDB{db: db, path: path}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SessionDB) init() error {
	_, err := s.db.Exec(`
		PRAGMA journal_mode=WAL;
		PRAGMA synchronous=NORMAL;
		PRAGMA foreign_keys=ON;
		PRAGMA busy_timeout=10000;

		CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			provider TEXT NOT NULL,
			model TEXT NOT NULL,
			key_index INTEGER NOT NULL,
			thinking_level TEXT NOT NULL,
			temperature REAL,
			top_p REAL,
			top_k REAL,
			stop_sequences TEXT,
			presence_penalty REAL,
			frequency_penalty REAL,
			seed INTEGER,
			max_output_tokens INTEGER NOT NULL DEFAULT 0,
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens INTEGER NOT NULL DEFAULT 0,
			cache_write_tokens INTEGER NOT NULL DEFAULT 0,
			cache_hits INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS turns (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			seq INTEGER NOT NULL,
			role TEXT NOT NULL,
			content_json TEXT NOT NULL,
			tool_call_json TEXT,
			tool_result_json TEXT,
			reasoning_json TEXT,
			created_at TEXT NOT NULL,
			UNIQUE(session_id, seq)
		);
		CREATE INDEX IF NOT EXISTS idx_turns_session_seq ON turns(session_id, seq);

		CREATE TABLE IF NOT EXISTS contexts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			system_prompt TEXT NOT NULL,
			tools_json TEXT NOT NULL,
			UNIQUE(session_id, system_prompt, tools_json)
		);

		CREATE TABLE IF NOT EXISTS attempts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			context_id INTEGER REFERENCES contexts(id) ON DELETE SET NULL,
			attempt INTEGER NOT NULL,
			created_at TEXT NOT NULL,
			provider TEXT NOT NULL,
			model TEXT NOT NULL,
			temperature REAL,
			thinking_level TEXT NOT NULL,
			max_output_tokens INTEGER NOT NULL,
			stream INTEGER NOT NULL,
			success INTEGER NOT NULL DEFAULT 0,
			finish_reason TEXT,
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens INTEGER NOT NULL DEFAULT 0,
			cache_write_tokens INTEGER NOT NULL DEFAULT 0,
			cache_hit INTEGER NOT NULL DEFAULT 0,
			cache_layer TEXT,
			error TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_attempts_session_created ON attempts(session_id, created_at);

	`)
	if err != nil {
		return err
	}

	if err := s.ensureSessionColumns(); err != nil {
		return err
	}
	if err := s.dropLegacyRawTables(); err != nil {
		return err
	}
	return nil
}

func (s *SessionDB) ensureSessionColumns() error {
	columns := []string{
		"agent_mode TEXT NOT NULL DEFAULT ''",
		"workspace TEXT",
		"input_tokens INTEGER NOT NULL DEFAULT 0",
		"output_tokens INTEGER NOT NULL DEFAULT 0",
		"total_tokens INTEGER NOT NULL DEFAULT 0",
		"cache_read_tokens INTEGER NOT NULL DEFAULT 0",
		"cache_write_tokens INTEGER NOT NULL DEFAULT 0",
		"cache_hits INTEGER NOT NULL DEFAULT 0",
		"top_p REAL",
		"top_k REAL",
		"stop_sequences TEXT",
		"presence_penalty REAL",
		"frequency_penalty REAL",
		"seed INTEGER",
		"max_output_tokens INTEGER NOT NULL DEFAULT 0",
	}
	for _, column := range columns {
		if _, err := s.db.Exec("ALTER TABLE sessions ADD COLUMN " + column); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return err
		}
	}
	return nil
}

// The old requests/responses tables stored complete request and response payloads.
// They are intentionally removed so the session DB cannot keep the raw protocol data.
func (s *SessionDB) dropLegacyRawTables() error {
	_, err := s.db.Exec(`DROP TABLE IF EXISTS responses; DROP TABLE IF EXISTS requests;`)
	return err
}

func (s *SessionDB) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}

func nullableFloat(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func nullableInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// nullableStrings stores the stop sequences as one JSON array so the column stays
// a single value; an unset list is NULL rather than an empty array, because
// "stop on nothing" and "never set" must not read the same after a round trip.
func nullableStrings(v []string) any {
	if len(v) == 0 {
		return nil
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return string(encoded)
}

func decodeStrings(v string) []string {
	if v == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return nil
	}
	return out
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (s *SessionDB) SaveSession(config provider.SessionConfig) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`
		INSERT INTO sessions(id,provider,model,key_index,thinking_level,temperature,top_p,top_k,stop_sequences,presence_penalty,frequency_penalty,seed,max_output_tokens,agent_mode,workspace,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			provider=excluded.provider,
			model=excluded.model,
			key_index=excluded.key_index,
			thinking_level=excluded.thinking_level,
			temperature=excluded.temperature,
			top_p=excluded.top_p,
			top_k=excluded.top_k,
			stop_sequences=excluded.stop_sequences,
			presence_penalty=excluded.presence_penalty,
			frequency_penalty=excluded.frequency_penalty,
			seed=excluded.seed,
			max_output_tokens=excluded.max_output_tokens,
			agent_mode=excluded.agent_mode,
			workspace=excluded.workspace,
			updated_at=excluded.updated_at`,
		config.ID,
		config.Provider,
		config.Model,
		config.KeyIndex,
		config.ThinkingLevel,
		nullableFloat(config.Temperature),
		nullableFloat(config.TopP),
		nullableFloat(config.TopK),
		nullableStrings(config.StopSequences),
		nullableFloat(config.PresencePenalty),
		nullableFloat(config.FrequencyPenalty),
		nullableInt64(config.Seed),
		config.MaxOutputTokens,
		string(config.AgentMode),
		nullableString(config.Workspace),
		now,
		now,
	)
	return err
}

func (s *SessionDB) LoadSession(sessionID string) (provider.SessionConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var config provider.SessionConfig
	var temperature, topP, topK, presence, frequency sql.NullFloat64
	var stopSequences sql.NullString
	var seed sql.NullInt64
	var agentMode string
	var workspace sql.NullString
	err := s.db.QueryRow(`
		SELECT id,provider,model,key_index,thinking_level,temperature,
		       top_p,top_k,stop_sequences,presence_penalty,frequency_penalty,seed,
		       max_output_tokens,agent_mode,workspace
		FROM sessions WHERE id=?`, sessionID).Scan(
		&config.ID,
		&config.Provider,
		&config.Model,
		&config.KeyIndex,
		&config.ThinkingLevel,
		&temperature,
		&topP,
		&topK,
		&stopSequences,
		&presence,
		&frequency,
		&seed,
		&config.MaxOutputTokens,
		&agentMode,
		&workspace,
	)
	if err != nil {
		return provider.SessionConfig{}, err
	}
	if temperature.Valid {
		v := temperature.Float64
		config.Temperature = &v
	}
	if topP.Valid {
		v := topP.Float64
		config.TopP = &v
	}
	if topK.Valid {
		v := topK.Float64
		config.TopK = &v
	}
	if stopSequences.Valid {
		config.StopSequences = decodeStrings(stopSequences.String)
	}
	if presence.Valid {
		v := presence.Float64
		config.PresencePenalty = &v
	}
	if frequency.Valid {
		v := frequency.Float64
		config.FrequencyPenalty = &v
	}
	if seed.Valid {
		v := seed.Int64
		config.Seed = &v
	}
	config.AgentMode = provider.AgentMode(agentMode)
	if workspace.Valid {
		config.Workspace = workspace.String
	}
	return config, nil
}

func (s *SessionDB) LoadUsage(sessionID string) (provider.Usage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var usage provider.Usage
	err := s.db.QueryRow(`
		SELECT input_tokens,output_tokens,total_tokens,cache_read_tokens,cache_write_tokens
		FROM sessions WHERE id=?`, sessionID).Scan(
		&usage.InputTokens,
		&usage.OutputTokens,
		&usage.TotalTokens,
		&usage.CacheReadTokens,
		&usage.CacheWriteTokens,
	)
	if err != nil {
		return provider.Usage{}, err
	}
	return usage, nil
}

func (s *SessionDB) LoadHistory(sessionID string) ([]provider.Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`
		SELECT role,content_json,tool_call_json,tool_result_json,reasoning_json
		FROM turns WHERE session_id=? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []provider.Turn
	for rows.Next() {
		var role, contentJSON string
		var callJSON, resultJSON, reasoningJSON sql.NullString
		if err := rows.Scan(&role, &contentJSON, &callJSON, &resultJSON, &reasoningJSON); err != nil {
			return nil, err
		}
		var content []provider.ContentPart
		if err := json.Unmarshal([]byte(contentJSON), &content); err != nil {
			return nil, err
		}
		turn := provider.Turn{Role: provider.Role(role), Content: content}
		if callJSON.Valid && callJSON.String != "" {
			var call provider.ToolCall
			if err := json.Unmarshal([]byte(callJSON.String), &call); err != nil {
				return nil, err
			}
			turn.ToolCall = &call
		}
		if resultJSON.Valid && resultJSON.String != "" {
			var result provider.ToolResult
			if err := json.Unmarshal([]byte(resultJSON.String), &result); err != nil {
				return nil, err
			}
			turn.ToolResult = &result
		}
		if reasoningJSON.Valid && reasoningJSON.String != "" {
			var reasoning provider.ReasoningState
			if err := json.Unmarshal([]byte(reasoningJSON.String), &reasoning); err != nil {
				return nil, err
			}
			turn.Reasoning = &reasoning
		}
		out = append(out, turn)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *SessionDB) AppendTurns(sessionID string, turns []provider.Turn, startSeq int) error {
	if len(turns) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO turns(session_id,seq,role,content_json,tool_call_json,tool_result_json,reasoning_json,created_at)
		VALUES(?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for i, turn := range turns {
		content, err := json.Marshal(turn.Content)
		if err != nil {
			return err
		}
		var call, result, reasoning any
		if turn.ToolCall != nil {
			b, err := json.Marshal(turn.ToolCall)
			if err != nil {
				return err
			}
			call = string(b)
		}
		if turn.ToolResult != nil {
			b, err := json.Marshal(turn.ToolResult)
			if err != nil {
				return err
			}
			result = string(b)
		}
		if turn.Reasoning != nil {
			b, err := json.Marshal(turn.Reasoning)
			if err != nil {
				return err
			}
			reasoning = string(b)
		}
		if _, err := stmt.Exec(
			sessionID,
			startSeq+i,
			string(turn.Role),
			string(content),
			call,
			result,
			reasoning,
			time.Now().UTC().Format(time.RFC3339Nano),
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *SessionDB) ReplaceTurns(sessionID string, turns []provider.Turn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM turns WHERE session_id=?`, sessionID); err != nil {
		return err
	}

	stmt, err := tx.Prepare(`
		INSERT INTO turns(session_id,seq,role,content_json,tool_call_json,tool_result_json,reasoning_json,created_at)
		VALUES(?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for i, turn := range turns {
		content, err := json.Marshal(turn.Content)
		if err != nil {
			return err
		}
		var call, result, reasoning any
		if turn.ToolCall != nil {
			b, err := json.Marshal(turn.ToolCall)
			if err != nil {
				return err
			}
			call = string(b)
		}
		if turn.ToolResult != nil {
			b, err := json.Marshal(turn.ToolResult)
			if err != nil {
				return err
			}
			result = string(b)
		}
		if turn.Reasoning != nil {
			b, err := json.Marshal(turn.Reasoning)
			if err != nil {
				return err
			}
			reasoning = string(b)
		}
		if _, err := stmt.Exec(
			sessionID,
			i,
			string(turn.Role),
			string(content),
			call,
			result,
			reasoning,
			time.Now().UTC().Format(time.RFC3339Nano),
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *SessionDB) RecordRequest(sessionID string, attempt int, req provider.Request) (int64, error) {
	tools, err := json.Marshal(req.Tools)
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	_, err = tx.Exec(`
		INSERT INTO contexts(session_id,system_prompt,tools_json)
		VALUES(?,?,?)
		ON CONFLICT(session_id,system_prompt,tools_json) DO NOTHING`,
		sessionID,
		req.SystemPrompt,
		string(tools),
	)
	if err != nil {
		return 0, err
	}

	var contextID int64
	if err := tx.QueryRow(`
		SELECT id FROM contexts
		WHERE session_id=? AND system_prompt=? AND tools_json=?`,
		sessionID,
		req.SystemPrompt,
		string(tools),
	).Scan(&contextID); err != nil {
		return 0, err
	}

	res, err := tx.Exec(`
		INSERT INTO attempts(
			session_id,context_id,attempt,created_at,provider,model,temperature,
			thinking_level,max_output_tokens,stream
		) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		sessionID,
		contextID,
		attempt,
		now,
		req.Provider,
		req.Model,
		nullableFloat(req.Temperature),
		req.ThinkingLevel,
		req.MaxOutputTokens,
		boolInt(req.Stream),
	)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *SessionDB) RecordResponse(requestID int64, resp provider.Response, err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, txErr := s.db.Begin()
	if txErr != nil {
		return txErr
	}
	defer tx.Rollback()

	var existingSessionID string
	if scanErr := tx.QueryRow(`SELECT session_id FROM attempts WHERE id=?`, requestID).Scan(&existingSessionID); scanErr != nil {
		return scanErr
	}

	_, txErr = tx.Exec(`
		UPDATE attempts SET
			success=?,
			finish_reason=?,
			input_tokens=?,
			output_tokens=?,
			total_tokens=?,
			cache_read_tokens=?,
			cache_write_tokens=?,
			cache_hit=?,
			cache_layer=?,
			error=?
		WHERE id=?`,
		boolInt(err == nil),
		nullableString(resp.FinishReason),
		resp.Usage.InputTokens,
		resp.Usage.OutputTokens,
		resp.Usage.TotalTokens,
		resp.Usage.CacheReadTokens,
		resp.Usage.CacheWriteTokens,
		boolInt(resp.Cache.Hit),
		nullableString(resp.Cache.Layer),
		nullableString(errorText(err)),
		requestID,
	)
	if txErr != nil {
		return txErr
	}

	_, txErr = tx.Exec(`
		UPDATE sessions SET
			input_tokens=input_tokens+?,
			output_tokens=output_tokens+?,
			total_tokens=total_tokens+?,
			cache_read_tokens=cache_read_tokens+?,
			cache_write_tokens=cache_write_tokens+?,
			cache_hits=cache_hits+?,
			updated_at=?
		WHERE id=?`,
		resp.Usage.InputTokens,
		resp.Usage.OutputTokens,
		resp.Usage.TotalTokens,
		resp.Usage.CacheReadTokens,
		resp.Usage.CacheWriteTokens,
		boolInt(resp.Cache.Hit),
		time.Now().UTC().Format(time.RFC3339Nano),
		existingSessionID,
	)
	if txErr != nil {
		return txErr
	}

	return tx.Commit()
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *SessionDB) Dir() string {
	if s == nil || s.path == "" {
		return ""
	}
	return filepath.Dir(s.path)
}
