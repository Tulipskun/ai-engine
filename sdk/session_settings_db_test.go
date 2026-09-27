package sdk

import (
	"path/filepath"
	"testing"
)

func TestSessionRuntimeSettingsPersistAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	pool := NewKeyPool("key-1", "key-2")
	s, err := OpenSession(path, SessionConfig{ID: "s1", Provider: ProviderOpenRouter, Model: "gpt-5", KeyIndex: 0}, pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetModel("gpt-5.1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTemperature(0.8); err != nil {
		t.Fatal(err)
	}
	if err := s.SetThinkingLevel(ThinkingHigh); err != nil {
		t.Fatal(err)
	}
	if err := s.SetKeyIndex(1); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSession(path, SessionConfig{ID: "s1", Provider: ProviderOpenRouter, Model: "ignored", KeyIndex: 0}, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	cfg := reopened.Config()
	if cfg.Model != "gpt-5.1" {
		t.Fatalf("model = %q", cfg.Model)
	}
	if cfg.Temperature == nil || *cfg.Temperature != 0.8 {
		t.Fatalf("temperature = %v", cfg.Temperature)
	}
	if cfg.ThinkingLevel != ThinkingHigh {
		t.Fatalf("thinking level = %q", cfg.ThinkingLevel)
	}
	if cfg.KeyIndex != 1 {
		t.Fatalf("key index = %d", cfg.KeyIndex)
	}
}
