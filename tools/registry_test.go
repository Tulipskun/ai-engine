package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tulipskun/ai-engine/sdk"
)

func TestRegistryDefinitions(t *testing.T) {
	r, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defs := r.Definitions()
	// read, bash and screen_control are the execution surface.
	if len(defs) != 3 {
		t.Fatalf("definitions=%d", len(defs))
	}
	if defs[0].Name != "bash" || defs[1].Name != "read" || defs[2].Name != "screen_control" {
		t.Fatalf("definitions=%q,%q want bash,read", defs[0].Name, defs[1].Name)
	}
}

func TestRegistryUnknownTool(t *testing.T) {
	r, _ := NewRegistry(t.TempDir())
	res := r.Execute(context.Background(), sdk.ToolCall{ID: "1", Name: "missing", Arguments: `{}`})
	if !res.IsError {
		t.Fatal("expected unknown tool error")
	}
}

func TestRunCommandDescriptionWarnsAboutDirectExec(t *testing.T) {
	r, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range r.Definitions() {
		if d.Name != "bash" {
			continue
		}
		for _, want := range []string{"bash command line", "chains", "pipes", "exit code"} {
			if !strings.Contains(d.Description, want) {
				t.Fatalf("bash description missing %q: %s", want, d.Description)
			}
		}
		return
	}
	t.Fatal("bash definition missing")
}

func TestWorkspaceResolverIsolatesSessionRoots(t *testing.T) {
	global := t.TempDir()
	sessionWorkspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(global, "shared.txt"), []byte("global file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionWorkspace, "session.txt"), []byte("session file"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry(global)
	if err != nil {
		t.Fatal(err)
	}
	r.SetWorkspaceResolver(func(ctx context.Context) string {
		if sdk.SessionIDFromContext(ctx) == "s2" {
			return sessionWorkspace
		}
		return ""
	})
	call := func(sessionID, name string, args any) sdk.ToolResult {
		raw, _ := json.Marshal(args)
		ctx := sdk.WithSessionID(context.Background(), sessionID)
		return r.Execute(ctx, sdk.ToolCall{ID: "x", Name: name, Arguments: string(raw)})
	}
	if result := call("s1", "read", map[string]string{"path": "shared.txt"}); result.IsError {
		t.Fatalf("global session lost global file: %s", result.Content)
	}
	if result := call("s2", "read", map[string]string{"path": "session.txt"}); result.IsError || result.Content != "session file" {
		t.Fatalf("session workspace not applied: %+v", result)
	}
	if result := call("s2", "read", map[string]string{"path": "shared.txt"}); !result.IsError {
		t.Fatal("session workspace still sees the global file")
	}
	bash := call("s2", "bash", map[string]string{"command": "pwd"})
	if bash.IsError || !strings.Contains(bash.Content, strings.TrimPrefix(sessionWorkspace, "/")) {
		t.Fatalf("bash cwd not session workspace: %+v", bash)
	}
	bashGlobal := call("s1", "bash", map[string]string{"command": "pwd"})
	if bashGlobal.IsError || !strings.Contains(bashGlobal.Content, strings.TrimPrefix(global, "/")) {
		t.Fatalf("bash cwd not global workspace: %+v", bashGlobal)
	}
}
