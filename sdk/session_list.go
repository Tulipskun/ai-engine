package sdk

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type SessionInfo struct {
	ID        string
	Provider  ProviderID
	Model     string
	UpdatedAt time.Time
	TurnCount int
}

// SessionDBPath returns the stable on-disk path for one session database.
func SessionDBPath(dir, sessionID string) string {
	encoded := base64.RawURLEncoding.EncodeToString([]byte(sessionID))
	return filepath.Join(dir, encoded+".db")
}

// ListSessionsInDir lists the sessions stored as one SQLite file per session.
func ListSessionsInDir(dir string, limit int) ([]SessionInfo, error) {
	if dir == "" {
		return nil, errors.New("sdk: session directory is required")
	}
	if limit <= 0 {
		limit = 20
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]SessionInfo, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".db" {
			continue
		}
		db, err := OpenSessionDB(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		items, err := db.ListSessions(1)
		_ = db.Close()
		if err != nil || len(items) == 0 {
			continue
		}
		out = append(out, items[0])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *SessionDB) ListSessions(limit int) ([]SessionInfo, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`
		SELECT s.id, s.provider, s.model, s.updated_at,
		       (SELECT COUNT(*) FROM turns t WHERE t.session_id = s.id)
		FROM sessions s
		ORDER BY s.updated_at DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionInfo
	for rows.Next() {
		var item SessionInfo
		var updated string
		if err := rows.Scan(&item.ID, &item.Provider, &item.Model, &updated, &item.TurnCount); err != nil {
			return nil, err
		}
		if parsed, err := time.Parse(time.RFC3339Nano, updated); err == nil {
			item.UpdatedAt = parsed
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
