package sdk

import (
	"context"
	"testing"
)

func TestRouterResolvesLogicalProviderAndModelToAdapter(t *testing.T) {
	r := NewRouter()
	r.Register(ModelRoute{Provider: ProviderOpenRouter, Model: "gpt-5", Adapter: AdapterOpenAI})
	r.Register(ModelRoute{Provider: ProviderOpenRouter, Model: "gemini-3.5", Adapter: AdapterGemini})
	r.Register(ModelRoute{Provider: ProviderOpenCode, Model: "opus", Adapter: AdapterAnthropic})

	cases := []struct {
		provider ProviderID
		model    string
		adapter  AdapterID
	}{
		{ProviderOpenRouter, "gpt-5", AdapterOpenAI},
		{ProviderOpenRouter, "gemini-3.5", AdapterGemini},
		{ProviderOpenCode, "opus", AdapterAnthropic},
	}
	for _, tc := range cases {
		route, err := r.Resolve(tc.provider, tc.model)
		if err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
		if route.Adapter != tc.adapter {
			t.Fatalf("adapter = %q, want %q", route.Adapter, tc.adapter)
		}
	}
}

func TestRouterRequiresExplicitProvider(t *testing.T) {
	r := NewRouter()
	r.Register(ModelRoute{Provider: ProviderOpenRouter, Model: "gpt-5", Adapter: AdapterOpenAI})
	if _, err := r.Resolve("", "gpt-5"); err == nil {
		t.Fatal("Resolve() expected an error for empty provider")
	}
}

func TestSessionPinsKeyIndex(t *testing.T) {
	pool := NewKeyPool("key-1", "key-2", "key-3")
	s1 := NewSession(SessionConfig{ID: "s1", Provider: ProviderOpenRouter, Model: "gpt-5", KeyIndex: 0}, pool)
	s2 := NewSession(SessionConfig{ID: "s2", Provider: ProviderOpenRouter, Model: "gemini-3.5", KeyIndex: 1}, pool)
	k1, err := s1.APIKey()
	if err != nil || k1 != "key-1" {
		t.Fatalf("session 1 key = %q, err=%v", k1, err)
	}
	k2, err := s2.APIKey()
	if err != nil || k2 != "key-2" {
		t.Fatalf("session 2 key = %q, err=%v", k2, err)
	}
	current, err := pool.Current()
	if err != nil || current != "key-1" {
		t.Fatalf("pool current = %q, err=%v", current, err)
	}
}

func TestFilterFreeModels(t *testing.T) {
	models := []Model{{ID: "gpt-5"}, {ID: "hy3-free"}, {ID: "QWEN-FREE"}, {ID: "stepfun/step-3.7-flash:free"}, {ID: "Free/qwen-3"}, {ID: "freebie"}, {ID: "gpt-5-free-tier"}, {ID: ""}}
	got := filterFreeModels(models, false)
	if len(got) != len(models) {
		t.Fatalf("disabled filter should pass through: %d", len(got))
	}
	got = filterFreeModels(models, true)
	want := []string{"hy3-free", "QWEN-FREE", "stepfun/step-3.7-flash:free", "Free/qwen-3"}
	if len(got) != len(want) {
		t.Fatalf("free filter = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Fatalf("free filter = %+v, want %+v", got, want)
		}
	}
	if got := filterFreeModels([]Model{{ID: "gpt-5"}}, true); len(got) != 0 {
		t.Fatalf("expected empty catalog, got %+v", got)
	}
}

type headerCaptureLister struct {
	got    map[string]string
	models []Model
}

func (f *headerCaptureLister) Name() string               { return "capture" }
func (f *headerCaptureLister) WithAPIKey(string) Provider { return f }
func (f *headerCaptureLister) WithHeaders(h map[string]string) Provider {
	got := make(map[string]string, len(h))
	for k, v := range h {
		got[k] = v
	}
	f.got = got
	return f
}
func (f *headerCaptureLister) Generate(context.Context, Request) (Response, error) {
	return Response{}, nil
}
func (f *headerCaptureLister) Stream(context.Context, Request) (<-chan Event, error) {
	ch := make(chan Event, 1)
	close(ch)
	return ch, nil
}
func (f *headerCaptureLister) ListModels(context.Context, string) ([]Model, error) {
	return f.models, nil
}

func TestRefreshModelsAppliesCustomHeaders(t *testing.T) {
	r := NewRouter()
	r.RegisterProvider(ProviderConfig{ID: "p1", Adapter: AdapterOpenAI, Keys: NewKeyPool("k"), Headers: map[string]string{"X-Title": "ai"}})
	adapter := &headerCaptureLister{models: []Model{{ID: "m"}}}
	if err := r.RefreshModels(context.Background(), "p1", adapter); err != nil {
		t.Fatal(err)
	}
	if adapter.got["X-Title"] != "ai" {
		t.Fatalf("custom headers not applied: %v", adapter.got)
	}
	if len(r.Models("p1")) != 1 {
		t.Fatalf("catalog = %+v", r.Models("p1"))
	}
}
