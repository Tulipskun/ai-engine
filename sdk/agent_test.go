package sdk

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type agentTestProvider struct {
	responses []Response
	errors    []error
	requests  []Request
	calls     int
}

func (p *agentTestProvider) Name() string { return "test" }
func (p *agentTestProvider) Generate(_ context.Context, req Request) (Response, error) {
	p.requests = append(p.requests, req)
	if p.calls < len(p.errors) && p.errors[p.calls] != nil {
		err := p.errors[p.calls]
		p.calls++
		return Response{}, err
	}
	idx := p.calls - len(p.errors)
	if idx < 0 || idx >= len(p.responses) {
		return Response{}, errors.New("unexpected call")
	}
	r := p.responses[idx]
	p.calls++
	return r, nil
}
func (p *agentTestProvider) Stream(context.Context, Request) (<-chan Event, error) {
	return nil, errors.New("not implemented")
}
func (p *agentTestProvider) WithAPIKey(string) Provider { return p }

type agentTestTools struct {
	definitions []Tool
	results     []ToolResult
}

func (t *agentTestTools) Definitions() []Tool { return t.definitions }
func (t *agentTestTools) Execute(_ context.Context, call ToolCall) ToolResult {
	t.results = append(t.results, ToolResult{ID: call.ID, Content: "ok"})
	return t.results[len(t.results)-1]
}

type blockingAgentProvider struct{ started chan struct{} }

func (p *blockingAgentProvider) Name() string { return "blocking" }
func (p *blockingAgentProvider) Generate(ctx context.Context, _ Request) (Response, error) {
	select {
	case <-p.started:
	default:
		close(p.started)
	}
	<-ctx.Done()
	return Response{}, ctx.Err()
}
func (p *blockingAgentProvider) Stream(context.Context, Request) (<-chan Event, error) {
	return nil, errors.New("not implemented")
}
func (p *blockingAgentProvider) WithAPIKey(string) Provider { return p }
func newAgentTestSession(p Provider) (*RouterClient, *Session) {
	r := NewRouter()
	r.RegisterProvider(ProviderConfig{ID: "test", BaseURL: "http://test", Keys: NewKeyPool("key"), Adapter: AdapterOpenAI})
	r.Register(ModelRoute{Provider: "test", Model: "model", Adapter: AdapterOpenAI})
	c := NewRouterClient(r)
	c.RegisterAdapter("test", AdapterOpenAI, p)
	s := NewSession(SessionConfig{ID: "s", Provider: "test", Model: "model", KeyIndex: 0}, NewKeyPool("key"))
	return c, s
}
func TestAgentFinalResponse(t *testing.T) {
	p := &agentTestProvider{responses: []Response{{Content: []ContentPart{{Type: ContentText, Text: "done"}}}}}
	c, s := newAgentTestSession(p)
	a := &Agent{Client: c, MaxRetries: 0}
	resp, err := a.RunTurn(context.Background(), s, Turn{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: "hi"}}}, Request{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "done" {
		t.Fatalf("unexpected response: %#v", resp)
	}
	if len(s.History()) != 2 {
		t.Fatalf("history=%d", len(s.History()))
	}
}
func TestAgentToolDefinitionsReachProvider(t *testing.T) {
	p := &agentTestProvider{responses: []Response{{Content: []ContentPart{{Type: ContentText, Text: "finished"}}}}}
	c, s := newAgentTestSession(p)
	tools := &agentTestTools{definitions: []Tool{{Name: "echo", Description: "echo text"}}}
	a := &Agent{Client: c, Tools: tools, MaxRetries: 0, DisablePlanning: true}
	_, err := a.RunTurn(context.Background(), s, Turn{Role: RoleUser}, Request{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.requests) != 1 {
		t.Fatalf("provider calls=%d", len(p.requests))
	}
	if len(p.requests[0].Tools) != 1 || p.requests[0].Tools[0].Name != "echo" {
		t.Fatalf("tools not sent: %#v", p.requests[0].Tools)
	}
}
func TestAgentToolThenFinal(t *testing.T) {
	p := &agentTestProvider{responses: []Response{
		{ToolCalls: []ToolCall{{ID: "plan-1", Name: "plan_create", Arguments: `{"goal":"report the echo tool result back","steps":["use the echo tool and return the result"]}`}}},
		{ToolCalls: []ToolCall{{ID: "1", Name: "echo", Arguments: "{}"}}},
		{ToolCalls: []ToolCall{{ID: "check-1", Name: "plan_check", Arguments: `{"step":1}`}}},
		{Content: []ContentPart{{Type: ContentText, Text: "finished"}}},
	}}
	c, s := newAgentTestSession(p)
	tools := &agentTestTools{definitions: []Tool{{Name: "echo"}}}
	a := &Agent{Client: c, Tools: tools, MaxRetries: 0, DisablePlanning: true}
	resp, err := a.RunTurn(context.Background(), s, Turn{Role: RoleUser}, Request{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content[0].Text != "finished" {
		t.Fatalf("unexpected final response")
	}
	h := s.History()
	if len(h) != 8 || h[2].Role != RoleToolResult || h[2].ToolResult.ID != "plan-1" || h[4].Role != RoleToolResult || h[4].ToolResult.ID != "1" {
		t.Fatalf("unexpected history: %#v", h)
	}
	if len(tools.results) != 3 {
		t.Fatalf("tool calls=%d", len(tools.results))
	}
}
func TestAgentPreservesReasoningAcrossToolContinuation(t *testing.T) {
	p := &agentTestProvider{responses: []Response{
		{ToolCalls: []ToolCall{{ID: "plan-1", Name: "plan_create", Arguments: `{"goal":"use the echo tool for the request","steps":["use the echo tool with empty arguments"]}`}}},
		{Reasoning: &ReasoningState{ID: "rs_123", Text: "think before using the tool"}, ToolCalls: []ToolCall{{ID: "call_1", Name: "echo", Arguments: "{}"}}},
		{ToolCalls: []ToolCall{{ID: "check-1", Name: "plan_check", Arguments: `{"step":1}`}}},
		{Content: []ContentPart{{Type: ContentText, Text: "finished"}}},
	}}
	c, s := newAgentTestSession(p)
	tools := &agentTestTools{definitions: []Tool{{Name: "echo"}}}
	a := &Agent{Client: c, Tools: tools, MaxRetries: 0, DisablePlanning: true}
	if _, err := a.RunTurn(context.Background(), s, Turn{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: "use the tool"}}}, Request{ThinkingLevel: ThinkingHigh}); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) != 4 {
		t.Fatalf("provider calls=%d", len(p.requests))
	}
	if len(p.requests[3].Messages) < 2 {
		t.Fatalf("second request history too short: %#v", p.requests[1].Messages)
	}
	var found bool
	for _, m := range p.requests[3].Messages {
		if m.Reasoning != nil && m.Reasoning.ID == "rs_123" && m.Reasoning.Text == "think before using the tool" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("reasoning was not replayed: %#v", p.requests[1].Messages)
	}
}
func TestAgentRunsBeyondPreviousIterationLimit(t *testing.T) {
	loop := Response{ToolCalls: []ToolCall{{ID: "echo", Name: "echo", Arguments: "{}"}}}
	responses := make([]Response, 23)
	responses[0] = Response{ToolCalls: []ToolCall{{ID: "plan-1", Name: "plan_create", Arguments: `{"goal":"loop the echo tool until finished","steps":["loop the echo tool until the work is finished"]}`}}}
	for i := 1; i < len(responses)-2; i++ {
		responses[i] = loop
	}
	responses[len(responses)-2] = Response{ToolCalls: []ToolCall{{ID: "check-1", Name: "plan_check", Arguments: `{"step":1}`}}}
	responses[len(responses)-1] = Response{Content: []ContentPart{{Type: ContentText, Text: "finished"}}}
	p := &agentTestProvider{responses: responses}
	c, s := newAgentTestSession(p)
	a := &Agent{Client: c, Tools: &agentTestTools{definitions: []Tool{{Name: "echo"}}}, MaxRetries: 0, DisablePlanning: true}
	resp, err := a.RunTurn(context.Background(), s, Turn{Role: RoleUser}, Request{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content[0].Text != "finished" {
		t.Fatalf("unexpected response: %#v", resp)
	}
	if p.calls != 23 {
		t.Fatalf("provider calls=%d", p.calls)
	}
}
func TestAgentInterruptStopsRun(t *testing.T) {
	p := &blockingAgentProvider{started: make(chan struct{})}
	c, s := newAgentTestSession(p)
	a := &Agent{Client: c, MaxRetries: 0}
	done := make(chan error, 1)
	go func() { _, err := a.RunTurn(context.Background(), s, Turn{Role: RoleUser}, Request{}); done <- err }()
	select {
	case <-p.started:
	case <-time.After(time.Second):
		t.Fatal("agent did not start")
	}
	if !a.Interrupt(s.ID()) {
		t.Fatal("interrupt did not find active session")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("agent did not stop after interrupt")
	}
}
func TestAgentErrorRollsBackAndRetries(t *testing.T) {
	p := &agentTestProvider{errors: []error{errors.New("transient agent failure")}, responses: []Response{{Content: []ContentPart{{Type: ContentText, Text: "recovered"}}}}}
	c, s := newAgentTestSession(p)
	a := &Agent{Client: c, MaxRetries: 1}
	resp, err := a.RunTurn(context.Background(), s, Turn{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: "retry me"}}}, Request{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content[0].Text != "recovered" {
		t.Fatalf("unexpected response: %#v", resp)
	}
	if p.calls != 2 {
		t.Fatalf("provider calls=%d", p.calls)
	}
	h := s.History()
	if len(h) != 2 || h[0].Role != RoleUser || h[0].Content[0].Text != "retry me" || h[1].Role != RoleModel {
		t.Fatalf("unexpected final history: %#v", h)
	}
}
func TestAgentRetriesExhaustedRollsBack(t *testing.T) {
	p := &agentTestProvider{errors: []error{errors.New("one"), errors.New("two")}}
	c, s := newAgentTestSession(p)
	a := &Agent{Client: c, MaxRetries: 1}
	_, err := a.RunTurn(context.Background(), s, Turn{Role: RoleUser}, Request{})
	if !errors.Is(err, ErrAgentRetriesExhausted) {
		t.Fatalf("err=%v", err)
	}
	if len(s.History()) != 0 {
		t.Fatalf("history=%d", len(s.History()))
	}
	if p.calls != 2 {
		t.Fatalf("provider calls=%d", p.calls)
	}
}
func TestTraceEventsCarryElapsed(t *testing.T) {
	p := &agentTestProvider{responses: []Response{{Content: []ContentPart{{Type: ContentText, Text: "done"}}}}}
	c, s := newAgentTestSession(p)
	a := &Agent{Client: c, MaxRetries: 0}
	var stages []TraceEvent
	_, err := a.RunTurnWithTrace(context.Background(), s, Turn{Role: RoleUser}, Request{}, func(_ context.Context, e TraceEvent) { stages = append(stages, e) })
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) == 0 {
		t.Fatal("no trace events captured")
	}
	for i, e := range stages {
		if e.Elapsed < 0 {
			t.Fatalf("event %d (%s) has negative elapsed", i, e.Stage)
		}
		if i > 0 && e.Elapsed < stages[i-1].Elapsed {
			t.Fatalf("elapsed went backwards at event %d (%s)", i, e.Stage)
		}
	}
}
func TestInterruptPreservesUserTurn(t *testing.T) {
	p := &blockingAgentProvider{started: make(chan struct{})}
	c, s := newAgentTestSession(p)
	a := &Agent{Client: c, MaxRetries: 0}
	done := make(chan error, 1)
	go func() {
		_, err := a.RunTurn(context.Background(), s, Turn{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: "do work"}}}, Request{})
		done <- err
	}()
	select {
	case <-p.started:
	case <-time.After(time.Second):
		t.Fatal("agent did not start")
	}
	if !a.Interrupt(s.ID()) {
		t.Fatal("interrupt did not find active session")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("agent did not stop after interrupt")
	}
	h := s.History()
	if len(h) != 1 || h[0].Role != RoleUser {
		t.Fatalf("interrupted turn was wiped: %+v", h)
	}
}
func TestSettleInterruptedTurnClosesPendingCalls(t *testing.T) {
	s := NewSession(SessionConfig{ID: "settle", Provider: "test", Model: "model", KeyIndex: 0}, NewKeyPool("key"))
	before := s.History()
	s.Append(Turn{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: "run it"}}})
	s.Append(Turn{Role: RoleToolCall, ToolCall: &ToolCall{ID: "c1", Name: "read", Arguments: `{"path":"a"}`}})
	s.Append(Turn{Role: RoleToolCall, ToolCall: &ToolCall{ID: "c2", Name: "read", Arguments: "{}"}})
	s.Append(Turn{Role: RoleToolResult, ToolResult: &ToolResult{ID: "c2", Content: "ok"}})
	settleInterruptedTurn(s, before)
	h := s.History()
	if len(h) != 5 {
		t.Fatalf("history=%d, want user+2 calls+2 results", len(h))
	}
	last := h[4]
	if last.Role != RoleToolResult || last.ToolResult == nil || last.ToolResult.ID != "c1" || !last.ToolResult.IsError {
		t.Fatalf("missing interrupted result: %+v", last)
	}
	if !strings.Contains(last.ToolResult.Content, "interrupted by the user") {
		t.Fatalf("result missing reason: %q", last.ToolResult.Content)
	}
}
func TestInterruptFlagLifecycle(t *testing.T) {
	a := &Agent{}
	if a.wasInterrupted("s") {
		t.Fatal("fresh agent should not be interrupted")
	}
	ctx, cleanup := a.beginInterrupt(context.Background(), "s")
	_ = ctx
	if !a.Interrupt("s") {
		t.Fatal("interrupt should find session")
	}
	if !a.wasInterrupted("s") {
		t.Fatal("interrupt flag missing")
	}
	cleanup()
	if a.wasInterrupted("s") {
		t.Fatal("flag should clear after turn cleanup")
	}
	if a.Interrupt("s") {
		t.Fatal("interrupt after cleanup should miss")
	}
}
func TestCloneResponseContentKeepsUsage(t *testing.T) {
	in := Response{Content: []ContentPart{{Type: ContentText, Text: "hi"}}, Usage: Usage{InputTokens: 100, OutputTokens: 5, TotalTokens: 105, CacheReadTokens: 10}}
	got := cloneResponseContent(in)
	if got == nil || got.Usage.InputTokens != 100 || got.Usage.CacheReadTokens != 10 || got.Usage.OutputTokens != 5 {
		t.Fatalf("usage dropped: %+v", got)
	}
}

func TestTerminalFailureEmitsTraceError(t *testing.T) {
	p := &agentTestProvider{errors: []error{errors.New("boom")}}
	c, s := newAgentTestSession(p)
	a := &Agent{Client: c, MaxRetries: 1}
	var stages []TraceEvent
	_, err := a.RunTurnWithTrace(context.Background(), s, Turn{Role: RoleUser}, Request{}, func(_ context.Context, e TraceEvent) { stages = append(stages, e) })
	if err == nil {
		t.Fatal("expected error")
	}
	found := false
	for _, e := range stages {
		if e.Stage == TraceError {
			found = true
		}
	}
	if !found {
		t.Fatalf("no TraceError emitted: %+v", stages)
	}
}
