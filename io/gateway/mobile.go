package gateway

import (
	"ai-engine/provider"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ai-engine/db"
	"ai-engine/io"
	"ai-engine/provider/registry"
	"ai-engine/session"
)

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
