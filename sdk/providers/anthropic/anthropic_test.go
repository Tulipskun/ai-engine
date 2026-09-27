package anthropic

import (
	"github.com/Tulipskun/ai-engine/sdk"
	"testing"
)

func TestBuildUsesAnthropicAssistantRole(t *testing.T) {
	r := build(sdk.Request{Model: "test", MaxOutputTokens: 1000, Messages: []sdk.Turn{{Role: sdk.RoleModel, Content: []sdk.ContentPart{{Type: sdk.ContentText, Text: "hello"}}}}})
	msgs := r["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "assistant" {
		t.Fatal("model role must map to anthropic assistant")
	}
}

func TestClientHeadersMergeCustom(t *testing.T) {
	c := &Client{BaseURL: "https://example.invalid", APIKey: "k", APIVersion: "2023-06-01"}
	got := c.WithHeaders(map[string]string{"X-Title": "ai"}).(*Client).headers()
	if got["x-api-key"] != "k" || got["anthropic-version"] != "2023-06-01" || got["X-Title"] != "ai" {
		t.Fatalf("unexpected headers: %v", got)
	}
}
