package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Tulipskun/ai-engine/runtime"
	"github.com/Tulipskun/ai-engine/runtime/d1store"
	"github.com/Tulipskun/ai-engine/sdk"
	mobiletransport "github.com/Tulipskun/ai-engine/transport/mobile"
)

// A provider that answers the health probe, so the admin surface can be exercised
// without a network.
func adminFixture(t *testing.T) (*adminStore, *sync.WaitGroup) {
	t.Helper()
	root := t.TempDir()
	providerFile := filepath.Join(root, "config", "provider.json")
	if err := os.MkdirAll(filepath.Dir(providerFile), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"providers":[{"name":"test-gateway","adapter":"openai","http_endpoint":"https://api.example.invalid/v1","api_keys":["k1"]}]}`
	if err := os.WriteFile(providerFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	rt := &runtime.Runtime{Router: sdk.NewRouter(), Client: sdk.NewRouterClient(sdk.NewRouter())}
	manager := runtime.NewProviderManager(providerFile, rt, runtime.ProviderFileConfig{})
	store := newAdminStore(root, (*d1store.Client)(nil), manager, &runtime.ProviderFileConfig{}, nil, nil,
		sdk.SessionConfig{Provider: "boot", Model: "boot-model"})
	return store, &sync.WaitGroup{}
}

func TestAdminAddsProviderAndNeverReturnsKeyMaterial(t *testing.T) {
	store, _ := adminFixture(t)
	view, err := store.AddProvider(context.Background(), mobiletransport.ProviderSpec{
		ID: "second", Adapter: "openai", Endpoint: "https://api.example.invalid/v1", Keys: []string{"secret-key"},
	})
	if err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	if view.ID != "second" || view.KeyCount != 1 {
		t.Fatalf("view = %+v", view)
	}
	list, err := store.Providers(context.Background())
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("providers = %+v", list)
	}
	for _, p := range list {
		if p.ID == "" {
			t.Fatalf("provider without an id: %+v", p)
		}
	}
}

func TestAdminRejectsBadProviders(t *testing.T) {
	store, _ := adminFixture(t)
	cases := []mobiletransport.ProviderSpec{
		{ID: "", Adapter: "openai", Endpoint: "https://x.example", Keys: []string{"k"}},
		{ID: "a", Adapter: "nope", Endpoint: "https://x.example", Keys: []string{"k"}},
		{ID: "a", Adapter: "openai", Endpoint: "http://x.example", Keys: []string{"k"}},
		{ID: "a", Adapter: "openai", Endpoint: "https://x.example"},
	}
	for i, spec := range cases {
		if _, err := store.AddProvider(context.Background(), spec); err == nil {
			t.Fatalf("case %d was accepted: %+v", i, spec)
		}
	}
	if _, err := store.AddProvider(context.Background(), mobiletransport.ProviderSpec{
		ID: "test-gateway", Adapter: "openai", Endpoint: "https://x.example", Keys: []string{"k"},
	}); err == nil {
		t.Fatal("a duplicate provider name must be refused")
	}
}

func TestAdminKeyPoolEdits(t *testing.T) {
	store, _ := adminFixture(t)
	if _, err := store.UpdateKeys(context.Background(), "test-gateway", mobiletransport.KeyChange{Add: []string{"k2"}}); err != nil {
		t.Fatalf("add: %v", err)
	}
	list, _ := store.Providers(context.Background())
	if list[0].KeyCount != 2 {
		t.Fatalf("key count after add = %d, want 2", list[0].KeyCount)
	}
	if _, err := store.UpdateKeys(context.Background(), "test-gateway", mobiletransport.KeyChange{Remove: []int{0, 1}}); err == nil {
		t.Fatal("emptying the key pool must be refused")
	}
	if _, err := store.UpdateKeys(context.Background(), "missing", mobiletransport.KeyChange{Add: []string{"k"}}); err == nil {
		t.Fatal("an unknown provider must be refused")
	}
}

// A provider nobody probed is unknown, not healthy, and a key change takes the
// old verdict away with it.
func TestAdminReportsUntestedUntilProbed(t *testing.T) {
	store, _ := adminFixture(t)
	list, err := store.Providers(context.Background())
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	if list[0].Probed {
		t.Fatalf("a provider nobody probed must not report probed: %+v", list[0])
	}
	store.setStatus("test-gateway", "401 Invalid API key")
	list, _ = store.Providers(context.Background())
	if !list[0].Probed || list[0].LastError == "" || list[0].Reachable {
		t.Fatalf("a failed probe must read as unusable: %+v", list[0])
	}
	if _, err := store.UpdateKeys(context.Background(), "test-gateway", mobiletransport.KeyChange{Add: []string{"k2"}}); err != nil {
		t.Fatalf("add key: %v", err)
	}
	list, _ = store.Providers(context.Background())
	if list[0].Probed || list[0].LastError == "401 Invalid API key" {
		t.Fatalf("changing the pool must clear the old verdict: %+v", list[0])
	}
}

func TestAdminSettingsRoundTrip(t *testing.T) {
	store, _ := adminFixture(t)
	view, err := store.Settings(context.Background())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if view.Main.Provider != "boot" || view.Main.Model != "boot-model" {
		t.Fatalf("default main route = %+v, want the daemon boot route", view.Main)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("settings should serialise")
	}
}

// The probe records the model that answered, and a key change forgets it: a new
// pool has not been tested yet, so the phone must not keep the old default.
func TestProbeVerdictLifecycle(t *testing.T) {
	store, _ := adminFixture(t)
	if got := store.WorkingModel("Opencode"); got != "" {
		t.Fatalf("nothing probed yet, but working model = %q", got)
	}
	store.probedModel["Opencode"] = "space-bunny-free"
	if got := store.WorkingModel("Opencode"); got != "space-bunny-free" {
		t.Fatalf("working model = %q", got)
	}
	store.forgetStatus("Opencode")
	if got := store.WorkingModel("Opencode"); got != "" {
		t.Fatalf("changing the key pool must forget the verdict, got %q", got)
	}
	if tools := probeTool(); len(tools) == 0 || tools[0].Name == "" || tools[0].InputSchema == nil {
		t.Fatalf("a health check must carry tools, got %+v", tools)
	}
}

// A health check starts with the model that answered last time: a gateway can
// serve one model and refuse another, and model #1 of the catalogue is not
// evidence about the provider.
func TestProbeStartsWithTheModelThatAnsweredLast(t *testing.T) {
	models := []sdk.Model{{ID: "first"}, {ID: "working"}, {ID: "third"}}
	ordered := probeOrder(models, "working")
	if len(ordered) != 3 || ordered[0].ID != "working" {
		t.Fatalf("probe order = %+v, want the working model first", ordered)
	}
	if ordered[1].ID != "first" || ordered[2].ID != "third" {
		t.Fatalf("probe order = %+v, want the catalogue order after the working model", ordered)
	}
	if got := probeOrder(models, ""); len(got) != 3 || got[0].ID != "first" {
		t.Fatalf("without a known model the catalogue order must stand, got %+v", got)
	}
	if got := probeOrder(models, "gone"); len(got) != 3 || got[0].ID != "first" {
		t.Fatalf("a model that left the catalogue must not be probed, got %+v", got)
	}
}

// A refusal the provider will repeat is reported as such and stops the walk:
// asking the next model spends the same quota for the same answer.
func TestProbeNamesARepeatedRefusal(t *testing.T) {
	for status, want := range map[int]string{401: "key", 403: "free tier", 429: "โควตา"} {
		_, ok := refusalMessage(&statusError{status: status})
		if !ok {
			t.Fatalf("status %d was not recognised as a refusal", status)
		}
		msg, _ := refusalMessage(&statusError{status: status})
		if !strings.Contains(msg.Error(), want) {
			t.Fatalf("status %d message = %q, want it to mention %q", status, msg, want)
		}
	}
	if _, ok := refusalMessage(&statusError{status: 503}); ok {
		t.Fatal("503 is not a refusal: the next model is still worth a try")
	}
	if _, ok := refusalMessage(errors.New("boom")); ok {
		t.Fatal("an error without a status must not be reported as a refusal")
	}
}

type statusError struct{ status int }

func (e *statusError) Error() string       { return "http status" }
func (e *statusError) HTTPStatusCode() int { return e.status }

// A daemon that boots with empty env must still route unpinned chats once D1
// has hydrated: the stored agent defaults are pushed into the live runtime,
// not just left on disk for the settings screen to display.
func TestRefreshRoutesFromDiskPicksUpStoredDefaults(t *testing.T) {
	store, _ := adminFixture(t)
	systemPath := store.systemPath
	if err := os.MkdirAll(filepath.Dir(systemPath), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"provider":"Opencode","model":"muse-spark-1.3-contributor-free","sub_agent":{"Provider":"Opencode","Model":"space-bunny-free","Enabled":true}}`
	if err := os.WriteFile(systemPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	store.RefreshRoutesFromDisk()
	route := store.defaultRoute()
	if route.Provider != "Opencode" || route.Model != "muse-spark-1.3-contributor-free" {
		t.Fatalf("default route = %+v, want the stored agent defaults", route)
	}
}
