package sdk

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type retryResultProvider struct {
	subAgentCaptureProvider
	fail bool
}

func (p *retryResultProvider) WithAPIKey(string) Provider { return p }
func (p *retryResultProvider) Generate(_ context.Context, req Request) (Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if len(p.requests) == 1 {
		if p.fail {
			return Response{}, errors.New("worker failed")
		}
		return Response{Content: []ContentPart{{Type: ContentText, Text: "BLOCKED: incomplete; tests not run"}}}, nil
	}
	return Response{Content: []ContentPart{{Type: ContentText, Text: "Implemented and tests passed"}}}, nil
}

func TestSubAgentBlockedResultRetryAndAcceptance(t *testing.T) {
	provider := &retryResultProvider{}
	r, events := orchestrationRunner(t, provider)
	_ = r.parent.SetPlan([]string{"implement", "validate"})
	id, err := r.Delegate(context.Background(), "implement")
	if err != nil {
		t.Fatal(err)
	}
	event := awaitReport(t, events, "final")
	if event.Status != "completed" || !strings.Contains(event.Report, "BLOCKED") {
		t.Fatalf("unexpected final report: %+v", event)
	}
	state := r.parent.Plan()
	if state.Current != 0 || state.Steps[0].Status != "awaiting_review" {
		t.Fatalf("blocked report advanced plan: %+v", state)
	}
	if _, err := r.Delegate(context.Background(), "skip to validation"); err == nil {
		t.Fatal("delegated without acceptance")
	}
	retry, err := r.FollowUp(context.Background(), id, "Resolve block and run tests")
	if err != nil {
		t.Fatal(err)
	}
	if retry == id {
		t.Fatal("retry reused result identity")
	}
	retryEvent := awaitReport(t, events, "final")
	if !strings.Contains(retryEvent.Report, "Implemented and tests passed") {
		t.Fatalf("retry lost its report: %s", retryEvent.Report)
	}
	req := provider.lastRequest()
	var history strings.Builder
	for _, turn := range req.Messages {
		for _, part := range turn.Content {
			history.WriteString(part.Text)
		}
	}
	if !strings.Contains(history.String(), "BLOCKED") || !strings.Contains(history.String(), "Resolve block") {
		t.Fatalf("retry lost worker history: %s", history.String())
	}
	if err := r.Accept(id, "old result"); err == nil {
		t.Fatal("accepted superseded attempt")
	}
	if _, err := r.FollowUp(context.Background(), id, "again"); err == nil {
		t.Fatal("retried superseded attempt")
	}
	if err := r.Accept(retry, ""); err == nil {
		t.Fatal("accepted without verification")
	}
	if err := r.Accept(retry, "Reviewed implementation and passing test report"); err != nil {
		t.Fatal(err)
	}
	if err := r.Accept(retry, "twice"); err == nil {
		t.Fatal("accepted result twice")
	}
	if _, err := r.FollowUp(context.Background(), retry, "again"); err == nil {
		t.Fatal("retried accepted result")
	}
	state = r.parent.Plan()
	if state.Current != 1 || state.Steps[1].Status != "ready" {
		t.Fatalf("accept did not advance exactly once: %+v", state)
	}
}

func TestSubAgentFailureRemainsRetryable(t *testing.T) {
	r, events := orchestrationRunner(t, &retryResultProvider{fail: true})
	r.manager.agent.MaxRetries = -1
	_ = r.parent.SetPlan([]string{"fix"})
	id, err := r.Delegate(context.Background(), "fix")
	if err != nil {
		t.Fatal(err)
	}
	if event := awaitReport(t, events, "final"); event.Status != "failed" {
		t.Fatalf("expected failure: %+v", event)
	}
	if err := r.Accept(id, "failed"); err == nil {
		t.Fatal("accepted failed execution")
	}
	if state := r.parent.Plan(); state.Current != 0 || state.Steps[0].Status != "failed" {
		t.Fatalf("bad failure state: %+v", state)
	}
	retry, err := r.FollowUp(context.Background(), id, "retry fix")
	if err != nil {
		t.Fatal(err)
	}
	event := awaitReport(t, events, "final")
	if event.Status != "completed" {
		t.Fatalf("retry did not complete: %+v", event)
	}
	if err := r.Accept(retry, "verified retry"); err != nil {
		t.Fatal(err)
	}
}

func TestSubAgentReservationAndStaleCompletion(t *testing.T) {
	for _, planned := range []bool{false, true} {
		t.Run(map[bool]string{false: "investigation", true: "planned"}[planned], func(t *testing.T) {
			provider := &subAgentBlockingProvider{started: make(chan struct{})}
			r, _ := orchestrationRunner(t, provider)
			if planned {
				_ = r.parent.SetPlan([]string{"old"})
			}
			var wg sync.WaitGroup
			ids := make(chan string, 32)
			for i := 0; i < 32; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					id, err := r.manager.startAndRegister(r.parent, "work")
					if err == nil {
						ids <- id
					}
				}()
			}
			wg.Wait()
			close(ids)
			var successes []string
			for id := range ids {
				successes = append(successes, id)
			}
			if len(successes) != 1 {
				t.Fatalf("reservation allowed %d jobs", len(successes))
			}
			id := successes[0]
			<-provider.started
			oldRevision := r.parent.Plan().Revision
			_ = r.parent.SetPlan([]string{"replacement"})
			if r.parent.Plan().Revision == oldRevision {
				t.Fatal("replacement reused revision")
			}
			if _, err := r.Delegate(context.Background(), "overlap replacement"); err == nil {
				t.Fatal("replacement bypassed reservation")
			}
			if _, err := r.FollowUp(context.Background(), id, "overlap retry"); err == nil {
				t.Fatal("running job retried")
			}
			if _, err := r.Stop(context.Background(), id); err != nil {
				t.Fatal("stop rejected")
			}
			if state := r.parent.Plan(); len(state.Steps) != 1 || state.Steps[0].Status != "ready" {
				t.Fatalf("stale completion mutated replacement: %+v", state)
			}
			if err := r.Accept(id, "stale"); err == nil {
				t.Fatal("accepted stale result")
			}
			if _, err := r.FollowUp(context.Background(), id, "stale"); err == nil {
				t.Fatal("retried stale result")
			}
		})
	}
}

func TestSubAgentStaleSuccessfulResultAndCompletedPlanInvestigation(t *testing.T) {
	r, events := orchestrationRunner(t, &subAgentImmediateProvider{})
	_ = r.parent.SetPlan([]string{"same text"})
	id, err := r.Delegate(context.Background(), "same text")
	if err != nil {
		t.Fatal(err)
	}
	awaitReport(t, events, "final")
	_ = r.parent.SetPlan([]string{"same text"})
	if err := r.Accept(id, "stale identical step"); err == nil {
		t.Fatal("accepted old revision")
	}
	id, err = r.Delegate(context.Background(), "same text")
	if err != nil {
		t.Fatal(err)
	}
	awaitReport(t, events, "final")
	if err := r.Accept(id, "verified"); err != nil {
		t.Fatal(err)
	}
	revision := r.parent.Plan().Revision
	report, err := r.Delegate(context.Background(), "investigate new task")
	if err != nil {
		t.Fatal(err)
	}
	event := awaitReport(t, events, "final")
	if event.PlanStep.Index != 0 || event.JobID != report {
		t.Fatalf("not investigation: %+v", event)
	}
	if state := r.parent.Plan(); state.Revision != revision || state.Current != 1 || state.Steps[0].Status != "completed" {
		t.Fatalf("investigation reset completed plan: %+v", state)
	}
}

func TestSubAgentCrossParentOperationsDenied(t *testing.T) {
	provider := &subAgentBlockingProvider{started: make(chan struct{})}
	r, events := orchestrationRunner(t, provider)
	other := &subAgentRunner{manager: r.manager, parent: NewSession(SessionConfig{ID: "other"}, nil)}
	_ = r.parent.SetPlan([]string{"secret"})
	id, err := r.manager.startAndRegister(r.parent, "secret")
	if err != nil {
		t.Fatal(err)
	}
	<-provider.started
	if _, err := other.Stop(context.Background(), id); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("cross-parent stop: %v", err)
	}
	if got := other.Status(id); !strings.Contains(got, "not found") || strings.Contains(got, "secret") {
		t.Fatalf("cross-parent status: %s", got)
	}
	if _, err := other.FollowUp(context.Background(), id, "steal"); err == nil {
		t.Fatal("cross-parent follow-up succeeded")
	}
	if err := other.Accept(id, "steal"); err == nil {
		t.Fatal("cross-parent acceptance succeeded")
	}
	if _, err := r.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		t.Fatalf("cross-parent leaked report: %+v", event)
	default:
	}
}

func TestPlanningGuidanceDescribesAsyncReports(t *testing.T) {
	for _, want := range []string{"Delegation returns control to you immediately", "progress report", "handoff report", "stop_subagent", "follow_up_subagent", "continue_subagent", "accept_subagent_result", "NOT verified success"} {
		if !strings.Contains(planningSystemInstruction, want) {
			t.Fatalf("missing guidance %q", want)
		}
	}
	if strings.Contains(planningSystemInstruction, "subagent_status") || strings.Contains(planningSystemInstruction, "subagent_history") {
		t.Fatal("removed polling tools still referenced")
	}
}

type releasedSubAgentProvider struct {
	subAgentImmediateProvider
	started chan struct{}
	release chan struct{}
}

func (p *releasedSubAgentProvider) WithAPIKey(string) Provider { return p }
func (p *releasedSubAgentProvider) Generate(ctx context.Context, req Request) (Response, error) {
	select {
	case <-p.started:
	default:
		close(p.started)
	}
	select {
	case <-p.release:
		return p.subAgentImmediateProvider.Generate(ctx, req)
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
}

func TestSubAgentSuccessfulStaleCompletionCannotMutateReplacement(t *testing.T) {
	p := &releasedSubAgentProvider{started: make(chan struct{}), release: make(chan struct{})}
	r, events := orchestrationRunner(t, p)
	_ = r.parent.SetPlan([]string{"original"})
	id, err := r.Delegate(context.Background(), "original")
	if err != nil {
		t.Fatal(err)
	}
	<-p.started
	revision := r.parent.Plan().Revision
	_ = r.parent.SetPlan([]string{"replacement"})
	close(p.release)
	event := awaitReport(t, events, "final")
	if event.Status != "completed" || event.PlanRevision != revision {
		t.Fatalf("lost captured revision: %+v", event)
	}
	if state := r.parent.Plan(); state.Current != 0 || state.Steps[0].Status != "ready" {
		t.Fatalf("stale success changed new plan: %+v", state)
	}
	if err := r.Accept(id, "stale success"); err == nil {
		t.Fatal("accepted stale success")
	}
}

func TestSubAgentConcurrentFollowUpReservation(t *testing.T) {
	r, events := orchestrationRunner(t, &subAgentImmediateProvider{})
	_ = r.parent.SetPlan([]string{"step"})
	id, err := r.Delegate(context.Background(), "step")
	if err != nil {
		t.Fatal(err)
	}
	awaitReport(t, events, "final")
	p := &subAgentBlockingProvider{started: make(chan struct{})}
	r.manager.agent.Client.RegisterAdapter(AdapterOpenAI, p)
	var wg sync.WaitGroup
	ids := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			retry, err := r.FollowUp(context.Background(), id, "retry")
			if err == nil {
				ids <- retry
			}
		}()
	}
	wg.Wait()
	close(ids)
	var winners []string
	for retry := range ids {
		winners = append(winners, retry)
	}
	if len(winners) != 1 {
		t.Fatalf("follow-up reserved %d jobs", len(winners))
	}
	select {
	case <-p.started:
	case <-time.After(2 * time.Second):
		t.Fatal("follow-up worker never started")
	}
	if _, err := r.Delegate(context.Background(), "overlap"); err == nil {
		t.Fatal("delegation overlapped follow-up")
	}
	if err := r.Accept(id, "overlap"); err == nil {
		t.Fatal("accepted while follow-up running")
	}
	if _, err := r.Stop(context.Background(), winners[0]); err != nil {
		t.Fatal(err)
	}
}

func TestSubAgentToolAcceptanceAndFollowUpValidation(t *testing.T) {
	r, events := orchestrationRunner(t, &subAgentImmediateProvider{})
	tool := newPlanningToolExecutor(nil, r.parent)
	tool.ConfigureSubAgent(r)
	if result := tool.Execute(context.Background(), ToolCall{Name: "follow_up_subagent", Arguments: `{"task":"missing id"}`}); !result.IsError {
		t.Fatal("follow-up without ID started investigation")
	}
	if state := r.parent.Plan(); len(state.Steps) != 0 {
		t.Fatalf("invalid follow-up changed plan: %+v", state)
	}
	_ = r.parent.SetPlan([]string{"verify"})
	id, err := r.Delegate(context.Background(), "verify")
	if err != nil {
		t.Fatal(err)
	}
	if result := tool.Execute(context.Background(), ToolCall{Name: "accept_subagent_result", Arguments: `{"job_id":"missing","verification":"x"}`}); !result.IsError {
		t.Fatal("tool accepted an unknown job")
	}
	final := awaitReport(t, events, "final")
	args := `{"job_id":"` + id + `","verification":"reviewed test evidence"}`
	if !strings.Contains(final.Report, "worker done") {
		t.Fatalf("final report lost the worker result: %s", final.Report)
	}
	if result := tool.Execute(context.Background(), ToolCall{Name: "accept_subagent_result", Arguments: args}); result.IsError {
		t.Fatalf("tool acceptance failed: %+v", result)
	}
	if state := r.parent.Plan(); state.Current != 1 {
		t.Fatalf("tool did not advance: %+v", state)
	}
}
