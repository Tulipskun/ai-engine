package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Tulipskun/ai-engine/provider"
)

func TestSaveSystemConfigRoundTripsThroughItsLoader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "system.json")
	temperature := 0.25
	want := SystemConfig{
		SystemPrompt: "be precise",
		Provider:     "openrouter",
		Model:        "some/model",
		Generation:   provider.GenerationSettings{Temperature: &temperature, ThinkingLevel: provider.ThinkingMedium},
		SubAgent:     SubAgentConfig{Enabled: true, Model: "some/model-mini"},
	}
	if err := SaveSystemConfig(path, want); err != nil {
		t.Fatalf("SaveSystemConfig: %v", err)
	}
	got, err := LoadSystemConfig(path)
	if err != nil {
		t.Fatalf("LoadSystemConfig: %v", err)
	}
	if got.SystemPrompt != want.SystemPrompt || got.Provider != want.Provider || got.Model != want.Model {
		t.Errorf("round trip lost a top-level field: %+v", got)
	}
	if got.Generation.Temperature == nil || *got.Generation.Temperature != temperature {
		t.Errorf("temperature did not survive the round trip: %+v", got.Generation.Temperature)
	}
	if got.Generation.ThinkingLevel != provider.ThinkingMedium {
		t.Errorf("thinking level did not survive: %q", got.Generation.ThinkingLevel)
	}
	if !got.SubAgent.Enabled || got.SubAgent.Model != "some/model-mini" {
		t.Errorf("sub-agent settings did not survive: %+v", got.SubAgent)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config holds provider routes and prompts, want 0600, got %o", perm)
	}
}

func TestLoadSystemConfigRejectsOutOfRangeValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "system.json")
	// A hand-edited file is held to the same bounds as one that arrived over the
	// wire, so the two cannot disagree about what a provider accepts.
	if err := os.WriteFile(path, []byte(`{"generation":{"temperature":9}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSystemConfig(path); err == nil {
		t.Error("an out-of-range temperature in a config file must be refused")
	}
}
