package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/Tulipskun/ai-engine/sdk"
	"github.com/Tulipskun/ai-engine/sdk/providers/openai"
)

func TestBuildFromOpenAIMatchesBuild(t *testing.T) {
	temp := 0.7
	req := sdk.Request{Model: "m", SystemPrompt: "sys", Temperature: &temp, ThinkingLevel: sdk.ThinkingHigh, MaxOutputTokens: 100, Messages: []sdk.Turn{
		{Role: sdk.RoleUser, Content: []sdk.ContentPart{{Type: sdk.ContentText, Text: "hi"}}},
		{Role: sdk.RoleToolCall, ToolCall: &sdk.ToolCall{ID: "c1", Name: "bash", Arguments: `{"command":"pwd"}`}},
		{Role: sdk.RoleToolResult, ToolResult: &sdk.ToolResult{ID: "c1", Content: "/tmp"}},
	}, Tools: []sdk.Tool{{Name: "bash", Description: "run", InputSchema: map[string]any{"type": "object"}}}}
	direct := build(req)
	viaCanonical := BuildFromOpenAI(openai.BuildResponsesRequest(req))
	if len(direct["messages"].([]any)) != len(viaCanonical["messages"].([]any)) {
		t.Fatalf("messages diverged: %#v vs %#v", direct["messages"], viaCanonical["messages"])
	}
	if direct["system"] != viaCanonical["system"] {
		t.Fatalf("system diverged: %v vs %v", direct["system"], viaCanonical["system"])
	}
}

func TestToOpenAIResponseKeepsProvider(t *testing.T) {
	var r response
	r.Model = "m2"
	r.StopReason = "stop"
	r.Content = append(r.Content, struct {
		Type, Text, ID, Name string
		Input                json.RawMessage `json:"input"`
	}{Type: "text", Text: "hi"})
	got := ToOpenAIResponse(r)
	if got.Provider != "anthropic" || len(got.Content) != 1 || got.Content[0].Text != "hi" {
		t.Fatalf("unexpected canonical response: %+v", got)
	}
}
