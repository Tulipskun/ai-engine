package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tulipskun/ai-engine/sdk"
)

// safePath is the workspace boundary every surviving tool relies on (REQ-045 /
// CHANGE-087): a model-supplied path may not leave the root, by traversal or
// through a symlink.

func TestSafePathAcceptsPathsInsideTheRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.md"), []byte("# map\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sdk"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"index.md", "sdk/agent.go", "./index.md", "sdk/../index.md", "."} {
		if _, err := safePath(root, name); err != nil {
			t.Errorf("safePath(%q) must be accepted: %v", name, err)
		}
	}
}

func TestSafePathRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../secret.txt", "../../etc/passwd", "sdk/../../secret.txt"} {
		got, err := safePath(root, name)
		if err == nil {
			t.Errorf("safePath(%q) must be rejected, got %q", name, got)
			continue
		}
		if !strings.Contains(err.Error(), "escapes workspace") {
			t.Errorf("safePath(%q) must say why, got %v", name, err)
		}
	}
}

func TestSafePathRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The target is outside the root and the link sits inside it, so only the
	// symlink resolution catches this.
	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got, err := safePath(root, "link.txt")
	if err == nil {
		t.Fatalf("a symlink out of the workspace must be rejected, got %q", got)
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("the error must name the symlink escape, got %v", err)
	}
}

func TestSafePathRejectsAnAbsolutePathOutsideTheRoot(t *testing.T) {
	root := t.TempDir()
	if _, err := safePath(root, "/etc/passwd"); err == nil {
		t.Error("an absolute path outside the workspace must be rejected")
	}
}

func TestReadToolRefusesToEscapeAndReadsWhatItShould(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.md"), []byte("# map\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(root)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	ctx := context.Background()

	ok := registry.Execute(ctx, sdk.ToolCall{ID: "1", Name: "read", Arguments: `{"path":"index.md"}`})
	if ok.IsError {
		t.Fatalf("reading a file inside the workspace must work: %q", ok.Content)
	}
	if ok.Content != "# map\n" {
		t.Errorf("unexpected content %q", ok.Content)
	}

	escaped := registry.Execute(ctx, sdk.ToolCall{ID: "2", Name: "read", Arguments: `{"path":"../escape.txt"}`})
	if !escaped.IsError {
		t.Error("reading outside the workspace must fail")
	}
}

func TestRegistryRejectsUnknownToolsAndBadArguments(t *testing.T) {
	registry, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	ctx := context.Background()

	if res := registry.Execute(ctx, sdk.ToolCall{ID: "1", Name: "nope"}); !res.IsError {
		t.Error("an unknown tool must be an error, not a silent success")
	}
	if res := registry.Execute(ctx, sdk.ToolCall{ID: "2", Name: "read", Arguments: "{not json"}); !res.IsError {
		t.Error("malformed tool arguments must be an error")
	}
}

// The worker's whole tool surface is read plus bash (CHANGE-087). A tool showing
// up here that the requirements retired is a scope regression.
func TestRegistrySurfaceIsExactlyReadAndBash(t *testing.T) {
	registry, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	got := map[string]bool{}
	for _, def := range registry.Definitions() {
		got[def.Name] = true
	}
	for _, want := range []string{"read", "bash"} {
		if !got[want] {
			t.Errorf("the worker surface must include %q", want)
		}
	}
	for _, retired := range []string{"screen_control", "write", "edit", "grep", "glob"} {
		if got[retired] {
			t.Errorf("the worker surface must not include the retired tool %q", retired)
		}
	}
	if len(got) != 2 {
		t.Errorf("the worker surface must be exactly read+bash, got %v", got)
	}
}
