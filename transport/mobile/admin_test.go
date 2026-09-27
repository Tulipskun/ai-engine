package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeAdmin struct {
	providers []ProviderStatus
	settings  SettingsView
	added     ProviderSpec
	keyEdit   string
	change    KeyChange
	removed   string
	refreshes int
}

func (f *fakeAdmin) Providers(context.Context) ([]ProviderStatus, error) { return f.providers, nil }

func (f *fakeAdmin) AddProvider(_ context.Context, spec ProviderSpec) (ProviderStatus, error) {
	f.added = spec
	if spec.Endpoint == "https://bad" {
		return ProviderStatus{}, errors.New("endpoint ต้องใช้งานได้")
	}
	return ProviderStatus{ID: spec.ID, Adapter: spec.Adapter, Endpoint: spec.Endpoint, KeyCount: len(spec.Keys), Reachable: true}, nil
}

func (f *fakeAdmin) UpdateKeys(_ context.Context, id string, change KeyChange) (ProviderStatus, error) {
	f.keyEdit, f.change = id, change
	if len(change.Replace) == 1 && change.Replace[0] == "empty" {
		return ProviderStatus{}, errors.New("key pool ใหม่ว่างเปล่า")
	}
	return ProviderStatus{ID: id, KeyCount: len(change.Replace)}, nil
}

func (f *fakeAdmin) RemoveProvider(_ context.Context, id string) error {
	f.removed = id
	return nil
}

func (f *fakeAdmin) RefreshProviders(context.Context) ([]ProviderStatus, error) {
	f.refreshes++
	return f.providers, nil
}

func (f *fakeAdmin) RefreshProvider(_ context.Context, id string) (ProviderStatus, error) {
	f.refreshes++
	for _, p := range f.providers {
		if p.ID == id {
			return p, nil
		}
	}
	return ProviderStatus{ID: id}, nil
}

func (f *fakeAdmin) Settings(context.Context) (SettingsView, error) { return f.settings, nil }

func (f *fakeAdmin) SaveSettings(_ context.Context, settings SettingsView) (SettingsView, error) {
	if settings.Main.Model == "nope" {
		return SettingsView{}, errors.New("main agent: model \"nope\" is not available")
	}
	f.settings = settings
	return settings, nil
}

func adminRequest(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return historyRequest(t, handler, method, path, "cf-token", body)
}

func TestAdminListsProvidersWithoutKeyMaterial(t *testing.T) {
	store := &fakeAdmin{providers: []ProviderStatus{
		{ID: "NousResearch", Adapter: "openai", Endpoint: "https://api.example", KeyCount: 2, ModelCount: 7, Reachable: true},
		{ID: "Opencode", Adapter: "opencode", Endpoint: "https://opencode.ai/zen/v1", KeyCount: 3, Reachable: false,
			LastError: "free tier can only be used from within OpenCode"},
	}}
	cache := &ramCache{}
	cache.Adopt("cf-token")
	handler := NewAdminHandler(store, NewGate(GateConfig{Verify: allowVerifier{}, Cache: cache}))

	rec := adminRequest(t, handler, http.MethodGet, "/api/providers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/providers = %d: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "api_keys") || strings.Contains(rec.Body.String(), "sk-") {
		t.Fatalf("provider list leaked key material: %s", rec.Body)
	}
	var page struct {
		Providers []ProviderStatus `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Providers) != 2 || page.Providers[0].KeyCount != 2 {
		t.Fatalf("providers = %+v", page.Providers)
	}
	if page.Providers[1].LastError == "" || page.Providers[1].Reachable {
		t.Fatalf("unusable provider should be marked: %+v", page.Providers[1])
	}
}

func TestAdminAddsProviderAndEditsTheKeyPool(t *testing.T) {
	store := &fakeAdmin{}
	cache := &ramCache{}
	cache.Adopt("cf-token")
	handler := NewAdminHandler(store, NewGate(GateConfig{Verify: allowVerifier{}, Cache: cache}))

	rec := adminRequest(t, handler, http.MethodPost, "/api/providers",
		`{"id":"my-gateway","adapter":"openai","endpoint":"https://api.example/v1","keys":["k1","k2"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/providers = %d: %s", rec.Code, rec.Body)
	}
	if store.added.ID != "my-gateway" || len(store.added.Keys) != 2 {
		t.Fatalf("stored spec = %+v", store.added)
	}
	if rec := adminRequest(t, handler, http.MethodPost, "/api/providers",
		`{"id":"bad","adapter":"openai","endpoint":"https://bad","keys":["k"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("rejected provider = %d, want 400", rec.Code)
	}

	rec = adminRequest(t, handler, http.MethodPost, "/api/providers/my-gateway/keys", `{"replace":["a","b","c"]}`)
	if rec.Code != http.StatusOK || store.keyEdit != "my-gateway" || len(store.change.Replace) != 3 {
		t.Fatalf("key replace = %d %q %+v", rec.Code, store.keyEdit, store.change)
	}
	rec = adminRequest(t, handler, http.MethodPost, "/api/providers/my-gateway/keys", `{"add":["d"]}`)
	if rec.Code != http.StatusOK || len(store.change.Add) != 1 {
		t.Fatalf("key add = %d %+v", rec.Code, store.change)
	}
	rec = adminRequest(t, handler, http.MethodPost, "/api/providers/my-gateway/keys", `{"remove":[0,2]}`)
	if rec.Code != http.StatusOK || len(store.change.Remove) != 2 {
		t.Fatalf("key remove = %d %+v", rec.Code, store.change)
	}
	rec = adminRequest(t, handler, http.MethodDelete, "/api/providers/my-gateway", "")
	if rec.Code != http.StatusOK || store.removed != "my-gateway" {
		t.Fatalf("delete = %d %q", rec.Code, store.removed)
	}
	rec = adminRequest(t, handler, http.MethodPost, "/api/providers/refresh", "{}")
	if rec.Code != http.StatusOK || store.refreshes != 1 {
		t.Fatalf("refresh = %d (%d calls)", rec.Code, store.refreshes)
	}
}

func TestAdminSavesMainAndSubAgentRoutes(t *testing.T) {
	store := &fakeAdmin{}
	cache := &ramCache{}
	cache.Adopt("cf-token")
	handler := NewAdminHandler(store, NewGate(GateConfig{Verify: allowVerifier{}, Cache: cache}))

	rec := adminRequest(t, handler, http.MethodPut, "/api/settings",
		`{"main":{"provider":"NousResearch","model":"m-main"},"sub":{"provider":"B.AI","model":"m-sub"},"sub_enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/settings = %d: %s", rec.Code, rec.Body)
	}
	if store.settings.Main.Model != "m-main" || store.settings.Sub.Model != "m-sub" || !store.settings.SubEnabled {
		t.Fatalf("saved settings = %+v", store.settings)
	}
	rec = adminRequest(t, handler, http.MethodPut, "/api/settings", `{"main":{"provider":"x","model":"nope"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unavailable model = %d, want 400", rec.Code)
	}
}

func TestAdminNeedsTheSameTokenAsTheSocket(t *testing.T) {
	store := &fakeAdmin{}
	handler := NewAdminHandler(store, NewGate(GateConfig{Verify: allowVerifier{}, Cache: &ramCache{}}))
	rec := historyRequest(t, handler, http.MethodGet, "/api/providers", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", rec.Code)
	}
}
