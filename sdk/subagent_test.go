package sdk

import (
	"github.com/Tulipskun/ai-engine/provider"
	"path/filepath"
	"strings"
	"testing"
)

// REQ-019/REQ-020: one running job per parent, plan/step identity on every
// transition, and a failed step must be recoverable in the same worker session.
// The reservation layer is exercised directly so the tests stay deterministic
// and never start a provider call.

func newTestSession(t *testing.T, id string) *Session {
	t.Helper()
	session, err := OpenSession(
		filepath.Join(t.TempDir(), "session.db"),
		provider.SessionConfig{ID: id, Provider: "test-provider", Model: "test-model"},
		provider.NewKeyPool("test-key"),
	)
	if err != nil {
		t.Fatalf("open session %s: %v", id, err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func newTestManager() *subAgentManager {
	return newSubAgentManager(&Agent{}, SubAgentConfig{})
}

// finishForTest drives a job to a terminal state without a worker behind it. It
// mirrors what run() does once the provider call ends: the status is recorded and
// the handoff report counts as delivered, which is what makes the result
// acceptable. A test must not make provider calls to reach that point.
func (m *subAgentManager) finishForTest(job *subAgentJob, status string) {
	m.mu.Lock()
	job.status, job.reviewed, job.reportDelivered = status, true, true
	m.mu.Unlock()
	job.parent.FinishSubAgent(BoundJob{ID: job.id, Revision: job.revision, Planned: job.planned, Step: job.step.Index}, status)
}

func TestReservationRejectsASecondRunningJob(t *testing.T) {
	parent := newTestSession(t, "parent")
	parent.cleanPlan([]string{"first", "second"})
	manager := newTestManager()

	first, err := manager.startLocked(parent, "task one", "", Input{})
	if err != nil {
		t.Fatalf("first delegation must be accepted: %v", err)
	}
	if _, err := manager.startLocked(parent, "task two", "", Input{}); err == nil {
		t.Fatal("a parent with a running job must not be able to start a second one")
	}
	if parent.Plan().Steps[0].Status != "running" {
		t.Errorf("the bound step must be running, got %q", parent.Plan().Steps[0].Status)
	}

	// A completed step is not accepted yet, so new work must wait: the planner
	// reviews the handoff report and accepts the step before moving on (REQ-019).
	manager.finishForTest(first, "completed")
	if _, err := manager.startContinueLocked(parent, "follow-on", first.id, Input{}); err == nil {
		t.Fatal("follow-on work must be refused while the step still awaits review")
	}
	if err := manager.Accept(parent, first.id, "the output matches the requirement"); err != nil {
		t.Fatalf("accepting a verified completed job must work: %v", err)
	}
	if _, err := manager.startContinueLocked(parent, "follow-on", first.id, Input{}); err != nil {
		t.Fatalf("follow-on work after acceptance must be accepted: %v", err)
	}
}

func TestFailedStepBindsItsJobForRetry(t *testing.T) {
	parent := newTestSession(t, "parent")
	parent.cleanPlan([]string{"only step"})
	manager := newTestManager()

	job, err := manager.startLocked(parent, "do the thing", "", Input{})
	if err != nil {
		t.Fatalf("delegation must be accepted: %v", err)
	}
	manager.finishForTest(job, "failed")

	if got := parent.Plan().Steps[0].Status; got != "failed" {
		t.Fatalf("a failed job must leave its step failed, got %q", got)
	}
	// This predicate is what routes a follow-up to the retry branch, so a failed
	// step has to bind its job: otherwise the step can never be retried.
	if !parent.BindsCurrentStep(BoundJob{ID: job.id, Revision: job.revision, Planned: job.planned, Step: job.step.Index}) {
		t.Fatal("a failed step must bind its job so a follow-up retries that step")
	}

	retry, err := manager.startLocked(parent, "do it again, differently", job.id, Input{})
	if err != nil {
		t.Fatalf("retrying a failed step must be accepted: %v", err)
	}
	if retry.workerID != job.workerID {
		t.Errorf("a retry must reuse the worker session, got %q want %q", retry.workerID, job.workerID)
	}
	if !job.superseded {
		t.Error("the retried job must be superseded so its late report cannot be accepted")
	}
	if got := parent.Plan().Steps[0].Status; got != "running" {
		t.Errorf("the retried step must be running again, got %q", got)
	}
}

func TestAcceptedStepDoesNotBindItsJob(t *testing.T) {
	parent := newTestSession(t, "parent")
	parent.cleanPlan([]string{"first", "second"})
	manager := newTestManager()

	job, err := manager.startLocked(parent, "task", "", Input{})
	if err != nil {
		t.Fatalf("delegation must be accepted: %v", err)
	}
	manager.finishForTest(job, "completed")
	if err := manager.Accept(parent, job.id, "read the file and it is correct"); err != nil {
		t.Fatalf("accepting a verified completed job must work: %v", err)
	}

	// After acceptance the plan moved on, so more work for that job is follow-on
	// work attached to the new current step, not a retry of the finished one.
	if parent.BindsCurrentStep(BoundJob{ID: job.id, Revision: job.revision, Planned: job.planned, Step: job.step.Index}) {
		t.Fatal("an accepted step must not keep binding its job")
	}
	next, err := manager.startContinueLocked(parent, "new work", job.id, Input{})
	if err != nil {
		t.Fatalf("follow-on work must be accepted: %v", err)
	}
	if next.workerID != job.workerID {
		t.Errorf("follow-on work must reuse the worker session, got %q want %q", next.workerID, job.workerID)
	}
	if next.step.Index != 2 {
		t.Errorf("follow-on work must attach to the new current step, got step %d", next.step.Index)
	}
	if job.superseded {
		t.Error("a follow-up must not supersede the job it continues from")
	}
}

func TestAcceptRequiresVerifiedEvidence(t *testing.T) {
	parent := newTestSession(t, "parent")
	parent.cleanPlan([]string{"only step"})
	manager := newTestManager()

	job, err := manager.startLocked(parent, "task", "", Input{})
	if err != nil {
		t.Fatalf("delegation must be accepted: %v", err)
	}
	manager.finishForTest(job, "completed")

	if err := manager.Accept(parent, job.id, ""); err == nil {
		t.Fatal("acceptance without verification evidence must be refused")
	}
	if got := parent.Plan().Steps[0].Status; got != "awaiting_review" {
		t.Errorf("a completed job must leave the step awaiting review, got %q", got)
	}
}

func TestFailedWorkCannotBeAccepted(t *testing.T) {
	parent := newTestSession(t, "parent")
	parent.cleanPlan([]string{"only step"})
	manager := newTestManager()

	job, err := manager.startLocked(parent, "task", "", Input{})
	if err != nil {
		t.Fatalf("delegation must be accepted: %v", err)
	}
	manager.finishForTest(job, "failed")

	if err := manager.Accept(parent, job.id, "it looks fine to me"); err == nil {
		t.Fatal("a failed job must never be accepted as verified success")
	}
}

func TestReplacementPlanInvalidatesAnInFlightJob(t *testing.T) {
	parent := newTestSession(t, "parent")
	parent.cleanPlan([]string{"first"})
	manager := newTestManager()

	job, err := manager.startLocked(parent, "task", "", Input{})
	if err != nil {
		t.Fatalf("delegation must be accepted: %v", err)
	}
	manager.finishForTest(job, "failed")

	// The planner rewrites the plan with identical wording. The revision counter is
	// what stops the old job's result being accepted against the new plan.
	parent.cleanPlan([]string{"first"})
	if _, err := manager.startLocked(parent, "retry", job.id, Input{}); err == nil {
		t.Fatal("a job from a replaced plan must not be retryable against the new one")
	}
}

func TestJobsAreScopedToTheirOwnParent(t *testing.T) {
	first := newTestSession(t, "first")
	second := newTestSession(t, "second")
	first.cleanPlan([]string{"step"})
	manager := newTestManager()

	job, err := manager.startLocked(first, "task", "", Input{})
	if err != nil {
		t.Fatalf("delegation must be accepted: %v", err)
	}
	manager.finishForTest(job, "completed")

	if _, err := manager.result(second, job.id, ""); err == nil {
		t.Error("another parent must not be able to read a job it does not own")
	}
	if err := manager.RequestStop(second.ID(), job.id); err == nil {
		t.Error("another parent must not be able to stop a job it does not own")
	}
}

func TestHandoffReportNamesTheToolsTheWorkerUsed(t *testing.T) {
	parent := newTestSession(t, "parent")
	parent.cleanPlan([]string{"step"})
	manager := newTestManager()

	job, err := manager.startLocked(parent, "task", "", Input{})
	if err != nil {
		t.Fatalf("delegation must be accepted: %v", err)
	}
	manager.mu.Lock()
	job.tools = []toolHistoryEntry{
		{ID: "t1", Name: "bash", Arguments: `{"command":"go test ./..."}`, Result: "ok", completed: true},
		{ID: "t2", Name: "bash", Arguments: `{"command":"rm x"}`, Result: "boom", IsError: true, completed: true},
	}
	report := manager.reportLocked(job)
	manager.mu.Unlock()

	// The planner reviews this report, so it must carry the tool names, their
	// arguments, and whether each result was an error.
	for _, want := range []string{"bash", "go test ./...", "error:"} {
		if !strings.Contains(report, want) {
			t.Errorf("handoff report must mention %q, got:\n%s", want, report)
		}
	}
	// It must also point at the tool that actually exists, not a retired name.
	for _, retired := range []string{"accept_subagent_result", "follow_up_subagent", "continue_subagent"} {
		if strings.Contains(report, retired) {
			t.Errorf("handoff report still names the retired tool %q", retired)
		}
	}
}
