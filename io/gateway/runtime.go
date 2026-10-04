package gateway

import (
	"ai-engine/db"
	"ai-engine/io"
	"ai-engine/provider"
	"ai-engine/provider/registry"
	"ai-engine/session"
	"ai-engine/tools"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---- from io/gateway/mobile.go ----
// MobileRuntime owns the stateless-mobile wiring for one daemon process
// (REQ-046). It holds the single credential the system has — the D1 token a
// phone presents — in memory only, hydrates runtime state from D1 once the
// first verified connection arrives, and pushes local state back up (CON-012).
type MobileRuntime struct {
	// Transport is the WebSocket gateway; the entry point starts it last,
	// after the harness is running.
	Transport *Transport
	// Tokens is the volatile credential. The entry point adopts the verified
	// bootstrap token here at boot so the tunnel URL is announced before
	// any phone connects; every verified handshake replaces it.
	Tokens     *db.MemoryToken
	client     *db.Client
	stateRoot  string
	sessionDir string
	cfg        RuntimeMobileConfig
	// reloadProviders runs after config is materialized from D1. The runtime
	// loaded providers at boot, before any phone connected, so without this
	// the daemon would know zero providers for its whole lifetime (REQ-046(4)).
	reloadProviders func(context.Context) error
	// applySystemRoutes runs after the reload above. The provider set is
	// useless without the routes that go with it: the phone's agent defaults
	// live in config/system, and a daemon that started with empty boot env
	// would otherwise route every unpinned chat to an empty provider forever.
	applySystemRoutes func()

	// sessions applies a phone's provider/model choice to a live chat.
	sessions *session.SessionManager
}

// RuntimeMobileConfig wires the gateway. CloudflareAPI and D1Database are
// overrides for the discovery the token already provides; empty means the
// defaults. Version is the build label the handover record carries.
type RuntimeMobileConfig struct {
	CloudflareAPI string
	D1Database    string
	Listen        string
	PublicListen  string
	Tunnel        bool
	Cloudflared   string
	SyncConfig    bool
	SyncSessions  bool
	Version       string
}

// instanceID names this daemon in the handover record: the build plus the moment
// it started, which is what the successor check compares against.
func (m *MobileRuntime) instanceID() string {
	version := "daemon"
	if v := strings.TrimSpace(m.cfg.Version); v != "" && v != "dev" {
		version = v
	}
	return fmt.Sprintf("%s-%d", version, startedAt)
}

// startedAt identifies this daemon for the handover: two daemons running
// the same build are otherwise indistinguishable, and the claim is what
// tells the older one it has been replaced.
var startedAt = time.Now().UnixNano() / int64(time.Millisecond)

// ListenAddr reports the localhost address the gateway serves.
func (m *MobileRuntime) ListenAddr() string {
	if m == nil {
		return ""
	}
	return m.cfg.Listen
}

// AttachParams carries what the gateway needs beyond itself to serve: the
// live provider router, the session table, the agent, and the admin
// collaborators. The entry point builds them from the leaf packages and
// hands them over in one call, so the root stays an entry point instead of
// growing wiring.
type AttachParams struct {
	Router         *provider.Router
	Sessions       *session.SessionManager
	Agent          *session.Agent
	Manager        *registry.ProviderManager
	ProviderConfig *registry.ProviderFileConfig
	MainRoute      provider.SessionConfig
	StateRoot      string
	Client         *db.Client
}

func NewMobileRuntime(stateRoot, sessionDir string, cfg RuntimeMobileConfig, reloadProviders func(context.Context) error) (*MobileRuntime, error) {
	api := strings.TrimSpace(cfg.CloudflareAPI)
	if api == "" {
		// REQ-046(3): the daemon owns no credential; the Cloudflare token
		// arrives with the phone's handshake. An empty override means the
		// default API base, not "no D1" — returning (nil, nil) here used to
		// crash the caller with a nil dereference on secretless boot.
		api = db.DefaultAPIBase
	}
	tokens := db.NewMemoryToken()
	client := db.NewClient(api, tokens.Get)
	client.SetDatabaseName(cfg.D1Database)
	rt := &MobileRuntime{
		client:          client,
		Tokens:          tokens,
		stateRoot:       stateRoot,
		sessionDir:      sessionDir,
		cfg:             cfg,
		reloadProviders: reloadProviders,
	}
	rt.Transport = New(Config{
		// The build label travels into the handover record, so a D1 query
		// names the build that is actually serving.
		Version:      cfg.Version,
		Listen:       cfg.Listen,
		PublicListen: cfg.PublicListen,
		Tunnel:       cfg.Tunnel,
		Cloudflared:  cfg.Cloudflared,
		Tokens:       tokens,
		Verifier:     d1storeVerifier{client: client},
		Hydrate:      rt,
		History:      historyStore{client: client},
		Announce: func(ctx context.Context, publicURL string) error {
			if target, ok := client.ResolvedTarget(); ok {
				log.Printf("mobile: announcing tunnel to D1 account=%s database=%s (%s)", target.AccountID, target.Name, target.DatabaseID)
			}
			// The phone reads the tunnel table directly to find the
			// daemon (CHANGE-106), so the public URL goes there — not
			// in the nodes row.
			return client.AnnounceTunnelURL(ctx, publicURL)
		},
		Claim: func(ctx context.Context, publicURL string) error {
			return client.ClaimHandover(ctx, rt.instanceID(), publicURL, rt.Transport.Version(), startedAt)
		},
		Successor: func(ctx context.Context) (string, bool) {
			claim, replaced, err := client.Successor(ctx, startedAt)
			if err != nil {
				log.Printf("mobile: look for a successor: %v", err)
				return "", false
			}
			if !replaced {
				return "", false
			}
			return claim.Instance, true
		},
		StandDown: func(reason string) {
			log.Printf("mobile: standing down: %s", reason)
			go func() {
				// Give the log line and the tunnel a moment to flush before the
				// process goes, so whoever is watching the run sees why it ended.
				time.Sleep(2 * time.Second)
				os.Exit(0)
			}()
		},
	})
	return rt, nil
}

// modelStore answers the phone's provider/model questions from the live router
// and keeps the choice on the chat, so a restart does not lose it.
type modelStore struct {
	router   *provider.Router
	client   *db.Client
	sessions *session.SessionManager
	// workingModel is the admin store's verified model per provider, so the
	// phone's pickers default to a model that is known to answer.
	workingModel func(provider.ProviderID) string
}

// adminSettings reads the global agent defaults (config:system) from D1.
// Called by ResolveAgentConfig to fall back when a session has no pin.
func (m modelStore) adminSettings(ctx context.Context) (SettingsView, error) {
	val, found, err := m.client.Get(ctx, "config:system")
	if err != nil {
		return SettingsView{}, err
	}
	if !found {
		return SettingsView{}, nil
	}
	var cfg SettingsView
	if err := json.Unmarshal([]byte(val), &cfg); err != nil {
		return SettingsView{}, fmt.Errorf("decode config:system: %w", err)
	}
	return cfg, nil
}

func (m modelStore) Providers(context.Context) ([]ProviderView, error) {
	if m.router == nil {
		return nil, errors.New("runtime: router is not available")
	}
	out := make([]ProviderView, 0, len(m.router.ProviderIDs()))
	for _, id := range m.router.ProviderIDs() {
		view := ProviderView{ID: string(id), Name: string(id), Models: []ModelView{}}
		for _, model := range m.router.Models(id) {
			view.Models = append(view.Models, ModelView{
				ID: model.ID, Name: model.Name,
				SupportsTools: model.SupportsTools, SupportsTemperature: model.SupportsTemperature,
				SupportsStreaming: model.SupportsStreaming, SupportsThinking: model.SupportsThinking,
				SupportsTopP: model.SupportsTopP, SupportsTopK: model.SupportsTopK,
				SupportsStopSequences:    model.SupportsStopSequences,
				SupportsPresencePenalty:  model.SupportsPresencePenalty,
				SupportsFrequencyPenalty: model.SupportsFrequencyPenalty,
				SupportsSeed:             model.SupportsSeed,
			})
		}
		if len(view.Models) > 0 {
			view.DefaultModel = view.Models[0].ID
			if m.workingModel != nil {
				for _, model := range view.Models {
					if model.ID == m.workingModel(id) {
						view.DefaultModel = model.ID
						break
					}
				}
			}
		}
		out = append(out, view)
	}
	return out, nil
}

func (m modelStore) SetSessionModel(ctx context.Context, sessionID string, choice ModelChoice) (SessionRow, error) {
	if m.router == nil || m.client == nil {
		return SessionRow{}, errors.New("runtime: model routing is not available")
	}
	if choice.Clear {
		// Explicitly follow the global agent defaults again. The D1 pin is
		// removed and the cached live session is forgotten, so the next turn
		// cannot keep using the old route from memory.
		if err := m.client.SetSessionRoute(ctx, sessionID, "", ""); err != nil {
			return SessionRow{}, err
		}
		if m.sessions != nil {
			m.sessions.Forget(sessionID)
		}
		return m.sessionRow(ctx, sessionID)
	}
	mainChanged, providerLocal, model, err := sessionRouteChange(choice, m.router)
	if err != nil {
		return SessionRow{}, err
	}
	subProvider := strings.TrimSpace(choice.SubProvider)
	subModel := strings.TrimSpace(choice.SubModel)
	subEnabled := choice.SubEnabled
	if choice.ClearSub {
		subEnabled = nil
	}
	// A sub route must resolve like a main route, otherwise the worker would
	// start a turn with a route that cannot run.
	subRouteChanged := subProvider != "" || subModel != "" || choice.ClearSub
	switch {
	case subProvider != "" && subModel != "":
		if _, err := m.router.Resolve(provider.ProviderID(subProvider), subModel); err != nil {
			return SessionRow{}, fmt.Errorf("sub agent %s/%s is not available: %w", subProvider, subModel, err)
		}
	case subProvider != "" || subModel != "":
		return SessionRow{}, fmt.Errorf("sub agent %q has no model %q", subProvider, subModel)
	}
	generationChanged := choice.Generation != nil || choice.ClearGeneration
	if !mainChanged && !subRouteChanged && subEnabled == nil && !generationChanged {
		// Nothing in this request targets the session: neither the main route
		// nor the sub-agent, and no knob. Report the row instead of rewriting
		// anything.
		return m.sessionRow(ctx, sessionID)
	}
	if generationChanged {
		g := GenerationSettings{}
		if choice.Generation != nil {
			g = *choice.Generation
		}
		if err := m.applySessionGeneration(ctx, sessionID, g, choice.ClearGeneration, choice.ClearKnobs); err != nil {
			return SessionRow{}, fmt.Errorf("generation settings: %w", err)
		}
	}
	if !mainChanged && !subRouteChanged && subEnabled == nil {
		// Only the knobs were sent, so the route pins were left alone above.
		return m.sessionRow(ctx, sessionID)
	}
	if mainChanged {
		if err := m.client.SetSessionRoute(ctx, sessionID, string(providerLocal), model); err != nil {
			return SessionRow{}, err
		}
	}
	if err := m.client.SetSessionSubAgent(ctx, sessionID, subProvider, subModel, subEnabled, subRouteChanged); err != nil {
		return SessionRow{}, err
	}
	row, err := m.sessionRow(ctx, sessionID)
	if err != nil {
		return SessionRow{}, err
	}
	if m.sessions != nil {
		session, err := m.sessions.Resolve(ctx, sessionID)
		if err != nil {
			log.Printf("mobile: apply model choice to open session %s: %v", sessionID, err)
		} else {
			applySessionModel(session, providerLocal, model, m.keysFor(providerLocal))
		}
	}
	return row, nil
}

func (m modelStore) SessionModel(ctx context.Context, sessionID string) (ModelChoice, bool, error) {
	cfg, err := m.ResolveAgentConfig(ctx, sessionID)
	if err != nil {
		return ModelChoice{}, false, err
	}
	if !cfg.Pinned && !cfg.SubPinned {
		return ModelChoice{}, false, nil
	}
	return ModelChoice{
		Provider: cfg.Provider, Model: cfg.Model,
		SubProvider: cfg.SubProvider, SubModel: cfg.SubModel, SubEnabled: &cfg.SubEnabled,
	}, true, nil
}

// ResolveAgentConfig returns the effective per-session agent setup: session
// pins when present, else the global agent defaults (config:system). This is
// the ACP session-config pattern: each chat carries its own config options.
func (m modelStore) ResolveAgentConfig(ctx context.Context, sessionID string) (SessionAgentConfig, error) {
	row, found, err := m.client.GetSession(ctx, sessionID)
	if err != nil {
		return SessionAgentConfig{}, err
	}
	cfg := SessionAgentConfig{}
	if found {
		cfg.Provider = row.Provider
		cfg.Model = row.Model
		cfg.Pinned = row.Provider != "" && row.Model != ""
		cfg.SubProvider = row.SubProvider
		cfg.SubModel = row.SubModel
		if row.SubEnabled >= 0 {
			cfg.SubEnabled = row.SubEnabled == 1
		}
		cfg.SubPinned = row.SubProvider != "" && row.SubModel != ""
	}
	global, err := m.adminSettings(ctx)
	if err != nil {
		return cfg, err
	}
	if !cfg.Pinned {
		cfg.Provider = global.Main.Provider
		cfg.Model = global.Main.Model
	}
	if !cfg.SubPinned {
		cfg.SubProvider = global.Sub.Provider
		cfg.SubModel = global.Sub.Model
		cfg.SubEnabled = global.SubEnabled
	}
	// The chat's own knobs come from the live session, falling back to the global
	// agent defaults for a chat that has never been given any. Saying which one
	// answered lets the phone show the difference instead of guessing (CHANGE-077).
	if m.sessions != nil {
		if stored, ok, err := m.sessions.SessionGeneration(ctx, sessionID); err != nil {
			log.Printf("mobile: read generation for %s: %v", sessionID, err)
		} else if ok {
			if storedGeneration(stored).IsEmpty() {
				cfg.Generation = global.Main.Generation
				cfg.Source = "default"
			} else {
				cfg.Generation = fromSDK(stored)
				cfg.Source = "session"
			}
		}
	}
	if cfg.Generation.IsEmpty() {
		cfg.Source = "default"
	}
	return cfg, nil
}

// storedGeneration is the emptiness test on the SDK shape.
func storedGeneration(g provider.GenerationSettings) GenerationSettings {
	return fromSDK(g)
}

// toSDK converts the wire knobs into the SDK's own type, which is what the
// session setters validate. The conversion is the boundary where "the phone sent
// nothing" stays nil instead of becoming a zero.
func toSDK(g GenerationSettings) provider.GenerationSettings {
	return provider.GenerationSettings{
		ThinkingLevel:    provider.ThinkingLevel(strings.TrimSpace(g.ThinkingLevel)),
		Temperature:      g.Temperature,
		TopP:             g.TopP,
		TopK:             g.TopK,
		StopSequences:    g.StopSequences,
		PresencePenalty:  g.PresencePenalty,
		FrequencyPenalty: g.FrequencyPenalty,
		Seed:             g.Seed,
		MaxOutputTokens:  g.MaxOutputTokens,
	}
}

// fromSDK converts back, so the phone is told exactly what is stored rather than
// what it last sent.
func fromSDK(g provider.GenerationSettings) GenerationSettings {
	return GenerationSettings{
		ThinkingLevel:    string(g.ThinkingLevel),
		Temperature:      g.Temperature,
		TopP:             g.TopP,
		TopK:             g.TopK,
		StopSequences:    g.StopSequences,
		PresencePenalty:  g.PresencePenalty,
		FrequencyPenalty: g.FrequencyPenalty,
		Seed:             g.Seed,
		MaxOutputTokens:  g.MaxOutputTokens,
	}
}

// applySessionGeneration writes a chat's own knobs and pushes the session file to
// D1, so the setting takes effect on the next turn and survives a restart
// (CHANGE-077).
func (m modelStore) applySessionGeneration(
	ctx context.Context,
	sessionID string,
	g GenerationSettings,
	clear bool,
	clearKnobs []string,
) error {
	if m.sessions == nil {
		return errors.New("runtime: session manager is not available")
	}
	if clear {
		clearKnobs = append(clearKnobs, MergeKnobNames()...)
	}
	path, err := m.sessions.ApplyGeneration(ctx, sessionID, toSDK(g), clear, clearKnobs)
	if err != nil {
		return err
	}
	if m.client != nil {
		if _, err := m.client.PushSession(ctx, sessionID, path); err != nil {
			return fmt.Errorf("persist generation settings: %w", err)
		}
	}
	return nil
}

// sessionRouteChange decides what a ModelChoice request does to the main route.
// mainChanged is false for a sub-agent-only request, which is what keeps a
// session's main pin alive when the phone saves sub-agent settings; the router
// fills in the missing half of a half-specified route and rejects a route that
// cannot run. A request with no main fields at all leaves the pin untouched.
func sessionRouteChange(choice ModelChoice, router *provider.Router) (bool, provider.ProviderID, string, error) {
	providerLocal := provider.ProviderID(choice.Provider)
	model := choice.Model
	if providerLocal == "" && model == "" {
		return false, "", "", nil
	}
	if providerLocal == "" || model == "" {
		// One side only: fill the other from the router so a phone can send just
		// the model, or just the provider, without guessing.
		if providerLocal == "" {
			for _, id := range router.ProviderIDs() {
				if hasModel(router, id, model) {
					providerLocal = id
					break
				}
			}
		} else if models := router.Models(providerLocal); len(models) > 0 {
			model = models[0].ID
		}
	}
	if providerLocal == "" || model == "" {
		return false, "", "", fmt.Errorf("provider %q has no model %q", choice.Provider, choice.Model)
	}
	if _, err := router.Resolve(providerLocal, model); err != nil {
		return false, "", "", fmt.Errorf("%s/%s is not available: %w", providerLocal, model, err)
	}
	return true, providerLocal, model, nil
}

func (m modelStore) sessionRow(ctx context.Context, sessionID string) (SessionRow, error) {
	row, found, err := m.client.GetSession(ctx, sessionID)
	if err != nil {
		return SessionRow{}, err
	}
	if !found {
		return SessionRow{ID: sessionID}, nil
	}
	return SessionRow{
		ID: row.ID, Title: row.Title, Provider: row.Provider, Model: row.Model,
		SubProvider: row.SubProvider, SubModel: row.SubModel, SubEnabled: row.SubEnabled,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

// keysFor is the key pool the runtime holds for a provider, so a live session
// can be switched to the phone's choice with a usable key.
func (m modelStore) keysFor(providerLocal provider.ProviderID) *provider.KeyPool {
	config, err := m.router.Provider(providerLocal)
	if err != nil {
		return nil
	}
	return config.Keys
}

func hasModel(router *provider.Router, providerLocal provider.ProviderID, model string) bool {
	for _, candidate := range router.Models(providerLocal) {
		if candidate.ID == model {
			return true
		}
	}
	return false
}

func applySessionModel(session *session.Session, providerLocal provider.ProviderID, model string, keys *provider.KeyPool) {
	if err := session.SetProvider(providerLocal, keys); err != nil {
		log.Printf("mobile: set provider %s on %s: %v", providerLocal, session.ID(), err)
		return
	}
	if err := session.SetModel(model); err != nil {
		log.Printf("mobile: set model %s on %s: %v", model, session.ID(), err)
	}
}

// historyStore narrows the D1 client to what the phone's history endpoints
// need, so the transport never sees the raw state table.
type historyStore struct{ client *db.Client }

func (h historyStore) ListSessions(ctx context.Context, limit int) ([]SessionRow, error) {
	rows, err := h.client.ListSessions(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]SessionRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, SessionRow{
			ID: row.ID, Title: row.Title, Provider: row.Provider, Model: row.Model,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		})
	}
	return out, nil
}

func (h historyStore) CreateSession(ctx context.Context, id, title, model string) (SessionRow, error) {
	row, err := h.client.CreateSession(ctx, id, title, model)
	if err != nil {
		return SessionRow{}, err
	}
	return SessionRow{ID: row.ID, Title: row.Title, Provider: row.Provider, Model: row.Model,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}, nil
}

func (h historyStore) RenameSession(ctx context.Context, id, title string) (SessionRow, bool, error) {
	row, found, err := h.client.RenameSession(ctx, id, title)
	return SessionRow{ID: row.ID, Title: row.Title}, found, err
}

func (h historyStore) DeleteSession(ctx context.Context, id string) (bool, error) {
	return h.client.DeleteSession(ctx, id)
}

func (h historyStore) Turns(ctx context.Context, sessionID string, beforeSeq int64, limit int) ([]TurnRow, error) {
	rows, err := h.client.Turns(ctx, sessionID, beforeSeq, limit)
	if err != nil {
		return nil, err
	}
	out := make([]TurnRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, TurnRow{
			Seq: row.Seq, Role: row.Role, Agent: row.Agent, JobID: row.JobID,
			Text: row.Text, CreatedAt: row.CreatedAt, Model: row.Model,
			InputTokens: row.InputTokens, OutputTokens: row.OutputTokens,
			CacheRead: row.CacheRead, CacheWrite: row.CacheWrite,
			DurationMs: row.DurationMs,
		})
	}
	return out, nil
}

func (h historyStore) AppendTurn(ctx context.Context, sessionID, role, agent, jobID, text string) (int64, error) {
	return h.client.AppendTurnAt(ctx, sessionID, role, agent, jobID, text)
}

func (h historyStore) Node(ctx context.Context) (NodeRow, bool, error) {
	node, found, err := h.client.Node(ctx)
	return NodeRow{TunnelURL: node.TunnelURL, Version: node.Version, Heartbeat: node.Heartbeat}, found, err
}

// d1storeVerifier adapts the D1 client to the transport's Verifier contract:
// a wrong credential is ErrTokenRejected (counted), anything else is a
// transport problem (503, not counted).
type d1storeVerifier struct{ client *db.Client }

func (v d1storeVerifier) VerifyToken(ctx context.Context, token string) error {
	err := v.client.VerifyToken(ctx, token)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, db.ErrTokenRejected):
		return ErrTokenRejected
	default:
		return err
	}
}

// Hydrate implements Hydrator: it runs once, after the first
// phone hands over a verified token, so a daemon that started with an empty
// state directory still comes up with the operator's config and sessions.
func (m *MobileRuntime) Hydrate(ctx context.Context) error {
	if m == nil || m.client == nil {
		return nil
	}
	// The daemon writes the per-message footer on every answer, so the columns
	// it needs have to be there before the first phone turn (AX-095).
	if err := m.client.EnsureTurnFooter(ctx); err != nil {
		log.Printf("mobile: prepare turns for the message footer: %v", err)
	}
	// The per-session sub-agent columns are read on every session row, so they
	// have to exist before the first turn (CHANGE-085, CHANGE-088).
	if err := m.client.EnsureSubAgentColumns(ctx); err != nil {
		log.Printf("mobile: prepare sessions for per-session sub-agent settings: %v", err)
	}
	if m.cfg.SyncConfig {
		report, err := m.client.HydrateConfig(ctx, db.DefaultConfigFiles(m.stateRoot))
		if err != nil {
			return err
		}
		if len(report.PulledConfig) > 0 {
			log.Printf("mobile: hydrated config from D1: %s", strings.Join(report.PulledConfig, ", "))
		}
		if m.reloadProviders != nil {
			if err := m.reloadProviders(ctx); err != nil {
				return fmt.Errorf("reload providers after hydrate: %w", err)
			}
		}
		if m.applySystemRoutes != nil {
			m.applySystemRoutes()
		}
	}
	if m.cfg.SyncSessions {
		report, err := m.client.HydrateSessions(ctx, m.sessionDir, sessionDBName)
		if err != nil {
			return err
		}
		if len(report.PulledSession) > 0 {
			log.Printf("mobile: hydrated %d session(s) from D1: %s", len(report.PulledSession), strings.Join(report.PulledSession, ", "))
		}
	}
	return nil
}

// PushState uploads the current config and session db. It is best effort:
// a failure here must never interrupt a turn, so the caller only logs it.
func (m *MobileRuntime) PushState(ctx context.Context) {
	if m == nil || m.client == nil {
		return
	}
	if m.cfg.SyncConfig {
		report, err := m.client.PushConfig(ctx, db.DefaultConfigFiles(m.stateRoot))
		if err != nil {
			log.Printf("mobile: push config to D1: %v", err)
		} else if len(report.PushedConfig) > 0 {
			log.Printf("mobile: pushed config to D1: %s", strings.Join(report.PushedConfig, ", "))
		}
	}
	if m.cfg.SyncSessions {
		ids, err := listLocalSessionIDs(m.sessionDir)
		if err != nil {
			return
		}
		report, err := m.client.PushSessions(ctx, m.sessionDir, ids)
		if err != nil {
			log.Printf("mobile: push sessions to D1: %v", err)
		}
		if len(report.SkippedOversize) > 0 {
			log.Printf("mobile: session too large for D1, kept local: %s", strings.Join(report.SkippedOversize, ", "))
		}
	}
}

// sessionDBName mirrors session.SessionDBPath naming for the sync layer.
func sessionDBName(sessionID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(sessionID)) + ".db"
}

func listLocalSessionIDs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".db") {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSuffix(entry.Name(), ".db"))
		if err != nil {
			continue
		}
		ids = append(ids, string(raw))
	}
	return ids, nil
}

// mobileSessionDir is where session databases live under the state root.
func mobileSessionDir(stateRoot string) string {
	return filepath.Join(stateRoot, "data", "sessions")
}

// MobileSessionDir reports where session databases live under the state
// root. The entry point needs it before the runtime exists.
func MobileSessionDir(stateRoot string) string {
	return mobileSessionDir(stateRoot)
}

// ProviderRowsToFile renders D1 provider rows as the file shape, so the
// table can be materialized to disk through the single file writer.
func ProviderRowsToFile(rows []db.ProviderRow) registry.ProviderFileConfig {
	file := registry.ProviderFileConfig{Providers: make([]registry.ProviderFile, 0, len(rows))}
	for _, row := range rows {
		file.Providers = append(file.Providers, registry.ProviderFile{
			Name: row.Name, Adapter: row.Adapter, HTTPEndpoint: row.Endpoint,
			APIKeys:  append([]string(nil), row.APIKeys...),
			FreeOnly: row.Free,
		})
	}
	return file
}

// ProviderFileToRows renders the file shape as D1 provider rows. Index is
// assigned by PutProviders in order, so it is not set here.
func ProviderFileToRows(file registry.ProviderFileConfig) []db.ProviderRow {
	rows := make([]db.ProviderRow, 0, len(file.Providers))
	for _, p := range file.Providers {
		rows = append(rows, db.ProviderRow{
			Name: p.Name, Adapter: p.Adapter, Endpoint: p.HTTPEndpoint,
			APIKeys: append([]string(nil), p.APIKeys...),
			Free:    p.FreeOnly,
		})
	}
	return rows
}

// Attach wires the live collaborators the entry point built from the leaf
// packages — provider router, session table, agent and admin pieces — into
// the transport, and returns the single display the harness needs: the D1
// mirror that writes a finished turn before the phone sees it.
func (m *MobileRuntime) Attach(p AttachParams) io.Display {
	admin := newAdminStore(p.StateRoot, p.Client, p.Manager, p.ProviderConfig, p.Sessions, p.Agent, p.MainRoute)
	m.applySystemRoutes = admin.RefreshRoutesFromDisk
	board := newMobileIO(m)
	m.Transport.SetInputMirror(board.MirrorInput)
	m.Transport.SetModelStore(&modelStore{
		router: p.Router, client: p.Client, sessions: p.Sessions,
		workingModel: admin.WorkingModel,
	})
	m.Transport.SetAdminStore(admin)
	return board
}

// ---- from io/gateway/display.go ----
// mobileIO is the daemon's input/output boundary for the phone. One direction
// turns an inbound WebSocket event into a canonical b.Input, the other turns a
// canonical b.Output back into outbound frames.
//
// Both directions also write to D1, because the app has to be able to rebuild a
// chat after it was closed (REQ-046(5)). The wire format itself is not decided
// here: transport/mobile owns the frames and the handshake, and this type only
// bridges them to the harness.
type mobileIO struct {
	mobile *MobileRuntime

	// turns keeps one D1 row per finished turn. The sdk reports the same answer
	// twice for some providers (a content event, then the terminal one), and a
	// later turn may legitimately repeat the same text, so the key is cleared as
	// soon as the terminal event has had its say.
	turns turnMirror

	// mirrorGates serializes one session's D1 mirrors so rows always land
	// user-before-answer. The user mirror runs in a goroutine while the turn
	// runs; without the gate a fast turn's answer can steal the lower seq and
	// the phone (which numbers its pending row from its own counter) drops the
	// answer on a seq collision it can never recover from.
	mirrorMu    sync.Mutex
	mirrorGates map[string]chan struct{}
}

func newMobileIO(mobile *MobileRuntime) *mobileIO {
	return &mobileIO{mobile: mobile}
}

// Source makes this an agent.RoutedDisplay, so agent.DispatchDisplay hands it only
// the output that belongs to the phone.
func (b *mobileIO) Source() string { return SourceName }

// ---------------------------------------------------------------- inbound

// MirrorInput is the hook the transport calls for every accepted inbound turn. It
// returns the work to run off the read loop (D1 is a network call), or nil when
// there is nothing worth mirroring.
func (b *mobileIO) MirrorInput(input io.Input) func(context.Context) error {
	if b == nil || b.mobile == nil || b.mobile.client == nil || input.SessionID == "" {
		return nil
	}
	text := inputText(input)
	if text == "" {
		return nil
	}
	return func(ctx context.Context) error {
		gate := make(chan struct{})
		b.mirrorMu.Lock()
		if b.mirrorGates == nil {
			b.mirrorGates = map[string]chan struct{}{}
		}
		b.mirrorGates[input.SessionID] = gate
		b.mirrorMu.Unlock()
		seq, err := b.mobile.client.AppendTurnAt(ctx, input.SessionID, "user", "user", "", text)
		b.mirrorMu.Lock()
		if b.mirrorGates[input.SessionID] == gate {
			delete(b.mirrorGates, input.SessionID)
		}
		b.mirrorMu.Unlock()
		close(gate)
		if err != nil {
			return err
		}
		log.Printf("mobile: mirrored user turn session=%s seq=%d", input.SessionID, seq)
		return nil
	}
}

// waitUserMirror blocks until this session's user row has landed, so the answer
// mirror that follows cannot take its seq. A newer turn replaces the gate, so
// waiting on a stale one is impossible: publish always reads the current gate
// after the turn that produced it.
func (b *mobileIO) waitUserMirror(sessionID string) {
	b.mirrorMu.Lock()
	gate := b.mirrorGates[sessionID]
	b.mirrorMu.Unlock()
	if gate == nil {
		return
	}
	select {
	case <-gate:
	case <-time.After(30 * time.Second):
		log.Printf("mobile: user mirror still pending for session=%s; mirroring answer anyway", sessionID)
	}
}

// ---------------------------------------------------------------- outbound

// Display is the harness-facing sink. A finished main turn is
// mirrored into D1 before the phone is told about it, because the
// app refreshes from D1 the moment the done frame arrives — a
// display-before-mirror order makes the streamed answer vanish for
// a race window (AXCH-025). The output is then forwarded to the
// transport in every shape it arrives: deltas, tool calls,
// reasoning, a worker's progress and the final answer, so the
// phone sees the turn live, not only its ending.
func (b *mobileIO) Display(ctx context.Context, output io.Output) error {
	if b == nil || b.mobile == nil || b.mobile.Transport == nil {
		return nil
	}
	b.mirrorOutput(ctx, output)
	return b.mobile.Transport.Display(ctx, output)
}

// mirrorOutput writes one finished turn to D1. A worker's own turn
// is reported through its job frames, not as a chat message, so it
// is not mirrored. A failure here is logged and never swallows the
// answer on the phone: the turn still displays, it only does not
// survive a restart.
func (b *mobileIO) mirrorOutput(ctx context.Context, output io.Output) {
	if b == nil || b.mobile == nil || b.mobile.Transport == nil {
		return
	}
	// A worker's own turn is reported through its job, not as a chat message.
	if output.Trace != nil && strings.EqualFold(output.Metadata["trace_actor"], "subagent") {
		return
	}
	text := FinalText(output)
	if output.SessionID == "" || text == "" {
		return
	}
	// One turn in D1 per turn on the wire.
	terminal := output.Trace != nil && output.Trace.Stage == io.TraceResponse
	key := output.SessionID + "\x00" + text
	if !b.turns.take(key, terminal) {
		return
	}
	jobID := output.Metadata["mobile_job_id"]
	b.waitUserMirror(output.SessionID)

	// The footer is read off the terminal trace: the model that answered, the
	// counts it reported (including cached usage) and how long it took (AX-095).
	var meta db.TurnMeta
	usage := output.Response.Usage
	if output.Trace != nil {
		if output.Trace.Response != nil {
			meta.Model = output.Trace.Response.Model
			usage = output.Trace.Response.Usage
		}
		if output.Trace.RequestStartedMs > 0 && output.Trace.AtMs >= output.Trace.RequestStartedMs {
			meta.DurationMs = output.Trace.AtMs - output.Trace.RequestStartedMs
		} else {
			meta.DurationMs = output.Trace.Elapsed.Milliseconds()
		}
	}
	meta.InputTokens = usage.InputTokens
	meta.OutputTokens = usage.OutputTokens
	meta.CacheRead = usage.CacheReadTokens
	meta.CacheWrite = usage.CacheWriteTokens
	meta.ReasoningTokens = usage.ReasoningTokens
	meta.InputIncludesCache = usage.InputIncludesCache

	seq, err := b.mobile.client.AppendModelTurn(ctx, output.SessionID, "main", jobID, text, meta)
	if err != nil {
		log.Printf("mobile: mirror turn to D1 session=%s: %v", output.SessionID, err)
		b.turns.forget(key)
		return
	}
	log.Printf("mobile: mirrored answer session=%s seq=%d model=%s in=%d out=%d ms=%d",
		output.SessionID, seq, meta.Model, meta.InputTokens, meta.OutputTokens, meta.DurationMs)
}

// turnMirror keeps one D1 row per finished turn, keyed by session and text.
type turnMirror struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (m *turnMirror) take(key string, terminal bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen == nil {
		m.seen = map[string]bool{}
	}
	if m.seen[key] {
		if terminal {
			delete(m.seen, key)
		}
		return false
	}
	m.seen[key] = true
	if terminal {
		delete(m.seen, key)
	}
	return true
}

func (m *turnMirror) forget(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.seen, key)
}

func inputText(input io.Input) string {
	var sb strings.Builder
	for _, part := range input.Turn.Content {
		sb.WriteString(part.Text)
	}
	return strings.TrimSpace(sb.String())
}

// ---- from io/gateway/admin_store.go ----
// adminStore is the phone's write surface for provider configuration and agent
// settings. It edits the same two files the daemon already hydrates
// (config/provider.json, config/system.json) and pushes them to D1, so a restart
// or a second phone sees the same setup (REQ-046(4)).
//
// Key material only ever travels one way: the phone sends keys, the store never
// sends them back. That keeps the tunnel from becoming a reader for the key pool
// it can already write.
type adminStore struct {
	// fileMu guards the provider file edit. It is held only for the local
	// read/modify/write, never across discovery or a provider call, so one slow
	// provider cannot block the settings screen.
	fileMu sync.Mutex
	// statusMu guards the per-provider status map, which is read while building
	// responses and written after a refresh. Two locks, because a self-deadlock
	// here would take the whole admin surface with it.
	statusMu sync.RWMutex

	// status is the last probe failure per provider, probed records which
	// providers a probe has actually answered for: a provider nobody tested is
	// unknown, not healthy.
	status      map[string]string
	probed      map[string]bool
	probedModel map[string]string

	providerPath string
	systemPath   string
	stateRoot    string

	manager *registry.ProviderManager
	// providerConfig is the daemon's in-memory copy of the provider file, kept
	// in step with disk and D1 by saveProvidersLocked.
	providerConfig *registry.ProviderFileConfig
	client         *db.Client

	sessions *session.SessionManager
	agent    *session.Agent

	// mainRoute is the daemon's boot default, used when the system config leaves
	// the main provider or model empty.
	mainRoute provider.SessionConfig
}

func newAdminStore(stateRoot string, client *db.Client, manager *registry.ProviderManager,
	providerConfig *registry.ProviderFileConfig, sessions *session.SessionManager, agent *session.Agent,
	mainRoute provider.SessionConfig) *adminStore {
	return &adminStore{
		providerPath:   filepath.Join(stateRoot, "config", "provider.json"),
		systemPath:     filepath.Join(stateRoot, "config", "system.json"),
		stateRoot:      stateRoot,
		manager:        manager,
		providerConfig: providerConfig,
		client:         client,
		sessions:       sessions,
		agent:          agent,
		mainRoute:      mainRoute,
		status:         map[string]string{},
		probed:         map[string]bool{},
		probedModel:    map[string]string{},
	}
}

// probeTools is the agent's own tool set when the daemon has one, so the health
// check is a real turn in miniature. The fallback keeps the store usable in
// tests and during boot.
func (a *adminStore) probeTools() []provider.Tool {
	if a.agent != nil && a.agent.Tools != nil {
		if defs := a.agent.Tools.Definitions(); len(defs) > 0 {
			return defs
		}
	}
	return probeTool()
}

// probeTool is the small tool set a health check falls back to.
func probeTool() []provider.Tool {
	schema := func(props map[string]any, required ...string) map[string]any {
		return map[string]any{
			"type":       "object",
			"properties": props,
			"required":   required,
		}
	}
	return []provider.Tool{
		{
			Name:        "bash",
			Description: "Run a shell command in a persistent session.",
			InputSchema: schema(map[string]any{"command": map[string]any{"type": "string"}}, "command"),
		},
		{
			Name:        "read",
			Description: "Read a file from the workspace.",
			InputSchema: schema(map[string]any{"path": map[string]any{"type": "string"}}, "path"),
		},
		{
			Name:        "write",
			Description: "Write a file in the workspace.",
			InputSchema: schema(map[string]any{"path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}, "path", "content"),
		},
	}
}

// drainProbe waits for the first answer of a streamed probe. Any event with
// content means the provider answered; only a channel that ends in an error
// counts as a failure.
func drainProbe(ch <-chan provider.Event) error {
	var failure error
	for event := range ch {
		switch event.Type {
		case provider.EventError:
			if failure == nil && event.Err != nil {
				failure = event.Err
			}
		case provider.EventText, provider.EventToolCall, provider.EventReasoning:
			if event.Text != "" || event.ToolCall != nil || event.Reasoning != nil {
				return nil
			}
		}
	}
	return failure
}

// WorkingModel reports the model that answered the last successful probe, so the
// phone can default to a model that is known to work instead of the first one
// in the catalogue.
func (a *adminStore) WorkingModel(id provider.ProviderID) string {
	a.statusMu.RLock()
	defer a.statusMu.RUnlock()
	return a.probedModel[string(id)]
}

// probeModelAttempts is how many models a health check tries before it calls the
// whole provider unusable. Enough to cover a catalogue where only some models
// are served, small enough that the phone is not left waiting.
const probeModelAttempts = 3

// setStatus records the last discovery failure per provider so the app can show
// why a provider is unusable instead of an empty model list.
func (a *adminStore) setStatus(id, message string) {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	a.probed[id] = true
	if message == "" {
		delete(a.status, id)
		return
	}
	a.status[id] = message
}

// forgetStatus drops what a probe knew, so a provider whose keys just changed is
// reported as untested rather than as still healthy.
func (a *adminStore) forgetStatus(id string) {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	delete(a.status, id)
	delete(a.probed, id)
	delete(a.probedModel, id)
}

// setDiscovery records what model discovery says without claiming a probe ran:
// a gateway can list models for a key that cannot generate anything.
func (a *adminStore) setDiscovery(id, message string) {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	if message == "" {
		delete(a.status, id)
		return
	}
	a.status[id] = message
}

func (a *adminStore) lastError(id string) string {
	a.statusMu.RLock()
	defer a.statusMu.RUnlock()
	return a.status[id]
}

func (a *adminStore) wasProbed(id string) bool {
	a.statusMu.RLock()
	defer a.statusMu.RUnlock()
	return a.probed[id]
}

func (a *adminStore) Providers(context.Context) ([]ProviderStatus, error) {
	file, err := registry.LoadProviderFile(a.providerPath)
	if err != nil {
		return nil, err
	}
	out := make([]ProviderStatus, 0, len(file.Providers))
	for _, p := range file.Providers {
		out = append(out, a.statusOf(p, a.manager))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (a *adminStore) statusOf(p registry.ProviderFile, manager *registry.ProviderManager) ProviderStatus {
	view := ProviderStatus{
		ID: p.Name, Adapter: p.Adapter, Endpoint: p.HTTPEndpoint,
		FreeOnly: p.FreeOnly, KeyCount: len(p.APIKeys), LastError: a.lastError(p.Name),
		Probed: a.wasProbed(p.Name), WorkingModel: a.WorkingModel(provider.ProviderID(p.Name)),
	}
	if manager == nil || manager.Rt() == nil || manager.Rt().Router == nil {
		return view
	}
	id := provider.ProviderID(p.Name)
	view.ModelCount = len(manager.Rt().Router.Models(id))
	view.Reachable = view.LastError == "" && view.ModelCount > 0
	return view
}

func (a *adminStore) AddProvider(ctx context.Context, spec ProviderSpec) (ProviderStatus, error) {
	name := strings.TrimSpace(spec.ID)
	if name == "" {
		return ProviderStatus{}, errors.New("ต้องตั้งชื่อ provider")
	}
	adapter := strings.ToLower(strings.TrimSpace(spec.Adapter))
	switch provider.AdapterID(adapter) {
	case provider.AdapterOpenAI, provider.AdapterAnthropic, provider.AdapterGemini, provider.AdapterOpenCode:
	default:
		return ProviderStatus{}, fmt.Errorf("adapter %q ไม่รู้จัก (ใช้ openai, anthropic, gemini หรือ opencode)", spec.Adapter)
	}
	endpoint := strings.TrimRight(strings.TrimSpace(spec.Endpoint), "/")
	if !strings.HasPrefix(endpoint, "https://") {
		return ProviderStatus{}, errors.New("endpoint ต้องเป็น https://")
	}
	keys := make([]string, 0, len(spec.Keys))
	for _, k := range spec.Keys {
		if trimmed := strings.TrimSpace(k); trimmed != "" {
			keys = append(keys, trimmed)
		}
	}
	if len(keys) == 0 {
		return ProviderStatus{}, errors.New("ต้องใส่ API key อย่างน้อยหนึ่งตัว")
	}

	entry := registry.ProviderFile{
		Name: name, Adapter: adapter, HTTPEndpoint: endpoint,
		APIKeys: keys, FreeOnly: spec.FreeOnly,
	}
	a.fileMu.Lock()
	file, err := registry.LoadProviderFile(a.providerPath)
	if err != nil {
		a.fileMu.Unlock()
		return ProviderStatus{}, err
	}
	for _, p := range file.Providers {
		if strings.EqualFold(p.Name, name) {
			a.fileMu.Unlock()
			return ProviderStatus{}, fmt.Errorf("มี provider %q อยู่แล้ว", name)
		}
	}
	file.Providers = append(file.Providers, entry)
	err = a.saveProvidersLocked(ctx, file)
	a.fileMu.Unlock()
	if err != nil {
		return ProviderStatus{}, err
	}
	return a.statusOf(entry, a.manager), nil
}

func (a *adminStore) UpdateKeys(ctx context.Context, id string, change KeyChange) (ProviderStatus, error) {
	a.fileMu.Lock()
	file, err := registry.LoadProviderFile(a.providerPath)
	if err != nil {
		a.fileMu.Unlock()
		return ProviderStatus{}, err
	}
	index := -1
	for i, p := range file.Providers {
		if strings.EqualFold(p.Name, id) {
			index = i
			break
		}
	}
	if index < 0 {
		a.fileMu.Unlock()
		return ProviderStatus{}, fmt.Errorf("ไม่พบ provider %q", id)
	}
	entry := file.Providers[index]
	switch {
	case change.Replace != nil:
		keys := make([]string, 0, len(change.Replace))
		for _, k := range change.Replace {
			if trimmed := strings.TrimSpace(k); trimmed != "" {
				keys = append(keys, trimmed)
			}
		}
		if len(keys) == 0 {
			a.fileMu.Unlock()
			return ProviderStatus{}, errors.New("key pool ใหม่ว่างเปล่า")
		}
		entry.APIKeys = keys
	case change.Add != nil:
		added := 0
		for _, k := range change.Add {
			if trimmed := strings.TrimSpace(k); trimmed != "" {
				entry.APIKeys = append(entry.APIKeys, trimmed)
				added++
			}
		}
		if added == 0 {
			a.fileMu.Unlock()
			return ProviderStatus{}, errors.New("ไม่มี key ใหม่")
		}
	case change.Remove != nil:
		drop := map[int]bool{}
		for _, i := range change.Remove {
			if i >= 0 && i < len(entry.APIKeys) {
				drop[i] = true
			}
		}
		kept := make([]string, 0, len(entry.APIKeys))
		for i, k := range entry.APIKeys {
			if !drop[i] {
				kept = append(kept, k)
			}
		}
		if len(kept) == 0 {
			a.fileMu.Unlock()
			return ProviderStatus{}, errors.New("ต้องเหลือ key อย่างน้อยหนึ่งตัว")
		}
		entry.APIKeys = kept
	default:
		a.fileMu.Unlock()
		return ProviderStatus{}, errors.New("ระบุ add, remove หรือ replace")
	}
	file.Providers[index] = entry
	a.forgetStatus(entry.Name)
	err = a.saveProvidersLocked(ctx, file)
	a.fileMu.Unlock()
	if err != nil {
		return ProviderStatus{}, err
	}
	return a.statusOf(entry, a.manager), nil
}

func (a *adminStore) RemoveProvider(ctx context.Context, id string) error {
	a.fileMu.Lock()
	defer a.fileMu.Unlock()
	file, err := registry.LoadProviderFile(a.providerPath)
	if err != nil {
		return err
	}
	kept := make([]registry.ProviderFile, 0, len(file.Providers))
	for _, p := range file.Providers {
		if strings.EqualFold(p.Name, id) {
			continue
		}
		kept = append(kept, p)
	}
	if len(kept) == len(file.Providers) {
		return fmt.Errorf("ไม่พบ provider %q", id)
	}
	file.Providers = kept
	return a.saveProvidersLocked(ctx, file)
}

// RefreshProviders re-reads the provider file, re-runs model discovery and then
// asks every provider for a one-token answer. Discovery alone is not enough: a
// gateway can list models and still reject the key, or limit the free tier to
// its own app, and that is exactly what the phone needs to know before it sends
// a real turn.
//
// The checks run a few at a time under a per-provider budget. One slow gateway
// must not hold the whole answer past the time the phone (and the tunnel in
// front of it) will wait, and the phone asked about all of them at once.
func (a *adminStore) RefreshProviders(ctx context.Context) ([]ProviderStatus, error) {
	if a.manager == nil {
		return nil, errors.New("runtime: provider manager is not available")
	}
	configs, err := a.manager.Reload(ctx)
	if err != nil {
		return nil, err
	}
	slots := make(chan struct{}, probeConcurrency)
	var wg sync.WaitGroup
	for _, config := range configs {
		wg.Add(1)
		go func(config provider.ProviderConfig) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			a.check(ctx, config)
		}(config)
	}
	wg.Wait()
	return a.Providers(ctx)
}

// probeConcurrency is how many providers are checked at once, and probeBudget is
// the time one of them may take: discovery plus the models it tries. The budget
// is what keeps a dead gateway from turning one button press into a minute of
// waiting on the phone.
const (
	probeConcurrency = 4
	probeBudget      = 25 * time.Second
)

// check runs discovery and the probe for one provider and records the verdict.
func (a *adminStore) check(ctx context.Context, config provider.ProviderConfig) {
	ctx, cancel := context.WithTimeout(ctx, probeBudget)
	defer cancel()
	if err := a.manager.RefreshProvider(ctx, config.ID); err != nil {
		a.setStatus(string(config.ID), discoveryMessage(err))
		return
	}
	if err := a.probe(ctx, config); err != nil {
		a.setStatus(string(config.ID), discoveryMessage(err))
		return
	}
	a.setStatus(string(config.ID), "")
}

// RefreshProvider re-runs discovery and the one-token probe for a single provider,
// so the phone does not have to wait for every other provider to answer.
func (a *adminStore) RefreshProvider(ctx context.Context, id string) (ProviderStatus, error) {
	if a.manager == nil {
		return ProviderStatus{}, errors.New("runtime: provider manager is not available")
	}
	configs, err := a.manager.Reload(ctx)
	if err != nil {
		return ProviderStatus{}, err
	}
	var found *provider.ProviderConfig
	for i := range configs {
		if strings.EqualFold(string(configs[i].ID), id) {
			found = &configs[i]
			break
		}
	}
	if found == nil {
		return ProviderStatus{}, fmt.Errorf("ไม่พบ provider %q", id)
	}
	a.check(ctx, *found)
	list, err := a.Providers(ctx)
	if err != nil {
		return ProviderStatus{}, err
	}
	for _, p := range list {
		if strings.EqualFold(p.ID, id) {
			return p, nil
		}
	}
	return ProviderStatus{ID: id, Probed: true}, nil
}

// probe asks for the smallest possible answer, so a dead key, an exhausted quota
// or a provider that only serves its own client shows up now. A provider can
// serve some models and refuse others, so it starts with the model that answered
// last time and only then walks the catalogue.
//
// The walk stops at the first refusal that will repeat: 401, 403 and 429 are the
// provider's answer about this client or this quota, not about the next model.
// OpenCode Zen in particular answers a free-tier request that arrives from
// outside its own client with 403 FreeTierError and counts every one of those
// requests against the quota it is already refusing, so a health check that kept
// asking would make its own recovery slower.
func (a *adminStore) probe(ctx context.Context, config provider.ProviderConfig) error {
	router := a.manager.Rt()
	if router == nil || router.Router == nil || router.Client == nil {
		return errors.New("runtime: router is not available")
	}
	models := router.Router.Models(config.ID)
	if len(models) == 0 {
		return errors.New("ไม่พบโมเดลที่ใช้ได้")
	}
	attempts := probeOrder(models, a.WorkingModel(config.ID))
	if len(attempts) > probeModelAttempts {
		attempts = attempts[:probeModelAttempts]
	}
	var last error
	for _, model := range attempts {
		session := session.NewSession(provider.SessionConfig{
			ID: "provider-probe-" + string(config.ID), Provider: config.ID, Model: model.ID,
		}, config.Keys)
		// The request must stream: some gateways (OpenCode Zen's free tier)
		// refuse a non-streaming completion outright, so a non-streaming health
		// check would report a working provider as dead.
		ch, err := router.Client.Stream(ctx, session, provider.Request{
			Model:           model.ID,
			Stream:          true,
			SystemPrompt:    "You are a coding session. Answer briefly.",
			Messages:        []provider.Turn{{Role: provider.RoleUser, Content: []provider.ContentPart{{Type: provider.ContentText, Text: "ping"}}}},
			MaxOutputTokens: 16,
			// The same tools a real turn carries: a health check that is not
			// shaped like a turn can be refused by a gateway that would serve it
			// (OpenCode Zen's free tier answers such a request with 403).
			Tools: a.probeTools(),
		})
		if err == nil {
			err = drainProbe(ch)
		}
		if err == nil {
			a.setProbedModel(string(config.ID), model.ID)
			return nil
		}
		last = err
		if refusal, ok := refusalMessage(err); ok {
			return refusal
		}
	}
	if len(attempts) < len(models) {
		return fmt.Errorf("%d โมเดลแรกใช้ไม่ได้: %w", len(attempts), last)
	}
	return last
}

// probeOrder puts the model that answered last time first, then the rest of the
// catalogue in its own order. A provider that works on one model should not be
// reported dead because model #1 happens to be unavailable.
func probeOrder(models []provider.Model, working string) []provider.Model {
	if working == "" {
		return models
	}
	ordered := make([]provider.Model, 0, len(models))
	for _, m := range models {
		if m.ID == working {
			ordered = append(ordered, m)
		}
	}
	for _, m := range models {
		if m.ID != working {
			ordered = append(ordered, m)
		}
	}
	return ordered
}

// setProbedModel remembers which model answered, under the same lock the
// readers use, so a health check racing a settings read cannot tear the map.
func (a *adminStore) setProbedModel(id, model string) {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	a.probedModel[id] = model
}

// refusalMessage turns a verdict the provider will repeat into copy the phone can
// act on. The quota-based refusals are temporary by nature, so they are named as
// such instead of leaving the phone to read a raw status code as "broken".
func refusalMessage(err error) (error, bool) {
	var statusErr provider.HTTPStatusError
	if !errors.As(err, &statusErr) {
		return nil, false
	}
	switch statusErr.HTTPStatusCode() {
	case 401:
		return fmt.Errorf("key ไม่ถูกยอมรับ (401): %w", err), true
	case 403:
		return fmt.Errorf("ผู้ให้บริการปฏิเสธคำขอนี้ (403) — เช่น free tier ที่ใช้ได้เฉพาะในตัว client ของผู้ให้บริการ: %w", err), true
	case 429:
		return fmt.Errorf("โควตาของ provider หมดชั่วคราว (429) ลองใหม่อีกครั้ง: %w", err), true
	}
	return nil, false
}

// saveProvidersLocked writes the providers to the D1 providers table and
// the local file cache, then rebuilds the live routers so the change takes
// effect without a restart. The caller holds a.mu.
func (a *adminStore) saveProvidersLocked(ctx context.Context, file registry.ProviderFileConfig) error {
	if err := registry.SaveProviderFile(a.providerPath, file); err != nil {
		return err
	}
	if a.client != nil {
		if err := a.client.PutProviders(ctx, ProviderFileToRows(file)); err != nil {
			return err
		}
	}
	if a.providerConfig != nil {
		*a.providerConfig = file
	}
	if a.manager == nil {
		return nil
	}
	configs, err := a.manager.Reload(ctx)
	if err != nil {
		return err
	}
	for _, config := range configs {
		if err := a.manager.RefreshProvider(ctx, config.ID); err != nil {
			a.setDiscovery(string(config.ID), discoveryMessage(err))
			log.Printf("provider %s: discovery failed after an update: %v", config.ID, err)
		} else {
			a.setDiscovery(string(config.ID), "")
		}
	}
	if a.sessions != nil {
		a.sessions.AdoptProviders(configs, a.defaultRoute())
	}
	return nil
}

// systemGenerationToWire converts the main agent's stored knobs for the phone.
func systemGenerationToWire(g provider.GenerationSettings) GenerationSettings {
	return fromSDK(g)
}

// subGenerationToWire converts the worker agent's stored knobs for the phone.
// The worker config carries its own temperature, thinking level and output cap,
// which are the subset of the knob set that applies to a delegated turn.
func subGenerationToWire(sub session.SubAgentConfig) GenerationSettings {
	out := GenerationSettings{
		ThinkingLevel:   string(sub.ThinkingLevel),
		Temperature:     sub.Temperature,
		MaxOutputTokens: sub.MaxOutputTokens,
	}
	return out
}

func (a *adminStore) Settings(context.Context) (SettingsView, error) {
	cfg, err := session.LoadSystemConfig(a.systemPath)
	if err != nil {
		return SettingsView{}, err
	}
	view := SettingsView{
		Main: AgentSettings{
			Provider: cfg.Provider, Model: cfg.Model,
			Generation: systemGenerationToWire(cfg.Settings()),
		},
		Sub: AgentSettings{
			Provider: cfg.SubAgent.Provider, Model: cfg.SubAgent.Model,
			Generation: subGenerationToWire(cfg.SubAgent),
		},
		SubEnabled: cfg.SubAgent.Enabled,
	}
	if view.Main.Generation.MaxOutputTokens == 0 {
		view.Main.Generation.MaxOutputTokens = cfg.MaxOutputTokens
	}
	if view.Main.Provider == "" {
		view.Main.Provider = string(a.mainRoute.Provider)
	}
	if view.Main.Model == "" {
		view.Main.Model = a.mainRoute.Model
	}
	return view, nil
}

func (a *adminStore) SaveSettings(ctx context.Context, settings SettingsView) (SettingsView, error) {
	if err := a.validateRoute(settings.Main); err != nil {
		return SettingsView{}, fmt.Errorf("main agent: %w", err)
	}
	if err := a.validateRoute(settings.Sub); err != nil && strings.TrimSpace(settings.Sub.Model) != "" {
		return SettingsView{}, fmt.Errorf("sub agent: %w", err)
	}

	a.fileMu.Lock()
	defer a.fileMu.Unlock()
	cfg, err := session.LoadSystemConfig(a.systemPath)
	if err != nil {
		return SettingsView{}, err
	}
	cfg.Provider = strings.TrimSpace(settings.Main.Provider)
	cfg.Model = strings.TrimSpace(settings.Main.Model)
	cfg.SubAgent.Provider = strings.TrimSpace(settings.Sub.Provider)
	cfg.SubAgent.Model = strings.TrimSpace(settings.Sub.Model)
	// Merge, never replace: a save that only picked a model sends an empty
	// generation block, and replacing would erase a temperature somebody set by
	// hand in this file or in the D1 copy of it, without a word (CHANGE-077).
	mainKnobs := settings.Main.Generation.Merge(
		GenerationSettings{
			ThinkingLevel:    string(cfg.Settings().ThinkingLevel),
			Temperature:      cfg.Settings().Temperature,
			TopP:             cfg.Settings().TopP,
			TopK:             cfg.Settings().TopK,
			StopSequences:    cfg.Settings().StopSequences,
			PresencePenalty:  cfg.Settings().PresencePenalty,
			FrequencyPenalty: cfg.Settings().FrequencyPenalty,
			Seed:             cfg.Settings().Seed,
			MaxOutputTokens:  cfg.MaxOutputTokensFor(),
		},
		settings.Main.ClearKnobs,
	)
	cfg.Generation = toSDK(mainKnobs)
	cfg.MaxOutputTokens = mainKnobs.MaxOutputTokens
	subKnobs := settings.Sub.Generation.Merge(
		GenerationSettings{
			ThinkingLevel:   string(cfg.SubAgent.ThinkingLevel),
			Temperature:     cfg.SubAgent.Temperature,
			MaxOutputTokens: cfg.SubAgent.MaxOutputTokens,
		},
		append(settings.Sub.ClearKnobs, "top_p", "top_k", "stop_sequences", "presence_penalty", "frequency_penalty", "seed"),
	)
	// A worker inherits the parent's knobs, so the ones the worker cannot own
	// follow the main agent rather than being stranded on a value it was never told about.
	cfg.SubAgent.Temperature = subKnobs.Temperature
	cfg.SubAgent.ThinkingLevel = provider.ThinkingLevel(subKnobs.ThinkingLevel)
	cfg.SubAgent.MaxOutputTokens = subKnobs.MaxOutputTokens
	if cfg.SubAgent.MaxOutputTokens == 0 {
		cfg.SubAgent.MaxOutputTokens = mainKnobs.MaxOutputTokens
	}
	if settings.SubEnabled {
		cfg.SubAgent.Enabled = true
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return SettingsView{}, err
	}
	// One writer for this file: session.SaveSystemConfig owns the mkdir/chmod/
	// write path, so the file cannot drift between the phone's save and the
	// config loader's idea of its permissions (CHANGE-099).
	if err := session.SaveSystemConfig(a.systemPath, cfg); err != nil {
		return SettingsView{}, err
	}
	if a.client != nil {
		if err := a.client.Put(ctx, "config:system", string(raw)); err != nil {
			return SettingsView{}, err
		}
	}
	a.applySettings(cfg)
	return SettingsView{
		Main: AgentSettings{
			Provider: cfg.Provider, Model: cfg.Model,
			Generation: systemGenerationToWire(cfg.Settings()),
		},
		Sub: AgentSettings{
			Provider: cfg.SubAgent.Provider, Model: cfg.SubAgent.Model,
			Generation: subGenerationToWire(cfg.SubAgent),
		},
		SubEnabled: cfg.SubAgent.Enabled,
	}, nil
}

// RefreshRoutesFromDisk re-reads the stored agent defaults and pushes them
// into the live runtime. It runs after D1 hydrate so the routes the phone
// saved earlier keep working across restarts without anyone re-saving them.
func (a *adminStore) RefreshRoutesFromDisk() {
	if a == nil {
		return
	}
	a.fileMu.Lock()
	defer a.fileMu.Unlock()
	cfg, err := session.LoadSystemConfig(a.systemPath)
	if err != nil {
		log.Printf("admin: reroute from stored settings: %v", err)
		return
	}
	a.applySettings(cfg)
	log.Printf("admin: agent defaults now %s/%s (sub %s/%s)", cfg.Provider, cfg.Model, cfg.SubAgent.Provider, cfg.SubAgent.Model)
}

// applySettings pushes the saved routes into the live runtime: the sub agent
// takes its provider/model, and the session manager learns the main default for
// chats that have never picked one.
func (a *adminStore) applySettings(cfg session.SystemConfig) {
	if a.agent != nil {
		if cfg.SubAgent.Provider != "" {
			a.agent.SubAgentConfig.Provider = cfg.SubAgent.Provider
		}
		if cfg.SubAgent.Model != "" {
			a.agent.SubAgentConfig.Model = cfg.SubAgent.Model
		}
		a.agent.SubAgentConfig.Enabled = cfg.SubAgent.Enabled
		// The worker's own knobs, applied live so a change from the phone takes
		// effect on the next delegation rather than after a restart (CHANGE-077).
		a.agent.SubAgentConfig.Temperature = cfg.SubAgent.Temperature
		a.agent.SubAgentConfig.ThinkingLevel = cfg.SubAgent.ThinkingLevel
		a.agent.SubAgentConfig.MaxOutputTokens = cfg.SubAgent.MaxOutputTokens
	}
	if a.sessions != nil {
		route := mainRouteFrom(cfg)
		if route.Provider == "" {
			route = a.defaultRoute()
		}
		if a.manager != nil {
			a.sessions.AdoptProviders(a.manager.Rt().ProviderConfigs, route)
		}
	}
	a.mainRoute = mainRouteFrom(cfg)
}

// mainRouteFrom is the session config a stored system config implies, knobs
// included, so the boot default and a live reload describe the same thing.
func mainRouteFrom(cfg session.SystemConfig) provider.SessionConfig {
	g := cfg.Settings()
	if g.MaxOutputTokens == 0 {
		g.MaxOutputTokens = cfg.MaxOutputTokensFor()
	}
	return provider.SessionConfig{
		Provider:         provider.ProviderID(cfg.Provider),
		Model:            cfg.Model,
		ThinkingLevel:    g.ThinkingLevel,
		Temperature:      g.Temperature,
		TopP:             g.TopP,
		TopK:             g.TopK,
		StopSequences:    g.StopSequences,
		PresencePenalty:  g.PresencePenalty,
		FrequencyPenalty: g.FrequencyPenalty,
		Seed:             g.Seed,
		MaxOutputTokens:  g.MaxOutputTokens,
	}
}

func (a *adminStore) defaultRoute() provider.SessionConfig {
	return a.mainRoute
}

// validateRoute rejects a pair the router cannot serve, so the phone learns
// immediately instead of on the next turn.
func (a *adminStore) validateRoute(route AgentSettings) error {
	providerLocal := provider.ProviderID(strings.TrimSpace(route.Provider))
	model := strings.TrimSpace(route.Model)
	if providerLocal == "" || model == "" {
		return nil
	}
	if a.manager == nil || a.manager.Rt() == nil || a.manager.Rt().Router == nil {
		return errors.New("router ยังไม่พร้อม")
	}
	if _, err := a.manager.Rt().Router.Resolve(providerLocal, model); err != nil {
		return err
	}
	return nil
}

// discoveryMessage keeps the phone's copy short: the full error stays in the log.
func discoveryMessage(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	if len(msg) > 160 {
		msg = msg[:160]
	}
	return msg
}

// ---- from io/gateway/agent.go ----
// This file owns how the daemon is wired to a provider and a workspace: the
// Agent with its tool registry, the planner and worker system prompts, the
// AGENTS.md instruction files, and the root those tools are confined to.
// main.go is the entry point; none of this belongs there.

// newAgentWithWorkspaces builds the Agent with its worker tool registry and the
// per-session workspace lookup, then applies the stored sub-ag defaults.
func NewAgentWithWorkspaces(client *provider.RouterClient, workspace, state string, cfg session.SystemConfig, workspaceFor func(context.Context) string, sessions *session.SessionManager) (*session.Agent, error) {
	registry, err := tools.NewRegistry(workspace)
	if err != nil {
		return nil, err
	}
	if workspaceFor != nil {
		registry.SetWorkspaceResolver(workspaceFor)
	}
	ag := &session.Agent{Client: client, Tools: registry, Sessions: sessions}
	ag.SubAgentConfig = session.SubAgentConfig{
		Enabled:              cfg.SubAgent.Enabled,
		Provider:             cfg.SubAgent.Provider,
		Model:                cfg.SubAgent.Model,
		MaxOutputTokens:      cfg.SubAgent.MaxOutputTokens,
		Temperature:          cfg.SubAgent.Temperature,
		ThinkingLevel:        cfg.SubAgent.ThinkingLevel,
		SystemPrompt:         cfg.SubAgent.SystemPrompt,
		Workspace:            workspace,
		ReportEveryToolCalls: cfg.SubAgent.ReportEveryToolCalls,
	}
	// Leave an empty worker prompt to the SDK so CLI and SDK defaults stay aligned.
	return ag, nil
}

// StateRoot answers where the daemon's local state lives: the
// ~/.local/share/ai layout. The local files are only the materialized copy
// of the D1 state — a daemon that loses its whole state directory comes
// back once a phone connects and hands its token over.
func StateRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "", err
	}
	root := filepath.Join(home, ".local", "share", "ai")
	if err := os.MkdirAll(filepath.Join(root, "config"), 0o700); err != nil {
		return "", err
	}
	if err := os.MkdirAll(MobileSessionDir(root), 0o700); err != nil {
		return "", err
	}
	return root, nil
}

func systemPrompt(ag *session.Agent) string {
	base := ""
	if value := strings.TrimSpace(os.Getenv("AI_SYSTEM_PROMPT")); value != "" {
		base = value
	} else if state, err := StateRoot(); err == nil {
		if cfg, err := session.LoadSystemConfig(filepath.Join(state, session.DefaultSystemConfigPath)); err == nil {
			base = cfg.SystemPrompt
		}
	}
	if base == "" {
		base = defaultSystemPrompt(ag)
	}
	return base
}

// NewRequestResolver builds the resolver the harness loop calls before a
// turn: the session's route, the Main Agent prompt and the instruction
// files. The agent fills in the per-attempt tools and the planning prompt
// itself, so the request stays the minimal shape the loop needs.
func NewRequestResolver(ag *session.Agent) session.RequestResolver {
	return func(ctx context.Context, input io.Input, sess *session.Session) (provider.Request, error) {
		cfg := sess.Config()
		return provider.Request{
			Model:           cfg.Model,
			SystemPrompt:    systemPrompt(ag),
			Instructions:    instructionFiles(),
			Stream:          true,
			MaxOutputTokens: cfg.MaxOutputTokens,
		}, nil
	}
}

// instructionFiles are the project's instruction files, found the way the
// OpenCode client finds them: the global one first, then AGENTS.md from the
// working directory upwards. Only the OpenCode adapter sends them on; every
// other provider keeps the system prompt alone.
func instructionFiles() []provider.Instruction {
	var out []provider.Instruction
	add := func(path string) {
		raw, err := os.ReadFile(path)
		if err != nil || len(raw) == 0 {
			return
		}
		// A rules file that grew into a novel costs the same input tokens on
		// every turn, so the tail is dropped and said out loud in the log.
		const maxBytes = 64 * 1024
		if len(raw) > maxBytes {
			log.Printf("instructions: %s is %d bytes, using the first %d", path, len(raw), maxBytes)
			raw = raw[:maxBytes]
		}
		out = append(out, provider.Instruction{Path: path, Text: string(raw)})
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		add(filepath.Join(home, ".config", "opencode", "AGENTS.md"))
	}
	dir, err := ResolveWorkspace()
	if err != nil || dir == "" {
		return out
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	for {
		add(filepath.Join(dir, "AGENTS.md"))
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return out
}

func defaultSystemPrompt(ag *session.Agent) string {
	var b strings.Builder
	b.WriteString("You are the Main Agent: the senior engineer, not a courier. Think in this context: analyze the goal, read code and context yourself with your one read tool (read index.md first - it is the map of the project - then read only the files it names; you cannot list, search or run anything), make the design calls, and break the work into minimal ordered steps. You never write, edit, run, or browse yourself; delegate execution to the worker sub-ag, and never pass the user's raw wording through as a worker task - write every delegated task as a scoped English engineering contract (Objective, Non-goals, Authority with allowed paths/commands/forbidden actions, Expected tests, Required evidence, Acceptance criteria) with the tool budget and validation you expect. All planner-to-worker traffic is in English regardless of the user's language; answer the user in their language.\n")
	b.WriteString("Stay strictly within the user's requested goal and scope. Do not start unrelated improvements, features, cleanup, or investigations.\n")
	b.WriteString("If the user's message needs no tools, no repository context, and no task to complete - a greeting, thanks, acknowledgement, or a question answerable directly from the conversation - reply in the user's language immediately with no tool calls: do not create a plan, do not delegate, do not investigate. Planning and delegation start only when there is real work to do.\n")
	b.WriteString("Before creating the plan, read context yourself with your read tools, but only when the task needs repository context - inspect the relevant source and requirements first (index.md is the map, then read the files it names), then use what you learned to write a precise contract. For a trivial task that needs no repository context, skip that investigation and make a minimal one-step plan. Keep every plan to the fewest steps that cover the goal. You have no write, exec, list or search tools - a bash call is rejected; the worker does the hands-on work.\n")
	b.WriteString("Create one ordered execution plan. The plan is the authoritative sequence of steps. Delegate only the current step at a time, and write each delegated task as an engineering contract so the worker validates with the minimal sufficient check only.\n")
	b.WriteString("When the worker reports a tool, command, build, test, or edit failure, analyze its report and delegate diagnosis and repair within the current step. A failure is not a reason to abandon the task or move to an unrelated step.\n")
	b.WriteString("For implementation work, delegate the current plan step with `delegate_task`; the call returns control to you at once with a job id. Progress reports arrive automatically after every few completed worker tool calls - use each one for a quick scope check (over/under/off-target work): if wrong, call `delegate_stop` (it blocks until stopped) and then `delegate_message` with that job id and the corrected task; if correct, reply briefly and stop calling tools so the next report arrives on its own. A complete handoff report (terminal status, final summary, every worker tool with arguments and result) arrives when the job ends; review the work package against its evidence - spot-check by reading files yourself when needed - and accept a step only with verification evidence. Verify, don't trust - a worker loop ending is not verified success. Retry failed, blocked, or incomplete work with `delegate_message` in the same worker session, which returns a new job id whose reports arrive the same way; the same call orders new follow-on work into that session. Call `delegate_result` with verification evidence before delegating the next step. `delegate_stop` waits until the worker has actually stopped and returns its final report. `delegate_status` shows one job's db. The worker has a separate session and never communicates with the user.\n")
	b.WriteString("Only mark a step complete after verifying that its intended result is actually achieved. After the final goal is complete, stop and send the final result.\n")
	b.WriteString("After tool results, summarize briefly what you did. Match the user's language.\n")
	return b.String()
}

// ResolveWorkspace answers where the worker tools run: AI_WORKSPACE when it
// names a real directory, else the operator's home.
func ResolveWorkspace() (string, error) {
	raw := strings.TrimSpace(os.Getenv("AI_WORKSPACE"))
	if raw == "" {
		home, err := os.UserHomeDir()
		if err != nil || strings.TrimSpace(home) == "" {
			return ".", nil
		}
		return home, nil
	}
	path := expandHome(raw)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", fmt.Errorf("AI_WORKSPACE: %w", err)
	}
	return path, nil
}

func expandHome(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
			return home
		}
		return path
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func envInt(name string) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return parsed, nil
}
