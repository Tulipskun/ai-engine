package sdk

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrAgentMaxIterations    = errors.New("sdk: agent reached maximum iterations")
	ErrAgentRetriesExhausted = errors.New("sdk: agent retries exhausted")
)

type ToolExecutor interface {
	Definitions() []Tool
	Execute(context.Context, ToolCall) ToolResult
}

type Agent struct {
	Client          *RouterClient
	Tools           ToolExecutor
	MaxRetries      int
	DisablePlanning bool
	SubAgentConfig  SubAgentConfig

	subAgentMu         sync.Mutex
	subAgents          *subAgentManager
	subAgentTraceSink  func(SubAgentEvent)
	subAgentReportSink func(SubAgentEvent)
	interruptMu        sync.Mutex
	interrupts         map[string]context.CancelFunc
	interrupted        map[string]bool
}

const (
	defaultAgentMaxRetries = 6
	maxRetryCooldown       = 96 * time.Second
)

func (a *Agent) subAgentManager() *subAgentManager {
	a.subAgentMu.Lock()
	defer a.subAgentMu.Unlock()
	if a.subAgents == nil {
		a.subAgents = newSubAgentManager(a, a.SubAgentConfig)
		a.subAgents.SetTraceSink(a.subAgentTraceSink)
		a.subAgents.SetEventSink(a.subAgentReportSink)
	}
	return a.subAgents
}

func (a *Agent) RunTurn(ctx context.Context, session *Session, user Turn, req Request) (Response, error) {
	return a.runTurn(ctx, session, user, req, nil, nil)
}

func (a *Agent) RunTurnWithTrace(ctx context.Context, session *Session, user Turn, req Request, trace TraceFunc) (Response, error) {
	return a.runTurn(ctx, session, user, req, trace, nil)
}

func (a *Agent) RunTurnWithTraceAndEntry(ctx context.Context, session *Session, user Turn, req Request, trace TraceFunc, entry func(context.Context, Input) error) (Response, error) {
	return a.runTurn(ctx, session, user, req, trace, entry)
}

func (a *Agent) Interrupt(sessionID string) bool {
	if a == nil || sessionID == "" {
		return false
	}
	a.interruptMu.Lock()
	cancel, ok := a.interrupts[sessionID]
	if ok {
		if a.interrupted == nil {
			a.interrupted = make(map[string]bool)
		}
		a.interrupted[sessionID] = true
	}
	a.interruptMu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

func (a *Agent) wasInterrupted(sessionID string) bool {
	if a == nil || sessionID == "" {
		return false
	}
	a.interruptMu.Lock()
	defer a.interruptMu.Unlock()
	return a.interrupted[sessionID]
}

// StopSubAgent asks one sub agent job to stop, without stopping the turn that
// delegated to it. The phone shows one row per sub agent and one stop button per
// row, so the request has to name the job; an unknown job or one that already
// ended is reported back instead of silently doing nothing.
func (a *Agent) StopSubAgent(parentSessionID, jobID string) error {
	if a == nil {
		return errors.New("sdk: agent is not configured")
	}
	return a.subAgentManager().RequestStop(parentSessionID, jobID)
}

func (a *Agent) beginInterrupt(ctx context.Context, sessionID string) (context.Context, func()) {
	turnCtx, cancel := context.WithCancel(ctx)
	if sessionID == "" {
		return turnCtx, cancel
	}
	a.interruptMu.Lock()
	if a.interrupts == nil {
		a.interrupts = make(map[string]context.CancelFunc)
	}
	a.interrupts[sessionID] = cancel
	a.interruptMu.Unlock()
	return turnCtx, func() {
		a.interruptMu.Lock()
		delete(a.interrupts, sessionID)
		delete(a.interrupted, sessionID)
		a.interruptMu.Unlock()
		cancel()
	}
}

func (a *Agent) runTurn(ctx context.Context, session *Session, user Turn, req Request, trace TraceFunc, entry func(context.Context, Input) error) (Response, error) {
	if a == nil || a.Client == nil || session == nil {
		return Response{}, errors.New("sdk: incomplete agent configuration")
	}
	ctx, cleanup := a.beginInterrupt(ctx, session.ID())
	defer cleanup()
	clock := &turnClock{}
	if trace != nil {
		inner := trace
		trace = func(ctx context.Context, event TraceEvent) { inner(ctx, clock.stamp(event)) }
	}

	before := session.History()
	retries := a.MaxRetries
	if retries < 0 {
		retries = 0
	}
	if a.MaxRetries == 0 {
		retries = defaultAgentMaxRetries
	}

	backoff := retryBackoff{}
	var lastErr error
	// A session with no key pool never rotates, so this stays at zero and the
	// behaviour is unchanged for one.
	triedKeys := 0
	for attempt := 0; attempt <= retries; attempt++ {
		session.ReplaceHistory(before)
		resp, err := a.runAttempt(ctx, session, user, req, trace, &backoff, entry, clock)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if a.wasInterrupted(session.ID()) {
			settleInterruptedTurn(session, before)
			break
		}
		if retryAfterAbort(err) {
			break
		}
		session.ReplaceHistory(before)
		// A rejected key is the one failure worth a different attempt rather than
		// a repeat: the pool may hold a working key, and asking again with the same
		// key gets the same answer. Move to the next one and try it immediately,
		// with no backoff, because the key is not coming back. Every key is tried
		// once and then the turn fails for real (CHANGE-077).
		if isKeyRejection(err) {
			triedKeys++
			if rotateToNextKey(session, triedKeys) {
				traceEvent(ctx, trace, newTraceEvent(TraceRetryWait, withErr(err)))
				continue
			}
		}
		if !retryableAgentError(ctx, err) || attempt == retries {
			break
		}

		delay := backoff.Delay(err)
		traceEvent(ctx, trace, newTraceEvent(TraceRetryWait, withErr(err), withRetryAfter(delay)))
		if err := waitRetry(ctx, delay); err != nil {
			if a.wasInterrupted(session.ID()) {
				settleInterruptedTurn(session, before)
			}
			traceEvent(ctx, trace, newTraceEvent(TraceError, withErr(err)))
			return Response{}, err
		}
	}

	traceEvent(ctx, trace, newTraceEvent(TraceError, withErr(lastErr)))
	if errors.Is(lastErr, context.Canceled) || errors.Is(lastErr, context.DeadlineExceeded) {
		return Response{}, lastErr
	}
	return Response{}, fmt.Errorf("%w: attempts=%d: %w", ErrAgentRetriesExhausted, retries+1, lastErr)
}

// planningFor reports whether the turn for session runs through the Main
// Agent planner. A session in sub mode answers as a worker with the full
// execution tool set, independent of the global DisablePlanning switch, so
// one channel can run direct while others keep planning (REQ-029).
func (a *Agent) planningFor(session *Session) bool {
	if a != nil && a.DisablePlanning {
		return false
	}
	if session == nil {
		return true
	}
	return session.Config().AgentMode != AgentModeSub
}

func (a *Agent) runAttempt(ctx context.Context, session *Session, user Turn, req Request, trace TraceFunc, backoff *retryBackoff, entry func(context.Context, Input) error, clock *turnClock) (Response, error) {
	var executor ToolExecutor
	if a.planningFor(session) {
		executor = newPlanningToolExecutor(a.Tools, session)
	} else {
		executor = a.Tools
	}
	if planner, ok := executor.(*planningToolExecutor); ok && a.SubAgentConfig.Enabled {
		planner.ConfigureSubAgent(&subAgentRunner{manager: a.subAgentManager(), parent: session})
	}
	session.Append(user)
	if req.Stream {
		return a.runStreamAttempt(ctx, session, req, trace, backoff, entry)
	}
	baseSystemPrompt := req.SystemPrompt

	// REQ-045: hard loop-control caps for this attempt. Normal turns stay
	// far under the limits and behave exactly as before.
	lc := &loopControlTracker{}
	for {
		if err := ctx.Err(); err != nil {
			return Response{}, err
		}
		req.Messages = buildContextWindow(session.History(), defaultContextWindowTokens)
		req.SystemPrompt = baseSystemPrompt
		if a.planningFor(session) {
			req.SystemPrompt = planningSystemPrompt(baseSystemPrompt, session.Plan())
		}
		if executor != nil {
			req.Tools = executor.Definitions()
		} else {
			req.Tools = nil
		}
		clock.begin()
		traceEvent(ctx, trace, newTraceEvent(TraceRequest))

		resp, err := a.Client.Generate(ctx, session, req)
		if err != nil {
			return Response{}, err
		}
		clock.accept()
		traceEvent(ctx, trace, newTraceEvent(TraceProviderReady))
		if err := ctx.Err(); err != nil {
			return Response{}, err
		}
		if backoff != nil {
			backoff.Reset()
		}
		commitResponse(session, resp)

		if len(resp.Content) > 0 {
			traceEvent(ctx, trace, newTraceEvent(TraceResponseContent, withResponse(resp)))
		}

		if len(resp.ToolCalls) == 0 {
			traceEvent(ctx, trace, newTraceEvent(TraceResponse, withResponse(resp)))
			return resp, nil
		}
		if executor == nil {
			for _, call := range resp.ToolCalls {
				if err := ctx.Err(); err != nil {
					return Response{}, err
				}
				callCopy := cloneToolCall(call)
				traceEvent(ctx, trace, newTraceEvent(TraceToolCall, withToolCall(callCopy)))
				traceEvent(ctx, trace, newTraceEvent(TraceToolRunning, withToolCall(callCopy)))
				if err := lc.noteCall(call.Name); err != nil {
					return Response{}, err
				}
				result := ToolResult{ID: call.ID, Content: "tool execution is not configured", IsError: true}
				traceEvent(ctx, trace, newTraceEvent(TraceToolResult, withToolCall(callCopy), withToolResult(cloneToolResult(result))))
				if entry != nil {
					if err := entry(ctx, Input{Source: "tool", SessionID: session.ID(), Turn: Turn{Role: RoleToolResult, ToolResult: cloneToolResult(result)}}); err != nil {
						return Response{}, err
					}
				} else {
					resultCopy := result
					session.Append(Turn{Role: RoleToolResult, ToolResult: &resultCopy})
				}
			}
			continue
		}
		for _, call := range resp.ToolCalls {
			if err := ctx.Err(); err != nil {
				return Response{}, err
			}
			callCopy := cloneToolCall(call)
			traceEvent(ctx, trace, newTraceEvent(TraceToolCall, withToolCall(callCopy)))
			traceEvent(ctx, trace, newTraceEvent(TraceToolRunning, withToolCall(callCopy)))
			if err := lc.noteCall(call.Name); err != nil {
				return Response{}, err
			}
			result := executor.Execute(ctx, call)
			if err := ctx.Err(); err != nil {
				return Response{}, err
			}
			if err := lc.noteResult(call.Name, result); err != nil {
				return Response{}, err
			}
			if result.ID == "" {
				result.ID = call.ID
			}
			traceEvent(ctx, trace, newTraceEvent(TraceToolResult, withToolCall(callCopy), withToolResult(cloneToolResult(result))))
			resultCopy := result
			if entry != nil {
				if err := entry(ctx, Input{Source: "tool", SessionID: session.ID(), Turn: Turn{Role: RoleToolResult, ToolResult: &resultCopy}}); err != nil {
					return Response{}, err
				}
			} else {
				session.Append(Turn{Role: RoleToolResult, ToolResult: &resultCopy})
			}
		}
	}
}

func (a *Agent) runStreamAttempt(ctx context.Context, session *Session, req Request, trace TraceFunc, backoff *retryBackoff, entry func(context.Context, Input) error) (Response, error) {
	clock := &turnClock{}
	if trace != nil {
		inner := trace
		trace = func(ctx context.Context, event TraceEvent) { inner(ctx, clock.stamp(event)) }
	}
	var executor ToolExecutor
	if a.planningFor(session) {
		executor = newPlanningToolExecutor(a.Tools, session)
	} else {
		executor = a.Tools
	}
	if planner, ok := executor.(*planningToolExecutor); ok && a.SubAgentConfig.Enabled {
		planner.ConfigureSubAgent(&subAgentRunner{manager: a.subAgentManager(), parent: session})
	}
	baseSystemPrompt := req.SystemPrompt
	// REQ-045: hard loop-control caps for this attempt (same as runAttempt).
	lc := &loopControlTracker{}
	for {
		if err := ctx.Err(); err != nil {
			return Response{}, err
		}
		req.Messages = buildContextWindow(session.History(), defaultContextWindowTokens)
		req.SystemPrompt = baseSystemPrompt
		if a.planningFor(session) {
			req.SystemPrompt = planningSystemPrompt(baseSystemPrompt, session.Plan())
		}
		if executor != nil {
			req.Tools = executor.Definitions()
		} else {
			req.Tools = nil
		}
		clock.begin()
		traceEvent(ctx, trace, newTraceEvent(TraceRequest))

		events, err := a.Client.Stream(ctx, session, req)
		if err != nil {
			return Response{}, err
		}
		clock.accept()
		traceEvent(ctx, trace, newTraceEvent(TraceProviderReady))

		var resp Response
		var text []ContentPart
		var calls []ToolCall
		var reasoning *ReasoningState
		var streamErr error
		for event := range events {
			switch event.Type {
			case EventText:
				if event.Text != "" {
					text = append(text, ContentPart{Type: ContentText, Text: event.Text})
					traceEvent(ctx, trace, TraceEvent{Stage: TraceResponseContent, Text: event.Text})
				}
			case EventReasoning:
				if event.Reasoning != nil {
					r := *event.Reasoning
					if reasoning == nil {
						reasoning = &ReasoningState{}
					}
					if r.ID != "" {
						reasoning.ID = r.ID
					}
					reasoning.Text += r.Text
					if r.Text != "" {
						traceEvent(ctx, trace, TraceEvent{Stage: TraceResponseText, Text: r.Text})
					}
				}
			case EventToolCall:
				if event.ToolCall != nil {
					call := *event.ToolCall
					calls = append(calls, call)
					traceEvent(ctx, trace, TraceEvent{Stage: TraceToolCall, ToolCall: cloneToolCall(call)})
				}
			case EventDone:
				if event.Response != nil {
					resp = *event.Response
				}
			case EventError:
				streamErr = event.Err
			}
			if event.Type == EventError {
				break
			}
		}
		if streamErr != nil {
			return Response{}, streamErr
		}
		if resp.Provider == "" {
			resp.Provider = string(session.Config().Provider)
		}
		if resp.Model == "" {
			resp.Model = session.Config().Model
		}
		if len(resp.Content) == 0 {
			resp.Content = text
		}
		if len(resp.ToolCalls) == 0 {
			resp.ToolCalls = calls
		}
		if resp.Reasoning == nil {
			resp.Reasoning = reasoning
		}
		// Some providers return canonical content only in EventDone. Emit it
		// once when no text deltas were received, never replay a streamed body.
		if len(text) == 0 && len(resp.Content) > 0 {
			traceEvent(ctx, trace, TraceEvent{Stage: TraceResponseContent, Response: cloneResponseContent(resp)})
		}
		if backoff != nil {
			backoff.Reset()
		}

		commitResponse(session, resp)
		if len(resp.ToolCalls) == 0 {
			traceEvent(ctx, trace, TraceEvent{Stage: TraceResponse, Response: cloneResponseContent(resp)})
			return resp, nil
		}
		if executor == nil {
			for _, call := range resp.ToolCalls {
				callCopy := cloneToolCall(call)
				traceEvent(ctx, trace, TraceEvent{Stage: TraceToolRunning, ToolCall: callCopy})
				if err := lc.noteCall(call.Name); err != nil {
					return Response{}, err
				}
				result := ToolResult{ID: call.ID, Content: "tool execution is not configured", IsError: true}
				traceEvent(ctx, trace, TraceEvent{Stage: TraceToolResult, ToolCall: callCopy, ToolResult: cloneToolResult(result)})
				resultCopy := result
				if entry != nil {
					if err := entry(ctx, Input{Source: "tool", SessionID: session.ID(), Turn: Turn{Role: RoleToolResult, ToolResult: &resultCopy}}); err != nil {
						return Response{}, err
					}
				} else {
					session.Append(Turn{Role: RoleToolResult, ToolResult: &resultCopy})
				}
			}
			continue
		}
		for _, call := range resp.ToolCalls {
			if err := ctx.Err(); err != nil {
				return Response{}, err
			}
			callCopy := cloneToolCall(call)
			traceEvent(ctx, trace, TraceEvent{Stage: TraceToolRunning, ToolCall: callCopy})
			if err := lc.noteCall(call.Name); err != nil {
				return Response{}, err
			}
			result := executor.Execute(ctx, call)
			if err := ctx.Err(); err != nil {
				return Response{}, err
			}
			if err := lc.noteResult(call.Name, result); err != nil {
				return Response{}, err
			}
			if result.ID == "" {
				result.ID = call.ID
			}
			traceEvent(ctx, trace, TraceEvent{Stage: TraceToolResult, ToolCall: callCopy, ToolResult: cloneToolResult(result)})
			resultCopy := result
			if entry != nil {
				if err := entry(ctx, Input{Source: "tool", SessionID: session.ID(), Turn: Turn{Role: RoleToolResult, ToolResult: &resultCopy}}); err != nil {
					return Response{}, err
				}
			} else {
				session.Append(Turn{Role: RoleToolResult, ToolResult: &resultCopy})
			}
		}
	}
}


func settleInterruptedTurn(session *Session, before []Turn) {
	if session == nil {
		return
	}
	history := session.History()
	start := len(before)
	if start > len(history) {
		start = len(history)
	}
	pending := make(map[string]string)
	var order []string
	for _, turn := range history[start:] {
		if turn.Role == RoleToolCall && turn.ToolCall != nil && turn.ToolCall.ID != "" {
			if _, dup := pending[turn.ToolCall.ID]; !dup {
				pending[turn.ToolCall.ID] = turn.ToolCall.Name
				order = append(order, turn.ToolCall.ID)
			}
		}
		if turn.Role == RoleToolResult && turn.ToolResult != nil {
			delete(pending, turn.ToolResult.ID)
		}
	}
	for _, id := range order {
		name, ok := pending[id]
		if !ok {
			continue
		}
		if name == "" {
			name = "tool"
		}
		session.Append(Turn{Role: RoleToolResult, ToolResult: &ToolResult{ID: id, Content: "tool `" + name + "` was interrupted by the user; it may have partially run, not run at all, or already finished - verify the actual state before retrying.", IsError: true}})
	}
}

func traceEvent(ctx context.Context, trace TraceFunc, event TraceEvent) {
	if trace != nil {
		trace(ctx, event)
	}
}

// turnClock records the Unix millisecond timestamps of one provider request
// so every trace event can carry its own timing instead of a display-side
// time.Since guess (REQ-033).
type turnClock struct {
	mu         sync.Mutex
	sentMs     int64
	acceptedMs int64
}

func (c *turnClock) begin() {
	if c == nil {
		return
	}
	now := time.Now().UnixMilli()
	c.mu.Lock()
	c.sentMs, c.acceptedMs = now, 0
	c.mu.Unlock()
}

func (c *turnClock) accept() {
	if c == nil {
		return
	}
	now := time.Now().UnixMilli()
	c.mu.Lock()
	c.acceptedMs = now
	c.mu.Unlock()
}

func (c *turnClock) stamp(event TraceEvent) TraceEvent {
	if c == nil {
		return event
	}
	c.mu.Lock()
	sent, accepted := c.sentMs, c.acceptedMs
	c.mu.Unlock()
	if sent == 0 {
		return event
	}
	event.RequestStartedMs = sent
	event.ProviderAcceptedMs = accepted
	if event.AtMs == 0 {
		event.AtMs = time.Now().UnixMilli()
	}
	event.Elapsed = event.TotalElapsed()
	return event
}

// newTraceEvent builds one stamped trace event for the current request turn.
func newTraceEvent(stage TraceStage, opts ...func(*TraceEvent)) TraceEvent {
	event := TraceEvent{Stage: stage, AtMs: time.Now().UnixMilli()}
	for _, opt := range opts {
		opt(&event)
	}
	return event
}

func withResponse(resp Response) func(*TraceEvent) {
	return func(event *TraceEvent) { clone := cloneResponseContent(resp); event.Response = clone }
}
func withToolCall(call *ToolCall) func(*TraceEvent) {
	return func(event *TraceEvent) { event.ToolCall = call }
}
func withToolResult(result *ToolResult) func(*TraceEvent) {
	return func(event *TraceEvent) { event.ToolResult = result }
}
func withText(text string) func(*TraceEvent) {
	return func(event *TraceEvent) { event.Text = text }
}
func withErr(err error) func(*TraceEvent) {
	return func(event *TraceEvent) { event.Err = err }
}
func withRetryAfter(d time.Duration) func(*TraceEvent) {
	return func(event *TraceEvent) { event.RetryAfter = d }
}

func cloneResponseContent(in Response) *Response {
	return &Response{Provider: in.Provider, Model: in.Model, Content: append([]ContentPart(nil), in.Content...), Reasoning: in.Reasoning, Usage: in.Usage}
}

func cloneToolCall(in ToolCall) *ToolCall       { out := in; return &out }
func cloneToolResult(in ToolResult) *ToolResult { out := in; return &out }

func retryableAgentError(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	// REQ-045: loop-control budget failures are fatal, never retried.
	// Retrying a runaway loop only burns more provider calls.
	if isLoopControlFatal(err) {
		return false
	}
	// A refused key or a refused client is an answer, not a hiccup: the same
	// request gets the same answer. OpenCode Zen answers a free-tier request
	// that arrives from outside its own client with 403 FreeTierError, and every
	// retry spends quota the tier is already refusing.
	if isPolicyRefusal(err) {
		return false
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// isKeyRejection reports the one refusal that is about the key rather than about
// the caller. A 403 is deliberately not here: REQ-048(9) records that a free tier
// answers the same way whatever key is presented, and every retry spends quota
// the tier is already refusing.
func isKeyRejection(err error) bool {
	var statusErr HTTPStatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	return statusErr.HTTPStatusCode() == 401
}

// rotateToNextKey moves the session onto the next key in its pool, and reports
// whether there was one left to try. tried counts the keys already presented, so
// each key is used once and the turn then fails for real.
func rotateToNextKey(session *Session, tried int) bool {
	pool := session.keyPoolSize()
	if pool <= 1 || tried >= pool {
		return false
	}
	if _, err := session.RotateAPIKey(); err != nil {
		return false
	}
	return true
}

func isRateLimitError(err error) bool {
	statusErr, ok := err.(HTTPStatusError)
	return ok && statusErr.HTTPStatusCode() == 429
}

// isPolicyRefusal reports a verdict the provider will repeat: 401 says the key
// is not accepted, 403 says this client or tier is not allowed. Neither becomes
// true by asking again right away, so a turn reports it instead of retrying.
func isPolicyRefusal(err error) bool {
	var statusErr HTTPStatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	return statusErr.HTTPStatusCode() == 401 || statusErr.HTTPStatusCode() == 403
}

type retryBackoff struct{ consecutive int }

func (b *retryBackoff) Reset() { b.consecutive = 0 }

func (b *retryBackoff) Delay(err error) time.Duration {
	b.consecutive++
	return retryDelay(err, b.consecutive)
}

func retryAfterAbort(err error) bool {
	if !isRateLimitError(err) {
		return false
	}
	ra, ok := err.(RetryAfterError)
	if !ok {
		return false
	}
	return ra.RetryAfter() > maxRetryCooldown
}

func retryDelay(err error, attempt int) time.Duration {
	if isRateLimitError(err) {
		if retryAfter, ok := err.(RetryAfterError); ok {
			if d := retryAfter.RetryAfter(); d > 0 {
				if d > maxRetryCooldown {
					return maxRetryCooldown
				}
				return d
			}
		}
	}
	if attempt <= 1 {
		return 3 * time.Second
	}
	d := 3 * time.Second
	for i := 1; i < attempt; i++ {
		if d >= maxRetryCooldown {
			return maxRetryCooldown
		}
		d *= 2
		if d > maxRetryCooldown {
			return maxRetryCooldown
		}
	}
	return d
}

func waitRetry(ctx context.Context, delay time.Duration) error {
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
