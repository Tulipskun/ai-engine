package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Tulipskun/ai-engine/sdk"
)

type handler func(context.Context, json.RawMessage) (string, error)

type Registry struct {
	workspace string
	resolver  func(context.Context) string
	handlers  map[string]handler
	defs      []sdk.Tool
}

// NewRegistry builds the worker's whole tool surface: read and bash. CHANGE-087
// retired every other tool (file writes/edits, listing, search, background jobs,
// web fetch, OS input, attachments, browser), so a turn reaches the workspace
// through these two — read for content, bash for everything else.
func NewRegistry(workspace string) (*Registry, error) {
	root, err := workspaceRoot(workspace)
	if err != nil {
		return nil, err
	}
	r := &Registry{workspace: root, handlers: make(map[string]handler)}
	r.register("read", `Read a UTF-8 text file inside the workspace. Named to match the OpenCode client's read tool, so a provider that only offers the client's own tool set still reaches this tool by its own name.`, readFileTool(r.rootFor), map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []string{"path"}})
	r.register("bash", `Run a bash command line synchronously in the workspace. Type shell exactly as you would in a terminal: chains (&&, ||, ;), pipes, redirects, globs, quoting and multi-line all work. Returns combined stdout/stderr and the exit code. This is the only execution tool: create, edit, search and delete files (printf, sed, python3, ripgrep), run builds and tests, and inspect git — all through the shell.`, bashTool(r.rootFor), map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}, "timeout_ms": map[string]any{"type": "integer"}}, "required": []string{"command"}})
	r.register("screen_control", "Delegate screen control to a separately running local JEV runtime. Main Agent or Sub-agent invokes JEV when pointer/screen control is needed; ai-engine and AIxodia do not execute the screen action.", screenControlTool(), map[string]any{"type": "object", "properties": map[string]any{"goal": map[string]any{"type": "string"}}, "required": []string{"goal"}})
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
		if ws := sdk.WorkspaceFromContext(ctx); ws != "" {
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
	r.defs = append(r.defs, sdk.Tool{Name: name, Description: description, InputSchema: schema})
}
func (r *Registry) Definitions() []sdk.Tool {
	out := append([]sdk.Tool(nil), r.defs...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
func (r *Registry) Execute(ctx context.Context, call sdk.ToolCall) sdk.ToolResult {
	result := sdk.ToolResult{ID: call.ID}
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
