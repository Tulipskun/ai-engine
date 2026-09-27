package mobile

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
)

// ProviderStatus is the phone's view of one configured provider. Key material is
// never sent back: the app can add, replace or drop keys, and a leaked screen
// must not turn into a leaked key pool.
type ProviderStatus struct {
	ID         string `json:"id"`
	Adapter    string `json:"adapter"`
	Endpoint   string `json:"endpoint"`
	FreeOnly   bool   `json:"free_only"`
	KeyCount   int    `json:"key_count"`
	ModelCount int    `json:"model_count"`
	Reachable  bool   `json:"reachable"`
	Probed     bool   `json:"probed"`
	// WorkingModel is the model that answered the last successful probe.
	WorkingModel string `json:"working_model,omitempty"`
	LastError    string `json:"last_error,omitempty"`
}

// AgentSettings is what the main agent and the sub agent each run on.
type AgentSettings struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type SettingsView struct {
	Main AgentSettings `json:"main"`
	Sub  AgentSettings `json:"sub"`
	// SubEnabled mirrors whether delegation is on at all.
	SubEnabled bool `json:"sub_enabled"`
}

// AdminStore is the write side the phone owns: which providers exist, which keys
// they may use, and which model each agent runs on. Everything it writes lands
// in `config/provider` and `config/system` in D1, which is the same place the
// daemon hydrates from, so a restart keeps the choices.
type AdminStore interface {
	Providers(ctx context.Context) ([]ProviderStatus, error)
	AddProvider(ctx context.Context, spec ProviderSpec) (ProviderStatus, error)
	UpdateKeys(ctx context.Context, id string, change KeyChange) (ProviderStatus, error)
	RemoveProvider(ctx context.Context, id string) error
	RefreshProviders(ctx context.Context) ([]ProviderStatus, error)
	RefreshProvider(ctx context.Context, id string) (ProviderStatus, error)
	Settings(ctx context.Context) (SettingsView, error)
	SaveSettings(ctx context.Context, settings SettingsView) (SettingsView, error)
}

// ProviderSpec is a new provider as the phone describes it.
type ProviderSpec struct {
	ID       string   `json:"id"`
	Adapter  string   `json:"adapter"`
	Endpoint string   `json:"endpoint"`
	Keys     []string `json:"keys"`
	FreeOnly bool     `json:"free_only"`
}

// KeyChange edits one provider's key pool. Exactly one of the three applies, so
// a half-understood request cannot quietly wipe a working pool.
type KeyChange struct {
	Add     []string `json:"add,omitempty"`
	Remove  []int    `json:"remove,omitempty"`
	Replace []string `json:"replace,omitempty"`
}

// NewAdminHandler serves the provider and agent settings surface. It shares the
// history gate, because it is the same trust: the Cloudflare token the phone
// already presents.
func NewAdminHandler(store AdminStore, gate *Gate) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "provider management is not configured"})
			return
		}
		decision := gate.Check(r)
		if !decision.Allowed {
			gate.Write(w, decision)
			return
		}
		if decision.Token != "" {
			gate.CachedTokens().Adopt(decision.Token)
		}
		path := strings.TrimSuffix(r.URL.Path, "/")
		switch {
		case path == "/api/providers" && r.Method == http.MethodGet:
			writeJSON(w, http.StatusOK, map[string]any{"providers": providerList(store, r)})
		case path == "/api/providers" && r.Method == http.MethodPost:
			adminAddProvider(w, r, store)
		case path == "/api/providers/refresh" && r.Method == http.MethodPost:
			providers, err := store.RefreshProviders(r.Context())
			if err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"providers": providers})
		case strings.HasPrefix(path, "/api/providers/"):
			adminProviderItem(w, r, store, strings.TrimPrefix(path, "/api/providers/"))
		case path == "/api/settings" && r.Method == http.MethodGet:
			settings, err := store.Settings(r.Context())
			if err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, settings)
		case path == "/api/settings" && r.Method == http.MethodPut:
			adminSaveSettings(w, r, store)
		default:
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		}
	})
}

func providerList(store AdminStore, r *http.Request) []ProviderStatus {
	providers, err := store.Providers(r.Context())
	if err != nil {
		return []ProviderStatus{}
	}
	if providers == nil {
		return []ProviderStatus{}
	}
	return providers
}

func adminAddProvider(w http.ResponseWriter, r *http.Request, store AdminStore) {
	var spec ProviderSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	view, err := store.AddProvider(r.Context(), spec)
	if err != nil {
		writeAdminError(w, "add "+spec.ID, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// writeAdminError answers with the reason and logs it once. Without the log a
// failed phone-side edit is invisible on the machine that owns the state.
func writeAdminError(w http.ResponseWriter, what string, err error) {
	log.Printf("admin %s: %v", what, err)
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
}

func adminProviderItem(w http.ResponseWriter, r *http.Request, store AdminStore, rest string) {
	parts := strings.Split(rest, "/")
	if parts[0] == "" || len(parts) > 2 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	id := parts[0]
	if len(parts) == 1 {
		// /api/providers/<id> — removing a provider is the only thing left to
		// do to one as a whole.
		if r.Method == http.MethodDelete {
			if err := store.RemoveProvider(r.Context(), id); err != nil {
				writeAdminError(w, "remove "+id, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	switch {
	case parts[1] == "keys" && r.Method == http.MethodPost:
		var change KeyChange
		raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			writeAdminError(w, "keys "+id, err)
			return
		}
		if err := json.Unmarshal(raw, &change); err != nil {
			// The shape, never the content: a key is in this body.
			log.Printf("admin: keys %s: bad request: %v (body %d bytes, %d quotes, %d braces)",
				id, err, len(raw), bytes.Count(raw, []byte{'"'}), bytes.Count(raw, []byte{'{', '}'}))
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
			return
		}
		view, err := store.UpdateKeys(r.Context(), id, change)
		if err != nil {
			writeAdminError(w, "keys "+id, err)
			return
		}
		writeJSON(w, http.StatusOK, view)
	case parts[1] == "refresh" && r.Method == http.MethodPost:
		// Testing one provider must not wait for the others: a phone that is
		// fixing a dead key needs that answer now.
		view, err := store.RefreshProvider(r.Context(), id)
		if err != nil {
			writeAdminError(w, "refresh "+id, err)
			return
		}
		writeJSON(w, http.StatusOK, view)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	}
}

func adminSaveSettings(w http.ResponseWriter, r *http.Request, store AdminStore) {
	var settings SettingsView
	if err := json.NewDecoder(r.Body).Decode(&settings); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	saved, err := store.SaveSettings(r.Context(), settings)
	if err != nil {
		writeAdminError(w, "settings", err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}
