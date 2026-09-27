package sdk

import "testing"

func TestCanonicalToolHistory(t *testing.T) {
	req := Request{SystemPrompt: "You are a coding agent.", Messages: []Turn{
		{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: "run pwd"}}},
		{Role: RoleToolCall, ToolCall: &ToolCall{ID: "call-1", Name: "bash", Arguments: `{"command":"pwd"}`}},
		{Role: RoleToolResult, ToolResult: &ToolResult{ID: "call-1", Content: "/workspace"}},
	}, ThinkingLevel: ThinkingMedium, Stream: true}
	if req.Messages[1].ToolCall.ID != req.Messages[2].ToolResult.ID {
		t.Fatal("tool call/result IDs must match")
	}
	if !req.Stream || req.ThinkingLevel != ThinkingMedium {
		t.Fatal("request controls were not preserved")
	}
}
