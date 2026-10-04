package registry

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/Tulipskun/ai-engine/provider"
	"github.com/Tulipskun/ai-engine/provider/anthropic"
	"github.com/Tulipskun/ai-engine/provider/gemini"
	"github.com/Tulipskun/ai-engine/provider/openai"
	"github.com/Tulipskun/ai-engine/provider/opencode"
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
