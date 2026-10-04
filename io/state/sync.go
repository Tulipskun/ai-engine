package state

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ConfigFile is one runtime config file that syncs with D1. Keys match the
// layout already mandated by CON-001, so the local file stays the format the
// existing loaders/savers use.
type ConfigFile struct {
	Key  string // D1 state key, e.g. config/provider
	Path string // local path, e.g. <state>/config/provider.json
}

// DefaultConfigFiles lists every config file the daemon syncs with D1. The
// Worker key charset is [A-Za-z0-9:_-], so the slash lives in the prefix.
//
// Providers are deliberately NOT here: they live in the D1 providers table
// (read row by row at boot, written row by row by the admin surface), not
// in a state blob, so the phone's provider edits never travel through the
// file sync path.
// config/entry.json is deliberately NOT here either: it is the gateway bootstrap (where
// the tunnel points, whether the tunnel runs at all), so it must exist locally
// before D1 is reachable. Syncing it would let a stale cloud copy disable the
// gateway that is supposed to fetch the cloud copy (CHANGE-059/CON-012).
func DefaultConfigFiles(stateRoot string) []ConfigFile {
	join := func(name string) string { return filepath.Join(stateRoot, "config", name) }
	return []ConfigFile{
		{Key: "config:system", Path: join("system.json")},
	}
}

// SyncReport records what a hydrate/push pass actually moved, so the daemon can
// log the outcome instead of failing silently.
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

// HydrateConfig writes the D1 copies of the config files into the state root.
// A missing key leaves the local file untouched, so a first run on an empty D1
// still boots from whatever the operator has locally.
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

// SessionBlob is the D1 representation of one session database: base64 of the
// SQLite file plus a flag telling HydrateSessions whether it came from a clean
// close. WAL contents are checkpointed by the caller before PushSession.
type SessionBlob struct {
	Version int    `json:"version"`
	Name    string `json:"name"`
	Data    string `json:"data"`
}

// HydrateSessions restores session databases from D1 into dir, using the same
// file naming the SDK already uses (base64url(sessionID) + ".db"). Existing
// files are replaced only when D1 actually has a copy, so an offline daemon
// keeps working from local state.
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
			return report, fmt.Errorf("d1store: decode session %q: %w", sessionID, err)
		}
		raw, err := base64.StdEncoding.DecodeString(blob.Data)
		if err != nil {
			return report, fmt.Errorf("d1store: session %q payload: %w", sessionID, err)
		}
		if err := writeFileAtomic(filepath.Join(dir, encodeName(sessionID)), raw, 0o600); err != nil {
			return report, err
		}
		report.PulledSession = append(report.PulledSession, sessionID)
	}
	return report, nil
}

// PushSession uploads one session database. Oversized files are skipped with a
// report entry rather than truncated (CON-012).
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

// PushSessions uploads several session databases, continuing past per-session
// failures so one oversized DB cannot block the rest.
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

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".d1store-*.tmp")
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
