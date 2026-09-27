package sdk

import (
	"strings"
	"testing"
)

func TestBuildContextWindowKeepsCompleteInteractionGroups(t *testing.T) {
	turns := []Turn{
		{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: strings.Repeat("a", 80)}}},
		{Role: RoleModel, Content: []ContentPart{{Type: ContentText, Text: strings.Repeat("b", 80)}}},
		{Role: RoleToolCall, ToolCall: &ToolCall{ID: "old", Name: "search", Arguments: strings.Repeat("c", 80)}},
		{Role: RoleToolResult, ToolResult: &ToolResult{ID: "old", Content: strings.Repeat("d", 80)}},
		{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: strings.Repeat("e", 80)}}},
		{Role: RoleModel, Content: []ContentPart{{Type: ContentText, Text: strings.Repeat("f", 80)}}},
		{Role: RoleToolCall, ToolCall: &ToolCall{ID: "new", Name: "search", Arguments: strings.Repeat("g", 80)}},
		{Role: RoleToolResult, ToolResult: &ToolResult{ID: "new", Content: strings.Repeat("h", 80)}},
	}

	window := buildContextWindow(turns, 100)
	if len(window) != 4 {
		t.Fatalf("window turns=%d, want 4", len(window))
	}
	if window[0].Role != RoleUser || window[0].Content[0].Text[0] != 'e' {
		t.Fatalf("old interaction was not evicted: %#v", window)
	}
	if window[1].Role != RoleModel || window[2].Role != RoleToolCall || window[3].Role != RoleToolResult {
		t.Fatalf("new interaction was split: %#v", window)
	}
}

func TestBuildContextWindowKeepsOversizedNewestGroupIntact(t *testing.T) {
	turns := []Turn{
		{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: "old"}}},
		{Role: RoleModel, Content: []ContentPart{{Type: ContentText, Text: "old response"}}},
		{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: strings.Repeat("x", 1000)}}},
		{Role: RoleModel, Reasoning: &ReasoningState{ID: "r1", Text: strings.Repeat("r", 1000)}},
		{Role: RoleToolCall, ToolCall: &ToolCall{ID: "c1", Name: "search", Arguments: strings.Repeat("a", 1000)}},
		{Role: RoleToolResult, ToolResult: &ToolResult{ID: "c1", Content: strings.Repeat("b", 1000)}},
	}

	window := buildContextWindow(turns, 10)
	if len(window) != 4 {
		t.Fatalf("oversized newest group was split: %d turns", len(window))
	}
	if window[0].Role != RoleUser || window[1].Reasoning == nil || window[2].ToolCall == nil || window[3].ToolResult == nil {
		t.Fatalf("incomplete newest group: %#v", window)
	}
}
