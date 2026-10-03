package sdk

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Tulipskun/ai-engine/provider"
)

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
func (s *Session) keyPoolSize() int {
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
	return cloneTurns(s.history)
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
func (s *Session) ReplaceHistory(turns []provider.Turn) {
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
func cloneTurns(in []provider.Turn) []provider.Turn {
	out := make([]provider.Turn, len(in))
	copy(out, in)
	for i := range out {
		out[i] = cloneTurn(out[i])
	}
	return out
}
func cloneTurn(turn provider.Turn) provider.Turn {
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
func commitResponse(session *Session, resp provider.Response) {
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
