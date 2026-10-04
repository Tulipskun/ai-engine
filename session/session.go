package session

import (
	"ai-engine/provider"
	"container/list"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// ---- from session/session.go ----
type PlanStep struct {
	Index  int
	Text   string
	Status string
}
type PlanState struct {
	Revision uint64
	Steps    []PlanStep
	Current  int
}

type Session struct {
	mu        sync.RWMutex
	config    provider.SessionConfig
	keys      *provider.KeyPool
	history   []provider.Turn
	store     *SessionDB
	plan      PlanState
	activeJob string
}

func NewSession(config provider.SessionConfig, keys *provider.KeyPool) *Session {
	return &Session{config: config, keys: keys}
}
func OpenSession(path string, config provider.SessionConfig, keys *provider.KeyPool) (*Session, error) {
	store, err := OpenSessionDB(path)
	if err != nil {
		return nil, err
	}
	persisted, loadErr := store.LoadSession(config.ID)
	if loadErr == nil {
		config = persisted
	} else if !errors.Is(loadErr, sql.ErrNoRows) {
		store.Close()
		return nil, loadErr
	} else if err := store.SaveSession(config); err != nil {
		store.Close()
		return nil, err
	}
	history, err := store.LoadHistory(config.ID)
	if err != nil {
		store.Close()
		return nil, err
	}
	return &Session{config: config, keys: keys, history: history, store: store}, nil
}
func (s *Session) Close() error {
	s.mu.RLock()
	store := s.store
	s.mu.RUnlock()
	if store == nil {
		return nil
	}
	return store.Close()
}
func (s *Session) ID() string { return s.config.ID }
func (s *Session) Config() provider.SessionConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config.Clone()
}

// keyPoolSize reports how many keys this session could try. A session with no
// pool, or a pool of one, has nothing to rotate to.
func (s *Session) KeyPoolSize() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	keys := s.keys
	s.mu.RUnlock()
	if keys == nil {
		return 0
	}
	return keys.Len()
}

func (s *Session) APIKey() (string, error) {
	s.mu.RLock()
	keys, index := s.keys, s.config.KeyIndex
	s.mu.RUnlock()
	if keys == nil {
		return "", errors.New("sdk: session has no key pool")
	}
	return keys.At(index)
}
func (s *Session) RotateAPIKey() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys == nil {
		return "", errors.New("sdk: session has no key pool")
	}
	key, err := s.keys.Rotate()
	if err != nil {
		return "", err
	}
	s.config.KeyIndex = s.keys.IndexOfCurrent()
	return key, nil
}
func (s *Session) History() []provider.Turn {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return CloneTurns(s.history)
}
func (s *Session) CleanPlan(steps []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.plan = PlanState{Revision: s.plan.Revision + 1}
	for _, step := range steps {
		step = strings.TrimSpace(step)
		if step == "" {
			continue
		}
		s.plan.Steps = append(s.plan.Steps, PlanStep{Index: len(s.plan.Steps) + 1, Text: step, Status: "pending"})
	}
	if len(s.plan.Steps) > 0 {
		s.plan.Steps[0].Status = "ready"
	}
}

func (s *Session) Plan() PlanState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	steps := append([]PlanStep(nil), s.plan.Steps...)
	return PlanState{Revision: s.plan.Revision, Steps: steps, Current: s.plan.Current}
}

func (s *Session) CurrentPlanStep() (PlanStep, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.plan.Current < 0 || s.plan.Current >= len(s.plan.Steps) {
		return PlanStep{}, false
	}
	return s.plan.Steps[s.plan.Current], true
}

// BoundJob is what the session records about work delegated out of it: enough
// to tell whether a later report still belongs to this plan and this step. The
// session deliberately knows nothing else about the delegating machinery, so the
// loop that delegates can change without touching storage.
type BoundJob struct {
	ID       string
	Revision uint64
	Planned  bool
	Step     int
}

// reserveSubAgent binds a reservation to a snapshot under the plan lock. A
// replacement plan does not clear activeJob: cancellation must finish first.
func (s *Session) ReserveSubAgent(id string, retry *BoundJob) (PlanState, PlanStep, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeJob != "" {
		return PlanState{}, PlanStep{}, false, errors.New("sdk: parent already has a running sub-agent")
	}
	planned := s.plan.Current < len(s.plan.Steps)
	step := PlanStep{}
	if planned {
		step = s.plan.Steps[s.plan.Current]
	}
	if retry != nil {
		if retry.Revision != s.plan.Revision || retry.Planned != planned || (planned && retry.Step != step.Index) {
			return PlanState{}, PlanStep{}, false, errors.New("sdk: stale sub-agent result belongs to another plan or step")
		}
	}
	if planned {
		allowed := step.Status == "ready"
		if retry != nil {
			allowed = step.Status == "awaiting_review" || step.Status == "failed"
		}
		if !allowed {
			return PlanState{}, PlanStep{}, false, errors.New("sdk: current step requires review/acceptance or follow-up of its existing job")
		}
		s.plan.Steps[s.plan.Current].Status = "running"
	}
	s.activeJob = id
	snapshot := s.plan
	snapshot.Steps = append([]PlanStep(nil), s.plan.Steps...)
	return snapshot, step, planned, nil
}

// reserveSubAgentForContinue binds a new-work reservation to the current plan
// state while reusing an existing worker session. Unlike retry it does not
// require the same revision/step: after acceptance or plan completion the new
// job attaches to the current ready step, or runs as investigation when no
// step is ready.
func (s *Session) ReserveSubAgentForContinue(id string) (PlanState, PlanStep, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeJob != "" {
		return PlanState{}, PlanStep{}, false, errors.New("sdk: parent already has a running sub-agent")
	}
	planned := s.plan.Current < len(s.plan.Steps)
	step := PlanStep{}
	if planned {
		step = s.plan.Steps[s.plan.Current]
		if step.Status != "ready" {
			return PlanState{}, PlanStep{}, false, errors.New("sdk: current step requires review/acceptance or follow-up of its existing job")
		}
		s.plan.Steps[s.plan.Current].Status = "running"
	}
	s.activeJob = id
	snapshot := s.plan
	snapshot.Steps = append([]PlanStep(nil), s.plan.Steps...)
	return snapshot, step, planned, nil
}

// bindsCurrentStep reports whether job is the job the session's current plan
// step is bound to. That is what separates "retry this step" from "add new work
// to this worker session" when the planner sends more work to an existing job
// (CHANGE-099). A completed step is excluded: its job was already accepted, so
// more work for it is follow-on, not a retry.
func (s *Session) BindsCurrentStep(job BoundJob) bool {
	if s == nil || !job.Planned {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.plan.Current >= len(s.plan.Steps) {
		return false
	}
	step := s.plan.Steps[s.plan.Current]
	return s.plan.Revision == job.Revision && step.Index == job.Step && step.Status != "completed"
}

func (s *Session) FinishSubAgent(job BoundJob, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeJob != job.ID {
		return
	}
	s.activeJob = ""
	if !s.matchesJob(job) {
		return
	}
	if status == "completed" {
		s.plan.Steps[s.plan.Current].Status = "awaiting_review"
	} else {
		s.plan.Steps[s.plan.Current].Status = "failed"
	}
}

// matchesJob requires s.mu. The revision protects even identically worded replacement plans.
func (s *Session) matchesJob(job BoundJob) bool {
	return job.Planned && s.plan.Revision == job.Revision && s.plan.Current < len(s.plan.Steps) && s.plan.Steps[s.plan.Current].Index == job.Step
}

func (s *Session) AcceptSubAgent(job BoundJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeJob != "" || !s.matchesJob(job) || s.plan.Steps[s.plan.Current].Status != "awaiting_review" {
		return errors.New("sdk: result is not awaiting acceptance for the current plan step")
	}
	s.plan.Steps[s.plan.Current].Status = "completed"
	s.plan.Current++
	if s.plan.Current < len(s.plan.Steps) {
		s.plan.Steps[s.plan.Current].Status = "ready"
	}
	return nil
}

func (s *Session) Append(turns ...provider.Turn) {
	if len(turns) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cloned := CloneTurns(turns)
	start := len(s.history)
	s.history = append(s.history, cloned...)
	if s.store != nil {
		if err := s.store.AppendTurns(s.config.ID, cloned, start); err != nil {
			s.history = s.history[:start]
			panic(fmt.Sprintf("sdk: persist session append: %v", err))
		}
	}
}
func (s *Session) ReplaceHistory(turns []provider.Turn) {
	repaired := repairTurns(CloneTurns(turns))
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.history
	s.history = repaired
	if s.store != nil {
		if err := s.store.ReplaceTurns(s.config.ID, repaired); err != nil {
			s.history = old
			panic(fmt.Sprintf("sdk: persist session replace: %v", err))
		}
	}
}
func (s *Session) RepairHistory() { s.ReplaceHistory(s.History()) }
func (s *Session) RecordRequest(attempt int, req provider.Request) (int64, error) {
	s.mu.RLock()
	store, id := s.store, s.config.ID
	s.mu.RUnlock()
	if store == nil {
		return 0, nil
	}
	return store.RecordRequest(id, attempt, req)
}
func (s *Session) RecordResponse(requestID int64, resp provider.Response, err error) error {
	s.mu.RLock()
	store := s.store
	s.mu.RUnlock()
	if store == nil {
		return nil
	}
	return store.RecordResponse(requestID, resp, err)
}

// LoadUsage reports the session's cumulative token/cache totals persisted in
// its database. A session without a store has no totals to report.
func (s *Session) LoadUsage() (provider.Usage, error) {
	s.mu.RLock()
	store, id := s.store, s.config.ID
	s.mu.RUnlock()
	if store == nil {
		return provider.Usage{}, nil
	}
	return store.LoadUsage(id)
}
func CloneTurns(in []provider.Turn) []provider.Turn {
	out := make([]provider.Turn, len(in))
	copy(out, in)
	for i := range out {
		out[i] = CloneTurn(out[i])
	}
	return out
}
func CloneTurn(turn provider.Turn) provider.Turn {
	turn.Content = append([]provider.ContentPart(nil), turn.Content...)
	if turn.ToolCall != nil {
		v := *turn.ToolCall
		turn.ToolCall = &v
	}
	if turn.ToolResult != nil {
		v := *turn.ToolResult
		turn.ToolResult = &v
	}
	if turn.Reasoning != nil {
		v := *turn.Reasoning
		turn.Reasoning = &v
	}
	return turn
}
func repairTurns(in []provider.Turn) []provider.Turn {
	out := make([]provider.Turn, 0, len(in))
	pending := make(map[string]bool)
	for _, turn := range in {
		switch turn.Role {
		case provider.RoleUser, provider.RoleModel:
			out = append(out, turn)
		case provider.RoleToolCall:
			if turn.ToolCall == nil || turn.ToolCall.ID == "" || turn.ToolCall.Name == "" || pending[turn.ToolCall.ID] {
				continue
			}
			pending[turn.ToolCall.ID] = true
			out = append(out, turn)
		case provider.RoleToolResult:
			if turn.ToolResult == nil || turn.ToolResult.ID == "" || !pending[turn.ToolResult.ID] {
				continue
			}
			delete(pending, turn.ToolResult.ID)
			out = append(out, turn)
		}
	}
	if len(pending) == 0 {
		return out
	}
	final := out[:0]
	for _, turn := range out {
		if turn.Role == provider.RoleToolCall && pending[turn.ToolCall.ID] {
			continue
		}
		final = append(final, turn)
	}
	return final
}
func CommitResponse(session *Session, resp provider.Response) {
	if len(resp.Content) > 0 || resp.Reasoning != nil {
		turn := provider.Turn{Role: provider.RoleModel, Content: append([]provider.ContentPart(nil), resp.Content...)}
		if resp.Reasoning != nil {
			r := *resp.Reasoning
			turn.Reasoning = &r
		}
		session.Append(turn)
	}
	for _, call := range resp.ToolCalls {
		call := call
		session.Append(provider.Turn{Role: provider.RoleToolCall, ToolCall: &call})
	}
}

// ---- from session/manager.go ----
const defaultMaxCachedSessions = 8

type sessionCacheEntry struct {
	id      string
	session *Session
}
type SessionManager struct {
	// defaults supplies the provider and model a phone chose for a chat; it may
	// do I/O, so it is guarded separately from the session table.
	defaultsMu sync.RWMutex
	defaults   func(context.Context, string) (provider.ProviderID, string, bool)

	dir          string
	base         provider.SessionConfig
	keys         *provider.KeyPool
	providerKeys map[provider.ProviderID]*provider.KeyPool
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

func NewSessionManagerWithProviders(path string, base provider.SessionConfig, providers []provider.ProviderConfig) *SessionManager {
	providerKeys := make(map[provider.ProviderID]*provider.KeyPool, len(providers))
	var fallback *provider.KeyPool
	for _, providerLocal := range providers {
		if providerLocal.Keys == nil {
			continue
		}
		providerKeys[providerLocal.ID] = providerLocal.Keys
		if fallback == nil {
			fallback = providerLocal.Keys
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
func (m *SessionManager) AdoptProviders(configs []provider.ProviderConfig, base provider.SessionConfig) {
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

func (m *SessionManager) repointUnknownProviderLocked(session *Session) error {
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

func (m *SessionManager) RegisterProvider(providerLocal provider.ProviderID, keys *provider.KeyPool) {
	if m == nil || providerLocal == "" || keys == nil {
		return
	}
	m.mu.Lock()
	m.providerKeys[providerLocal] = keys
	m.mu.Unlock()
}

// SetSessionDefaults installs the lookup that supplies the provider and model a
// chat was last saved with. It runs outside the manager's lock, so it may do
// network work (the daemon reads the row from D1), and it is only consulted for
// a session the manager has to open.
func (m *SessionManager) SetSessionDefaults(lookup func(context.Context, string) (provider.ProviderID, string, bool)) {
	if m == nil {
		return
	}
	m.defaultsMu.Lock()
	m.defaults = lookup
	m.defaultsMu.Unlock()
}

func (m *SessionManager) sessionDefaults(ctx context.Context, sessionID string) (provider.ProviderID, string, bool) {
	m.defaultsMu.RLock()
	lookup := m.defaults
	m.defaultsMu.RUnlock()
	if lookup == nil {
		return "", "", false
	}
	return lookup(ctx, sessionID)
}

// Resolve answers the one question this package exists for: which session does
// this id belong to. Every conversation goes through here — a phone's chat and a
// worker's own session alike — so identity, key assignment, provider repointing
// and eviction behave identically for both.
func (m *SessionManager) Resolve(ctx context.Context, id string) (*Session, error) {
	if m == nil {
		return nil, errors.New("session: manager is nil")
	}
	if id == "" {
		return nil, errors.New("session: id is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if elem, ok := m.sessions[id]; ok {
		m.lru.MoveToFront(elem)
		session := elem.Value.(*sessionCacheEntry).session
		m.mu.Unlock()
		return session, nil
	}
	m.mu.Unlock()
	return m.open(ctx, id, nil)
}

// ResolveWorker returns the session a delegated worker runs in. It is the same
// path as Resolve — same table, same eviction, same key rules — with the
// caller's overrides applied to a session that does not exist yet. A worker that
// already has a session (a follow-up or continued job) keeps the stored one, so
// its history survives; only a first delegation takes the overrides.
//
// This used to be a direct OpenSession call inside the delegation code, which
// meant a worker bypassed key assignment, provider repointing and eviction
// entirely, and re-opened its database on every follow-up.
func (m *SessionManager) ResolveWorker(ctx context.Context, id string, firstTime func(*provider.SessionConfig)) (*Session, error) {
	if m == nil {
		return nil, errors.New("session: manager is nil")
	}
	if id == "" {
		return nil, errors.New("session: id is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if elem, ok := m.sessions[id]; ok {
		m.lru.MoveToFront(elem)
		session := elem.Value.(*sessionCacheEntry).session
		m.mu.Unlock()
		return session, nil
	}
	m.mu.Unlock()
	return m.open(ctx, id, firstTime)
}

// open loads or creates the session for id and puts it in the table. When
// firstTime is set it shapes a session that is about to be created; an existing
// database is never overwritten by it.
func (m *SessionManager) open(ctx context.Context, id string, firstTime func(*provider.SessionConfig)) (*Session, error) {
	m.mu.Lock()
	config := m.base
	config.ID = id
	keys := m.keys
	if providerKeys := m.providerKeys[config.Provider]; providerKeys != nil {
		keys = providerKeys
	}
	m.mu.Unlock()
	if firstTime != nil {
		firstTime(&config)
		if providerKeys := m.keysFor(config.Provider); providerKeys != nil {
			keys = providerKeys
		}
	}

	path := SessionDBPath(m.dir, id)
	session, err := OpenSession(path, config, keys)
	if err != nil {
		return nil, err
	}
	// What the phone picked last time wins over the boot default, and only when
	// the daemon can still route to it.
	applied := false
	if id, model, ok := m.sessionDefaults(ctx, id); ok && id != "" && model != "" {
		m.mu.RLock()
		providerKeys := m.providerKeys[id]
		m.mu.RUnlock()
		if providerKeys != nil {
			if err := session.SetProvider(id, providerKeys); err == nil {
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
	// Another goroutine may have opened the same session while this one waited
	// outside the lock: keep one session object per id.
	if elem, ok := m.sessions[id]; ok {
		existing := elem.Value.(*sessionCacheEntry).session
		m.mu.Unlock()
		_ = session.Close()
		return existing, nil
	}
	elem := m.lru.PushFront(&sessionCacheEntry{id: id, session: session})
	m.sessions[id] = elem
	m.evictLocked()
	m.mu.Unlock()
	return session, nil
}

// keysFor is the pool the manager holds for a provider, if any.
func (m *SessionManager) keysFor(id provider.ProviderID) *provider.KeyPool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.providerKeys[id]
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
func (m *SessionManager) ListSessions(limit int) ([]SessionInfo, error) {
	if m == nil {
		return nil, errors.New("session: manager is nil")
	}
	return ListSessionsInDir(m.dir, limit)
}

// Forget drops one live chat from the in-memory cache and closes it, so the
// next turn reopens that session from the current defaults. Clearing a session
// route calls this after removing the D1 pin: the old provider/model must not
// survive in a cached object.
// ApplyGeneration writes the generation knobs onto one chat and returns the
// session database it wrote to, so the caller can push that file to D1. The knobs
// are set through the session setters so the same bounds apply and the change is
// persisted by the same path as everything else about the session (CHANGE-077).
func (m *SessionManager) ApplyGeneration(
	ctx context.Context,
	sessionID string,
	settings provider.GenerationSettings,
	clear bool,
	clearKnobs []string,
) (string, error) {
	if m == nil {
		return "", errors.New("session: manager is nil")
	}
	session, err := m.Resolve(ctx, sessionID)
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
		return SessionDBPath(m.dir, sessionID), nil
	}
	// A knob that was not sent keeps whatever the chat already had; only a name
	// in cleared is removed. Replacing the whole set would mean that asking for a
	// temperature silently took the chat's reasoning level with it (CHANGE-077).
	removing := make(map[string]bool, len(clearKnobs))
	for _, name := range clearKnobs {
		removing[name] = true
	}
	sets := []struct {
		name    string
		applied bool
		set     func() error
		clear   func() error
	}{
		{"temperature", settings.Temperature != nil,
			func() error { return session.SetTemperature(*settings.Temperature) },
			func() error { return session.ClearTemperature() }},
		{"thinking_level", settings.ThinkingLevel != "",
			func() error { return session.SetThinkingLevel(settings.ThinkingLevel) },
			func() error { return session.ClearThinkingLevel() }},
		{"top_p", settings.TopP != nil,
			func() error { return session.SetTopP(*settings.TopP) },
			func() error { return session.ClearTopP() }},
		{"top_k", settings.TopK != nil,
			func() error { return session.SetTopK(*settings.TopK) },
			func() error { return session.ClearTopK() }},
		{"stop_sequences", len(settings.StopSequences) > 0,
			func() error { return session.SetStopSequences(settings.StopSequences) },
			func() error { return session.ClearStopSequences() }},
		{"presence_penalty", settings.PresencePenalty != nil,
			func() error { return session.SetPresencePenalty(*settings.PresencePenalty) },
			func() error { return session.ClearPresencePenalty() }},
		{"frequency_penalty", settings.FrequencyPenalty != nil,
			func() error { return session.SetFrequencyPenalty(*settings.FrequencyPenalty) },
			func() error { return session.ClearFrequencyPenalty() }},
		{"seed", settings.Seed != nil,
			func() error { return session.SetSeed(*settings.Seed) },
			func() error { return session.ClearSeed() }},
		{"max_output_tokens", settings.MaxOutputTokens > 0,
			func() error { return session.SetMaxOutputTokens(settings.MaxOutputTokens) },
			func() error { return session.SetMaxOutputTokens(0) }},
	}
	for _, knob := range sets {
		switch {
		case removing[knob.name]:
			if err := knob.clear(); err != nil {
				return "", err
			}
		case knob.applied:
			if err := knob.set(); err != nil {
				return "", err
			}
		}
	}
	return SessionDBPath(m.dir, sessionID), nil
}

// SessionGeneration reads back what one chat runs on, so the phone can be told
// what it is editing rather than guessing from what it last sent.
func (m *SessionManager) SessionGeneration(ctx context.Context, sessionID string) (provider.GenerationSettings, bool, error) {
	if m == nil {
		return provider.GenerationSettings{}, false, errors.New("session: manager is nil")
	}
	session, err := m.Resolve(ctx, sessionID)
	if err != nil {
		return provider.GenerationSettings{}, false, err
	}
	cfg := session.Config()
	return provider.GenerationSettings{
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
