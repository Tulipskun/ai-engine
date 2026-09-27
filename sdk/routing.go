package sdk

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

type routeKey struct {
	provider ProviderID
	model    string
}
type Router struct {
	mu           sync.RWMutex
	routes       map[routeKey]ModelRoute
	providers    map[ProviderID]ProviderConfig
	catalogs     map[ProviderID][]Model
	catalogReady map[ProviderID]bool
}

func NewRouter() *Router {
	return &Router{routes: make(map[routeKey]ModelRoute), providers: make(map[ProviderID]ProviderConfig), catalogs: make(map[ProviderID][]Model), catalogReady: make(map[ProviderID]bool)}
}
func (r *Router) RegisterProvider(config ProviderConfig) {
	if config.ID == "" || config.Adapter == "" {
		panic("sdk: invalid provider config")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[config.ID] = config
	delete(r.catalogs, config.ID)
	r.catalogReady[config.ID] = false
}
func (r *Router) Provider(provider ProviderID) (ProviderConfig, error) {
	r.mu.RLock()
	config, ok := r.providers[provider]
	r.mu.RUnlock()
	if !ok {
		return ProviderConfig{}, fmt.Errorf("sdk: provider %q is not registered", provider)
	}
	return config, nil
}
func (r *Router) Register(route ModelRoute) {
	if route.Provider == "" || route.Model == "" || route.Adapter == "" {
		panic("sdk: invalid model route")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes[routeKey{route.Provider, route.Model}] = route
}
func (r *Router) RefreshModels(ctx context.Context, provider ProviderID, adapter Provider) error {
	config, err := r.Provider(provider)
	if err != nil {
		return err
	}
	lister, ok := adapter.(ModelLister)
	if !ok {
		return fmt.Errorf("sdk: adapter %q does not support model discovery", config.Adapter)
	}
	if config.Keys == nil {
		return errors.New("sdk: provider has no key pool")
	}
	key, err := config.Keys.Current()
	if err != nil {
		return err
	}
	p := adapter
	if kp, ok := p.(KeyedProvider); ok {
		p = kp.WithAPIKey(key)
	}
	if ep, ok := p.(EndpointProvider); ok && config.BaseURL != "" {
		p = ep.WithBaseURL(config.BaseURL)
	}
	if len(config.Headers) > 0 {
		if hp, ok := p.(HeaderedProvider); ok {
			p = hp.WithHeaders(config.Headers)
		}
	}
	lister, ok = p.(ModelLister)
	if !ok {
		return fmt.Errorf("sdk: configured adapter %q cannot discover models after configuration", config.Adapter)
	}
	models, err := lister.ListModels(ctx, key)
	if err != nil {
		return err
	}
	clean := make([]Model, 0, len(models))
	seen := make(map[string]struct{})
	for _, model := range models {
		if model.ID == "" {
			continue
		}
		if _, exists := seen[model.ID]; exists {
			continue
		}
		seen[model.ID] = struct{}{}
		clean = append(clean, model)
	}
	clean = filterFreeModels(clean, config.FreeOnly)
	r.mu.Lock()
	r.catalogs[provider] = clean
	r.catalogReady[provider] = true
	r.mu.Unlock()
	return nil
}
func filterFreeModels(models []Model, freeOnly bool) []Model {
	if !freeOnly {
		return models
	}
	out := make([]Model, 0, len(models))
	for _, model := range models {
		if isFreeModelID(model.ID) {
			out = append(out, model)
		}
	}
	return out
}

// isFreeModelID matches the free-tier naming of the gateways in use:
// "-free" suffix (opencode Zen), ":free" suffix (OpenRouter-style,
// e.g. NousResearch) and "free/" prefix used by some providers.
func isFreeModelID(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return false
	}
	return strings.HasSuffix(id, "-free") || strings.HasSuffix(id, ":free") || strings.HasPrefix(id, "free/")
}

// ProviderIDs lists the registered providers, so a caller can offer the whole
// catalogue (the phone's provider picker) without tracking it separately.
func (r *Router) ProviderIDs() []ProviderID {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ProviderID, 0, len(r.providers))
	for id := range r.providers {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (r *Router) Models(provider ProviderID) []Model {
	r.mu.RLock()
	models := append([]Model(nil), r.catalogs[provider]...)
	r.mu.RUnlock()
	return models
}
func (r *Router) Resolve(provider ProviderID, model string) (ModelRoute, error) {
	if provider == "" {
		return ModelRoute{}, errors.New("sdk: provider is required")
	}
	if model == "" {
		return ModelRoute{}, errors.New("sdk: model is required")
	}
	r.mu.RLock()
	config, providerOK := r.providers[provider]
	ready := r.catalogReady[provider]
	route, staticOK := r.routes[routeKey{provider, model}]
	if ready {
		for _, discovered := range r.catalogs[provider] {
			if discovered.ID == model {
				r.mu.RUnlock()
				return ModelRoute{Provider: provider, Model: model, Adapter: config.Adapter}, nil
			}
		}
		r.mu.RUnlock()
		return ModelRoute{}, fmt.Errorf("sdk: model %q is not available for provider=%q", model, provider)
	}
	r.mu.RUnlock()
	if staticOK {
		return route, nil
	}
	if providerOK {
		return ModelRoute{}, fmt.Errorf("sdk: model catalogue for provider=%q has not been refreshed", provider)
	}
	return ModelRoute{}, fmt.Errorf("sdk: no route for provider=%q model=%q", provider, model)
}

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
	config    SessionConfig
	keys      *KeyPool
	history   []Turn
	store     *SessionDB
	plan      PlanState
	activeJob string
}

func NewSession(config SessionConfig, keys *KeyPool) *Session {
	return &Session{config: config, keys: keys}
}
func OpenSession(path string, config SessionConfig, keys *KeyPool) (*Session, error) {
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
func (s *Session) Config() SessionConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.config
	if c.Temperature != nil {
		v := *c.Temperature
		c.Temperature = &v
	}
	return c
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
func (s *Session) History() []Turn { s.mu.RLock(); defer s.mu.RUnlock(); return cloneTurns(s.history) }
func (s *Session) SetPlan(steps []string) error {
	s.cleanPlan(steps)
	return nil
}

func (s *Session) cleanPlan(steps []string) {
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

// reserveSubAgent binds a reservation to a snapshot under the plan lock. A
// replacement plan does not clear activeJob: cancellation must finish first.
func (s *Session) reserveSubAgent(id string, retry *subAgentJob) (PlanState, PlanStep, bool, error) {
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
		if retry.revision != s.plan.Revision || retry.planned != planned || (planned && retry.step.Index != step.Index) {
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
func (s *Session) reserveSubAgentForContinue(id string) (PlanState, PlanStep, bool, error) {
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

func (s *Session) finishSubAgent(job *subAgentJob, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeJob != job.id {
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
func (s *Session) matchesJob(job *subAgentJob) bool {
	return job.planned && s.plan.Revision == job.revision && s.plan.Current < len(s.plan.Steps) && s.plan.Steps[s.plan.Current].Index == job.step.Index
}

func (s *Session) acceptSubAgent(job *subAgentJob) error {
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

func (s *Session) Append(turns ...Turn) {
	if len(turns) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cloned := cloneTurns(turns)
	start := len(s.history)
	s.history = append(s.history, cloned...)
	if s.store != nil {
		if err := s.store.AppendTurns(s.config.ID, cloned, start); err != nil {
			s.history = s.history[:start]
			panic(fmt.Sprintf("sdk: persist session append: %v", err))
		}
	}
}
func (s *Session) ReplaceHistory(turns []Turn) {
	repaired := repairTurns(cloneTurns(turns))
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
func (s *Session) RecordRequest(attempt int, req Request) (int64, error) {
	s.mu.RLock()
	store, id := s.store, s.config.ID
	s.mu.RUnlock()
	if store == nil {
		return 0, nil
	}
	return store.RecordRequest(id, attempt, req)
}
func (s *Session) RecordResponse(requestID int64, resp Response, err error) error {
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
func (s *Session) LoadUsage() (Usage, error) {
	s.mu.RLock()
	store, id := s.store, s.config.ID
	s.mu.RUnlock()
	if store == nil {
		return Usage{}, nil
	}
	return store.LoadUsage(id)
}
func cloneTurns(in []Turn) []Turn {
	out := make([]Turn, len(in))
	copy(out, in)
	for i := range out {
		out[i] = cloneTurn(out[i])
	}
	return out
}
func cloneTurn(turn Turn) Turn {
	turn.Content = append([]ContentPart(nil), turn.Content...)
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
func repairTurns(in []Turn) []Turn {
	out := make([]Turn, 0, len(in))
	pending := make(map[string]bool)
	for _, turn := range in {
		switch turn.Role {
		case RoleUser, RoleModel:
			out = append(out, turn)
		case RoleToolCall:
			if turn.ToolCall == nil || turn.ToolCall.ID == "" || turn.ToolCall.Name == "" || pending[turn.ToolCall.ID] {
				continue
			}
			pending[turn.ToolCall.ID] = true
			out = append(out, turn)
		case RoleToolResult:
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
		if turn.Role == RoleToolCall && pending[turn.ToolCall.ID] {
			continue
		}
		final = append(final, turn)
	}
	return final
}
