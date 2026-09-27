package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Tulipskun/ai-engine/sdk"
)

func sdkCall(name string, args any) sdk.ToolCall {
	b, _ := json.Marshal(args)
	return sdk.ToolCall{ID: "test", Name: name, Arguments: string(b)}
}

func TestReadToolStaysInsideTheWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	res := r.Execute(context.Background(), sdkCall("read", map[string]string{"path": "main.go"}))
	if res.IsError || res.Content != "one\ntwo\n" {
		t.Fatalf("read failed: %+v", res)
	}
	if res := r.Execute(context.Background(), sdkCall("read", map[string]string{"path": "../outside"})); !res.IsError {
		t.Fatal("expected traversal rejection")
	}
	if res := r.Execute(context.Background(), sdkCall("read", map[string]string{"path": "."})); !res.IsError {
		t.Fatal("expected a directory to be refused")
	}
}

func TestReadToolRejectsOversizedFile(t *testing.T) {
	root := t.TempDir()
	big := make([]byte, maxFileBytes+1)
	if err := os.WriteFile(filepath.Join(root, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	r, _ := NewRegistry(root)
	if res := r.Execute(context.Background(), sdkCall("read", map[string]string{"path": "big.bin"})); !res.IsError {
		t.Fatal("expected the byte limit to refuse the file")
	}
}
