package mobile

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

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
		}
		if err := decodeBody(r, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
			return
		}
		choice := ModelChoice{
			Provider: strings.TrimSpace(body.Provider), Model: strings.TrimSpace(body.Model), Clear: body.ClearModel,
			SubProvider: strings.TrimSpace(body.SubProvider), SubModel: strings.TrimSpace(body.SubModel),
			SubEnabled: body.SubEnabled, ClearSub: body.ClearSub,
		}
		// A sub-agent-only save is a real change: it carries no main route at
		// all, and letting it fall through would answer "title required".
		if models != nil && (body.ClearModel || choice.Provider != "" || choice.Model != "" ||
			choice.SubProvider != "" || choice.SubModel != "" || choice.SubEnabled != nil || choice.ClearSub) {
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
