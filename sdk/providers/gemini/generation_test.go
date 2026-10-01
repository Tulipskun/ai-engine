package gemini

import (
	"testing"

	"github.com/Tulipskun/ai-engine/sdk"
)

func fp(v float64) *float64 { return &v }

func mustGenerationConfig(t *testing.T, b map[string]any) map[string]any {
	t.Helper()
	cfg, _ := b["generationConfig"].(map[string]any)
	if cfg == nil {
		t.Fatalf("no generationConfig: %v", b)
	}
	return cfg
}

// Gemini keeps everything under generationConfig, with its own spelling.
func TestTheKnobsGeminiTakesAreAllSent(t *testing.T) {
	b := build(sdk.Request{
		Model:         "gemini-2.5-pro",
		Temperature:   fp(0.3),
		TopP:          fp(0.8),
		TopK:          fp(0.4),
		StopSequences: []string{"END", "HALT"},
		ThinkingLevel: sdk.ThinkingMedium,
	})
	cfg, _ := b["generationConfig"].(map[string]any)
	if cfg == nil {
		t.Fatalf("no generationConfig: %v", b)
	}
	if cfg["temperature"] != 0.3 || cfg["topP"] != 0.8 || cfg["topK"] != 0.4 {
		t.Errorf("sampling = %v / %v / %v", cfg["temperature"], cfg["topP"], cfg["topK"])
	}
	stop, _ := cfg["stopSequences"].([]string)
	if len(stop) != 2 || stop[0] != "END" {
		t.Errorf("stopSequences = %v", cfg["stopSequences"])
	}
	thinking, _ := cfg["thinkingConfig"].(map[string]any)
	if thinking["thinkingLevel"] != "medium" {
		t.Errorf("thinkingConfig = %v", cfg["thinkingConfig"])
	}
}

// Gemini has no presence or frequency penalty, and seed is not one of its
// parameters; the canonical map must not smuggle them into this payload.
func TestUnsetAndForeignKnobsStayOffTheWire(t *testing.T) {
	b := build(sdk.Request{Model: "gemini-2.5-pro", TopP: fp(0.5)})
	cfg, _ := b["generationConfig"].(map[string]any)
	if cfg == nil {
		t.Fatalf("no generationConfig: %v", b)
	}
	for _, key := range []string{"temperature", "topK", "stopSequences", "thinkingConfig", "presence_penalty", "frequency_penalty", "seed", "reasoning", "reasoning_effort"} {
		if _, present := cfg[key]; present {
			t.Errorf("%s was sent without being set", key)
		}
	}
}

func TestMaxOutputTokensOnlyWhenCapped(t *testing.T) {
	// With nothing set there is no generationConfig at all, so the provider keeps
	// every one of its own defaults.
	if _, present := build(sdk.Request{Model: "gemini-2.5-pro"})["generationConfig"]; present {
		t.Error("an uncapped turn must leave the generation defaults alone")
	}
	capped := mustGenerationConfig(t, build(sdk.Request{Model: "gemini-2.5-pro", MaxOutputTokens: 900}))
	if capped["maxOutputTokens"] != 900 {
		t.Errorf("maxOutputTokens = %v", capped["maxOutputTokens"])
	}
}
