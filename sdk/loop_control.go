package sdk

import (
	"errors"
	"fmt"
	"github.com/Tulipskun/ai-engine/provider"
)

// Hard loop-control caps (REQ-045). These are system-level fail-fast limits
// that sit underneath prompt discipline: when any cap is exceeded the turn
// fails immediately with a clear error instead of looping forever. Normal
// turns that stay under the limits behave exactly as before.
const (
	// LoopControlMaxToolCallsPerTurn caps executed tool calls per attempt.
	// The slide-window incident burned 47 tools (sed loops + test drift);
	// this cap stops that class of runaway well before that point while
	// leaving ordinary turns (single-digit calls) untouched.
	LoopControlMaxToolCallsPerTurn = 30
	// LoopControlMaxConsecutiveReadEdit caps consecutive read calls without
	// an intervening progress tool (bash, or anything else). CHANGE-087: read
	// is the only remaining probe tool, so eight reads in a row is the stall
	// signature it catches.
	LoopControlMaxConsecutiveReadEdit = 8
	// LoopControlMaxBashOutputBytes caps a single bash tool result payload.
	// It mirrors the tools/command.go truncation budget (1 MiB) so the
	// loop layer and the tool layer agree on what "too large" means.
	LoopControlMaxBashOutputBytes = 1 << 20
)

var (
	ErrLoopControlToolBudgetExceeded = errors.New("sdk: loop-control tool budget exceeded")
	ErrLoopControlReadEditStall      = errors.New("sdk: loop-control consecutive read/edit stall")
	ErrLoopControlBashOutputTooLarge = errors.New("sdk: loop-control bash output too large")
)

// loopControlTracker counts tool executions inside one turn attempt.
type loopControlTracker struct {
	total       int
	consecutive int
}

func isLoopControlReadEditTool(name string) bool {
	switch name {
	case "read":
		return true
	default:
		return false
	}
}

// noteCall records one executed tool call and fails when the per-turn
// budget is exceeded. Call it for every tool execution in order.
func (t *loopControlTracker) noteCall(name string) error {
	if t == nil {
		return nil
	}
	t.total++
	if t.total > LoopControlMaxToolCallsPerTurn {
		return fmt.Errorf("%w: %d calls (max %d); stop, report failure, and write a lesson per requirements/loop-control.md",
			ErrLoopControlToolBudgetExceeded, t.total, LoopControlMaxToolCallsPerTurn)
	}
	if isLoopControlReadEditTool(name) {
		t.consecutive++
		if t.consecutive > LoopControlMaxConsecutiveReadEdit {
			return fmt.Errorf("%w: %d consecutive read/edit calls without progress (max %d); stop, report failure, and write a lesson per requirements/loop-control.md",
				ErrLoopControlReadEditStall, t.consecutive, LoopControlMaxConsecutiveReadEdit)
		}
		return nil
	}
	t.consecutive = 0
	return nil
}

// noteResult enforces the per-result bash output cap. Non-bash tools and
// payloads under the limit are untouched.
func (t *loopControlTracker) noteResult(name string, result provider.ToolResult) error {
	if len(result.Content) > LoopControlMaxBashOutputBytes && name == "bash" {
		return fmt.Errorf("%w: %d bytes (max %d); narrow the command, redirect to a file, or page the output",
			ErrLoopControlBashOutputTooLarge, len(result.Content), LoopControlMaxBashOutputBytes)
	}
	if len(result.Content) > LoopControlMaxBashOutputBytes {
		return fmt.Errorf("%w: tool %q returned %d bytes (max %d)",
			ErrLoopControlBashOutputTooLarge, name, len(result.Content), LoopControlMaxBashOutputBytes)
	}
	return nil
}

// isLoopControlFatal reports whether err is a loop-control budget failure.
// Budget failures must not be retried: retrying a runaway loop only burns
// more provider calls (REQ-045).
func isLoopControlFatal(err error) bool {
	return errors.Is(err, ErrLoopControlToolBudgetExceeded) ||
		errors.Is(err, ErrLoopControlReadEditStall) ||
		errors.Is(err, ErrLoopControlBashOutputTooLarge)
}
