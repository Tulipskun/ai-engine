package session

import (
	"testing"

	"ai-engine/provider"
)

func TestTrailRoundTripsThroughTurns(t *testing.T) {
	trail := []provider.Message{
		{Role: "assistant", Content: "checking", ToolCalls: []provider.ToolCall{
			{ID: "a", Name: "current_time", Arguments: "{}"},
			{ID: "b", Name: "weather", Arguments: `{"city":"Bangkok"}`},
		}},
		{Role: "tool", ToolCallID: "a", ToolName: "current_time", Content: "10:00"},
		{Role: "tool", ToolCallID: "b", ToolName: "weather", Content: "hot"},
	}

	var turns []Turn
	for _, input := range TrailTurns(trail) {
		turns = append(turns, Turn{Role: input.Role, Text: input.Text})
	}
	if len(turns) != 4 {
		t.Fatalf("stored rows = %d, want 4", len(turns))
	}

	rebuilt := MessagesFromTurns(turns)
	// Each stored call becomes its own assistant message, followed by its result.
	if len(rebuilt) != 4 {
		t.Fatalf("rebuilt = %d messages, want 4", len(rebuilt))
	}
	if rebuilt[0].Content != "checking" || rebuilt[0].ToolCalls[0].ID != "a" {
		t.Errorf("first call lost its text or id: %+v", rebuilt[0])
	}
	if rebuilt[3].Role != "tool" || rebuilt[3].ToolCallID != "b" || rebuilt[3].Content != "hot" {
		t.Errorf("last result = %+v", rebuilt[3])
	}
}

func TestMessagesFromTurnsSkipsBrokenRows(t *testing.T) {
	rebuilt := MessagesFromTurns([]Turn{
		{Role: "tool_call", Text: "not json"},
		{Role: "tool_result", Text: `{"content":"orphan"}`},
		{Role: "user", Text: "still here"},
	})
	if len(rebuilt) != 1 || rebuilt[0].Content != "still here" {
		t.Errorf("rebuilt = %+v", rebuilt)
	}
}
