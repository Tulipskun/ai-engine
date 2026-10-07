package registry

import (
	"fmt"
	"sort"

	"ai-engine/provider"
	"ai-engine/provider/openai"
	"ai-engine/provider/opencode"
)

type Registry struct {
	adapters map[string]provider.Adapter
}

func New() *Registry {
	adapter := openai.New()
	return &Registry{adapters: map[string]provider.Adapter{
		"openai":   adapter,
		"opencode": opencode.New(),
	}}
}

func (r *Registry) Get(name string) (provider.Adapter, bool) {
	adapter, ok := r.adapters[name]
	return adapter, ok
}

func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.adapters))
	for name := range r.adapters {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *Registry) Require(name string) (provider.Adapter, error) {
	adapter, ok := r.Get(name)
	if !ok {
		return nil, fmt.Errorf("unknown adapter %q (available: %v)", name, r.Names())
	}
	return adapter, nil
}
