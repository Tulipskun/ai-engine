package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The shipped system config example has to parse, validate and say nothing false,
// because it is how anyone discovers which knobs exist. An example that is never
// parsed is an example that rots (CHANGE-077).

func repoFile(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{".."}, parts...)...)
	if _, err := os.Stat(path); err != nil {
		t.Skipf("example file is not in this checkout: %v", err)
	}
	return path
}

func TestTheSystemExampleParsesAndValidates(t *testing.T) {
	cfg, err := LoadSystemConfig(repoFile(t, ".config", "system.example.json"))
	if err != nil {
		t.Fatalf("the shipped example does not parse or validate: %v", err)
	}
	if cfg.SubAgent.Enabled != true {
		t.Error("the example leaves delegation off, which is not the default")
	}
	if cfg.Generation.MaxOutputTokens != 0 || cfg.MaxOutputTokens != 0 {
		t.Error("the example sets an output cap; the default is to let the provider decide")
	}
	for name, set := range map[string]*float64{
		"temperature": cfg.Generation.Temperature, "top_p": cfg.Generation.TopP,
		"top_k": cfg.Generation.TopK, "presence_penalty": cfg.Generation.PresencePenalty,
		"frequency_penalty": cfg.Generation.FrequencyPenalty,
	} {
		if set != nil {
			t.Errorf("the example sets %s to %v; the default is the provider's own", name, *set)
		}
	}
	if cfg.Generation.ThinkingLevel != "" {
		t.Errorf("the example asks for reasoning level %q by default", cfg.Generation.ThinkingLevel)
	}
}

// A knob the daemon accepts has to be spellable in the example, or nobody finds it.
func TestTheSystemExampleShowsTheGenerationBlock(t *testing.T) {
	raw, err := os.ReadFile(repoFile(t, ".config", "system.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatal(err)
	}
	generation, ok := shape["generation"]
	if !ok {
		t.Fatal("the example has no generation block")
	}
	var knobs map[string]json.RawMessage
	if err := json.Unmarshal(generation, &knobs); err != nil {
		t.Fatal(err)
	}
	for _, knob := range []string{
		"thinking_level", "temperature", "top_p", "top_k", "stop_sequences",
		"presence_penalty", "frequency_penalty", "seed", "max_output_tokens",
	} {
		if _, ok := knobs[knob]; !ok {
			t.Errorf("the example does not mention %q", knob)
		}
	}
}

// The bounds enforced on the wire have to be enforced on a file too.
func TestTheSystemExampleIsRefusedWhenAValueIsOutOfRange(t *testing.T) {
	broken := filepath.Join(t.TempDir(), "system.json")
	if err := os.WriteFile(broken, []byte(`{"generation":{"temperature":9}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSystemConfig(broken); err == nil {
		t.Fatal("an out-of-range temperature in a config file was accepted")
	}
}
