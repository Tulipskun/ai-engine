package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ---- from io/gateway/history.go ----
// SessionRow and TurnRow are the history shapes the phone reads. The JSON names
// are the contract with AIxodia's HistoryApi, so they must not drift.
type SessionRow struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	SubProvider string `json:"sub_provider"`
	SubModel    string `json:"sub_model"`
	SubEnabled  int    `json:"sub_enabled"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

type TurnRow struct {
	Seq       int64  `json:"seq"`
	Role      string `json:"role"`
	Agent     string `json:"agent"`
	JobID     string `json:"job_id"`
	Text      string `json:"text"`
	CreatedAt int64  `json:"created_at"`
	// Footer of an answered turn (AX-095).
	Model        string `json:"model,omitempty"`
	InputTokens  int    `json:"input_tokens,omitempty"`
	OutputTokens int    `json:"output_tokens,omitempty"`
	CacheRead    int    `json:"cache_read_tokens,omitempty"`
	CacheWrite   int    `json:"cache_write_tokens,omitempty"`
	DurationMs   int64  `json:"duration_ms,omitempty"`
}

type NodeRow struct {
	TunnelURL string `json:"tunnel_url"`
	Version   string `json:"version"`
	Heartbeat int64  `json:"heartbeat"`
}

// ModelView and ProviderView are the model catalogue the phone offers, so the
// operator can pick a provider and model instead of editing entry.json.
type ModelView struct {
	ID                  string `json:"id"`
	Name                string `json:"name,omitempty"`
	SupportsTools       bool   `json:"supports_tools"`
	SupportsTemperature bool   `json:"supports_temperature"`
	SupportsStreaming   bool   `json:"supports_streaming"`
	// SupportsThinking was discovered but never carried here, so the phone had
	// no way to know a reasoning control was worth showing (CHANGE-077).
	SupportsThinking bool `json:"supports_thinking"`
	// The knobs this model accepts at all, so the phone can grey out the rest
	// instead of offering a setting that would be dropped on the way to the
	// provider.
	SupportsTopP             bool `json:"supports_top_p"`
	SupportsTopK             bool `json:"supports_top_k"`
	SupportsStopSequences    bool `json:"supports_stop_sequences"`
	SupportsPresencePenalty  bool `json:"supports_presence_penalty"`
	SupportsFrequencyPenalty bool `json:"supports_frequency_penalty"`
	SupportsSeed             bool `json:"supports_seed"`
}

type ProviderView struct {
	ID           string      `json:"id"`
	Name         string      `json:"name,omitempty"`
	DefaultModel string      `json:"default_model,omitempty"`
	Models       []ModelView `json:"models"`
}

// ModelChoice is the provider and model a chat runs on. Clear removes an
// explicit session pin and returns the chat to the global agent defaults.
// Sub carries the per-session sub-agent override (ACP session config pattern):
// each chat may pin its own sub provider/model instead of inheriting the
// global agent defaults.
type ModelChoice struct {
	Provider    string `json:"provider,omitempty"`
	Model       string `json:"model,omitempty"`
	Clear       bool   `json:"clear_model,omitempty"`
	SubProvider string `json:"sub_provider,omitempty"`
	SubModel    string `json:"sub_model,omitempty"`
	SubEnabled  *bool  `json:"sub_enabled,omitempty"`
	ClearSub    bool   `json:"clear_sub,omitempty"`
	// Generation is this chat's own set of knobs. A pointer field that is nil
	// means "leave whatever is stored alone"; a pointer to zero means "store
	// zero". ClearGeneration empties the whole set back to the provider defaults.
	Generation      *GenerationSettings `json:"generation,omitempty"`
	ClearGeneration bool                `json:"clear_generation,omitempty"`
	// ClearKnobs removes named settings and leaves the rest alone, which is the
	// difference between "I set the temperature to 0.5" and "take the temperature
	// away" (CHANGE-077).
	ClearKnobs []string `json:"clear_knobs,omitempty"`
}

// SessionAgentConfig is the resolved per-session agent setup: the main route
// (possibly pinned) and the sub-agent route (possibly pinned). Empty fields
// mean "follow the global default".
type SessionAgentConfig struct {
	Provider    string `json:"provider,omitempty"`
	Model       string `json:"model,omitempty"`
	Pinned      bool   `json:"pinned"`
	SubProvider string `json:"sub_provider,omitempty"`
	SubModel    string `json:"sub_model,omitempty"`
	SubEnabled  bool   `json:"sub_enabled"`
	SubPinned   bool   `json:"sub_pinned"`
	// Generation is what this chat actually runs on, and Source says whether it
	// came from the chat itself or from the agent defaults, so the phone can show
	// the difference instead of guessing which it is editing.
	Generation GenerationSettings `json:"generation"`
	Source     string             `json:"generation_source,omitempty"`
}

// ModelStore is the live half of provider configuration: what the runtime can
// actually route to, and how a chat's choice is validated and remembered.
type ModelStore interface {
	Providers(ctx context.Context) ([]ProviderView, error)
	SetSessionModel(ctx context.Context, sessionID string, choice ModelChoice) (SessionRow, error)
	SessionModel(ctx context.Context, sessionID string) (ModelChoice, bool, error)
	// ResolveAgentConfig returns the effective per-session agent config: the
	// session pin when present, else the global agent defaults. Sub-agent
	// fields follow the same rule.
	ResolveAgentConfig(ctx context.Context, sessionID string) (SessionAgentConfig, error)
}

// HistoryStore is the D1 side of the chat list: exactly what the phone needs to
// render and manage chats, and nothing else. The raw state table stays out of
// reach, so a tunnel URL can never become a reader for provider API keys.
type HistoryStore interface {
	ListSessions(ctx context.Context, limit int) ([]SessionRow, error)
	CreateSession(ctx context.Context, id, title, model string) (SessionRow, error)
	RenameSession(ctx context.Context, id, title string) (SessionRow, bool, error)
	DeleteSession(ctx context.Context, id string) (bool, error)
	Turns(ctx context.Context, sessionID string, beforeSeq int64, limit int) ([]TurnRow, error)
	AppendTurn(ctx context.Context, sessionID, role, agent, jobID, text string) (int64, error)
	Node(ctx context.Context) (NodeRow, bool, error)
}

type nodeView struct {
	TunnelURL      *string `json:"tunnel_url"`
	Version        string  `json:"version"`
	HeartbeatAgeS  int64   `json:"heartbeat_age_s"`
	Online         bool    `json:"online"`
	heartbeatFound bool
}

// NewHistoryHandler serves the phone's history endpoints from D1 through the
// tunnel, so the app needs exactly one address. Every request goes through the
// same gate as the WebSocket handshake: the phone presents the Cloudflare token
// it already stores, and the daemon uses the copy it holds in RAM.
func NewHistoryHandler(store HistoryStore, gate *Gate, models ...ModelStore) http.Handler {
	var modelStore ModelStore
	if len(models) > 0 {
		modelStore = models[0]
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "history is not configured"})
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
		path := r.URL.Path
		switch {
		case path == "/api/ping":
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "aixodia"})
		case path == "/api/node" && r.Method == http.MethodGet:
			serveNode(w, r, store)
		case path == "/api/sessions" && r.Method == http.MethodGet:
			serveSessionList(w, r, store)
		case path == "/api/sessions" && r.Method == http.MethodPost:
			serveSessionCreate(w, r, store, modelStore)
		case path == "/api/models" && r.Method == http.MethodGet:
			serveModels(w, r, modelStore)
		case strings.HasPrefix(path, "/api/sessions/"):
			serveSessionItem(w, r, store, modelStore, strings.TrimPrefix(path, "/api/sessions/"))
		default:
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		}
	})
}

func serveNode(w http.ResponseWriter, r *http.Request, store HistoryStore) {
	node, found, err := store.Node(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	view := nodeView{Version: node.Version, heartbeatFound: found}
	if found {
		age := time.Now().Unix() - node.Heartbeat
		view.HeartbeatAgeS = age
		view.Online = age >= 0 && age < 90
		view.TunnelURL = &node.TunnelURL
	}
	writeJSON(w, http.StatusOK, view)
}

func serveSessionList(w http.ResponseWriter, r *http.Request, store HistoryStore) {
	rows, err := store.ListSessions(r.Context(), 200)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if rows == nil {
		rows = []SessionRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

func serveModels(w http.ResponseWriter, r *http.Request, store ModelStore) {
	if store == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "model catalogue is not configured"})
		return
	}
	providers, err := store.Providers(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if providers == nil {
		providers = []ProviderView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": providers})
}

func serveSessionCreate(w http.ResponseWriter, r *http.Request, store HistoryStore, models ModelStore) {
	var body struct {
		ID       string `json:"id"`
		Title    string `json:"title"`
		Model    string `json:"model"`
		Provider string `json:"provider"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	id := strings.TrimSpace(body.ID)
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id required"})
		return
	}
	row, err := store.CreateSession(r.Context(), id, strings.TrimSpace(body.Title), body.Model)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	choice := ModelChoice{Provider: strings.TrimSpace(body.Provider), Model: strings.TrimSpace(body.Model)}
	if models != nil && (choice.Provider != "" || choice.Model != "") {
		row, err = models.SetSessionModel(r.Context(), id, choice)
		if err != nil {
			writeModelError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, row)
}

// serveSessionItem handles /api/sessions/<id> and /api/sessions/<id>/turns.
func serveSessionItem(w http.ResponseWriter, r *http.Request, store HistoryStore, models ModelStore, rest string) {
	parts := strings.Split(rest, "/")
	if len(parts) == 2 && parts[0] != "" && parts[1] == "turns" {
		serveTurns(w, r, store, parts[0])
		return
	}
	if len(parts) != 1 || parts[0] == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	id := parts[0]
	switch r.Method {
	case http.MethodGet:
		// The phone opens the per-chat agent sheet and needs the effective
		// config first: the session pin when it has one, otherwise the agent's
		// global defaults, so the sheet can say which of the two it is showing.
		cfg, err := models.ResolveAgentConfig(r.Context(), id)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, cfg)
	case http.MethodPatch:
		var body struct {
			Title       string `json:"title"`
			Model       string `json:"model"`
			Provider    string `json:"provider"`
			ClearModel  bool   `json:"clear_model"`
			SubProvider string `json:"sub_provider"`
			SubModel    string `json:"sub_model"`
			SubEnabled  *bool  `json:"sub_enabled"`
			ClearSub    bool   `json:"clear_sub"`

			Generation      *GenerationSettings `json:"generation"`
			ClearGeneration bool                `json:"clear_generation"`
			ClearKnobs      []string            `json:"clear_knobs"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
			return
		}
		if body.Generation != nil {
			if err := ValidateGenerationSettings(*body.Generation); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		}
		if err := ValidateCleared(body.ClearKnobs); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		choice := ModelChoice{
			Provider: strings.TrimSpace(body.Provider), Model: strings.TrimSpace(body.Model), Clear: body.ClearModel,
			SubProvider: strings.TrimSpace(body.SubProvider), SubModel: strings.TrimSpace(body.SubModel),
			SubEnabled: body.SubEnabled, ClearSub: body.ClearSub,
			Generation: body.Generation, ClearGeneration: body.ClearGeneration,
			ClearKnobs: body.ClearKnobs,
		}
		// A sub-agent-only save is a real change: it carries no main route at
		// all, and letting it fall through would answer "title required".
		if models != nil && (body.ClearModel || choice.Provider != "" || choice.Model != "" ||
			choice.SubProvider != "" || choice.SubModel != "" || choice.SubEnabled != nil || choice.ClearSub ||
			choice.Generation != nil || choice.ClearGeneration || len(choice.ClearKnobs) > 0) {
			row, err := models.SetSessionModel(r.Context(), id, choice)
			if err != nil {
				writeModelError(w, err)
				return
			}
			if strings.TrimSpace(body.Title) == "" {
				writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": row.ID, "model": row.Model, "provider": row.Provider})
				return
			}
		}
		title := strings.TrimSpace(body.Title)
		if title == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title required"})
			return
		}
		row, found, err := store.RenameSession(r.Context(), id, title)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": row.ID, "title": row.Title})
	case http.MethodDelete:
		found, err := store.DeleteSession(r.Context(), id)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	}
}

func serveTurns(w http.ResponseWriter, r *http.Request, store HistoryStore, sessionID string) {
	switch r.Method {
	case http.MethodGet:
		before, _ := strconv.ParseInt(r.URL.Query().Get("before_seq"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		rows, err := store.Turns(r.Context(), sessionID, before, limit)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if rows == nil {
			rows = []TurnRow{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"turns": rows})
	case http.MethodPost:
		var body struct {
			Role  string `json:"role"`
			Text  string `json:"text"`
			Agent string `json:"agent"`
			JobID string `json:"job_id"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
			return
		}
		role := body.Role
		if role == "" {
			role = "model"
		}
		seq, err := store.AppendTurn(r.Context(), sessionID, role, body.Agent, body.JobID, body.Text)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"seq": seq})
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	}
}

func decodeBody(r *http.Request, out any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil && err.Error() != "EOF" {
		return err
	}
	return nil
}

func writeStoreError(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
}

// writeModelError answers a rejected provider/model choice with 400, so the
// phone can say which option is wrong instead of showing a server fault.
func writeModelError(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// ---- from io/gateway/admin.go ----
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

// GenerationSettings is the set of knobs the phone may set. Every numeric field
// is a pointer or a slice so "not set" is told apart from zero: an omitted knob
// leaves the provider on its own default, whereas a zero would be an instruction
// the model obeyed (CHANGE-077).
type GenerationSettings struct {
	ThinkingLevel    string   `json:"thinking_level,omitempty"`
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"top_p,omitempty"`
	TopK             *float64 `json:"top_k,omitempty"`
	StopSequences    []string `json:"stop_sequences,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	Seed             *int64   `json:"seed,omitempty"`
	MaxOutputTokens  int      `json:"max_output_tokens,omitempty"`
}

// AgentSettings is what the main agent and the sub agent each run on.
type AgentSettings struct {
	Provider   string             `json:"provider"`
	Model      string             `json:"model"`
	Generation GenerationSettings `json:"generation"`
	// ClearKnobs names the settings to remove. A knob that was simply not sent has
	// to mean "leave what is stored alone", because a save that only picks a model
	// would otherwise quietly drop a temperature somebody set by hand in the
	// config file, or the D1 copy of it (CHANGE-077).
	ClearKnobs []string `json:"clear_knobs,omitempty"`
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
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&settings); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	// Reject a value no provider would accept before anything is written, so a
	// bad setting leaves the stored configuration exactly as it was.
	if err := ValidateGenerationSettings(settings.Main.Generation); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := ValidateGenerationSettings(settings.Sub.Generation); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	saved, err := store.SaveSettings(r.Context(), settings)
	if err != nil {
		writeAdminError(w, "settings", err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// ---- from io/gateway/generation.go ----
// The bounds a provider accepts. These mirror the ones the SDK enforces, and they
// live here as well because the transport must be able to refuse a bad request
// before it writes anything, and the SDK must be able to refuse it without the
// transport knowing about the SDK (CHANGE-077, REQ-049(1)).
const (
	minTemperature = 0.0
	maxTemperature = 2.0

	minTopP = 0.0
	maxTopP = 1.0

	minTopK = 0.0
	maxTopK = 1.0

	minPenalty = -2.0
	maxPenalty = 2.0
)

// ValidateGenerationSettings checks every knob that was sent. A field left out is
// not checked and not stored, which is how a caller clears a setting: send it
// null, and the stored value goes away.
func ValidateGenerationSettings(g GenerationSettings) error {
	if g.Temperature != nil {
		if err := inRange("temperature", *g.Temperature, minTemperature, maxTemperature); err != nil {
			return err
		}
	}
	if g.TopP != nil {
		if err := inRange("top_p", *g.TopP, minTopP, maxTopP); err != nil {
			return err
		}
	}
	if g.TopK != nil {
		if err := inRange("top_k", *g.TopK, minTopK, maxTopK); err != nil {
			return err
		}
	}
	if g.PresencePenalty != nil {
		if err := inRange("presence_penalty", *g.PresencePenalty, minPenalty, maxPenalty); err != nil {
			return err
		}
	}
	if g.FrequencyPenalty != nil {
		if err := inRange("frequency_penalty", *g.FrequencyPenalty, minPenalty, maxPenalty); err != nil {
			return err
		}
	}
	if g.MaxOutputTokens < 0 {
		return errors.New("max_output_tokens must not be negative")
	}
	if level := strings.TrimSpace(g.ThinkingLevel); level != "" {
		switch level {
		case "none", "low", "medium", "high":
		default:
			return fmt.Errorf("thinking_level must be none, low, medium or high, got %q", level)
		}
	}
	return nil
}

// IsEmpty reports whether nothing was sent, which is how a caller asks to leave
// the stored settings alone rather than clear them.
func (g GenerationSettings) IsEmpty() bool {
	return strings.TrimSpace(g.ThinkingLevel) == "" && g.Temperature == nil &&
		g.TopP == nil && g.TopK == nil && len(g.StopSequences) == 0 &&
		g.PresencePenalty == nil && g.FrequencyPenalty == nil && g.Seed == nil &&
		g.MaxOutputTokens == 0
}

func inRange(name string, v, lo, hi float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("%s must be a number", name)
	}
	if v < lo || v > hi {
		return fmt.Errorf("%s must be between %g and %g", name, lo, hi)
	}
	return nil
}

// MergeKnobNames lists every name Merge understands, so a caller can refuse one
// it does not rather than ignoring it in silence.
func MergeKnobNames() []string {
	return []string{
		"thinking_level", "temperature", "top_p", "top_k", "stop_sequences",
		"presence_penalty", "frequency_penalty", "seed", "max_output_tokens",
	}
}

// ValidateCleared refuses a knob name that would otherwise be silently dropped.
func ValidateCleared(lists ...[]string) error {
	known := map[string]bool{}
	for _, name := range MergeKnobNames() {
		known[name] = true
	}
	for _, list := range lists {
		for _, name := range list {
			if !known[name] {
				return fmt.Errorf("clear_knobs: %q is not a setting", name)
			}
		}
	}
	return nil
}

// Merge writes the knobs the caller actually sent over a stored set. A field that
// was not sent keeps the stored value; a field named in cleared is removed. This
// is what keeps "not mentioned" and "delete it" from being the same request
// (CHANGE-077).
func (g GenerationSettings) Merge(stored GenerationSettings, cleared []string) GenerationSettings {
	out := stored
	if g.ThinkingLevel != "" {
		out.ThinkingLevel = g.ThinkingLevel
	}
	if g.Temperature != nil {
		out.Temperature = g.Temperature
	}
	if g.TopP != nil {
		out.TopP = g.TopP
	}
	if g.TopK != nil {
		out.TopK = g.TopK
	}
	if len(g.StopSequences) > 0 {
		out.StopSequences = g.StopSequences
	}
	if g.PresencePenalty != nil {
		out.PresencePenalty = g.PresencePenalty
	}
	if g.FrequencyPenalty != nil {
		out.FrequencyPenalty = g.FrequencyPenalty
	}
	if g.Seed != nil {
		out.Seed = g.Seed
	}
	if g.MaxOutputTokens > 0 {
		out.MaxOutputTokens = g.MaxOutputTokens
	}
	for _, knob := range cleared {
		switch knob {
		case "thinking_level":
			out.ThinkingLevel = ""
		case "temperature":
			out.Temperature = nil
		case "top_p":
			out.TopP = nil
		case "top_k":
			out.TopK = nil
		case "stop_sequences":
			out.StopSequences = nil
		case "presence_penalty":
			out.PresencePenalty = nil
		case "frequency_penalty":
			out.FrequencyPenalty = nil
		case "seed":
			out.Seed = nil
		case "max_output_tokens":
			out.MaxOutputTokens = 0
		}
	}
	return out
}
