package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Tulipskun/ai-engine/provider"
	"strings"
)

const planningToolName = "plan"

// mainReadToolNames is the planner read-only allowlist (REQ-016, CHANGE-087):
// the tools the senior Main Agent may execute itself. Since CHANGE-087 the
// registry is read + bash, so read is the only tool the planner keeps: bash is
// hidden from Definitions and rejected by Execute, which leaves the planner
// unable to list or search — index.md is how it learns what exists.
var mainReadToolNames = map[string]bool{
	"read": true,
}

const planningToolDescription = "Create or replace the execution checklist for the user's goal before delegating execution. Describe the intended work in concise, ordered steps; the checklist with per-step status is shown to you on every turn. Read index.md first yourself, then delegate. This is a planning action, not the final answer."

const planningSystemInstruction = "You are the Main Agent, the senior engineer. You think in this context: analyze the goal, read code and context yourself, make the design calls, and break the work into minimal ordered steps. Never hand the user's raw message to the worker as its task. You have exactly one tool: read. There is no list, no search and no shell, so you cannot discover paths yourself - index.md is the map of the project, so read it first in one call, then read only the few files it names as relevant; max ~10 reads per planning round and no repeated re-reads of a file you already read. You never write, edit, run or browse - a bash call is rejected; the worker is the only hands on the project. Every delegation is an engineering contract in English: Objective, Non-goals, Authority (allowed paths, allowed commands, forbidden actions), Expected tests, Required evidence (summary, changed files with reasons, commands run, tests and results, known limitations), Acceptance criteria; write each delegated task so the worker validates with the minimal sufficient check only. Declare a tool budget with every delegation (max reads, max edits, max bash calls, per requirements/loop-control.md); the worker must read index.md first and stop as failed when its budget is exceeded, and record the lesson in requirements/lessons.md instead of looping. All planner-to-worker traffic (task text, follow-ups, and reports) must be in English regardless of the user's language; answer the user in their own language. You hold the project overview and own the checklist: every turn shows the current plan steps with their statuses, and only you advance them through explicit acceptance. These role boundaries take precedence over any conflicting direct-execution instructions in the supplied context. If the user's message needs no tools, no repository context, and no task to complete - a greeting, thanks, acknowledgement, or a question answerable directly from the conversation - reply in the user's language immediately with no tool calls and no plan. Scale effort to the task: for a trivial task that needs no repository context, skip the separate investigation and make a minimal one-step plan. Keep every plan to the fewest steps that cover the goal. After the plan exists, delegate exactly one current plan step at a time with `delegate_task`. Delegation returns control to you immediately with a job id. Reports then arrive on their own: a progress report after every few completed worker tool calls (use it for one quick scope check - over/under/off-target work; if wrong, call `delegate_stop`, which blocks until the worker really stops, then `delegate_message` with the corrected task; if correct, answer briefly and stop calling tools so you wait cheaply), and the complete handoff report (terminal status, final summary, every worker tool with arguments and result) when the job ends. There are no history-polling tools: `delegate_status` shows one job's state on demand, but prefer waiting for the automatic reports over asking again. Review the work package against its evidence - spot-check by reading files yourself when needed - and call `delegate_result` with verification evidence to advance exactly one step. Verify, don't trust: a worker loop ending, including a textual blocked or incomplete report, is NOT verified success and never advances the plan. For failed, blocked, or incomplete work use `delegate_message` with the job ID to retry in the same worker session (returns a new job id; its reports arrive the same way); for new follow-on work use `delegate_message` with the job ID to keep the same worker session and its history. `delegate_stop` blocks until the worker has actually stopped and returns its final partial report. After acceptance, delegate the next step or continue the same worker session. Stale results from replacement plans must not be accepted. Keep tool calls minimal: read once, delegate once, react only to the automatic reports, one acceptance per step. System loop-control caps (REQ-045) fail a runaway turn automatically; staying under budget is part of verified success."

type planningToolInput struct {
	Plan string `json:"plan"`
}

type planningToolExecutor struct {
	base     ToolExecutor
	subAgent SubAgentRunner
	session  *Session
}

func newPlanningToolExecutor(base ToolExecutor, session *Session) *planningToolExecutor {
	return &planningToolExecutor{base: base, session: session}
}

func (e *planningToolExecutor) ConfigureSubAgent(runner SubAgentRunner) {
	if e != nil {
		e.subAgent = runner
	}
}

func (e *planningToolExecutor) Definitions() []provider.Tool {
	if e == nil {
		return nil
	}
	defs := []provider.Tool{{
		Name: planningToolName, Description: planningToolDescription, InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"plan": map[string]any{"type": "string"}}, "required": []string{"plan"},
		},
	}}
	if e.subAgent != nil {
		defs = append(defs, (&subAgentTool{runner: e.subAgent}).Definitions()...)
	}
	// CHANGE-054: the senior planner reads code and context itself through
	// these read-only tools. Write/exec tools stay hidden AND rejected.
	if e.base != nil {
		for _, d := range e.base.Definitions() {
			if mainReadToolNames[d.Name] {
				defs = append(defs, d)
			}
		}
	}
	return defs
}

func planningSystemPrompt(base string, plan PlanState) string {
	// Keep repository requirements and custom context intact. The appended role
	// boundary and executor allowlist supersede conflicting execution guidance.
	// The checklist rides every request so the planner always sees the whole
	// project and each step's status (REQ-016, REQ-034).
	base = strings.TrimSpace(base)
	prompt := planningSystemInstruction
	if len(plan.Steps) > 0 {
		prompt = "Current checklist (you own these statuses; they advance only through `delegate_result`):\n" + formatPlanStatus(plan) + "\n\n" + prompt
	}
	if base == "" {
		return prompt
	}
	return base + "\n\n" + prompt
}

func formatPlanStatus(plan PlanState) string {
	var b strings.Builder
	for _, step := range plan.Steps {
		marker := "[ ]"
		switch step.Status {
		case "completed":
			marker = "[x]"
		case "ready":
			marker = "[>]"
		case "running":
			marker = "[~]"
		case "awaiting_review":
			marker = "[?]"
		case "failed":
			marker = "[!]"
		}
		fmt.Fprintf(&b, "%s %d. %s (%s)\n", marker, step.Index, step.Text, step.Status)
	}
	return strings.TrimSpace(b.String())
}

// orchestrationTools is every tool the planner may drive a worker with. It is
// the single answer for both Definitions and Execute: a name that is advertised
// but missing here is a tool the model can see and never successfully call,
// which is how `delegate_status` became dead on arrival before (CHANGE-099).
var orchestrationTools = map[string]bool{
	"delegate_task":    true,
	"delegate_message": true,
	"delegate_status":  true,
	"delegate_stop":    true,
	"delegate_result":  true,
}

func (e *planningToolExecutor) Execute(ctx context.Context, call provider.ToolCall) provider.ToolResult {
	if e == nil {
		return provider.ToolResult{ID: call.ID, Content: "tool execution is not configured", IsError: true}
	}
	if orchestrationTools[call.Name] {
		if e.subAgent == nil {
			return provider.ToolResult{ID: call.ID, Content: "sub-agent is not configured", IsError: true}
		}
		return (&subAgentTool{runner: e.subAgent}).Execute(ctx, call)
	}
	if call.Name == planningToolName {
		var input planningToolInput
		if err := json.Unmarshal([]byte(call.Arguments), &input); err != nil {
			return provider.ToolResult{ID: call.ID, Content: "invalid plan tool arguments", IsError: true}
		}
		input.Plan = strings.TrimSpace(input.Plan)
		if input.Plan == "" {
			return provider.ToolResult{ID: call.ID, Content: "plan is required", IsError: true}
		}
		steps := parsePlanSteps(input.Plan)
		if len(steps) == 0 {
			return provider.ToolResult{ID: call.ID, Content: "plan must contain at least one step", IsError: true}
		}
		if e.session != nil {
			e.session.cleanPlan(steps)
		}
		return provider.ToolResult{ID: call.ID, Content: "Checklist recorded with " + fmt.Sprint(len(steps)) + " ordered step(s); statuses are shown in your system prompt every turn. Start with step 1: delegate it and advance only after reviewing the blocking report and accepting verified success with delegate_result."}
	}
	if mainReadToolNames[call.Name] && e.base != nil {
		return e.base.Execute(ctx, call)
	}
	return provider.ToolResult{ID: call.ID, Content: "Main Agent has no write/exec or search tools; read context yourself with read and delegate execution to `delegate_task`", IsError: true}
}

func parsePlanSteps(plan string) []string {
	lines := strings.Split(plan, "\n")
	steps := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "- *")
		line = strings.TrimSpace(line)
		for len(line) > 0 && line[0] >= '0' && line[0] <= '9' {
			line = line[1:]
		}
		line = strings.TrimSpace(strings.TrimLeft(line, ".):"))
		if line != "" {
			steps = append(steps, line)
		}
	}
	if len(steps) == 0 && strings.TrimSpace(plan) != "" {
		return []string{strings.TrimSpace(plan)}
	}
	return steps
}
