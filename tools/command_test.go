package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func staticRoot(path string) func(context.Context) string {
	return func(context.Context) string { return path }
}

func bashOutput(t *testing.T, workspace, command string) commandOutput {
	t.Helper()
	raw, err := json.Marshal(bashArgs{Command: command})
	if err != nil {
		t.Fatal(err)
	}
	data, err := bashTool(staticRoot(workspace))(context.Background(), raw)
	if err != nil {
		t.Fatalf("bash %q: %v (output %s)", command, err, data)
	}
	var out commandOutput
	if err := json.Unmarshal([]byte(data), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBashRunsMultiWordCommandDirectly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash availability is platform-specific")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := bashOutput(t, dir, "wc -l a.txt")
	if out.ExitCode != 0 || out.Output == "" {
		t.Fatalf("multi-word command must run through bash: %+v", out)
	}
	if out.Output[0] != '3' {
		t.Fatalf("unexpected wc output: %q", out.Output)
	}
}

func TestBashChainPipeRedirect(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash syntax is platform-specific")
	}
	dir := t.TempDir()
	out := bashOutput(t, dir, "echo chain-a && echo chain-b | tr a-z A-Z > out.txt && cat out.txt")
	if out.ExitCode != 0 {
		t.Fatalf("chain failed: %+v", out)
	}
	if out.Output != "chain-a\nCHAIN-B" {
		t.Fatalf("unexpected combined result: %+v", out)
	}
}

func TestBashGlob(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out := bashOutput(t, dir, "ls *.go")
	if out.Output != "x.go" {
		t.Fatalf("globs must work: %+v", out)
	}
}

func TestBashNonZeroExitReports(t *testing.T) {
	raw, _ := json.Marshal(bashArgs{Command: "false"})
	_, err := bashTool(staticRoot(t.TempDir()))(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
}

func TestBashTimeoutKillsProcessTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group behavior is platform-specific")
	}
	raw, _ := json.Marshal(bashArgs{Command: "sleep 5", TimeoutMS: 100})
	start := time.Now()
	_, err := bashTool(staticRoot(t.TempDir()))(context.Background(), raw)
	if elapsed := time.Since(start); err == nil || elapsed > 2*time.Second {
		t.Fatalf("timeout did not kill promptly: err=%v elapsed=%s", err, elapsed)
	}
}

func TestBashRejectsEmptyCommand(t *testing.T) {
	if _, err := bashTool(staticRoot(t.TempDir()))(context.Background(), []byte(`{"command":"  "}`)); err == nil {
		t.Fatal("empty command accepted")
	}
}
