package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSystemConfigMissingIsEmpty(t *testing.T) {
	cfg, err := LoadSystemConfig(filepath.Join(t.TempDir(), "system.json"))
	if err != nil { t.Fatal(err) }
	if cfg.SystemPrompt != "" { t.Fatalf("expected empty prompt, got %q", cfg.SystemPrompt) }
}

func TestSaveSystemConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "system.json")
	want := SystemConfig{SystemPrompt: "  be helpful  ", Provider: " test ", Model: " m ", MaxOutputTokens: 1024, Workspace: " ~/work "}
	if err := SaveSystemConfig(path, want); err != nil { t.Fatal(err) }
	got, err := LoadSystemConfig(path)
	if err != nil { t.Fatal(err) }
	if got.SystemPrompt != "be helpful" { t.Fatalf("round trip prompt = %q", got.SystemPrompt) }
	if got.Provider != "test" || got.Model != "m" || got.Workspace != "~/work" {
		t.Fatalf("round trip fields = %+v", got)
	}
	if got.MaxOutputTokens != 1024 { t.Fatalf("round trip max_output_tokens = %d", got.MaxOutputTokens) }
}

func TestLoadSystemConfigRejectsNegativeMaxOutputTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "system.json")
	if err := os.WriteFile(path, []byte(`{"max_output_tokens":-1}`), 0o600); err != nil { t.Fatal(err) }
	if _, err := LoadSystemConfig(path); err == nil { t.Fatal("expected negative max_output_tokens error") }
}

func TestLoadSystemConfigRejectsBadJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "system.json")
	if err := os.WriteFile(path, []byte("{oops"), 0o600); err != nil { t.Fatal(err) }
	if _, err := LoadSystemConfig(path); err == nil { t.Fatal("expected decode error") }
}
