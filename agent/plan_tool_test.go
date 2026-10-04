package agent

import (
	"context"
	"github.com/Tulipskun/ai-engine/provider"
	"strings"
	"testing"
)

// The Main Agent is a planner: it may read and it may drive a worker, and nothing
// else. REQ-016 keeps it read-only plus orchestration; CHANGE-087 left `read` as
// its only context tool because the registry's execution tool is `bash`.

// stubTools stands in for the worker registry so the planner's own surface can be
// checked without a workspace or a provider.
type stubTools struct{ executed []string }

func (s *stubTools) Definitions() []provider.Tool {
	return []provider.Tool{
		{Name: "read", Description: "read a file"},
		{Name: "bash", Description: "run a command"},
	}
}

func (s *stubTools) Execute(_ context.Context, call provider.ToolCall) provider.ToolResult {
	s.executed = append(s.executed, call.Name)
	return provider.ToolResult{ID: call.ID, Content: "ok"}
}

func newPlannerUnderTest(t *testing.T) (*planningToolExecutor, *stubTools) {
	t.Helper()
	base := &stubTools{}
	executor := newPlanningToolExecutor(base, nil)
	executor.ConfigureSubAgent(&subAgentRunner{manager: newTestManager(), parent: newTestSession(t, "planner")})
	return executor, base
}

func TestPlannerAdvertisesNoExecutionTools(t *testing.T) {
	executor, _ := newPlannerUnderTest(t)
	allowed := map[string]bool{"plan": true, "read": true}
	for name := range orchestrationTools {
		allowed[name] = true
	}
	for _, def := range executor.Definitions() {
		if !allowed[def.Name] {
			t.Errorf("planner must not be offered %q", def.Name)
		}
	}
}

func TestPlannerRejectsDirectExecution(t *testing.T) {
	executor, base := newPlannerUnderTest(t)
	result := executor.Execute(context.Background(), provider.ToolCall{ID: "1", Name: "bash", Arguments: `{"command":"rm -rf /"}`})
	if !result.IsError {
		t.Error("a bash call from the planner must be refused")
	}
	if len(base.executed) != 0 {
		t.Errorf("the refused call must never reach the worker registry, got %v", base.executed)
	}
}

// A tool the planner can see but cannot execute is worse than one it never sees:
// the model burns a turn on it and then reports a bogus failure. This is the
// regression guard for delegate_status, which was advertised but unrouted.
func TestEveryAdvertisedOrchestrationToolIsRoutable(t *testing.T) {
	executor, _ := newPlannerUnderTest(t)
	advertised := map[string]bool{}
	for _, def := range executor.Definitions() {
		advertised[def.Name] = true
	}
	for name := range orchestrationTools {
		if !advertised[name] {
			t.Errorf("%q is routable but not advertised to the planner", name)
			continue
		}
		result := executor.Execute(context.Background(), provider.ToolCall{ID: "1", Name: name, Arguments: `{}`})
		// An unknown job id is the correct answer here; being told the planner has
		// no such tool is the bug.
		if result.IsError && strings.Contains(result.Content, "no write/exec") {
			t.Errorf("%q is advertised but Execute rejects it as an unknown planner tool", name)
		}
	}
}

func TestPlannerReadsThroughTheRegistry(t *testing.T) {
	executor, base := newPlannerUnderTest(t)
	result := executor.Execute(context.Background(), provider.ToolCall{ID: "1", Name: "read", Arguments: `{"path":"index.md"}`})
	if result.IsError {
		t.Fatalf("read must be allowed for the planner, got %q", result.Content)
	}
	if len(base.executed) != 1 || base.executed[0] != "read" {
		t.Errorf("read must reach the registry, got %v", base.executed)
	}
}

func TestChecklistIsInjectedOnEveryPlannerTurn(t *testing.T) {
	executor, _ := newPlannerUnderTest(t)
	sessionLocal := newTestSession(t, "with-plan")
	executor.chat = sessionLocal
	sessionLocal.CleanPlan([]string{"read the spec", "change the code"})

	prompt := planningSystemPrompt("base prompt", sessionLocal.Plan())
	for _, want := range []string{"base prompt", "read the spec", "change the code", "pending", "ready"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("planner prompt must carry %q, got:\n%s", want, prompt)
		}
	}
}

func TestPlannerPromptNamesOnlyLiveOrchestrationTools(t *testing.T) {
	prompt := planningSystemInstruction + defaultSubAgentSystemPrompt
	for _, retired := range []string{
		"delegate_to_subagent", "follow_up_subagent", "continue_subagent",
		"stop_subagent", "accept_subagent_result",
	} {
		if strings.Contains(prompt, retired) {
			t.Errorf("prompts still name the retired tool %q", retired)
		}
	}
	// Every orchestration tool the planner is offered has to be named, or the model
	// cannot discover it from the prompt alone.
	for name := range orchestrationTools {
		if !strings.Contains(prompt, name) {
			t.Errorf("planner prompt never mentions the tool it is offered, %q", name)
		}
	}
}
