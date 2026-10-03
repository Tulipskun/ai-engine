package main

import (
	"context"
	"fmt"
	"github.com/Tulipskun/ai-engine/provider"
	"github.com/Tulipskun/ai-engine/runtime"
	"github.com/Tulipskun/ai-engine/session"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Tulipskun/ai-engine/sdk"
	"github.com/Tulipskun/ai-engine/tools"
)

// This file owns how the daemon is wired to a provider and a workspace: the
// Agent with its tool registry, the planner and worker system prompts, the
// AGENTS.md instruction files, and the root those tools are confined to.
// main.go is the entry point; none of this belongs there.

// newAgentWithWorkspaces builds the Agent with its worker tool registry and the
// per-session workspace lookup, then applies the stored sub-agent defaults.
func newAgentWithWorkspaces(client *provider.RouterClient, workspace, state string, cfg runtime.SystemConfig, workspaceFor func(context.Context) string, sessions *session.SessionManager) (*sdk.Agent, error) {
	registry, err := tools.NewRegistry(workspace)
	if err != nil {
		return nil, err
	}
	if workspaceFor != nil {
		registry.SetWorkspaceResolver(workspaceFor)
	}
	agent := &sdk.Agent{Client: client, Tools: registry, Sessions: sessions}
	agent.SubAgentConfig = sdk.SubAgentConfig{
		Enabled:              cfg.SubAgent.Enabled,
		Provider:             cfg.SubAgent.Provider,
		Model:                cfg.SubAgent.Model,
		MaxOutputTokens:      cfg.SubAgent.MaxOutputTokens,
		Temperature:          cfg.SubAgent.Temperature,
		ThinkingLevel:        cfg.SubAgent.ThinkingLevel,
		SystemPrompt:         cfg.SubAgent.SystemPrompt,
		Workspace:            workspace,
		ReportEveryToolCalls: cfg.SubAgent.ReportEveryToolCalls,
	}
	// Leave an empty worker prompt to the SDK so CLI and SDK defaults stay aligned.
	return agent, nil
}

func systemPrompt(agent *sdk.Agent) string {
	base := ""
	if value := strings.TrimSpace(os.Getenv("AI_SYSTEM_PROMPT")); value != "" {
		base = value
	} else if state, err := stateRoot(); err == nil {
		if cfg, err := runtime.LoadSystemConfig(filepath.Join(state, runtime.DefaultSystemConfigPath)); err == nil {
			base = cfg.SystemPrompt
		}
	}
	if base == "" {
		base = defaultSystemPrompt(agent)
	}
	return base
}

// instructionFiles are the project's instruction files, found the way the
// OpenCode client finds them: the global one first, then AGENTS.md from the
// working directory upwards. Only the OpenCode adapter sends them on; every
// other provider keeps the system prompt alone.
func instructionFiles() []provider.Instruction {
	var out []provider.Instruction
	add := func(path string) {
		raw, err := os.ReadFile(path)
		if err != nil || len(raw) == 0 {
			return
		}
		// A rules file that grew into a novel costs the same input tokens on
		// every turn, so the tail is dropped and said out loud in the log.
		const maxBytes = 64 * 1024
		if len(raw) > maxBytes {
			log.Printf("instructions: %s is %d bytes, using the first %d", path, len(raw), maxBytes)
			raw = raw[:maxBytes]
		}
		out = append(out, provider.Instruction{Path: path, Text: string(raw)})
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		add(filepath.Join(home, ".config", "opencode", "AGENTS.md"))
	}
	dir, err := resolveWorkspace()
	if err != nil || dir == "" {
		return out
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	for {
		add(filepath.Join(dir, "AGENTS.md"))
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return out
}

func defaultSystemPrompt(agent *sdk.Agent) string {
	var b strings.Builder
	b.WriteString("You are the Main Agent: the senior engineer, not a courier. Think in this context: analyze the goal, read code and context yourself with your one read tool (read index.md first - it is the map of the project - then read only the files it names; you cannot list, search or run anything), make the design calls, and break the work into minimal ordered steps. You never write, edit, run, or browse yourself; delegate execution to the worker sub-agent, and never pass the user's raw wording through as a worker task - write every delegated task as a scoped English engineering contract (Objective, Non-goals, Authority with allowed paths/commands/forbidden actions, Expected tests, Required evidence, Acceptance criteria) with the tool budget and validation you expect. All planner-to-worker traffic is in English regardless of the user's language; answer the user in their language.\n")
	b.WriteString("Stay strictly within the user's requested goal and scope. Do not start unrelated improvements, features, cleanup, or investigations.\n")
	b.WriteString("If the user's message needs no tools, no repository context, and no task to complete - a greeting, thanks, acknowledgement, or a question answerable directly from the conversation - reply in the user's language immediately with no tool calls: do not create a plan, do not delegate, do not investigate. Planning and delegation start only when there is real work to do.\n")
	b.WriteString("Before creating the plan, read context yourself with your read tools, but only when the task needs repository context - inspect the relevant source and requirements first (index.md is the map, then read the files it names), then use what you learned to write a precise contract. For a trivial task that needs no repository context, skip that investigation and make a minimal one-step plan. Keep every plan to the fewest steps that cover the goal. You have no write, exec, list or search tools - a bash call is rejected; the worker does the hands-on work.\n")
	b.WriteString("Create one ordered execution plan. The plan is the authoritative sequence of steps. Delegate only the current step at a time, and write each delegated task as an engineering contract so the worker validates with the minimal sufficient check only.\n")
	b.WriteString("When the worker reports a tool, command, build, test, or edit failure, analyze its report and delegate diagnosis and repair within the current step. A failure is not a reason to abandon the task or move to an unrelated step.\n")
	b.WriteString("For implementation work, delegate the current plan step with `delegate_task`; the call returns control to you at once with a job id. Progress reports arrive automatically after every few completed worker tool calls - use each one for a quick scope check (over/under/off-target work): if wrong, call `delegate_stop` (it blocks until stopped) and then `delegate_message` with that job id and the corrected task; if correct, reply briefly and stop calling tools so the next report arrives on its own. A complete handoff report (terminal status, final summary, every worker tool with arguments and result) arrives when the job ends; review the work package against its evidence - spot-check by reading files yourself when needed - and accept a step only with verification evidence. Verify, don't trust - a worker loop ending is not verified success. Retry failed, blocked, or incomplete work with `delegate_message` in the same worker session, which returns a new job id whose reports arrive the same way; the same call orders new follow-on work into that session. Call `delegate_result` with verification evidence before delegating the next step. `delegate_stop` waits until the worker has actually stopped and returns its final report. `delegate_status` shows one job's state. The worker has a separate session and never communicates with the user.\n")
	b.WriteString("Only mark a step complete after verifying that its intended result is actually achieved. After the final goal is complete, stop and send the final result.\n")
	b.WriteString("After tool results, summarize briefly what you did. Match the user's language.\n")
	return b.String()
}

func resolveWorkspace() (string, error) {
	raw := strings.TrimSpace(os.Getenv("AI_WORKSPACE"))
	if raw == "" {
		home, err := os.UserHomeDir()
		if err != nil || strings.TrimSpace(home) == "" {
			return ".", nil
		}
		return home, nil
	}
	path := expandHome(raw)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", fmt.Errorf("AI_WORKSPACE: %w", err)
	}
	return path, nil
}

func expandHome(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
			return home
		}
		return path
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func envInt(name string) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return parsed, nil
}
