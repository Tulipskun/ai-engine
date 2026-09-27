package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadProviderFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "provider.json")
	data := []byte(`{"providers":[{"name":"openrouter","http_endpoint":"https://openrouter.ai/api/v1/","api_keys":[" key-1 ","key-2"]}]}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadProviderFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Providers) != 1 {
		t.Fatalf("providers=%d", len(config.Providers))
	}
	p := config.Providers[0]
	if p.Name != "openrouter" || p.HTTPEndpoint != "https://openrouter.ai/api/v1" {
		t.Fatalf("provider=%+v", p)
	}
	if len(p.APIKeys) != 2 || p.APIKeys[0] != "key-1" || p.APIKeys[1] != "key-2" {
		t.Fatalf("keys=%v", p.APIKeys)
	}
}

func TestProviderConfigsInferAdapters(t *testing.T) {
	config := ProviderFileConfig{Providers: []ProviderFile{
		{Name: "openrouter", HTTPEndpoint: "https://openrouter.ai/api/v1", APIKeys: []string{"key"}},
		{Name: "opencode", HTTPEndpoint: "https://example.invalid/v1", APIKeys: []string{"key"}},
	}}
	providers, err := config.ProviderConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if providers[0].Adapter != "openai" || providers[1].Adapter != "anthropic" {
		t.Fatalf("adapters=%v,%v", providers[0].Adapter, providers[1].Adapter)
	}
}

func TestProviderConfigsUseExplicitAdapter(t *testing.T) {
	config := ProviderFileConfig{Providers: []ProviderFile{
		{Name: "B.ai", Adapter: "openai", HTTPEndpoint: "https://api.b.ai/v1", APIKeys: []string{"key"}},
	}}
	providers, err := config.ProviderConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if providers[0].Adapter != "openai" {
		t.Fatalf("adapter=%v", providers[0].Adapter)
	}
}

func TestProviderConfigsUseExplicitOpenCodeAdapter(t *testing.T) {
	config := ProviderFileConfig{Providers: []ProviderFile{
		{Name: "zen", Adapter: "opencode", HTTPEndpoint: "https://opencode.ai/zen/v1", APIKeys: []string{"key"}},
	}}
	providers, err := config.ProviderConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if providers[0].Adapter != "opencode" {
		t.Fatalf("adapter=%v", providers[0].Adapter)
	}
}

func TestProviderConfigsRejectUnknownExplicitAdapter(t *testing.T) {
	config := ProviderFileConfig{Providers: []ProviderFile{
		{Name: "custom", Adapter: "unknown", HTTPEndpoint: "https://example.invalid/v1", APIKeys: []string{"key"}},
	}}
	if _, err := config.ProviderConfigs(); err == nil {
		t.Fatal("expected unknown adapter error")
	}
}

func TestLoadProviderFileRejectsMissingKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "provider.json")
	if err := os.WriteFile(path, []byte(`{"providers":[{"name":"openrouter","http_endpoint":"https://example.invalid/v1","api_keys":[]}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProviderFile(path); err == nil {
		t.Fatal("expected missing key error")
	}
}


func TestLoadProviderFileAllowsMissingConfig(t *testing.T) {
	path := t.TempDir() + "/provider.json"
	config, err := LoadProviderFile(path)
	if err != nil { t.Fatal(err) }
	if len(config.Providers) != 0 { t.Fatalf("providers = %d, want 0", len(config.Providers)) }
}

func TestProviderFileFreeOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider.json")
	data := []byte(`{"providers":[{"name":"mix","adapter":"openai","http_endpoint":"https://aihubmix.com/v1","api_keys":["k"],"free_only":true}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil { t.Fatal(err) }
	file, err := LoadProviderFile(path)
	if err != nil { t.Fatal(err) }
	if !file.Providers[0].FreeOnly { t.Fatal("free_only was not loaded") }
	configs, err := file.ProviderConfigs()
	if err != nil { t.Fatal(err) }
	if !configs[0].FreeOnly { t.Fatal("free_only was not passed to SDK config") }
}

func TestProviderFileHeadersPassthrough(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider.json")
	data := []byte(`{"providers":[{"name":"h","adapter":"openai","http_endpoint":"https://example.invalid/v1","api_keys":["k"],"headers":{"X-Title":"ai","X-Empty":""}}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil { t.Fatal(err) }
	file, err := LoadProviderFile(path)
	if err != nil { t.Fatal(err) }
	configs, err := file.ProviderConfigs()
	if err != nil { t.Fatal(err) }
	if configs[0].Headers["X-Title"] != "ai" { t.Fatalf("headers = %v", configs[0].Headers) }
}
