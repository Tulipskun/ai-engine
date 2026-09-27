package sdk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type subAgentBlockingProvider struct{ started chan struct{} }

func (p *subAgentBlockingProvider) Name() string { return "test" }
func (p *subAgentBlockingProvider) Generate(ctx context.Context, _ Request) (Response, error) {
	select {
	case <-p.started:
	default:
		close(p.started)
	}
	<-ctx.Done()
	return Response{}, ctx.Err()
}
func (p *subAgentBlockingProvider) Stream(context.Context, Request) (<-chan Event, error) {
	return nil, errors.New("not implemented")
}
func (p *subAgentBlockingProvider) WithAPIKey(string) Provider { return p }

type subAgentImmediateProvider struct{}

func (p *subAgentImmediateProvider) Name() string { return "test" }
func (p *subAgentImmediateProvider) Generate(context.Context, Request) (Response, error) {
	return Response{Content: []ContentPart{{Type: ContentText, Text: "worker done"}}}, nil
}
func (p *subAgentImmediateProvider) Stream(context.Context, Request) (<-chan Event, error) {
	return nil, errors.New("not implemented")
}
func (p *subAgentImmediateProvider) WithAPIKey(string) Provider { return p }

type subAgentCaptureProvider struct {
	mu       sync.Mutex
	requests []Request
}

func (p *subAgentCaptureProvider) Name() string { return "test" }
func (p *subAgentCaptureProvider) Generate(_ context.Context, req Request) (Response, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	return Response{Content: []ContentPart{{Type: ContentText, Text: "worker done"}}}, nil
}
func (p *subAgentCaptureProvider) Stream(context.Context, Request) (<-chan Event, error) {
	return nil, errors.New("not implemented")
}
func (p *subAgentCaptureProvider) WithAPIKey(string) Provider { return p }
func (p *subAgentCaptureProvider) lastRequest() Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[len(p.requests)-1]
}

// orchestrationRunner wires a runner whose report sink feeds a channel so
// tests can await progress/final events deterministically.
func orchestrationRunner(t *testing.T, provider Provider) (*subAgentRunner, <-chan SubAgentEvent) {
	t.Helper()
	agent, parent := newSubAgentTest(t, provider)
	manager := newSubAgentManager(agent, agent.SubAgentConfig)
	events := make(chan SubAgentEvent, 32)
	manager.SetEventSink(func(event SubAgentEvent) { events <- event })
	return &subAgentRunner{manager: manager, parent: parent}, events
}

func awaitReport(t *testing.T, events <-chan SubAgentEvent, kind string) SubAgentEvent {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case event := <-events:
			if event.Kind == kind {
				return event
			}
		case <-deadline:
			t.Fatalf("missing %s report", kind)
			return SubAgentEvent{}
		}
	}
}

func TestSubAgentInvestigationWorksBeforePlan(t *testing.T) {
	r, events := orchestrationRunner(t, &subAgentImmediateProvider{})
	id, err := r.Delegate(context.Background(), "read the repository requirements and summarize the current architecture")
	if err != nil {
		t.Fatal(err)
	}
	event := awaitReport(t, events, "final")
	if event.JobID != id || event.Status != "completed" {
		t.Fatalf("unexpected final report: %+v", event)
	}
	if !strings.Contains(event.Report, "worker done") {
		t.Fatalf("report missing result: %s", event.Report)
	}
	if len(r.parent.Plan().Steps) != 0 {
		t.Fatalf("investigation unexpectedly created plan: %#v", r.parent.Plan())
	}
}

func TestSubAgentReceivesFullPlanAndRequiresAcceptance(t *testing.T) {
	provider := &subAgentCaptureProvider{}
	r, events := orchestrationRunner(t, provider)
	_ = r.parent.SetPlan([]string{"inspect and change A", "implement B", "verify C"})
	id, err := r.Delegate(context.Background(), "inspect and change A")
	if err != nil {
		t.Fatal(err)
	}
	awaitReport(t, events, "final")
	state := r.parent.Plan()
	if state.Current != 0 || state.Steps[0].Status != "awaiting_review" || state.Steps[1].Status != "pending" {
		t.Fatalf("worker must await review: %+v", state)
	}
	if err := r.Accept(id, "Reviewed worker changes and validation"); err != nil {
		t.Fatal(err)
	}
	state = r.parent.Plan()
	if state.Current != 1 || state.Steps[0].Status != "completed" || state.Steps[1].Status != "ready" {
		t.Fatalf("unexpected plan state: %+v", state)
	}
	req := provider.lastRequest()
	if !strings.Contains(req.SystemPrompt, "1. inspect and change A") || !strings.Contains(req.SystemPrompt, "2. implement B") || !strings.Contains(req.SystemPrompt, "3. verify C") {
		t.Fatalf("full plan missing from worker system prompt: %s", req.SystemPrompt)
	}
	if !strings.Contains(req.SystemPrompt, "in English") {
		t.Fatalf("worker prompt missing English requirement: %s", req.SystemPrompt)
	}
}

func TestSubAgentDelegateReturnsImmediately(t *testing.T) {
	provider := &subAgentBlockingProvider{started: make(chan struct{})}
	r, _ := orchestrationRunner(t, provider)
	_ = r.parent.SetPlan([]string{"long task"})
	start := time.Now()
	id, err := r.Delegate(context.Background(), "long task")
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("delegate must return control immediately: %s", time.Since(start))
	}
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	if !strings.Contains(r.Status(id), "status=running") {
		t.Fatalf("unexpected status: %s", r.Status(id))
	}
	report, err := r.Stop(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "status=stopped") {
		t.Fatalf("stop did not wait for the terminal state: %s", report)
	}
}

func TestSubAgentProgressReportsArriveEveryXToolCalls(t *testing.T) {
	agent, parent := newSubAgentTest(t, &countingToolProvider{rounds: 4})
	agent.Tools = &echoExecutor{}
	manager := newSubAgentManager(agent, SubAgentConfig{Enabled: true, ReportEveryToolCalls: 2})
	events := make(chan SubAgentEvent, 32)
	manager.SetEventSink(func(event SubAgentEvent) { events <- event })
	r := &subAgentRunner{manager: manager, parent: parent}
	if _, err := r.Delegate(context.Background(), "touch a lot"); err != nil {
		t.Fatal(err)
	}
	first := awaitReport(t, events, "progress")
	if !strings.Contains(first.Report, "read") || !strings.Contains(first.Report, "[ok]") {
		t.Fatalf("progress report missing tool lines: %s", first.Report)
	}
	second := awaitReport(t, events, "progress")
	if !strings.Contains(second.Report, "4. read") {
		t.Fatalf("second progress report missing later tools: %s", second.Report)
	}
	final := awaitReport(t, events, "final")
	if final.Status != "completed" || !strings.Contains(final.Report, "tools_used: 4") {
		t.Fatalf("bad final: %+v", final)
	}
	if len(events) != 0 {
		t.Fatalf("extra reports beyond 2 progress + 1 final: %d", len(events))
	}
}

type echoExecutor struct{}

func (e *echoExecutor) Definitions() []Tool { return []Tool{{Name: "read"}} }
func (e *echoExecutor) Execute(_ context.Context, call ToolCall) ToolResult {
	return ToolResult{ID: call.ID, Content: "file body"}
}

type countingToolProvider struct {
	subAgentCaptureProvider
	rounds int
}

func (p *countingToolProvider) WithAPIKey(string) Provider { return p }
func (p *countingToolProvider) Generate(_ context.Context, req Request) (Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	n := len(p.requests)
	if n <= p.rounds {
		return Response{ToolCalls: []ToolCall{{ID: fmt.Sprintf("c%d", n), Name: "read", Arguments: `{"path":"a.txt"}`}}}, nil
	}
	return Response{Content: []ContentPart{{Type: ContentText, Text: "worker done"}}}, nil
}

func TestSubAgentProgressMessageGuidesScopeCheck(t *testing.T) {
	progress := SubAgentEvent{Kind: "progress", JobID: "sa-1", Report: "1. bash [ok]"}.Message()
	for _, want := range []string{"Scope check required", "stop_subagent", "follow_up_subagent"} {
		if !strings.Contains(progress, want) {
			t.Fatalf("progress guidance missing %q: %s", want, progress)
		}
	}
	final := SubAgentEvent{Kind: "final", JobID: "sa-1", Report: "status=completed"}.Message()
	for _, want := range []string{"accept_subagent_result", "follow_up_subagent", "continue_subagent"} {
		if !strings.Contains(final, want) {
			t.Fatalf("final guidance missing %q: %s", want, final)
		}
	}
}

func newSubAgentTest(t *testing.T, provider Provider) (*Agent, *Session) {
	t.Helper()
	r := NewRouter()
	keys := NewKeyPool("key")
	r.RegisterProvider(ProviderConfig{ID: "test", BaseURL: "http://test", Keys: keys, Adapter: AdapterOpenAI})
	r.Register(ModelRoute{Provider: "test", Model: "model", Adapter: AdapterOpenAI})
	client := NewRouterClient(r)
	client.RegisterAdapter(AdapterOpenAI, provider)
	db, err := OpenSessionDB(t.TempDir() + "/parent.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	session, err := OpenSession(dbPathForTest(db), SessionConfig{ID: "parent", Provider: "test", Model: "model"}, keys)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return &Agent{Client: client, SubAgentConfig: SubAgentConfig{Enabled: true}}, session
}

func dbPathForTest(db *SessionDB) string { return db.path }

func TestSubAgentUsesSeparateSession(t *testing.T) {
	r, events := orchestrationRunner(t, &subAgentImmediateProvider{})
	if _, err := r.Delegate(context.Background(), "isolated task"); err != nil {
		t.Fatal(err)
	}
	awaitReport(t, events, "final")
	if len(r.parent.History()) != 0 {
		t.Fatalf("worker changed parent history: %#v", r.parent.History())
	}
}

func TestSubAgentRunnerHasNoPollingTools(t *testing.T) {
	names := map[string]bool{}
	for _, tool := range (&subAgentTool{}).Definitions() {
		names[tool.Name] = true
	}
	for gone := range map[string]bool{"subagent_status": true, "subagent_history": true} {
		if names[gone] {
			t.Fatalf("polling tool %q must not be exposed (REQ-034)", gone)
		}
	}
	for _, want := range []string{"delegate_to_subagent", "follow_up_subagent", "continue_subagent", "stop_subagent", "accept_subagent_result"} {
		if !names[want] {
			t.Fatalf("missing orchestration tool %q", want)
		}
	}
}

func TestSubAgentStopWaitsForStoppedWorker(t *testing.T) {
	provider := &subAgentBlockingProvider{started: make(chan struct{})}
	r, events := orchestrationRunner(t, provider)
	id, err := r.manager.startAndRegister(r.parent, "never finishing")
	if err != nil {
		t.Fatal(err)
	}
	<-provider.started
	report, err := r.Stop(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "status=stopped") {
		t.Fatalf("stop returned before the worker stopped: %s", report)
	}
	if !strings.Contains(report, "task: never finishing") {
		t.Fatalf("stop report lost the task: %s", report)
	}
	select {
	case event := <-events:
		t.Fatalf("stop delivered a duplicate final report: %+v", event)
	default:
	}
	if _, err := r.Stop(context.Background(), id); err == nil {
		t.Fatal("stop accepted for an already stopped job")
	}
}

func TestSubAgentInheritsParentWorkspace(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "requirements"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "requirements", "functional.md"), []byte("workspace requirement marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &subAgentCaptureProvider{}
	agent, parent := newSubAgentTest(t, provider)
	if err := parent.SetWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	manager := newSubAgentManager(agent, SubAgentConfig{Enabled: true})
	r := &subAgentRunner{manager: manager, parent: parent}
	events := make(chan SubAgentEvent, 8)
	manager.SetEventSink(func(event SubAgentEvent) { events <- event })
	if _, err := r.Delegate(context.Background(), "check workspace"); err != nil {
		t.Fatal(err)
	}
	awaitReport(t, events, "final")
	req := provider.lastRequest()
	if !strings.Contains(req.SystemPrompt, "workspace requirement marker") {
		t.Fatalf("worker requirements not loaded from parent workspace: %s", req.SystemPrompt)
	}
}

// The phone stops one worker by its job id, without waiting for it to wind down
// and without touching the turn that delegated to it.
func TestRequestStopSubAgentStopsOneJobByID(t *testing.T) {
	provider := &subAgentBlockingProvider{started: make(chan struct{})}
	r, events := orchestrationRunner(t, provider)
	if _, err := r.Delegate(context.Background(), "long task"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	r.manager.mu.RLock()
	var jobID string
	for id := range r.manager.jobs {
		jobID = id
		break
	}
	r.manager.mu.RUnlock()
	if jobID == "" {
		t.Fatal("no job was registered")
	}
	if err := r.manager.RequestStop(r.parent.ID(), jobID); err != nil {
		t.Fatalf("RequestStop: %v", err)
	}
	final := awaitReport(t, events, "final")
	if final.JobID != jobID || !strings.Contains(final.Status, "stopped") {
		t.Fatalf("final report = %+v, want job %s stopped", final, jobID)
	}
	// A second press is not an error: the job is already gone.
	if err := r.manager.RequestStop(r.parent.ID(), jobID); err != nil {
		t.Fatalf("stopping a finished job must be accepted, got %v", err)
	}
	if err := r.manager.RequestStop(r.parent.ID(), "sa-does-not-exist"); err == nil {
		t.Fatal("an unknown job must be reported, not silently accepted")
	}
	// A job belonging to another chat is not this chat's to stop.
	if other, err := r.Delegate(context.Background(), "another task"); err == nil {
		if err := r.manager.RequestStop("other-chat", jobID); err == nil {
			t.Fatal("a job from another chat must not be stoppable")
		}
		// Stop the second worker before the test returns: a leaked job keeps
		// writing into this test's temp dir and makes its cleanup racy.
		if err := r.manager.RequestStop(r.parent.ID(), other); err != nil {
			t.Fatalf("stopping the second job: %v", err)
		}
		awaitReport(t, events, "final")
	}
}

// The agent's own hook is what the daemon hands the phone's cancel frame.
func TestStopSubAgentReportsUnknownJob(t *testing.T) {
	agent := &Agent{}
	if err := agent.StopSubAgent("s1", "sa-nope"); err == nil {
		t.Fatal("an agent with no sub agent manager must report the job as unknown")
	}
}
