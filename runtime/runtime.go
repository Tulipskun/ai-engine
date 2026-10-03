package runtime

import (
	"context"
	"fmt"
	"github.com/Tulipskun/ai-engine/sdk"
	"github.com/Tulipskun/ai-engine/sdk/providers/anthropic"
	"github.com/Tulipskun/ai-engine/sdk/providers/gemini"
	"github.com/Tulipskun/ai-engine/sdk/providers/openai"
	"github.com/Tulipskun/ai-engine/sdk/providers/opencode"
)

type Runtime struct {
	Router          *sdk.Router
	Client          *sdk.RouterClient
	Providers       []sdk.ProviderID
	ProviderConfigs []sdk.ProviderConfig
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
	router := sdk.NewRouter()
	client := sdk.NewRouterClient(router)
	providers := make([]sdk.ProviderID, 0, len(configs))
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

func newAdapter(id sdk.AdapterID) (sdk.Provider, error) {
	switch id {
	case sdk.AdapterOpenAI:
		return openai.New(""), nil
	case sdk.AdapterAnthropic:
		return anthropic.New(""), nil
	case sdk.AdapterGemini:
		return gemini.New(""), nil
	case sdk.AdapterOpenCode:
		return opencode.New(""), nil
	default:
		return nil, fmt.Errorf("runtime: unsupported adapter %q", id)
	}
}

func (r *Runtime) RefreshModels(ctx context.Context) error {
	if r == nil || r.Router == nil || r.Client == nil {
		return fmt.Errorf("runtime: runtime is not initialized")
	}
	for _, provider := range r.Providers {
		if err := r.Client.RefreshModels(ctx, provider); err != nil {
			return fmt.Errorf("runtime: refresh provider %q: %w", provider, err)
		}
	}
	return nil
}
func (r *Runtime) RefreshProvider(ctx context.Context, provider sdk.ProviderID) error {
	if r == nil || r.Client == nil {
		return fmt.Errorf("runtime: runtime is not initialized")
	}
	return r.Client.RefreshModels(ctx, provider)
}
