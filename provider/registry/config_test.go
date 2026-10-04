package registry

import (
	"ai-engine/provider"
	"os"
	"path/filepath"
	"testing"
)

// Each config file has exactly one writer, and the phone's admin surface goes
// through it (CHANGE-099). A loader/writer pair that disagrees on shape or
// permissions is how a hand-edited file and a phone-saved file diverge.

func TestSaveProviderFileRoundTripsThroughItsLoader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "json")
	want := ProviderFileConfig{Providers: []ProviderFile{{
		Name: "openrouter", Adapter: "openai",
		HTTPEndpoint: "https://openrouter.ai/api/v1",
		APIKeys:      []string{"k1", "k2"},
	}}}
	if err := SaveProviderFile(path, want); err != nil {
		t.Fatalf("SaveProviderFile: %v", err)
	}
	got, err := LoadProviderFile(path)
	if err != nil {
		t.Fatalf("LoadProviderFile: %v", err)
	}
	if len(got.Providers) != 1 {
		t.Fatalf("expected one provider, got %d", len(got.Providers))
	}
	p := got.Providers[0]
	if p.Name != "openrouter" || p.HTTPEndpoint != "https://openrouter.ai/api/v1" {
		t.Errorf("provider did not survive the round trip: %+v", p)
	}
	if len(p.APIKeys) != 2 {
		t.Errorf("key pool did not survive: %v", p.APIKeys)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config holds API keys, want 0600, got %o", perm)
	}
}

func TestLoadProviderFileRejectsAnIncompleteProvider(t *testing.T) {
	for name, body := range map[string]string{
		"no endpoint": `{"providers":[{"name":"x","api_keys":["k"]}]}`,
		"no keys":     `{"providers":[{"name":"x","http_endpoint":"https://e/v1","api_keys":[]}]}`,
		"duplicate":   `{"providers":[{"name":"x","http_endpoint":"https://e/v1","api_keys":["k"]},{"name":"x","http_endpoint":"https://e/v1","api_keys":["k"]}]}`,
	} {
		path := filepath.Join(t.TempDir(), "json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadProviderFile(path); err == nil {
			t.Errorf("%s: an incomplete provider config must be refused", name)
		}
	}
}

func TestAdapterForProviderRequiresAnExplicitChoiceForUnknownNames(t *testing.T) {
	// Name inference only covers the four known gateways; anything else has to say
	// what it speaks, or the daemon would guess a wire format.
	if _, err := adapterForProvider("my-gateway", ""); err == nil {
		t.Error("an unknown provider name must require an explicit adapter")
	}
	if got, err := adapterForProvider("my-gateway", "gemini"); err != nil || got != provider.AdapterGemini {
		t.Errorf("an explicit adapter must win, got %q err %v", got, err)
	}
	if got, err := adapterForProvider("openrouter", ""); err != nil || got != provider.AdapterOpenAI {
		t.Errorf("openrouter must infer the openai adapter, got %q err %v", got, err)
	}
}
