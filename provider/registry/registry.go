package registry

import (
	"ai-engine/provider"
	"ai-engine/provider/anthropic"
	"ai-engine/provider/gemini"
	"ai-engine/provider/openai"
	"ai-engine/provider/opencode"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ---- from provider/registry/config.go ----
const DefaultProviderConfigPath = "config/provider.json"

type ProviderFile struct {
	Name         string            `json:"name"`
	Adapter      string            `json:"adapter,omitempty"`
	HTTPEndpoint string            `json:"http_endpoint"`
	APIKeys      []string          `json:"api_keys"`
	FreeOnly     bool              `json:"free_only,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
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

// SaveProviderFile is the single writer for config/provider.json. The phone's
// admin surface goes through it so the mkdir/chmod/write rules cannot drift
// between the two callers that used to spell them out separately (CHANGE-099).
func SaveProviderFile(path string, config ProviderFileConfig) error {
	if path == "" {
		return errors.New("runtime: provider config path is required")
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("runtime: encode provider config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("runtime: create provider config directory: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("runtime: write provider config %q: %w", path, err)
	}
	return nil
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (c ProviderFileConfig) ProviderConfigs() ([]provider.ProviderConfig, error) {
	configs := make([]provider.ProviderConfig, 0, len(c.Providers))
	for _, p := range c.Providers {
		adapter, err := adapterForProvider(p.Name, p.Adapter)
		if err != nil {
			return nil, err
		}
		configs = append(configs, provider.ProviderConfig{
			ID:       provider.ProviderID(p.Name),
			BaseURL:  p.HTTPEndpoint,
			Keys:     provider.NewKeyPool(p.APIKeys...),
			Adapter:  adapter,
			FreeOnly: p.FreeOnly,
			Headers:  cloneStringMap(p.Headers),
		})
	}
	return configs, nil
}

func adapterForProvider(name, explicit string) (provider.AdapterID, error) {
	if explicit != "" {
		switch strings.ToLower(strings.TrimSpace(explicit)) {
		case string(provider.AdapterOpenAI):
			return provider.AdapterOpenAI, nil
		case string(provider.AdapterAnthropic):
			return provider.AdapterAnthropic, nil
		case string(provider.AdapterGemini):
			return provider.AdapterGemini, nil
		case string(provider.AdapterOpenCode):
			return provider.AdapterOpenCode, nil
		default:
			return "", fmt.Errorf("runtime: provider %q has unsupported adapter %q", name, explicit)
		}
	}
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "openai", "openrouter":
		return provider.AdapterOpenAI, nil
	case "anthropic", "opencode":
		return provider.AdapterAnthropic, nil
	case "gemini", "google":
		return provider.AdapterGemini, nil
	default:
		return "", fmt.Errorf("runtime: provider %q requires an explicit adapter", name)
	}
}

// ---- from provider/registry/manager.go ----
type ProviderManager struct {
	mu     sync.Mutex
	path   string
	rt     *Runtime
	config ProviderFileConfig
}

func NewProviderManager(path string, rt *Runtime, config ProviderFileConfig) *ProviderManager {
	if path == "" {
		path = DefaultProviderConfigPath
	}
	return &ProviderManager{path: path, rt: rt, config: config}
}

// Rt exposes the runtime the manager rebuilds, so the phone-facing admin
// surface can read the live router and refresh one provider's catalogue.
func (m *ProviderManager) Rt() *Runtime {
	if m == nil {
		return nil
	}
	return m.rt
}

// RefreshProvider re-runs model discovery for one provider.
func (m *ProviderManager) RefreshProvider(ctx context.Context, id provider.ProviderID) error {
	if m == nil || m.rt == nil {
		return errors.New("runtime: provider manager is not initialized")
	}
	return m.rt.RefreshProvider(ctx, id)
}

// Reload re-reads provider.json from disk and re-registers every provider on
// the live router. The stateless daemon needs it: the file is materialized from
// D1 only after a phone hands over the token, which happens long after boot, so
// a runtime loaded at start would otherwise keep an empty provider set for its
// whole life (REQ-046(4), REQ-047).
func (m *ProviderManager) Reload(ctx context.Context) ([]provider.ProviderConfig, error) {
	if m == nil || m.rt == nil || m.rt.Router == nil || m.rt.Client == nil {
		return nil, errors.New("runtime: provider manager is not initialized")
	}
	file, err := LoadProviderFile(m.path)
	if err != nil {
		return nil, err
	}
	configs, err := file.ProviderConfigs()
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	loaded := make([]provider.ProviderConfig, 0, len(configs))
	for _, config := range configs {
		if err := m.ensureAdapter(config.ID, config.Adapter); err != nil {
			return nil, err
		}
		m.rt.Router.RegisterProvider(config)
		// A catalogue refresh is best effort: a provider without reachable
		// models is still usable, and the turn reports the failure if it needs
		// one that does not exist.
		if err := m.rt.RefreshProvider(ctx, config.ID); err != nil {
			log.Printf("provider %s: model discovery failed after reload: %v", config.ID, err)
		}
		loaded = append(loaded, config)
	}
	m.config = file
	m.rt.ProviderConfigs = configs
	m.rt.Providers = providerIDs(configs)
	return loaded, nil
}

func (m *ProviderManager) ensureAdapter(providerLocal provider.ProviderID, id provider.AdapterID) error {
	if _, ok := m.rt.Client.Adapters[provider.AdapterBinding{Provider: providerLocal, Adapter: id}]; ok {
		return nil
	}
	switch id {
	case provider.AdapterOpenAI:
		m.rt.Client.RegisterAdapter(providerLocal, id, openai.New(""))
	case provider.AdapterAnthropic:
		m.rt.Client.RegisterAdapter(providerLocal, id, anthropic.New(""))
	case provider.AdapterGemini:
		m.rt.Client.RegisterAdapter(providerLocal, id, gemini.New(""))
	case provider.AdapterOpenCode:
		m.rt.Client.RegisterAdapter(providerLocal, id, opencode.New(""))
	default:
		return fmt.Errorf("runtime: unsupported adapter %q", id)
	}
	return nil
}
func providerIDs(configs []provider.ProviderConfig) []provider.ProviderID {
	ids := make([]provider.ProviderID, 0, len(configs))
	for _, config := range configs {
		ids = append(ids, config.ID)
	}
	return ids
}

// ---- from provider/registry/load.go ----
type Runtime struct {
	Router          *provider.Router
	Client          *provider.RouterClient
	Providers       []provider.ProviderID
	ProviderConfigs []provider.ProviderConfig
}

func Load(path string) (*Runtime, error) {
	file, err := LoadProviderFile(path)
	if err != nil {
		return nil, err
	}
	configs, err := file.ProviderConfigs()
	if err != nil {
		return nil, err
	}
	router := provider.NewRouter()
	client := provider.NewRouterClient(router)
	providers := make([]provider.ProviderID, 0, len(configs))
	for _, config := range configs {
		router.RegisterProvider(config)
		providers = append(providers, config.ID)
		adapter, err := newAdapter(config.Adapter)
		if err != nil {
			return nil, err
		}
		client.RegisterAdapter(config.ID, config.Adapter, adapter)
	}
	return &Runtime{Router: router, Client: client, Providers: providers, ProviderConfigs: configs}, nil
}

func newAdapter(id provider.AdapterID) (provider.Provider, error) {
	switch id {
	case provider.AdapterOpenAI:
		return openai.New(""), nil
	case provider.AdapterAnthropic:
		return anthropic.New(""), nil
	case provider.AdapterGemini:
		return gemini.New(""), nil
	case provider.AdapterOpenCode:
		return opencode.New(""), nil
	default:
		return nil, fmt.Errorf("runtime: unsupported adapter %q", id)
	}
}

func (r *Runtime) RefreshModels(ctx context.Context) error {
	if r == nil || r.Router == nil || r.Client == nil {
		return fmt.Errorf("runtime: runtime is not initialized")
	}
	for _, providerLocal := range r.Providers {
		if err := r.Client.RefreshModels(ctx, providerLocal); err != nil {
			return fmt.Errorf("runtime: refresh provider %q: %w", providerLocal, err)
		}
	}
	return nil
}
func (r *Runtime) RefreshProvider(ctx context.Context, providerLocal provider.ProviderID) error {
	if r == nil || r.Client == nil {
		return fmt.Errorf("runtime: runtime is not initialized")
	}
	return r.Client.RefreshModels(ctx, providerLocal)
}
