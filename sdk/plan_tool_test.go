package sdk

import (
	"context"
	"strings"
	"testing"
)

type planningTestExecutor struct {
	called []string
	defs   []Tool
}

func (e *planningTestExecutor) Definitions() []Tool {
	if e.defs != nil {
		return e.defs
	}
	return []Tool{{Name: "bash"}}
}
func (e *planningTestExecutor) Execute(_ context.Context, call ToolCall) ToolResult {
	e.called = append(e.called, call.Name)
	return ToolResult{Content: "executed:" + call.Name}
}

// mainReadTestDefs stands in for the real registry. It carries bash (the one
// tool the planner must never see) plus two names that no longer exist, on
// purpose: the planner's filter is a name allowlist, so an unknown name has to
// be filtered out too rather than falling through to the base executor.
func mainReadTestDefs() []Tool {
	names := []string{"read", "write_file", "edit_file", "bash"}
	defs := make([]Tool, 0, len(names))
	for _, n := range names {
		defs = append(defs, Tool{Name: n, Description: n, InputSchema: map[string]any{"type": "object"}})
	}
	return defs
}

func TestPlanningToolRequiresPlanFirst(t *testing.T) {
	base := &planningTestExecutor{}
	session := NewSession(SessionConfig{ID: "test"}, nil)
	e := newPlanningToolExecutor(base, session)
	blocked := e.Execute(context.Background(), ToolCall{ID: "1", Name: "bash", Arguments: `{}`})
	if !blocked.IsError || len(base.called) != 0 {
		t.Fatal("execution ran before plan")
	}
	planned := e.Execute(context.Background(), ToolCall{ID: "2", Name: "plan", Arguments: `{"plan":"inspect A\nimplement B\nverify C"}`})
	if planned.IsError {
		t.Fatalf("plan failed: %s", planned.Content)
	}
	blocked = e.Execute(context.Background(), ToolCall{ID: "after-plan", Name: "bash", Arguments: `{}`})
	if !blocked.IsError || len(base.called) != 0 {
		t.Fatal("main executed a worker tool after planning")
	}
	state := session.Plan()
	if len(state.Steps) != 3 || state.Current != 0 {
		t.Fatalf("unexpected plan: %+v", state)
	}
	delegated := &subAgentStub{}
	e.ConfigureSubAgent(delegated)
	result := e.Execute(context.Background(), ToolCall{ID: "3", Name: "delegate_to_subagent", Arguments: `{"task":"inspect A"}`})
	if result.IsError {
		t.Fatalf("delegation failed: %s", result.Content)
	}
	if !delegated.called {
		t.Fatal("sub-agent was not invoked")
	}
}

type subAgentStub struct{ called bool }

func (s *subAgentStub) FollowUp(context.Context, string, string) (string, error) {
	return "followup", nil
}
func (s *subAgentStub) Continue(context.Context, string, string) (string, error) {
	return "continued", nil
}
func (s *subAgentStub) Accept(string, string) error { return nil }
func (s *subAgentStub) Delegate(context.Context, string) (string, error) {
	s.called = true
	return "sa-test", nil
}
func (s *subAgentStub) Status(string) string { return "status=running" }
func (s *subAgentStub) Stop(context.Context, string) (string, error) {
	return "stopped", nil
}

func TestPlanningToolDefinition(t *testing.T) {
	e := newPlanningToolExecutor(&planningTestExecutor{}, nil)
	defs := e.Definitions()
	var plan, delegate bool
	for _, d := range defs {
		if d.Name == planningToolName {
			plan = true
		}
		if d.Name == "delegate_to_subagent" {
			delegate = true
		}
	}
	if !plan {
		t.Fatal("plan tool definition missing")
	}
	if delegate {
		t.Fatal("sub-agent tools should not exist before runner configuration")
	}
}

func TestMainAgentToolAllowlist(t *testing.T) {
	base := &planningTestExecutor{defs: mainReadTestDefs()}
	e := newPlanningToolExecutor(base, nil)
	e.ConfigureSubAgent(&subAgentStub{})
	defs := e.Definitions()
	names := make(map[string]bool)
	for _, d := range defs {
		names[d.Name] = true
	}
	for _, want := range []string{planningToolName, "delegate_to_subagent", "stop_subagent", "follow_up_subagent", "continue_subagent", "accept_subagent_result", "read"} {
		if !names[want] {
			t.Fatalf("Main Agent missing allowed tool %q: %+v", want, names)
		}
	}
	for _, forbidden := range []string{"write_file", "edit_file", "bash"} {
		if names[forbidden] {
			t.Fatalf("Main Agent exposed write/exec tool %q", forbidden)
		}
	}
}

func TestMainAgentReadToolsExecuteViaBase(t *testing.T) {
	base := &planningTestExecutor{defs: mainReadTestDefs()}
	session := NewSession(SessionConfig{ID: "test-reads"}, nil)
	e := newPlanningToolExecutor(base, session)
	// Reads run before any plan: the senior reads context first, then plans.
	got := e.Execute(context.Background(), ToolCall{ID: "read", Name: "read", Arguments: `{}`})
	if got.IsError {
		t.Fatalf("read must execute via base: %s", got.Content)
	}
	if len(base.called) != 1 {
		t.Fatalf("base executions=%v", base.called)
	}
	for _, name := range []string{"write_file", "edit_file", "bash"} {
		got := e.Execute(context.Background(), ToolCall{ID: "x-" + name, Name: name, Arguments: `{}`})
		if !got.IsError || !strings.Contains(got.Content, "no write/exec or search tools") {
			t.Fatalf("%s must be rejected: %+v", name, got)
		}
	}
	if len(base.called) != 1 {
		t.Fatalf("write/exec must not reach base: %v", base.called)
	}
}

func TestMainAgentDelegationContractGuidance(t *testing.T) {
	for _, want := range []string{"engineering contract", "Objective", "Non-goals", "Authority", "Required evidence", "Acceptance criteria", "Verify, don't trust"} {
		if !strings.Contains(planningSystemInstruction, want) {
			t.Fatalf("planner prompt missing contract element %q", want)
		}
	}
	for _, want := range []string{"delegation contract", "Authority", "work package plus evidence bundle", "known limitations"} {
		if !strings.Contains(defaultSubAgentSystemPrompt, want) {
			t.Fatalf("worker prompt missing contract element %q", want)
		}
	}
}

func TestPlanningSystemPromptPreservesContext(t *testing.T) {
	base := `Custom instruction: preserve this context.
Available tools:
- bash: legacy execution description
Project Requirements (repository source of truth):
- read must stay inside the workspace.
Unrelated custom instructions after the requirements must survive.`
	got := planningSystemPrompt(base, PlanState{})
	if !strings.HasPrefix(got, base+"\n\n") {
		t.Fatalf("context was modified: %s", got)
	}
	for _, want := range []string{"You are the Main Agent", "a bash call is rejected", "take precedence over any conflicting direct-execution instructions"} {
		if !strings.Contains(got, want) {
			t.Fatalf("role boundary missing %q: %s", want, got)
		}
	}
}

func TestPlanningPromptInjectsChecklist(t *testing.T) {
	plan := PlanState{Revision: 2, Steps: []PlanStep{
		{Index: 1, Text: "inspect A", Status: "completed"},
		{Index: 2, Text: "implement B", Status: "ready"},
		{Index: 3, Text: "verify C", Status: "pending"},
	}}
	got := planningSystemPrompt("base", plan)
	for _, want := range []string{"[x] 1. inspect A (completed)", "[>] 2. implement B (ready)", "[ ] 3. verify C (pending)", "you own these statuses"} {
		if !strings.Contains(got, want) {
			t.Fatalf("checklist injection missing %q: %s", want, got)
		}
	}
	if strings.Contains(planningSystemPrompt("base", PlanState{}), "Current checklist") {
		t.Fatal("empty plan must not inject a checklist")
	}
}

func TestParsePlanSteps(t *testing.T) {
	steps := parsePlanSteps("1. inspect A\n2) implement B\n- verify C")
	if len(steps) != 3 || !strings.EqualFold(steps[0], "inspect A") || !strings.EqualFold(steps[2], "verify C") {
		t.Fatalf("unexpected steps: %#v", steps)
	}
}

func TestPlanningGuidanceRequiresProportionalEffort(t *testing.T) {
	for _, want := range []string{"Scale effort to the task", "skip the separate investigation", "fewest steps", "minimal sufficient check"} {
		if !strings.Contains(planningSystemInstruction, want) {
			t.Fatalf("planning guidance missing %q", want)
		}
	}
	for _, want := range []string{"minimal sufficient check", "Do not repeat equivalent listings", "do not try another command formulation"} {
		if !strings.Contains(defaultSubAgentSystemPrompt, want) {
			t.Fatalf("worker prompt missing %q", want)
		}
	}
}

func TestPlanningGuidanceShortCircuitsNonTasks(t *testing.T) {
	if !strings.Contains(planningSystemInstruction, "no tool calls and no plan") {
		t.Fatal("planner must answer non-task messages directly without planning")
	}
}

func TestLoopControlDisciplineInPrompts(t *testing.T) {
	for _, want := range []string{"tool budget", "requirements/loop-control.md", "requirements/lessons.md", "REQ-045"} {
		if !strings.Contains(planningSystemInstruction, want) {
			t.Fatalf("planner prompt missing loop-control discipline %q", want)
		}
	}
	for _, want := range []string{"tool budget", "requirements/loop-control.md", "requirements/lessons.md"} {
		if !strings.Contains(defaultSubAgentSystemPrompt, want) {
			t.Fatalf("worker prompt missing loop-control discipline %q", want)
		}
	}
}
