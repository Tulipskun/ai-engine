package sdk

import "context"

type sessionContextKey struct{}

func WithSessionID(ctx context.Context, sessionID string) context.Context {
	if sessionID == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionContextKey{}, sessionID)
}

func SessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(sessionContextKey{}).(string)
	return value
}

type workspaceContextKey struct{}

// WithWorkspace pins the working directory for tool executions derived from
// ctx. Worker jobs stamp it from their parent session so per-channel
// workspaces survive delegation (REQ-038).
func WithWorkspace(ctx context.Context, workspace string) context.Context {
	if workspace == "" {
		return ctx
	}
	return context.WithValue(ctx, workspaceContextKey{}, workspace)
}

func WorkspaceFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(workspaceContextKey{}).(string)
	return value
}
