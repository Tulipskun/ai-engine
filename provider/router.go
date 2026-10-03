package provider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

type routeKey struct {
	provider ProviderID
	model    string
}
type Router struct {
	mu           sync.RWMutex
	routes       map[routeKey]ModelRoute
	providers    map[ProviderID]ProviderConfig
	catalogs     map[ProviderID][]Model
	catalogReady map[ProviderID]bool
}

func NewRouter() *Router {
	return &Router{routes: make(map[routeKey]ModelRoute), providers: make(map[ProviderID]ProviderConfig), catalogs: make(map[ProviderID][]Model), catalogReady: make(map[ProviderID]bool)}
}
func (r *Router) RegisterProvider(config ProviderConfig) {
	if config.ID == "" || config.Adapter == "" {
		panic("sdk: invalid provider config")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[config.ID] = config
	delete(r.catalogs, config.ID)
	r.catalogReady[config.ID] = false
}
func (r *Router) Provider(provider ProviderID) (ProviderConfig, error) {
	r.mu.RLock()
	config, ok := r.providers[provider]
	r.mu.RUnlock()
	if !ok {
		return ProviderConfig{}, fmt.Errorf("sdk: provider %q is not registered", provider)
	}
	return config, nil
}
func (r *Router) Register(route ModelRoute) {
	if route.Provider == "" || route.Model == "" || route.Adapter == "" {
		panic("sdk: invalid model route")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes[routeKey{route.Provider, route.Model}] = route
}
func (r *Router) RefreshModels(ctx context.Context, provider ProviderID, adapter Provider) error {
	config, err := r.Provider(provider)
	if err != nil {
		return err
	}
	lister, ok := adapter.(ModelLister)
	if !ok {
		return fmt.Errorf("sdk: adapter %q does not support model discovery", config.Adapter)
	}
	if config.Keys == nil {
		return errors.New("sdk: provider has no key pool")
	}
	key, err := config.Keys.Current()
	if err != nil {
		return err
	}
	p := adapter
	if kp, ok := p.(KeyedProvider); ok {
		p = kp.WithAPIKey(key)
	}
	if ep, ok := p.(EndpointProvider); ok && config.BaseURL != "" {
		p = ep.WithBaseURL(config.BaseURL)
	}
	if len(config.Headers) > 0 {
		if hp, ok := p.(HeaderedProvider); ok {
			p = hp.WithHeaders(config.Headers)
		}
	}
	lister, ok = p.(ModelLister)
	if !ok {
		return fmt.Errorf("sdk: configured adapter %q cannot discover models after configuration", config.Adapter)
	}
	models, err := lister.ListModels(ctx, key)
	if err != nil {
		return err
	}
	clean := make([]Model, 0, len(models))
	seen := make(map[string]struct{})
	for _, model := range models {
		if model.ID == "" {
			continue
		}
		if _, exists := seen[model.ID]; exists {
			continue
		}
		seen[model.ID] = struct{}{}
		clean = append(clean, ModelCapabilities(config.Adapter, model))
	}
	clean = filterFreeModels(clean, config.FreeOnly)
	r.mu.Lock()
	r.catalogs[provider] = clean
	r.catalogReady[provider] = true
	r.mu.Unlock()
	return nil
}
func filterFreeModels(models []Model, freeOnly bool) []Model {
	if !freeOnly {
		return models
	}
	out := make([]Model, 0, len(models))
	for _, model := range models {
		if isFreeModelID(model.ID) {
			out = append(out, model)
		}
	}
	return out
}

// isFreeModelID matches the free-tier naming of the gateways in use:
// "-free" suffix (opencode Zen), ":free" suffix (OpenRouter-style,
// e.g. NousResearch) and "free/" prefix used by some providers.
func isFreeModelID(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return false
	}
	return strings.HasSuffix(id, "-free") || strings.HasSuffix(id, ":free") || strings.HasPrefix(id, "free/")
}

// ProviderIDs lists the registered providers, so a caller can offer the whole
// catalogue (the phone's provider picker) without tracking it separately.
func (r *Router) ProviderIDs() []ProviderID {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ProviderID, 0, len(r.providers))
	for id := range r.providers {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (r *Router) Models(provider ProviderID) []Model {
	r.mu.RLock()
	models := append([]Model(nil), r.catalogs[provider]...)
	r.mu.RUnlock()
	return models
}
func (r *Router) Resolve(provider ProviderID, model string) (ModelRoute, error) {
	if provider == "" {
		return ModelRoute{}, errors.New("sdk: provider is required")
	}
	if model == "" {
		return ModelRoute{}, errors.New("sdk: model is required")
	}
	r.mu.RLock()
	config, providerOK := r.providers[provider]
	ready := r.catalogReady[provider]
	route, staticOK := r.routes[routeKey{provider, model}]
	if ready {
		for _, discovered := range r.catalogs[provider] {
			if discovered.ID == model {
				r.mu.RUnlock()
				return ModelRoute{Provider: provider, Model: model, Adapter: config.Adapter}, nil
			}
		}
		r.mu.RUnlock()
		return ModelRoute{}, fmt.Errorf("sdk: model %q is not available for provider=%q", model, provider)
	}
	r.mu.RUnlock()
	if staticOK {
		return route, nil
	}
	if providerOK {
		return ModelRoute{}, fmt.Errorf("sdk: model catalogue for provider=%q has not been refreshed", provider)
	}
	return ModelRoute{}, fmt.Errorf("sdk: no route for provider=%q model=%q", provider, model)
}
