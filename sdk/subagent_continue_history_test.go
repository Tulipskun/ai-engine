package sdk

import (
	"context"
	"strings"
	"sync"
	"testing"
)

type toolUsingProvider struct {
	subAgentCaptureProvider
}

func (p *toolUsingProvider) WithAPIKey(string) Provider { return p }
func (p *toolUsingProvider) Generate(_ context.Context, req Request) (Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if len(p.requests) == 1 {
		return Response{ToolCalls: []ToolCall{{ID: "c1", Name: "read", Arguments: `{"path":"a.txt"}`}}}, nil
	}
	return Response{Content: []ContentPart{{Type: ContentText, Text: "worker done after tool"}}}, nil
}

func TestSubAgentFinalReportShowsToolsArgsAndResults(t *testing.T) {
	r, events := orchestrationRunner(t, &toolUsingProvider{})
	if _, err := r.Delegate(context.Background(), "use tools"); err != nil {
		t.Fatal(err)
	}
	event := awaitReport(t, events, "final")
	report := event.Report
	for _, want := range []string{"read", `{"path":"a.txt"}`, "tools_used:", "worker done after tool", "task: use tools"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q:\n%s", want, report)
		}
	}
	if !strings.Contains(report, "result:") {
		t.Fatalf("report missing final result:\n%s", report)
	}
}

func TestSubAgentContinueReusesWorkerSession(t *testing.T) {
	provider := &subAgentCaptureProvider{}
	r, events := orchestrationRunner(t, provider)
	_ = r.parent.SetPlan([]string{"step one"})
	id, err := r.Delegate(context.Background(), "step one")
	if err != nil {
		t.Fatal(err)
	}
	awaitReport(t, events, "final")
	if err := r.Accept(id, "verified step one"); err != nil {
		t.Fatal(err)
	}
	// New work into the same worker session must be allowed after acceptance.
	next, err := r.Continue(context.Background(), id, "follow-on work in same session")
	if err != nil {
		t.Fatalf("continue rejected: %v", err)
	}
	if next == id {
		t.Fatal("continue reused job identity")
	}
	manager := r.manager
	manager.mu.RLock()
	first, ok1 := manager.jobs[id]
	second, ok2 := manager.jobs[next]
	manager.mu.RUnlock()
	if !ok1 || !ok2 {
		t.Fatal("jobs missing")
	}
	if first.workerID != second.workerID {
		t.Fatalf("worker session not reused: %q vs %q", first.workerID, second.workerID)
	}
	awaitReport(t, events, "final")
	req := provider.lastRequest()
	var history strings.Builder
	for _, turn := range req.Messages {
		for _, part := range turn.Content {
			history.WriteString(part.Text)
		}
	}
	if !strings.Contains(history.String(), "step one") || !strings.Contains(history.String(), "follow-on work in same session") {
		t.Fatalf("continued session lost history: %s", history.String())
	}
	// Follow-up on the accepted job must still be rejected; continue is the path.
	if _, err := r.FollowUp(context.Background(), id, "retry accepted"); err == nil {
		t.Fatal("follow-up on accepted job should fail")
	}
	// Cross-parent continue must be denied.
	other := &subAgentRunner{manager: manager, parent: NewSession(SessionConfig{ID: "other"}, nil)}
	if _, err := other.Continue(context.Background(), id, "steal"); err == nil {
		t.Fatal("cross-parent continue succeeded")
	}
	// Continue while running must be denied.
	blocking := &subAgentBlockingProvider{started: make(chan struct{})}
	rb, evts := orchestrationRunner(t, blocking)
	_ = rb.parent.SetPlan([]string{"long"})
	running, err := rb.manager.startAndRegister(rb.parent, "long")
	if err != nil {
		t.Fatal(err)
	}
	<-blocking.started
	if _, err := rb.Continue(context.Background(), running, "overlap"); err == nil {
		t.Fatal("continue overlapped running job")
	}
	if _, err := rb.Stop(context.Background(), running); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-evts:
		t.Fatalf("stopped job leaked a report: %+v", event)
	default:
	}
}

func TestSubAgentContinueToolValidation(t *testing.T) {
	r, events := orchestrationRunner(t, &subAgentImmediateProvider{})
	tool := newPlanningToolExecutor(nil, r.parent)
	tool.ConfigureSubAgent(r)
	if result := tool.Execute(context.Background(), ToolCall{Name: "continue_subagent", Arguments: `{"task":"missing id"}`}); !result.IsError {
		t.Fatal("continue without ID started work")
	}
	_ = r.parent.SetPlan([]string{"step"})
	id, err := r.Delegate(context.Background(), "step")
	if err != nil {
		t.Fatal(err)
	}
	awaitReport(t, events, "final")
	if err := r.Accept(id, "verified"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Continue(context.Background(), id, "extra"); err == nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("continue reserved %d jobs", winners)
	}
	awaitReport(t, events, "final")
}
