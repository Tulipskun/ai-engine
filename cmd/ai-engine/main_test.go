package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tulipskun/ai-engine/sdk"
)

func TestNewAgentWithTools(t *testing.T) {
	client := sdk.NewRouterClient(sdk.NewRouter())
	agent, err := newAgent(client, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if agent == nil || agent.Client != client || agent.Tools == nil {
		t.Fatal("agent wiring is incomplete")
	}
}

func TestStateRootDefaultsToLocalShare(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory")
	}
	got, err := stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".local", "share", "ai"); got != want {
		t.Fatalf("stateRoot = %q, want %q", got, want)
	}
}

func TestResolveWorkspaceDefaultsToHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory")
	}
	if got, err := resolveWorkspace(); err != nil || got != home {
		t.Fatalf("resolveWorkspace = %q, %v", got, err)
	}
}

func TestResolveWorkspaceConfiguredDir(t *testing.T) {
	target := filepath.Join(t.TempDir(), "my-project")
	if err := os.Setenv("AI_WORKSPACE", target); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv("AI_WORKSPACE")
	got, err := resolveWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	if got != target {
		t.Fatalf("resolveWorkspace = %q, want %q", got, target)
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Fatalf("workspace dir was not created: %v", err)
	}
}

func TestDefaultSystemPromptUsesPlanAndSubagent(t *testing.T) {
	client := sdk.NewRouterClient(sdk.NewRouter())
	agent := &sdk.Agent{Client: client, Tools: &testPromptTools{}}
	got := defaultSystemPrompt(agent)
	for _, forbidden := range []string{"gets things done with tools", "CALL the matching tool", "Prefer acting first", "write_file", "edit_file", "Available tools:", "call the tool in the SAME response", "Execute only the current step"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("main default contains execution instruction %q: %s", forbidden, got)
		}
	}
	for _, want := range []string{"Before creating the plan", "ordered execution plan", "delegate the current plan step to `delegate_to_subagent`", "Call `accept_subagent_result` with verification evidence before delegating the next step", "your one read tool", "engineering contract", "Required evidence", "Verify, don't trust"} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt missing %q", want)
		}
	}
}

type testPromptTools struct{}

func (testPromptTools) Definitions() []sdk.Tool {
	return []sdk.Tool{{Name: "bash", Description: "run command"}}
}
func (testPromptTools) Execute(_ context.Context, _ sdk.ToolCall) sdk.ToolResult {
	return sdk.ToolResult{}
}

func TestDefaultPromptRequiresSameSessionRetryAndAcceptance(t *testing.T) {
	prompt := defaultSystemPrompt(nil)
	for _, name := range []string{"delegate_to_subagent", "follow_up_subagent", "continue_subagent", "accept_subagent_result", "stop_subagent"} {
		if !strings.Contains(prompt, "`"+name+"`") {
			t.Fatalf("missing orchestration tool %s", name)
		}
	}
}

func TestDefaultPromptRequiresProportionalEffort(t *testing.T) {
	prompt := defaultSystemPrompt(nil)
	for _, want := range []string{"only when the task needs repository context", "skip that investigation", "fewest steps", "minimal sufficient check"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("default prompt missing %q", want)
		}
	}
}

func TestDefaultSystemPromptShortCircuitsNonTasks(t *testing.T) {
	prompt := defaultSystemPrompt(nil)
	for _, want := range []string{"needs no tools, no repository context, and no task to complete", "do not create a plan, do not delegate, do not investigate"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("main default missing non-task short-circuit %q", want)
		}
	}
}

// The instruction files are found the way the client finds them: the global one
// first, then AGENTS.md from the working directory upwards.
func TestInstructionFilesFollowTheClientOrder(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	global := filepath.Join(root, ".config", "opencode", "AGENTS.md")
	if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(global), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, text := range map[string]string{
		global:                           "global rules",
		filepath.Join(root, "AGENTS.md"): "rules above the workspace",
		filepath.Join(repo, "AGENTS.md"): "project rules",
	} {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("AI_WORKSPACE", repo)
	t.Setenv("HOME", root)

	var got []string
	for _, f := range instructionFiles() {
		got = append(got, f.Path+"="+f.Text)
	}
	want := []string{
		global + "=global rules",
		filepath.Join(repo, "AGENTS.md") + "=project rules",
		filepath.Join(root, "AGENTS.md") + "=rules above the workspace",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("instruction files:\n got %v\nwant %v", got, want)
	}
}

func TestNewMobileRuntimeBootsWithoutCloudflareAPIOverride(t *testing.T) {
	rt, err := newMobileRuntime(t.TempDir(), t.TempDir(), runtimeMobileConfig{
		d1Database: "aixodia",
		listen:     "127.0.0.1:0",
	}, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if rt == nil || rt.client == nil {
		t.Fatal("secretless boot must build a runtime, not (nil, nil) (REQ-046(3))")
	}
}
