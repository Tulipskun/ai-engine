package sdk

import (
	"context"
	"testing"
)

type discoveryTestAdapter struct {
	models []Model
	key    string
	base   string
}

func (f *discoveryTestAdapter) Name() string                   { return "discovery-test" }
func (f *discoveryTestAdapter) WithAPIKey(key string) Provider { cp := *f; cp.key = key; return &cp }
func (f *discoveryTestAdapter) WithBaseURL(base string) Provider {
	cp := *f
	cp.base = base
	return &cp
}
func (f *discoveryTestAdapter) ListModels(_ context.Context, _ string) ([]Model, error) {
	return f.models, nil
}
func (f *discoveryTestAdapter) Generate(_ context.Context, req Request) (Response, error) {
	return Response{Model: req.Model}, nil
}
func (f *discoveryTestAdapter) Stream(_ context.Context, _ Request) (<-chan Event, error) {
	ch := make(chan Event)
	close(ch)
	return ch, nil
}

func TestRouterRefreshModelsReplacesStaleCatalogue(t *testing.T) {
	r := NewRouter()
	adapter := &discoveryTestAdapter{models: []Model{{ID: "new-model"}, {ID: "another-model"}}}
	r.RegisterProvider(ProviderConfig{ID: "test", BaseURL: "https://example.test/v1", Keys: NewKeyPool("key-1"), Adapter: "discovery-test"})
	if err := r.RefreshModels(context.Background(), "test", adapter); err != nil {
		t.Fatal(err)
	}
	models := r.Models("test")
	if len(models) != 2 || models[0].ID != "new-model" {
		t.Fatalf("models = %+v", models)
	}
	if _, err := r.Resolve("test", "old-model"); err == nil {
		t.Fatal("stale model should be removed")
	}
	if _, err := r.Resolve("test", "new-model"); err != nil {
		t.Fatalf("new model should resolve: %v", err)
	}
}

func TestRouterRefreshModelsDoesNotMutateSharedAdapter(t *testing.T) {
	r := NewRouter()
	adapter := &discoveryTestAdapter{models: []Model{{ID: "m"}}}
	r.RegisterProvider(ProviderConfig{ID: "test", BaseURL: "https://provider.test/v1", Keys: NewKeyPool("key-7"), Adapter: "discovery-test"})
	if err := r.RefreshModels(context.Background(), "test", adapter); err != nil {
		t.Fatal(err)
	}
	if adapter.key != "" || adapter.base != "" {
		t.Fatal("RefreshModels mutated the shared adapter")
	}
}
