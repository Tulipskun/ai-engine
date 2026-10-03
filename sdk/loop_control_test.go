package sdk

import (
	"context"
	"errors"
	"github.com/Tulipskun/ai-engine/provider"
	"strings"
	"testing"
)

// REQ-045: the caps are fail-fast and fatal. A turn that stays under them must
// behave exactly as before, so these tests pin both sides of every cap.

func TestLoopControlAllowsTurnUnderToolCallCap(t *testing.T) {
	tracker := &loopControlTracker{}
	for i := 0; i < LoopControlMaxToolCallsPerTurn; i++ {
		if err := tracker.noteCall("bash"); err != nil {
			t.Fatalf("call %d within budget must not fail: %v", i+1, err)
		}
	}
	err := tracker.noteCall("bash")
	if !errors.Is(err, ErrLoopControlToolBudgetExceeded) {
		t.Fatalf("call %d over budget must fail with the budget error, got %v", LoopControlMaxToolCallsPerTurn+1, err)
	}
	if !strings.Contains(err.Error(), "requirements/loop-control.md") {
		t.Errorf("budget failure must name the lesson file, got %q", err)
	}
}

func TestLoopControlReadStallCapsConsecutiveReads(t *testing.T) {
	// A progress tool resets the streak, so reads interleaved with bash never
	// stall. Ten rounds stay well under the separate per-turn call cap.
	tracker := &loopControlTracker{}
	for i := 0; i < 10; i++ {
		if err := tracker.noteCall("read"); err != nil {
			t.Fatalf("read %d within budget must not fail: %v", i+1, err)
		}
		if err := tracker.noteCall("bash"); err != nil {
			t.Fatalf("progress tool must not fail: %v", err)
		}
	}

	stalled := &loopControlTracker{}
	for i := 0; i < LoopControlMaxConsecutiveReadEdit; i++ {
		if err := stalled.noteCall("read"); err != nil {
			t.Fatalf("read %d within budget must not fail: %v", i+1, err)
		}
	}
	if err := stalled.noteCall("read"); !errors.Is(err, ErrLoopControlReadEditStall) {
		t.Fatalf("read %d in a row must fail as a stall, got %v", LoopControlMaxConsecutiveReadEdit+1, err)
	}
}

func TestLoopControlCapsBashOutputPayload(t *testing.T) {
	tracker := &loopControlTracker{}
	small := provider.ToolResult{Content: strings.Repeat("x", LoopControlMaxBashOutputBytes-1)}
	if err := tracker.noteResult("bash", small); err != nil {
		t.Fatalf("payload under the cap must pass: %v", err)
	}
	big := provider.ToolResult{Content: strings.Repeat("x", LoopControlMaxBashOutputBytes+1)}
	if err := tracker.noteResult("bash", big); !errors.Is(err, ErrLoopControlBashOutputTooLarge) {
		t.Fatalf("oversized bash result must fail, got %v", err)
	}
}

func TestLoopControlBudgetFailuresAreNeverRetried(t *testing.T) {
	// Retrying a runaway loop only spends more provider calls, so the agent must
	// treat every budget failure as fatal.
	for _, err := range []error{
		ErrLoopControlToolBudgetExceeded,
		ErrLoopControlReadEditStall,
		ErrLoopControlBashOutputTooLarge,
	} {
		if !isLoopControlFatal(err) {
			t.Errorf("%v must be classified fatal", err)
		}
		if retryableAgentError(context.Background(), err) {
			t.Errorf("%v must not be retryable at the turn level", err)
		}
	}
	if isLoopControlFatal(errors.New("some other failure")) {
		t.Error("an unrelated error must not be classified as a loop-control failure")
	}
}
