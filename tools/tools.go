package tools

import (
	"ai-engine/provider"
	"ai-engine/session"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---- from tools/registry.go ----
type handler func(context.Context, json.RawMessage) (string, error)

type Registry struct {
	workspace string
	resolver  func(context.Context) string
	handlers  map[string]handler
	defs      []provider.Tool
}

// NewRegistry builds the worker's execution tool surface: read and bash. All
// delegated-agent orchestration is owned by the SDK planner, not this registry.
func NewRegistry(workspace string) (*Registry, error) {
	root, err := workspaceRoot(workspace)
	if err != nil {
		return nil, err
	}
	r := &Registry{workspace: root, handlers: make(map[string]handler)}
	r.register("read", `Read a UTF-8 text file inside the workspace. Named to match the OpenCode client's read tool, so a provider that only offers the client's own tool set still reaches this tool by its own name.`, readFileTool(r.rootFor), map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []string{"path"}})
	r.register("bash", `Run a bash command line synchronously in the workspace. Type shell exactly as you would in a terminal: chains (&&, ||, ;), pipes, redirects, globs, quoting and multi-line all work. Returns combined stdout/stderr and the exit code. This is the only execution tool: create, edit, search and delete files (printf, sed, python3, ripgrep), run builds and tests, and inspect git — all through the shell.`, bashTool(r.rootFor), map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}, "timeout_ms": map[string]any{"type": "integer"}}, "required": []string{"command"}})
	return r, nil
}

// SetWorkspaceResolver installs a per-invocation workspace lookup used by
// every path-sensitive tool. A session with its own workspace (REQ-038)
// resolves through ctx; anything else keeps the process default.
func (r *Registry) SetWorkspaceResolver(resolver func(context.Context) string) {
	if r != nil {
		r.resolver = resolver
	}
}

func (r *Registry) rootFor(ctx context.Context) string {
	if ctx != nil {
		if ws := session.WorkspaceFromContext(ctx); ws != "" {
			if cleaned, err := filepathAbsClean(ws); err == nil {
				return cleaned
			}
		}
	}
	if r != nil && r.resolver != nil {
		if root := strings.TrimSpace(r.resolver(ctx)); root != "" {
			if cleaned, err := filepathAbsClean(root); err == nil {
				return cleaned
			}
		}
	}
	return r.workspace
}

func (r *Registry) register(name, description string, fn handler, schema any) {
	r.handlers[name] = fn
	r.defs = append(r.defs, provider.Tool{Name: name, Description: description, InputSchema: schema})
}
func (r *Registry) Definitions() []provider.Tool {
	out := append([]provider.Tool(nil), r.defs...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
func (r *Registry) Execute(ctx context.Context, call provider.ToolCall) provider.ToolResult {
	result := provider.ToolResult{ID: call.ID}
	fn, ok := r.handlers[call.Name]
	if !ok {
		result.Content = fmt.Sprintf("unknown tool: %s", call.Name)
		result.IsError = true
		return result
	}
	if call.Arguments == "" {
		call.Arguments = "{}"
	}
	if !json.Valid([]byte(call.Arguments)) {
		result.Content = "invalid tool arguments JSON"
		result.IsError = true
		return result
	}
	content, err := fn(ctx, json.RawMessage(call.Arguments))
	if err != nil {
		result.Content = err.Error()
		result.IsError = true
		return result
	}
	result.Content = content
	return result
}
func workspaceRoot(workspace string) (string, error) {
	if workspace == "" {
		return "", errors.New("tools: workspace is required")
	}
	return filepathAbsClean(workspace)
}

// ---- from tools/files.go ----
const maxFileBytes = 4 << 20

func filepathAbsClean(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("tools: workspace is not a directory")
	}
	return abs, nil
}

func safePath(root, name string) (string, error) {
	if name == "" {
		name = "."
	}
	candidate := name
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	abs, err := filepath.Abs(filepath.Clean(candidate))
	if err != nil {
		return "", err
	}
	root, err = filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", err
	}
	if !withinRoot(root, abs) {
		return "", fmt.Errorf("path escapes workspace: %q", name)
	}
	resolved, err := resolveExistingPath(abs)
	if err != nil {
		return "", err
	}
	if !withinRoot(root, resolved) {
		return "", fmt.Errorf("path escapes workspace through symlink: %q", name)
	}
	return abs, nil
}

func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func resolveExistingPath(path string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Abs(resolved)
	}
	current := path
	var missing []string
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			resolved, err = filepath.Abs(resolved)
			if err != nil {
				return "", err
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("cannot resolve path: %q", path)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

type readFileArgs struct {
	Path string `json:"path"`
}

func readFileTool(rootAt func(context.Context) string) handler {
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		root := rootAt(ctx)
		var args readFileArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", err
		}
		path, err := safePath(root, args.Path)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		if info.IsDir() {
			return "", errors.New("path is a directory")
		}
		if info.Size() > maxFileBytes {
			return "", fmt.Errorf("file exceeds %d byte limit", maxFileBytes)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
}

// ---- from tools/command.go ----
const defaultCommandTimeout = 30 * time.Second
const maxCommandOutputBytes = 1 << 20

type commandOutput struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
}

type bashArgs struct {
	Command   string `json:"command"`
	TimeoutMS int    `json:"timeout_ms"`
}

// bashTool runs a full bash command line directly in the workspace so the
// model can type shell the way a human would (CHANGE-024, REQ-035).
func bashTool(rootAt func(context.Context) string) handler {
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		workspace := rootAt(ctx)
		var args bashArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", err
		}
		if strings.TrimSpace(args.Command) == "" {
			return "", errors.New("command is required")
		}

		runCtx := ctx
		cancel := func() {}
		if args.TimeoutMS > 0 {
			runCtx, cancel = context.WithTimeout(ctx, time.Duration(args.TimeoutMS)*time.Millisecond)
		} else {
			runCtx, cancel = context.WithTimeout(ctx, defaultCommandTimeout)
		}
		defer cancel()

		cmd := shellCommand(args.Command)
		cmd.Dir = workspace
		configureCommandProcess(cmd)

		var out limitedCommandBuffer
		cmd.Stdout, cmd.Stderr = &out, &out

		if err := cmd.Start(); err != nil {
			return "", err
		}

		waitCh := make(chan error, 1)
		go func() { waitCh <- cmd.Wait() }()

		var err error
		select {
		case err = <-waitCh:
		case <-runCtx.Done():
			killCommandProcessTree(cmd)
			err = <-waitCh
		}

		code := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
			} else {
				code = -1
			}
		}
		result := commandOutput{Output: out.String(), ExitCode: code}
		data, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			return "", marshalErr
		}
		if err != nil {
			if runCtx.Err() != nil {
				return string(data), fmt.Errorf("command %s: %w", args.Command, runCtx.Err())
			}
			return string(data), fmt.Errorf("command %s failed: %w", args.Command, err)
		}
		return string(data), nil
	}
}

type limitedCommandBuffer struct{ data []byte }

func (b *limitedCommandBuffer) Write(p []byte) (int, error) {
	if len(b.data) < maxCommandOutputBytes {
		n := len(p)
		if len(b.data)+n > maxCommandOutputBytes {
			n = maxCommandOutputBytes - len(b.data)
		}
		b.data = append(b.data, p[:n]...)
	}
	return len(p), nil
}

func (b *limitedCommandBuffer) String() string { return strings.TrimSpace(string(b.data)) }
