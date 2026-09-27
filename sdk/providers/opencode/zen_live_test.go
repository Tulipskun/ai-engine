package opencode_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Tulipskun/ai-engine/sdk"
	"github.com/Tulipskun/ai-engine/sdk/providers/opencode"
)

// TestZenFreeModelsLive asks every free model on the tier for a real turn, with
// the tool set a phone session actually offers. It costs quota, so it only runs
// when asked for:
//
//	AI_ZEN_KEY=<key> go test ./sdk/providers/opencode -run ZenFreeModelsLive
func TestZenFreeModelsLive(t *testing.T) {
	key := os.Getenv("AI_ZEN_KEY")
	if key == "" {
		t.Skip("set AI_ZEN_KEY to run the live Zen check")
	}
	client := opencode.New(key)
	tools := []sdk.Tool{}
	for _, name := range []string{"read", "bash"} {
		tools = append(tools, sdk.Tool{Name: name, Description: "Work on the device.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"a": map[string]any{"type": "string"}}}})
	}
	models, err := client.ListModels(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range models {
		if !strings.HasSuffix(model.ID, "-free") {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		start := time.Now()
		text, called, err := ask(ctx, client, model.ID, tools)
		cancel()
		if err != nil {
			t.Errorf("%-34s FAIL %v", model.ID, err)
			time.Sleep(5 * time.Second)
			continue
		}
		what := text
		if called {
			// A model that reaches for a tool has answered too; the probe does
			// not run tools, so there is nothing more to read.
			what = "(asked for a tool)"
		}
		t.Logf("%-34s OK  %-18s %q", model.ID, time.Since(start).Round(time.Millisecond), what)
		time.Sleep(5 * time.Second)
	}
}

func ask(ctx context.Context, client *opencode.Client, model string, tools []sdk.Tool) (string, bool, error) {
	stream, err := client.Stream(ctx, sdk.Request{
		Model:           model,
		SessionID:       "zen-live-" + model,
		Stream:          true,
		SystemPrompt:    "You are a coding agent working on a phone. Answer briefly.",
		Messages:        []sdk.Turn{{Role: sdk.RoleUser, Content: []sdk.ContentPart{{Type: sdk.ContentText, Text: "ping"}}}},
		MaxOutputTokens: 2048,
		Tools:           tools,
	})
	if err != nil {
		return "", false, err
	}
	var text string
	called := false
	for event := range stream {
		switch event.Type {
		case sdk.EventText:
			text += event.Text
		case sdk.EventToolCall:
			called = true
		case sdk.EventError:
			return text, called, event.Err
		}
	}
	return text, called, nil
}
