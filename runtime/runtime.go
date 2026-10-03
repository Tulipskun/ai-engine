package runtime

import (
	"context"
	"fmt"
	"github.com/Tulipskun/ai-engine/provider"
	"github.com/Tulipskun/ai-engine/provider/anthropic"
	"github.com/Tulipskun/ai-engine/provider/gemini"
	"github.com/Tulipskun/ai-engine/provider/openai"
	"github.com/Tulipskun/ai-engine/provider/opencode"
)

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
