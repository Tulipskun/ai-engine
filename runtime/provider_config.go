package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Tulipskun/ai-engine/sdk"
)

const DefaultProviderConfigPath = "config/provider.json"

type ProviderFile struct {
	Name          string   `json:"name"`
	Adapter       string   `json:"adapter,omitempty"`
	HTTPEndpoint  string   `json:"http_endpoint"`
	APIKeys       []string `json:"api_keys"`
	FreeOnly      bool     `json:"free_only,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
}

type ProviderFileConfig struct {
	Providers []ProviderFile `json:"providers"`
}

func LoadProviderFile(path string) (ProviderFileConfig, error) {
	if path == "" {
		return ProviderFileConfig{}, errors.New("runtime: provider config path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ProviderFileConfig{}, nil
		}
		return ProviderFileConfig{}, fmt.Errorf("runtime: read provider config %q: %w", path, err)
	}
	var config ProviderFileConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return ProviderFileConfig{}, fmt.Errorf("runtime: decode provider config %q: %w", path, err)
	}
	if len(config.Providers) == 0 {
		return ProviderFileConfig{}, errors.New("runtime: provider config contains no providers")
	}
	seen := make(map[string]struct{}, len(config.Providers))
	for i := range config.Providers {
		p := &config.Providers[i]
		p.Name = strings.TrimSpace(p.Name)
		p.Adapter = strings.ToLower(strings.TrimSpace(p.Adapter))
		p.HTTPEndpoint = strings.TrimRight(strings.TrimSpace(p.HTTPEndpoint), "/")
		if p.Name == "" {
			return ProviderFileConfig{}, fmt.Errorf("runtime: provider[%d] name is required", i)
		}
		if p.HTTPEndpoint == "" {
			return ProviderFileConfig{}, fmt.Errorf("runtime: provider %q http_endpoint is required", p.Name)
		}
		if _, ok := seen[p.Name]; ok {
			return ProviderFileConfig{}, fmt.Errorf("runtime: duplicate provider %q", p.Name)
		}
		seen[p.Name] = struct{}{}
		keys := make([]string, 0, len(p.APIKeys))
		for _, key := range p.APIKeys {
			key = strings.TrimSpace(key)
			if key != "" {
				keys = append(keys, key)
			}
		}
		if len(keys) == 0 {
			return ProviderFileConfig{}, fmt.Errorf("runtime: provider %q has no api_keys", p.Name)
		}
		p.APIKeys = keys
	}
	return config, nil
}

func cloneStringMap(in map[string]string) map[string]string { if len(in) == 0 { return nil }; out := make(map[string]string, len(in)); for k, v := range in { out[k] = v }; return out }

func (c ProviderFileConfig) ProviderConfigs() ([]sdk.ProviderConfig, error) {
	configs := make([]sdk.ProviderConfig, 0, len(c.Providers))
	for _, p := range c.Providers {
		adapter, err := adapterForProvider(p.Name, p.Adapter)
		if err != nil {
			return nil, err
		}
		configs = append(configs, sdk.ProviderConfig{
			ID:       sdk.ProviderID(p.Name),
			BaseURL:  p.HTTPEndpoint,
			Keys:     sdk.NewKeyPool(p.APIKeys...),
			Adapter:  adapter,
			FreeOnly: p.FreeOnly,
			Headers:  cloneStringMap(p.Headers),
		})
	}
	return configs, nil
}

func adapterForProvider(name, explicit string) (sdk.AdapterID, error) {
	if explicit != "" {
		switch strings.ToLower(strings.TrimSpace(explicit)) {
		case string(sdk.AdapterOpenAI):
			return sdk.AdapterOpenAI, nil
		case string(sdk.AdapterAnthropic):
			return sdk.AdapterAnthropic, nil
		case string(sdk.AdapterGemini):
			return sdk.AdapterGemini, nil
		case string(sdk.AdapterOpenCode):
			return sdk.AdapterOpenCode, nil
		default:
			return "", fmt.Errorf("runtime: provider %q has unsupported adapter %q", name, explicit)
		}
	}
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "openai", "openrouter":
		return sdk.AdapterOpenAI, nil
	case "anthropic", "opencode":
		return sdk.AdapterAnthropic, nil
	case "gemini", "google":
		return sdk.AdapterGemini, nil
	default:
		return "", fmt.Errorf("runtime: provider %q requires an explicit adapter", name)
	}
}
