package sdk

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestLoopControlToolBudgetCapsTurn(t *testing.T) {
	var tracker loopControlTracker
	var err error
	for i := 0; i < LoopControlMaxToolCallsPerTurn; i++ {
		if err = tracker.noteCall("bash"); err != nil {
			t.Fatalf("call %d under the cap must pass: %v", i+1, err)
		}
	}
	if err = tracker.noteCall("bash"); !errors.Is(err, ErrLoopControlToolBudgetExceeded) {
		t.Fatalf("call over the cap must fail with the budget error, got: %v", err)
	}
	if !isLoopControlFatal(err) {
		t.Fatal("budget error must be classified fatal (never retried)")
	}
}

func TestLoopControlReadEditStallFails(t *testing.T) {
	var tracker loopControlTracker
	var err error
	for i := 0; i < LoopControlMaxConsecutiveReadEdit; i++ {
		if err = tracker.noteCall("read"); err != nil {
			t.Fatalf("probe %d under the cap must pass: %v", i+1, err)
		}
	}
	if err = tracker.noteCall("read"); !errors.Is(err, ErrLoopControlReadEditStall) {
		t.Fatalf("consecutive read/edit over the cap must stall-fail, got: %v", err)
	}
	if !isLoopControlFatal(err) {
		t.Fatal("stall error must be classified fatal (never retried)")
	}
}

func TestLoopControlProgressResetsStall(t *testing.T) {
	var tracker loopControlTracker
	for i := 0; i < LoopControlMaxConsecutiveReadEdit; i++ {
		if err := tracker.noteCall("read"); err != nil {
			t.Fatal(err)
		}
	}
	// A progress tool (write/bash/anything non-read-edit) resets the streak.
	if err := tracker.noteCall("bash"); err != nil {
		t.Fatalf("progress tool must reset the stall counter: %v", err)
	}
	if err := tracker.noteCall("read"); err != nil {
		t.Fatalf("reads after progress must pass: %v", err)
	}
}

func TestLoopControlBashOutputCap(t *testing.T) {
	var tracker loopControlTracker
	ok := ToolResult{ID: "1", Content: "small output"}
	if err := tracker.noteResult("bash", ok); err != nil {
		t.Fatalf("small bash output must pass: %v", err)
	}
	big := ToolResult{ID: "2", Content: strings.Repeat("x", LoopControlMaxBashOutputBytes+1)}
	if err := tracker.noteResult("bash", big); !errors.Is(err, ErrLoopControlBashOutputTooLarge) {
		t.Fatalf("oversize bash output must fail, got: %v", err)
	}
}

func TestLoopControlOrdinaryErrorsStillRetryable(t *testing.T) {
	if isLoopControlFatal(errors.New("transient agent failure")) {
		t.Fatal("ordinary errors must stay retryable")
	}
	if !retryableAgentError(context.Background(), errors.New("transient agent failure")) {
		t.Fatal("ordinary errors must stay retryable at the turn level")
	}
	budget := &loopControlTracker{}
	_ = budget.noteCall("bash")
	fatal := budget.noteCall("bash")
	for fatal == nil {
		fatal = budget.noteCall("bash")
	}
	if !errors.Is(fatal, ErrLoopControlToolBudgetExceeded) {
		t.Fatalf("expected budget error, got: %v", fatal)
	}
	if retryableAgentError(context.Background(), fatal) {
		t.Fatal("budget failures must never be retried")
	}
}

func TestAgentRunawayTurnFailsFast(t *testing.T) {
	responses := make([]Response, LoopControlMaxToolCallsPerTurn+5)
	for i := range responses {
		responses[i] = Response{ToolCalls: []ToolCall{{ID: "echo", Name: "echo", Arguments: "{}"}}}
	}
	p := &agentTestProvider{responses: responses}
	c, s := newAgentTestSession(p)
	a := &Agent{Client: c, Tools: &agentTestTools{definitions: []Tool{{Name: "echo"}}}, MaxRetries: 0, DisablePlanning: true}
	_, err := a.RunTurn(context.Background(), s, Turn{Role: RoleUser}, Request{})
	if !errors.Is(err, ErrLoopControlToolBudgetExceeded) {
		t.Fatalf("runaway turn must fail with the budget error, got: %v", err)
	}
	if p.calls > LoopControlMaxToolCallsPerTurn+1 {
		t.Fatalf("turn kept looping past the cap: provider calls=%d", p.calls)
	}
}

func TestAgentNormalTurnUnaffectedByCaps(t *testing.T) {
	p := &agentTestProvider{responses: []Response{
		{ToolCalls: []ToolCall{{ID: "1", Name: "echo", Arguments: "{}"}}},
		{Content: []ContentPart{{Type: ContentText, Text: "finished"}}},
	}}
	c, s := newAgentTestSession(p)
	a := &Agent{Client: c, Tools: &agentTestTools{definitions: []Tool{{Name: "echo"}}}, MaxRetries: 0, DisablePlanning: true}
	resp, err := a.RunTurn(context.Background(), s, Turn{Role: RoleUser}, Request{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content[0].Text != "finished" {
		t.Fatalf("normal turn changed behavior: %#v", resp)
	}
}
