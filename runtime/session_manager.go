package runtime

import (
	"container/list"
	"context"
	"errors"
	"path/filepath"
	"sync"

	"github.com/Tulipskun/ai-engine/sdk"
)

const defaultMaxCachedSessions = 8

type sessionCacheEntry struct {
	id      string
	session *sdk.Session
}
type SessionManager struct {
	// defaults supplies the provider and model a phone chose for a chat; it may
	// do I/O, so it is guarded separately from the session table.
	defaultsMu sync.RWMutex
	defaults   func(context.Context, string) (sdk.ProviderID, string, bool)

	dir          string
	base         sdk.SessionConfig
	keys         *sdk.KeyPool
	providerKeys map[sdk.ProviderID]*sdk.KeyPool
	mu           sync.RWMutex
	sessions     map[string]*list.Element
	lru          *list.List
	maxCached    int
}

func sessionDir(path string) string {
	clean := filepath.Clean(path)
	if filepath.Ext(clean) == ".db" {
		return filepath.Join(filepath.Dir(clean), "sessions")
	}
	return clean
}

func NewSessionManager(path string, base sdk.SessionConfig, keys *sdk.KeyPool) *SessionManager {
	return &SessionManager{dir: sessionDir(path), base: base, keys: keys, providerKeys: make(map[sdk.ProviderID]*sdk.KeyPool), sessions: make(map[string]*list.Element), lru: list.New(), maxCached: defaultMaxCachedSessions}
}
func NewSessionManagerWithProviders(path string, base sdk.SessionConfig, providers []sdk.ProviderConfig) *SessionManager {
	providerKeys := make(map[sdk.ProviderID]*sdk.KeyPool, len(providers))
	var fallback *sdk.KeyPool
	for _, provider := range providers {
		if provider.Keys == nil {
			continue
		}
		providerKeys[provider.ID] = provider.Keys
		if fallback == nil {
			fallback = provider.Keys
		}
	}
	if base.Provider != "" && providerKeys[base.Provider] != nil {
		fallback = providerKeys[base.Provider]
	}
	return &SessionManager{dir: sessionDir(path), base: base, keys: fallback, providerKeys: providerKeys, sessions: make(map[string]*list.Element), lru: list.New(), maxCached: defaultMaxCachedSessions}
}

// AdoptProviders swaps in the provider set the daemon actually has after a
// config reload, and re-points sessions whose stored provider is gone. A
// session row hydrated from D1 keeps whatever provider wrote it, so without
// this a chat created before the restart would keep a provider that no longer
// exists and every turn on it fails with "provider is required" (REQ-046(4)).
func (m *SessionManager) AdoptProviders(configs []sdk.ProviderConfig, base sdk.SessionConfig) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if base.Provider != "" {
		m.base.Provider = base.Provider
	}
	if base.Model != "" {
		m.base.Model = base.Model
	}
	for _, config := range configs {
		if config.ID != "" && config.Keys != nil {
			m.providerKeys[config.ID] = config.Keys
		}
	}
	for _, entry := range m.sessions {
		_ = m.repointUnknownProviderLocked(entry.Value.(*sessionCacheEntry).session)
	}
}

func (m *SessionManager) repointUnknownProviderLocked(session *sdk.Session) error {
	if session == nil || m.base.Provider == "" {
		return nil
	}
	stored := session.Config().Provider
	if stored != "" && m.providerKeys[stored] != nil {
		return nil
	}
	keys := m.keys
	if providerKeys := m.providerKeys[m.base.Provider]; providerKeys != nil {
		keys = providerKeys
	}
	if err := session.SetProvider(m.base.Provider, keys); err != nil {
		return err
	}
	if m.base.Model != "" {
		return session.SetModel(m.base.Model)
	}
	return nil
}

func (m *SessionManager) RegisterProvider(provider sdk.ProviderID, keys *sdk.KeyPool) {
	if m == nil || provider == "" || keys == nil {
		return
	}
	m.mu.Lock()
	m.providerKeys[provider] = keys
	m.mu.Unlock()
}

// SetSessionDefaults installs the lookup that supplies the provider and model a
// chat was last saved with. It runs outside the manager's lock, so it may do
// network work (the daemon reads the row from D1), and it is only consulted for
// a session the manager has to open.
func (m *SessionManager) SetSessionDefaults(lookup func(context.Context, string) (sdk.ProviderID, string, bool)) {
	if m == nil {
		return
	}
	m.defaultsMu.Lock()
	m.defaults = lookup
	m.defaultsMu.Unlock()
}

func (m *SessionManager) sessionDefaults(ctx context.Context, sessionID string) (sdk.ProviderID, string, bool) {
	m.defaultsMu.RLock()
	lookup := m.defaults
	m.defaultsMu.RUnlock()
	if lookup == nil {
		return "", "", false
	}
	return lookup(ctx, sessionID)
}

func (m *SessionManager) Resolve(ctx context.Context, input sdk.Input) (*sdk.Session, error) {
	if m == nil {
		return nil, errors.New("runtime: session manager is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input.SessionID == "" {
		return nil, errors.New("runtime: input SessionID is required")
	}
	m.mu.Lock()
	if elem, ok := m.sessions[input.SessionID]; ok {
		m.lru.MoveToFront(elem)
		session := elem.Value.(*sessionCacheEntry).session
		m.mu.Unlock()
		return session, nil
	}
	config := m.base
	config.ID = input.SessionID
	keys := m.keys
	if providerKeys := m.providerKeys[config.Provider]; providerKeys != nil {
		keys = providerKeys
	}
	m.mu.Unlock()

	path := sdk.SessionDBPath(m.dir, input.SessionID)
	session, err := sdk.OpenSession(path, config, keys)
	if err != nil {
		return nil, err
	}
	// What the phone picked last time wins over the boot default, and only when
	// the daemon can still route to it.
	applied := false
	if provider, model, ok := m.sessionDefaults(ctx, input.SessionID); ok && provider != "" && model != "" {
		m.mu.RLock()
		providerKeys := m.providerKeys[provider]
		m.mu.RUnlock()
		if providerKeys != nil {
			if err := session.SetProvider(provider, providerKeys); err == nil {
				if err := session.SetModel(model); err == nil {
					applied = true
				}
			}
		}
	}
	m.mu.Lock()
	loadedProvider := session.Config().Provider
	if providerKeys := m.providerKeys[loadedProvider]; providerKeys != nil {
		if err := session.SetKeyPool(providerKeys); err != nil {
			m.mu.Unlock()
			_ = session.Close()
			return nil, err
		}
	}
	if !applied {
		if err := m.repointUnknownProviderLocked(session); err != nil {
			m.mu.Unlock()
			_ = session.Close()
			return nil, err
		}
	}
	// Another goroutine may have opened the same chat while this one waited
	// outside the lock: keep one session object per id.
	if elem, ok := m.sessions[input.SessionID]; ok {
		existing := elem.Value.(*sessionCacheEntry).session
		m.mu.Unlock()
		_ = session.Close()
		return existing, nil
	}
	elem := m.lru.PushFront(&sessionCacheEntry{id: input.SessionID, session: session})
	m.sessions[input.SessionID] = elem
	m.evictLocked()
	m.mu.Unlock()
	return session, nil
}
func (m *SessionManager) evictLocked() {
	limit := m.maxCached
	if limit <= 0 {
		limit = defaultMaxCachedSessions
	}
	for m.lru.Len() > limit {
		elem := m.lru.Back()
		if elem == nil {
			return
		}
		entry := elem.Value.(*sessionCacheEntry)
		delete(m.sessions, entry.id)
		m.lru.Remove(elem)
	}
}
func (m *SessionManager) ListSessions(limit int) ([]sdk.SessionInfo, error) {
	if m == nil {
		return nil, errors.New("runtime: session manager is nil")
	}
	return sdk.ListSessionsInDir(m.dir, limit)
}

// Forget drops one live chat from the in-memory cache and closes it, so the
// next turn reopens that session from the current defaults. Clearing a session
// route calls this after removing the D1 pin: the old provider/model must not
// survive in a cached object.
// ApplyGeneration writes the generation knobs onto one chat and returns the
// session database it wrote to, so the caller can push that file to D1. The knobs
// are set through the session setters so the same bounds apply and the change is
// persisted by the same path as everything else about the session (CHANGE-077).
func (m *SessionManager) ApplyGeneration(ctx context.Context, sessionID string, settings sdk.GenerationSettings, clear bool) (string, error) {
	if m == nil {
		return "", errors.New("runtime: session manager is nil")
	}
	session, err := m.Resolve(ctx, sdk.Input{SessionID: sessionID})
	if err != nil {
		return "", err
	}
	if clear {
		for _, unset := range []func() error{
			func() error { return session.ClearTemperature() },
			func() error { return session.ClearThinkingLevel() },
			func() error { return session.ClearTopP() },
			func() error { return session.ClearTopK() },
			func() error { return session.ClearStopSequences() },
			func() error { return session.ClearPresencePenalty() },
			func() error { return session.ClearFrequencyPenalty() },
			func() error { return session.ClearSeed() },
		} {
			if err := unset(); err != nil {
				return "", err
			}
		}
		if err := session.SetMaxOutputTokens(0); err != nil {
			return "", err
		}
		return sdk.SessionDBPath(m.dir, sessionID), nil
	}
	if settings.Temperature != nil {
		if err := session.SetTemperature(*settings.Temperature); err != nil {
			return "", err
		}
	}
	if settings.ThinkingLevel != "" {
		if err := session.SetThinkingLevel(sdk.ThinkingLevel(settings.ThinkingLevel)); err != nil {
			return "", err
		}
	}
	if settings.TopP != nil {
		if err := session.SetTopP(*settings.TopP); err != nil {
			return "", err
		}
	}
	if settings.TopK != nil {
		if err := session.SetTopK(*settings.TopK); err != nil {
			return "", err
		}
	}
	if len(settings.StopSequences) > 0 {
		if err := session.SetStopSequences(settings.StopSequences); err != nil {
			return "", err
		}
	}
	if settings.PresencePenalty != nil {
		if err := session.SetPresencePenalty(*settings.PresencePenalty); err != nil {
			return "", err
		}
	}
	if settings.FrequencyPenalty != nil {
		if err := session.SetFrequencyPenalty(*settings.FrequencyPenalty); err != nil {
			return "", err
		}
	}
	if settings.Seed != nil {
		if err := session.SetSeed(*settings.Seed); err != nil {
			return "", err
		}
	}
	if err := session.SetMaxOutputTokens(settings.MaxOutputTokens); err != nil {
		return "", err
	}
	return sdk.SessionDBPath(m.dir, sessionID), nil
}

// SessionGeneration reads back what one chat runs on, so the phone can be told
// what it is editing rather than guessing from what it last sent.
func (m *SessionManager) SessionGeneration(ctx context.Context, sessionID string) (sdk.GenerationSettings, bool, error) {
	if m == nil {
		return sdk.GenerationSettings{}, false, errors.New("runtime: session manager is nil")
	}
	session, err := m.Resolve(ctx, sdk.Input{SessionID: sessionID})
	if err != nil {
		return sdk.GenerationSettings{}, false, err
	}
	cfg := session.Config()
	return sdk.GenerationSettings{
		ThinkingLevel:    cfg.ThinkingLevel,
		Temperature:      cfg.Temperature,
		TopP:             cfg.TopP,
		TopK:             cfg.TopK,
		StopSequences:    cfg.StopSequences,
		PresencePenalty:  cfg.PresencePenalty,
		FrequencyPenalty: cfg.FrequencyPenalty,
		Seed:             cfg.Seed,
		MaxOutputTokens:  cfg.MaxOutputTokens,
	}, true, nil
}

func (m *SessionManager) Forget(id string) {
	if m == nil || id == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	elem, ok := m.sessions[id]
	if !ok {
		return
	}
	if entry, ok := elem.Value.(*sessionCacheEntry); ok && entry.session != nil {
		_ = entry.session.Close()
	}
	delete(m.sessions, id)
	m.lru.Remove(elem)
}

func (m *SessionManager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var firstErr error
	for id, elem := range m.sessions {
		entry := elem.Value.(*sessionCacheEntry)
		if err := entry.session.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(m.sessions, id)
	}
	m.lru.Init()
	return firstErr
}

// WorkspaceFor reports the persisted workspace of a live session without
// opening anything new. Empty means "use the process default" (REQ-038).
func (m *SessionManager) WorkspaceFor(sessionID string) string {
	if m == nil || sessionID == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if elem, ok := m.sessions[sessionID]; ok {
		return elem.Value.(*sessionCacheEntry).session.Config().Workspace
	}
	return ""
}
