package gemini

import (
	"testing"

	"github.com/Tulipskun/ai-engine/sdk"
	"github.com/Tulipskun/ai-engine/sdk/providers/openai"
)

func TestBuildFromOpenAIMatchesBuild(t *testing.T) {
	temp := 0.5
	req := sdk.Request{Model: "m", SystemPrompt: "sys", Temperature: &temp, ThinkingLevel: sdk.ThinkingMedium, MaxOutputTokens: 200, Messages: []sdk.Turn{
		{Role: sdk.RoleModel, Content: []sdk.ContentPart{{Type: sdk.ContentText, Text: "calling"}}},
		{Role: sdk.RoleToolCall, ToolCall: &sdk.ToolCall{ID: "c1", Name: "bash", Arguments: `{"command":"pwd"}`}},
		{Role: sdk.RoleToolResult, ToolResult: &sdk.ToolResult{ID: "c1", Content: "/tmp"}},
	}, Tools: []sdk.Tool{{Name: "bash", Description: "run", InputSchema: map[string]any{"type": "object"}}}}
	direct := build(req)
	viaCanonical := BuildFromOpenAI(openai.BuildResponsesRequest(req))
	if len(direct["contents"].([]any)) != len(viaCanonical["contents"].([]any)) {
		t.Fatalf("contents diverged: %#v vs %#v", direct["contents"], viaCanonical["contents"])
	}
}

func TestToOpenAIResponseKeepsProvider(t *testing.T) {
	var r response
	r.Candidates = []candidate{{FinishReason: "STOP"}}
	r.Candidates[0].Content.Parts = []part{{Text: "hello"}}
	r.Usage.Prompt = 1
	r.Usage.Output = 2
	r.Usage.Total = 3
	got := ToOpenAIResponse(r, "m")
	if got.Provider != "gemini" || len(got.Content) != 1 || got.Content[0].Text != "hello" {
		t.Fatalf("unexpected canonical response: %+v", got)
	}
	if got.Usage.InputTokens != 1 || got.Usage.OutputTokens != 2 || got.Usage.TotalTokens != 3 {
		t.Fatalf("unexpected usage: %+v", got.Usage)
	}
}
