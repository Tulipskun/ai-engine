package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Tulipskun/ai-engine/sdk"
	"github.com/Tulipskun/ai-engine/sdk/providers/anthropic"
	"github.com/Tulipskun/ai-engine/sdk/providers/gemini"
	"github.com/Tulipskun/ai-engine/sdk/providers/openai"
	"github.com/Tulipskun/ai-engine/sdk/providers/opencode"
)

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
func (m *ProviderManager) RefreshProvider(ctx context.Context, id sdk.ProviderID) error {
	if m == nil || m.rt == nil {
		return errors.New("runtime: provider manager is not initialized")
	}
	return m.rt.RefreshProvider(ctx, id)
}

func (m *ProviderManager) Adapters() []sdk.AdapterID {
	return []sdk.AdapterID{sdk.AdapterOpenAI, sdk.AdapterAnthropic, sdk.AdapterGemini, sdk.AdapterOpenCode}
}
func (m *ProviderManager) Providers() []sdk.ProviderID {
	if m == nil || m.rt == nil {
		return nil
	}
	return append([]sdk.ProviderID(nil), m.rt.Providers...)
}
func (m *ProviderManager) KeyPools() map[sdk.ProviderID]*sdk.KeyPool {
	if m == nil || m.rt == nil {
		return nil
	}
	out := make(map[sdk.ProviderID]*sdk.KeyPool, len(m.rt.ProviderConfigs))
	for _, config := range m.rt.ProviderConfigs {
		out[config.ID] = config.Keys
	}
	return out
}

func (m *ProviderManager) Upsert(ctx context.Context, name, adapter, endpoint, apiKey string, freeOnly bool) error {
	if m == nil || m.rt == nil || m.rt.Router == nil || m.rt.Client == nil {
		return errors.New("runtime: provider manager is not initialized")
	}
	name = strings.TrimSpace(name)
	adapter = strings.ToLower(strings.TrimSpace(adapter))
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	apiKey = strings.TrimSpace(apiKey)
	if name == "" {
		return errors.New("provider name is required")
	}
	if endpoint == "" {
		return errors.New("provider URL is required")
	}
	if apiKey == "" {
		return errors.New("provider API key is required")
	}
	if _, err := adapterForProvider(name, adapter); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	file := m.config
	index := -1
	for i := range file.Providers {
		if strings.EqualFold(file.Providers[i].Name, name) {
			index = i
			break
		}
	}
	entry := ProviderFile{Name: name, Adapter: adapter, HTTPEndpoint: endpoint, APIKeys: []string{apiKey}, FreeOnly: freeOnly}
	if index >= 0 {
		file.Providers[index] = entry
	} else {
		file.Providers = append(file.Providers, entry)
	}
	configs, err := file.ProviderConfigs()
	if err != nil {
		return err
	}
	var selected sdk.ProviderConfig
	for _, config := range configs {
		if string(config.ID) == name {
			selected = config
			break
		}
	}
	if selected.ID == "" {
		return fmt.Errorf("provider %q was not converted to an SDK config", name)
	}
	if err := m.persist(file); err != nil {
		return err
	}
	if err := m.ensureAdapter(selected.Adapter); err != nil {
		return err
	}
	m.rt.Router.RegisterProvider(selected)
	m.rt.ProviderConfigs = configs
	m.rt.Providers = providerIDs(configs)
	m.config = file
	if err := m.rt.RefreshProvider(ctx, selected.ID); err != nil {
		return fmt.Errorf("provider %q saved but model discovery failed: %w", name, err)
	}
	return nil
}

// Reload re-reads provider.json from disk and re-registers every provider on
// the live router. The stateless daemon needs it: the file is materialized from
// D1 only after a phone hands over the token, which happens long after boot, so
// a runtime loaded at start would otherwise keep an empty provider set for its
// whole life (REQ-046(4), REQ-047).
func (m *ProviderManager) Reload(ctx context.Context) ([]sdk.ProviderConfig, error) {
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
	loaded := make([]sdk.ProviderConfig, 0, len(configs))
	for _, config := range configs {
		if err := m.ensureAdapter(config.Adapter); err != nil {
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

func (m *ProviderManager) ensureAdapter(id sdk.AdapterID) error {
	if _, ok := m.rt.Client.Adapters[id]; ok {
		return nil
	}
	switch id {
	case sdk.AdapterOpenAI:
		m.rt.Client.RegisterAdapter(id, openai.New(""))
	case sdk.AdapterAnthropic:
		m.rt.Client.RegisterAdapter(id, anthropic.New(""))
	case sdk.AdapterGemini:
		m.rt.Client.RegisterAdapter(id, gemini.New(""))
	case sdk.AdapterOpenCode:
		m.rt.Client.RegisterAdapter(id, opencode.New(""))
	default:
		return fmt.Errorf("runtime: unsupported adapter %q", id)
	}
	return nil
}
func (m *ProviderManager) persist(config ProviderFileConfig) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("runtime: encode provider config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return fmt.Errorf("runtime: create provider config directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(m.path), ".provider-*.json")
	if err != nil {
		return fmt.Errorf("runtime: create provider config temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("runtime: chmod provider config temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("runtime: write provider config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("runtime: close provider config temp file: %w", err)
	}
	if err := os.Rename(tmpName, m.path); err != nil {
		return fmt.Errorf("runtime: replace provider config: %w", err)
	}
	return nil
}
func providerIDs(configs []sdk.ProviderConfig) []sdk.ProviderID {
	ids := make([]sdk.ProviderID, 0, len(configs))
	for _, config := range configs {
		ids = append(ids, config.ID)
	}
	return ids
}
