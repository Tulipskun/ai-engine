package sdk

import (
	"context"
	"errors"
	"github.com/Tulipskun/ai-engine/provider"
	"github.com/Tulipskun/ai-engine/session"
	"log"
	"sync"
	"time"
)

type RequestResolver func(context.Context, Input, *session.Session) (provider.Request, error)

type HarnessLoop struct {
	Client         *provider.RouterClient
	Agent          *Agent
	Source         InputSource
	ResolveSession func(context.Context, Input) (*session.Session, error)
	BuildRequest   RequestResolver
	Displays       []Display
	DisplayTimeout time.Duration
	OnTurnError    func(Input, error)
	// ApplySessionConfig, when set, is called with the session id before a
	// turn runs, so the agent's sub-agent config can follow the per-session
	// pin instead of only the global defaults (ACP session config pattern).
	ApplySessionConfig func(sessionID string)

	sessionLocks sync.Map
	turns        sync.Map // session id -> cancel func of the turn in flight
}

// CancelTurn stops the turn currently running for a session and reports whether
// there was one. The phone's stop button needs this: the turn owns a provider
// request and a set of tool calls that must stop now, not after the next step.
func (h *HarnessLoop) CancelTurn(sessionID string) bool {
	if h == nil || sessionID == "" {
		return false
	}
	cancel, ok := h.turns.LoadAndDelete(sessionID)
	if !ok {
		return false
	}
	if fn, ok := cancel.(context.CancelFunc); ok && fn != nil {
		fn()
	}
	return true
}

// Busy reports whether a session has a turn in flight.
func (h *HarnessLoop) Busy(sessionID string) bool {
	if h == nil {
		return false
	}
	_, ok := h.turns.Load(sessionID)
	return ok
}

func (h *HarnessLoop) Run(ctx context.Context) error {
	if h == nil || (h.Client == nil && h.Agent == nil) || h.Source == nil || h.ResolveSession == nil {
		return errors.New("sdk: incomplete harness loop configuration")
	}
	if h.Agent != nil {
		h.Agent.SetSubAgentSinks(func(event SubAgentEvent) {
			if event.Parent == nil {
				return
			}
			input := cloneInputRoute(event.Input)
			if input.SessionID == "" {
				input.SessionID = event.Parent.ID()
			}
			input.Turn = provider.Turn{Role: provider.RoleUser, Content: []provider.ContentPart{{Type: provider.ContentText, Text: event.Message()}}}
			go func() {
				if err := h.Entry(context.WithoutCancel(ctx), input); err != nil {
					if h.OnTurnError != nil {
						h.OnTurnError(input, err)
					} else {
						log.Printf("sdk: sub-agent report continuation failed source=%s session=%s: %v", input.Source, input.SessionID, err)
					}
				}
			}()
		}, func(event SubAgentEvent) {
			if event.Parent == nil || event.Trace == nil {
				return
			}
			input := cloneInputRoute(event.Input)
			if input.SessionID == "" {
				input.SessionID = event.Parent.ID()
			}
			metadata := cloneMetadata(input.Metadata)
			if metadata == nil {
				metadata = map[string]string{}
			}
			metadata["trace_actor"] = "subagent"
			metadata["trace_job_id"] = event.JobID
			traceCopy := *event.Trace
			for _, display := range h.Displays {
				DispatchDisplay(context.WithoutCancel(ctx), display, Output{Source: input.Source, SessionID: input.SessionID, Trace: &traceCopy, Metadata: metadata}, h.DisplayTimeout)
			}
		})
	}
	inputs, err := h.Source.Receive(ctx)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case input, ok := <-inputs:
			if !ok {
				return nil
			}
			if err := h.Entry(ctx, input); err != nil && h.OnTurnError != nil {
				h.OnTurnError(input, err)
			}
		}
	}
}

func (h *HarnessLoop) Entry(ctx context.Context, input Input) error {
	if h == nil || (h.Client == nil && h.Agent == nil) || h.ResolveSession == nil {
		return errors.New("sdk: incomplete harness loop configuration")
	}
	sessionLocal, err := h.ResolveSession(ctx, input)
	if err != nil {
		return err
	}
	if sessionLocal == nil {
		return errors.New("sdk: session resolver returned nil session")
	}
	if input.Turn.Role == provider.RoleToolResult || input.Turn.ToolResult != nil {
		sessionLocal.Append(input.Turn)
		return nil
	}
	lockKey := sessionLocal.ID()
	if lockKey != "" {
		lock := h.sessionLock(lockKey)
		lock.Lock()
		defer lock.Unlock()
	}
	// One cancelable context per turn, published so CancelTurn can stop it.
	if lockKey != "" {
		turnCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		ctx = turnCtx
		h.turns.Store(lockKey, context.CancelFunc(cancel))
		defer h.turns.Delete(lockKey)
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	var req provider.Request
	if h.BuildRequest != nil {
		req, err = h.BuildRequest(ctx, input, sessionLocal)
		if err != nil {
			return err
		}
	}
	req.Messages = session.CloneTurns(sessionLocal.History())
	req.Messages = append(req.Messages, session.CloneTurn(input.Turn))
	var responseTraced bool
	dispatchTrace := func(traceCtx context.Context, event TraceEvent) {
		switch event.Stage {
		case TraceProviderReady, TraceResponse, TraceResponseText, TraceResponseContent:
			responseTraced = true
		}
		traceCopy := event
		metadata := cloneMetadata(input.Metadata)
		if metadata == nil {
			metadata = map[string]string{}
		}
		metadata["trace_actor"] = "main"
		for _, display := range h.Displays {
			DispatchDisplay(traceCtx, display, Output{Source: input.Source, SessionID: input.SessionID, Trace: &traceCopy, Metadata: metadata}, h.DisplayTimeout)
		}
	}
	ctx = session.WithSessionID(ctx, sessionLocal.ID())
	ctx = context.WithValue(ctx, lifecycleInputKey{}, cloneInputRoute(input))
	// Per-session agent config (sub-agent pin) takes precedence over the
	// global defaults for this turn only (ACP session config pattern).
	if h.ApplySessionConfig != nil {
		h.ApplySessionConfig(sessionLocal.ID())
	}
	var resp provider.Response
	if h.Agent != nil {
		resp, err = h.Agent.RunTurnWithTraceAndEntry(ctx, sessionLocal, input.Turn, req, dispatchTrace, h.Entry)
	} else {
		resp, err = h.Client.GenerateTurn(ctx, sessionLocal, input.Turn, req)
	}
	if err != nil {
		return err
	}
	channel := ""
	if input.Metadata != nil {
		channel = input.Metadata["channel_id"]
	}
	log.Printf("turn ok source=%s session=%s channel=%s", input.Source, input.SessionID, channel)
	if responseTraced {
		return nil
	}
	output := Output{Source: input.Source, SessionID: input.SessionID, Content: append([]provider.ContentPart(nil), resp.Content...), Response: resp, Metadata: cloneMetadata(input.Metadata)}
	if output.Metadata == nil {
		output.Metadata = map[string]string{}
	}
	output.Metadata["trace_actor"] = "main"
	for _, display := range h.Displays {
		DispatchDisplay(ctx, display, output, h.DisplayTimeout)
	}
	return nil
}

func (h *HarnessLoop) Handle(ctx context.Context, input Input) error {
	return h.Entry(ctx, input)
}

func (h *HarnessLoop) sessionLock(id string) *sync.Mutex {
	if existing, ok := h.sessionLocks.Load(id); ok {
		return existing.(*sync.Mutex)
	}
	created := &sync.Mutex{}
	actual, _ := h.sessionLocks.LoadOrStore(id, created)
	return actual.(*sync.Mutex)
}

func cloneMetadata(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

type lifecycleInputKey struct{}

func cloneInputRoute(input Input) Input {
	return Input{Source: input.Source, SessionID: input.SessionID, Metadata: cloneMetadata(input.Metadata)}
}
