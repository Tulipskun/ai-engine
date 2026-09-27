package sdk

import "context"

// ToolGuard may approve or deny a tool call immediately before execution.
type ToolGuard interface {
	Allow(context.Context, Turn, ToolCall) (bool, error)
}
