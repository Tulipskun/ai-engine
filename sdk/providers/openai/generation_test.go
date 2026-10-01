package openai

import (
	"testing"

	"github.com/Tulipskun/ai-engine/sdk"
)

func fp(v float64) *float64 { return &v }
func ip(v int64) *int64     { return &v }

// The dialect decides which knobs can be sent. Getting this wrong does not show
// up as a wrong answer, it shows up as a refused request (CHANGE-077).

func TestResponsesSendsTheKnobsOpenAITakes(t *testing.T) {
	b := BuildResponsesRequest(sdk.Request{
		Model:           "gpt-5",
		Temperature:     fp(0.4),
		TopP:            fp(0.9),
		ThinkingLevel:   sdk.ThinkingHigh,
		MaxOutputTokens: 2048,
	})
	if got := b["temperature"]; got != 0.4 {
		t.Errorf("temperature = %v", got)
	}
	if got := b["top_p"]; got != 0.9 {
		t.Errorf("top_p = %v", got)
	}
	if got := b["max_output_tokens"]; got != 2048 {
		t.Errorf("max_output_tokens = %v", got)
	}
	reasoning, _ := b["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" {
		t.Errorf("reasoning = %v", b["reasoning"])
	}
}

// top_k, stop, the penalties and seed are not /responses parameters. They are
// kept off this body on purpose, because a key OpenAI does not know is a
// rejected request rather than a carried setting.
func TestResponsesDoesNotSendKnobsOpenAIHasNoPlaceFor(t *testing.T) {
	b := BuildResponsesRequest(sdk.Request{
		Model:            "gpt-5",
		TopK:             fp(0.5),
		StopSequences:    []string{"END"},
		PresencePenalty:  fp(0.5),
		FrequencyPenalty: fp(0.5),
		Seed:             ip(7),
	})
	for _, key := range []string{"top_k", "stop", "presence_penalty", "frequency_penalty", "seed"} {
		if _, present := b[key]; present {
			t.Errorf("%s must not be sent on /responses", key)
		}
	}
	// The same knobs are still available to the adapters that do take them.
	knobs := SamplingKnobs(sdk.Request{TopK: fp(0.5), StopSequences: []string{"END"}, Seed: ip(7)})
	if knobs["top_k"] != 0.5 || knobs["seed"] != int64(7) {
		t.Errorf("sampling knobs were dropped on the way out: %v", knobs)
	}
}

// A turn that thinks before it answers takes a different set of parameters on
// this endpoint, and this is the path the reasoning setting used to vanish on.
func TestChatKeepsTheReasoningSettingAndAsksForTheRightLimit(t *testing.T) {
	b := BuildChatRequest(sdk.Request{
		Model:           "gpt-5",
		ThinkingLevel:   sdk.ThinkingMedium,
		MaxOutputTokens: 4096,
	})
	if got := b["reasoning_effort"]; got != "medium" {
		t.Errorf("reasoning_effort = %v, want medium", got)
	}
	if got := b["max_completion_tokens"]; got != 4096 {
		t.Errorf("max_completion_tokens = %v", got)
	}
	if _, present := b["max_tokens"]; present {
		t.Error("max_tokens is the old name and is refused by these models")
	}
}

func TestChatOmitsSamplingKnobsWhileReasoning(t *testing.T) {
	b := BuildChatRequest(sdk.Request{
		Model:            "gpt-5",
		ThinkingLevel:    sdk.ThinkingLow,
		Temperature:      fp(0.2),
		TopP:             fp(0.3),
		PresencePenalty:  fp(1),
		FrequencyPenalty: fp(1),
	})
	for _, key := range []string{"temperature", "top_p", "presence_penalty", "frequency_penalty"} {
		if _, present := b[key]; present {
			t.Errorf("%s must be left at its default while reasoning is on", key)
		}
	}
}

func TestChatSendsEveryKnobWhenNotReasoning(t *testing.T) {
	b := BuildChatRequest(sdk.Request{
		Model:            "gpt-4o",
		Temperature:      fp(0.2),
		TopP:             fp(0.3),
		PresencePenalty:  fp(1),
		FrequencyPenalty: fp(-1),
		Seed:             ip(7),
		StopSequences:    []string{"END", "HALT"},
		MaxOutputTokens:  512,
		ThinkingLevel:    sdk.ThinkingNone,
	})
	if b["temperature"] != 0.2 || b["top_p"] != 0.3 {
		t.Errorf("sampling = %v / %v", b["temperature"], b["top_p"])
	}
	if b["presence_penalty"] != 1.0 || b["frequency_penalty"] != -1.0 {
		t.Errorf("penalties = %v / %v", b["presence_penalty"], b["frequency_penalty"])
	}
	if b["seed"] != int64(7) {
		t.Errorf("seed = %v", b["seed"])
	}
	if stop, _ := b["stop"].([]string); len(stop) != 2 {
		t.Errorf("stop = %v", b["stop"])
	}
	if b["max_tokens"] != 512 {
		t.Errorf("max_tokens = %v", b["max_tokens"])
	}
	if _, present := b["reasoning_effort"]; present {
		t.Error("thinking turned off must not ask for reasoning")
	}
}

// Unset means unset. Sending a zero we invented would be a real instruction to
// the model, not an absence of one.
func TestUnsetKnobsAreLeftOutEntirely(t *testing.T) {
	for name, b := range map[string]map[string]any{
		"responses": BuildResponsesRequest(sdk.Request{Model: "gpt-5"}),
		"chat":      BuildChatRequest(sdk.Request{Model: "gpt-4o"}),
	} {
		for _, key := range []string{"temperature", "top_p", "top_k", "stop", "seed", "reasoning", "reasoning_effort", "max_output_tokens", "max_tokens"} {
			if _, present := b[key]; present {
				t.Errorf("%s: %s was sent without being set", name, key)
			}
		}
	}
}

func TestThinkingNoneIsNotAskedForAsReasoning(t *testing.T) {
	if effort := ReasoningEffortOf(BuildResponsesRequest(sdk.Request{Model: "gpt-5", ThinkingLevel: sdk.ThinkingNone})); effort != "" {
		t.Errorf("none became %q", effort)
	}
	if effort := ReasoningEffortOf(BuildResponsesRequest(sdk.Request{Model: "gpt-5"})); effort != "" {
		t.Errorf("unset became %q", effort)
	}
}
