package tools

import (
	"ai-engine/provider"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"ai-engine/session"
)

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
