package sdk

import (
	"math"
	"testing"
)

func TestSessionRuntimeSettingsCanBeUpdatedWithoutChangingHistory(t *testing.T) {
	pool := NewKeyPool("key-1", "key-2")
	temperature := 0.7
	s := NewSession(SessionConfig{ID: "s1", Provider: ProviderOpenRouter, Model: "gpt-5", KeyIndex: 0, ThinkingLevel: ThinkingLow}, pool)
	s.Append(Turn{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: "hello"}}})

	if err := s.SetModel("gpt-5.1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTemperature(temperature); err != nil {
		t.Fatal(err)
	}
	if err := s.SetThinkingLevel(ThinkingHigh); err != nil {
		t.Fatal(err)
	}
	if err := s.SetKeyIndex(1); err != nil {
		t.Fatal(err)
	}

	cfg := s.Config()
	if cfg.Model != "gpt-5.1" {
		t.Fatalf("model = %q", cfg.Model)
	}
	if cfg.Temperature == nil || *cfg.Temperature != temperature {
		t.Fatalf("temperature = %v", cfg.Temperature)
	}
	if cfg.ThinkingLevel != ThinkingHigh {
		t.Fatalf("thinking level = %q", cfg.ThinkingLevel)
	}
	if cfg.KeyIndex != 1 {
		t.Fatalf("key index = %d", cfg.KeyIndex)
	}
	if got := len(s.History()); got != 1 {
		t.Fatalf("history length = %d, want 1", got)
	}
}

func TestSessionRuntimeSettingsCanClearOptionalValues(t *testing.T) {
	temperature := 0.4
	s := NewSession(SessionConfig{ID: "s1", Provider: ProviderOpenRouter, Model: "gpt-5", Temperature: &temperature, ThinkingLevel: ThinkingHigh}, nil)
	if err := s.ClearTemperature(); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearThinkingLevel(); err != nil {
		t.Fatal(err)
	}
	cfg := s.Config()
	if cfg.Temperature != nil {
		t.Fatal("temperature should be cleared")
	}
	if cfg.ThinkingLevel != "" {
		t.Fatalf("thinking level = %q, want empty", cfg.ThinkingLevel)
	}
}

func TestSessionRuntimeSettingsRejectInvalidValues(t *testing.T) {
	s := NewSession(SessionConfig{ID: "s1", Provider: ProviderOpenRouter, Model: "gpt-5"}, nil)
	if err := s.SetModel(""); err == nil {
		t.Fatal("empty model should fail")
	}
	if err := s.SetTemperature(math.NaN()); err == nil {
		t.Fatal("NaN temperature should fail")
	}
	if err := s.SetTemperature(math.Inf(1)); err == nil {
		t.Fatal("infinite temperature should fail")
	}
	if err := s.SetThinkingLevel(ThinkingLevel("extreme")); err == nil {
		t.Fatal("invalid thinking level should fail")
	}
	if err := s.SetKeyIndex(0); err == nil {
		t.Fatal("key index without key pool should fail")
	}
}
