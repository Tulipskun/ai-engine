package anthropic

import (
	"testing"

	"github.com/Tulipskun/ai-engine/sdk"
)

func fp(v float64) *float64 { return &v }

func native(t *testing.T, req sdk.Request) map[string]any {
	t.Helper()
	return build(req)
}

// Anthropic refuses a temperature that is not the default once extended thinking
// is on, and it never accepts a budget below its floor or one that leaves no
// room for the answer. Sending them together used to produce a rejected request
// (CHANGE-077).
func TestThinkingAndTemperatureAreNotSentTogether(t *testing.T) {
	b := native(t, sdk.Request{
		Model:         "claude-sonnet-4",
		ThinkingLevel: sdk.ThinkingHigh,
		Temperature:   fp(0.3),
		TopP:          fp(0.8),
	})
	if _, present := b["temperature"]; present {
		t.Error("temperature must be left at its default while thinking is on")
	}
	thinking, _ := b["thinking"].(map[string]any)
	if thinking["type"] != "enabled" {
		t.Fatalf("thinking = %v", b["thinking"])
	}
	budget, _ := thinking["budget_tokens"].(int)
	if budget < minThinkingBudget {
		t.Errorf("budget_tokens = %d, below the %d floor", budget, minThinkingBudget)
	}
	maxTokens, _ := b["max_tokens"].(int)
	if budget >= maxTokens {
		t.Errorf("budget %d leaves no room inside max_tokens %d", budget, maxTokens)
	}
}

// Without thinking, temperature is an ordinary parameter.
func TestTemperatureIsSentWhenNotThinking(t *testing.T) {
	b := native(t, sdk.Request{Model: "claude-sonnet-4", Temperature: fp(0.3)})
	if b["temperature"] != 0.3 {
		t.Errorf("temperature = %v", b["temperature"])
	}
	if _, present := b["thinking"]; present {
		t.Error("thinking appeared without being asked for")
	}
}

// max_tokens is required by this endpoint and zero is not a valid value.
func TestMaxTokensIsNeverZero(t *testing.T) {
	for name, req := range map[string]sdk.Request{
		"plain":    {Model: "claude-sonnet-4"},
		"thinking": {Model: "claude-sonnet-4", ThinkingLevel: sdk.ThinkingLow},
	} {
		b := native(t, req)
		got, _ := b["max_tokens"].(int)
		if got <= 0 {
			t.Errorf("%s: max_tokens = %v", name, b["max_tokens"])
		}
	}
}

// A cap smaller than the thinking budget is widened rather than silently
// producing a request the provider will refuse.
func TestACapSmallerThanTheThinkingBudgetIsWidened(t *testing.T) {
	b := native(t, sdk.Request{
		Model:           "claude-sonnet-4",
		ThinkingLevel:   sdk.ThinkingHigh,
		MaxOutputTokens: 512,
	})
	budget, _ := b["thinking"].(map[string]any)["budget_tokens"].(int)
	maxTokens, _ := b["max_tokens"].(int)
	if budget >= maxTokens {
		t.Errorf("budget %d is not inside max_tokens %d", budget, maxTokens)
	}
	if maxTokens <= 512 {
		t.Errorf("max_tokens = %d, the caller's cap was not widened", maxTokens)
	}
}

func TestTheKnobsAnthropicTakesAreAllSent(t *testing.T) {
	b := native(t, sdk.Request{
		Model:         "claude-sonnet-4",
		Temperature:   fp(0.3),
		TopP:          fp(0.8),
		TopK:          fp(0.4),
		StopSequences: []string{"END", "HALT"},
	})
	if b["temperature"] != 0.3 || b["top_p"] != 0.8 || b["top_k"] != 0.4 {
		t.Errorf("sampling = %v / %v / %v", b["temperature"], b["top_p"], b["top_k"])
	}
	stop, _ := b["stop_sequences"].([]string)
	if len(stop) != 2 || stop[0] != "END" {
		t.Errorf("stop_sequences = %v", b["stop_sequences"])
	}
}

// Anthropic has no presence or frequency penalty, and seed is not a parameter
// here either; nothing invented should reach the wire.
func TestUnsetKnobsStayOffTheWire(t *testing.T) {
	b := native(t, sdk.Request{Model: "claude-sonnet-4"})
	for _, key := range []string{"top_p", "top_k", "stop_sequences", "presence_penalty", "frequency_penalty", "seed", "thinking"} {
		if _, present := b[key]; present {
			t.Errorf("%s was sent without being set", key)
		}
	}
}
