package main

import (
	"context"
	"testing"

	"github.com/Tulipskun/ai-engine/sdk"
	"github.com/Tulipskun/ai-engine/transport/mobile"
)

// A sub-agent-only save must not touch the main route. Before this rule existed
// the phone could silently unpin a session's main provider/model by saving
// sub-agent settings, because the request carried no main fields.
func TestSessionRouteChangeLeavesMainPinAloneForSubOnlyRequests(t *testing.T) {
	router := testSessionRouter()
	enabled := true
	for _, tc := range []struct {
		name   string
		choice mobile.ModelChoice
	}{
		{"sub route only", mobile.ModelChoice{SubProvider: "test", SubModel: "model", SubEnabled: &enabled}},
		{"sub flag only", mobile.ModelChoice{SubEnabled: &enabled}},
		{"clear sub", mobile.ModelChoice{ClearSub: true}},
		{"nothing at all", mobile.ModelChoice{}},
	} {
		changed, provider, model, err := sessionRouteChange(tc.choice, router)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if changed || provider != "" || model != "" {
			t.Fatalf("%s: main route changed=%v %q/%q, want untouched", tc.name, changed, provider, model)
		}
	}
}

func TestSessionRouteChangeResolvesAndValidatesMainRoutes(t *testing.T) {
	router := testSessionRouter()
	changed, provider, model, err := sessionRouteChange(mobile.ModelChoice{Provider: "test", Model: "model"}, router)
	if err != nil || !changed || provider != "test" || model != "model" {
		t.Fatalf("full route: changed=%v %q/%q err=%v", changed, provider, model, err)
	}
	if _, _, _, err = sessionRouteChange(mobile.ModelChoice{Provider: "test", Model: "nope"}, router); err == nil {
		t.Fatal("an unknown model must be rejected")
	}
	// With no catalogue loaded there is nothing to complete a half-specified
	// route with, so it is rejected rather than guessed.
	if _, _, _, err = sessionRouteChange(mobile.ModelChoice{Model: "model"}, router); err == nil {
		t.Fatal("a model with no known provider must be rejected")
	}
}

// With a catalogue loaded, one side only is completed from it: the phone can
// send just the provider or just the model.
func TestSessionRouteChangeCompletesAHalfSpecifiedRoute(t *testing.T) {
	router := testSessionRouterWithCatalog(t)
	if _, provider, model, err := sessionRouteChange(mobile.ModelChoice{Model: "model"}, router); err != nil || provider != "test" {
		t.Fatalf("model only: %q/%q err=%v", provider, model, err)
	}
	if _, provider, model, err := sessionRouteChange(mobile.ModelChoice{Provider: "test"}, router); err != nil || model != "model" {
		t.Fatalf("provider only: %q/%q err=%v", provider, model, err)
	}
}

func testSessionRouter() *sdk.Router {
	router := sdk.NewRouter()
	router.RegisterProvider(sdk.ProviderConfig{
		ID: "test", BaseURL: "http://test", Keys: sdk.NewKeyPool("test-key"), Adapter: sdk.AdapterOpenAI,
	})
	router.Register(sdk.ModelRoute{Provider: "test", Model: "model", Adapter: sdk.AdapterOpenAI})
	return router
}

// catalogProvider answers model discovery so the router has a catalogue to
// complete half-specified routes from.
type catalogProvider struct{}

func (catalogProvider) Name() string { return "test" }
func (catalogProvider) Generate(context.Context, sdk.Request) (sdk.Response, error) {
	return sdk.Response{}, nil
}
func (catalogProvider) Stream(context.Context, sdk.Request) (<-chan sdk.Event, error) {
	return nil, nil
}
func (p catalogProvider) WithAPIKey(string) sdk.Provider { return p }
func (catalogProvider) ListModels(context.Context, string) ([]sdk.Model, error) {
	return []sdk.Model{{ID: "model"}, {ID: "other"}}, nil
}

func testSessionRouterWithCatalog(t *testing.T) *sdk.Router {
	t.Helper()
	router := testSessionRouter()
	if err := router.RefreshModels(context.Background(), "test", catalogProvider{}); err != nil {
		t.Fatal(err)
	}
	return router
}
