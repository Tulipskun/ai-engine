package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Tulipskun/ai-engine/runtime"
	"github.com/Tulipskun/ai-engine/runtime/d1store"
	"github.com/Tulipskun/ai-engine/sdk"
	mobiletransport "github.com/Tulipskun/ai-engine/transport/mobile"
)

// mobileRuntime owns the stateless-mobile wiring for one daemon process
// (REQ-046). It holds the single credential the system has — the D1 token a
// phone presents — in memory only, hydrates runtime state from D1 once the
// first verified connection arrives, and pushes local state back up (CON-012).
type mobileRuntime struct {
	transport  *mobiletransport.Transport
	client     *d1store.Client
	tokens     *d1store.MemoryToken
	stateRoot  string
	sessionDir string
	cfg        runtimeMobileConfig
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
	sessions *runtime.SessionManager

	turns turnMirror

	// mirrorGates serializes one session's D1 mirrors so rows always land
	// user-before-answer. The user mirror runs in a goroutine while the turn
	// runs; without the gate a fast turn's answer can steal the lower seq and
	// the phone (which numbers its pending row from its own counter) drops the
	// answer on a seq collision it can never recover from.
	mirrorMu    sync.Mutex
	mirrorGates map[string]chan struct{}
}

type runtimeMobileConfig struct {
	cloudflareAPI string
	d1Database    string
	listen        string
	publicListen  string
	tunnel        bool
	cloudflared   string
	syncConfig    bool
	syncSessions  bool
}

func newMobileRuntime(stateRoot, sessionDir string, cfg runtimeMobileConfig, reloadProviders func(context.Context) error) (*mobileRuntime, error) {
	api := strings.TrimSpace(cfg.cloudflareAPI)
	if api == "" {
		// REQ-046(3): the daemon owns no credential; the Cloudflare token
		// arrives with the phone's handshake. An empty override means the
		// default API base, not "no D1" — returning (nil, nil) here used to
		// crash the caller with a nil dereference on secretless boot.
		api = d1store.DefaultAPIBase
	}
	tokens := d1store.NewMemoryToken()
	client := d1store.NewClient(api, tokens.Get)
	client.SetDatabaseName(cfg.d1Database)
	rt := &mobileRuntime{
		client:          client,
		tokens:          tokens,
		stateRoot:       stateRoot,
		sessionDir:      sessionDir,
		cfg:             cfg,
		reloadProviders: reloadProviders,
	}
	rt.transport = mobiletransport.New(mobiletransport.Config{
		// The build label travels into the D1 nodes row, so the phone's daemon
		// card and a D1 query both name the build that is actually serving.
		Version:      version,
		MirrorInput:  rt.mirrorUserTurn,
		Listen:       cfg.listen,
		PublicListen: cfg.publicListen,
		Tunnel:       cfg.tunnel,
		Cloudflared:  cfg.cloudflared,
		Tokens:       tokens,
		Verifier:     d1storeVerifier{client: client},
		Hydrate:      rt,
		History:      historyStore{client: client},
		Announce: func(ctx context.Context, publicURL string) error {
			if target, ok := client.ResolvedTarget(); ok {
				log.Printf("mobile: announcing tunnel to D1 account=%s database=%s (%s)", target.AccountID, target.Name, target.DatabaseID)
			}
			return client.Heartbeat(ctx, publicURL, rt.transport.Version())
		},
	})
	return rt, nil
}

// modelStore answers the phone's provider/model questions from the live router
// and keeps the choice on the chat, so a restart does not lose it.
type modelStore struct {
	router   *sdk.Router
	client   *d1store.Client
	sessions *runtime.SessionManager
	// workingModel is the admin store's verified model per provider, so the
	// phone's pickers default to a model that is known to answer.
	workingModel func(sdk.ProviderID) string
}

// adminSettings reads the global agent defaults (config:system) from D1.
// Called by ResolveAgentConfig to fall back when a session has no pin.
func (m modelStore) adminSettings(ctx context.Context) (mobiletransport.SettingsView, error) {
	val, found, err := m.client.Get(ctx, "config:system")
	if err != nil {
		return mobiletransport.SettingsView{}, err
	}
	if !found {
		return mobiletransport.SettingsView{}, nil
	}
	var cfg mobiletransport.SettingsView
	if err := json.Unmarshal([]byte(val), &cfg); err != nil {
		return mobiletransport.SettingsView{}, fmt.Errorf("decode config:system: %w", err)
	}
	return cfg, nil
}

func (m modelStore) Providers(context.Context) ([]mobiletransport.ProviderView, error) {
	if m.router == nil {
		return nil, errors.New("runtime: router is not available")
	}
	out := make([]mobiletransport.ProviderView, 0, len(m.router.ProviderIDs()))
	for _, id := range m.router.ProviderIDs() {
		view := mobiletransport.ProviderView{ID: string(id), Name: string(id), Models: []mobiletransport.ModelView{}}
		for _, model := range m.router.Models(id) {
			view.Models = append(view.Models, mobiletransport.ModelView{
				ID: model.ID, Name: model.Name,
				SupportsTools: model.SupportsTools, SupportsTemperature: model.SupportsTemperature,
				SupportsStreaming: model.SupportsStreaming, SupportsThinking: model.SupportsThinking,
				SupportsTopP: model.SupportsTopP, SupportsTopK: model.SupportsTopK,
				SupportsStopSequences: model.SupportsStopSequences,
				SupportsPresencePenalty: model.SupportsPresencePenalty,
				SupportsFrequencyPenalty: model.SupportsFrequencyPenalty,
				SupportsSeed: model.SupportsSeed,
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

func (m modelStore) SetSessionModel(ctx context.Context, sessionID string, choice mobiletransport.ModelChoice) (mobiletransport.SessionRow, error) {
	if m.router == nil || m.client == nil {
		return mobiletransport.SessionRow{}, errors.New("runtime: model routing is not available")
	}
	if choice.Clear {
		// Explicitly follow the global agent defaults again. The D1 pin is
		// removed and the cached live session is forgotten, so the next turn
		// cannot keep using the old route from memory.
		if err := m.client.SetSessionRoute(ctx, sessionID, "", ""); err != nil {
			return mobiletransport.SessionRow{}, err
		}
		if m.sessions != nil {
			m.sessions.Forget(sessionID)
		}
		return m.sessionRow(ctx, sessionID)
	}
	mainChanged, provider, model, err := sessionRouteChange(choice, m.router)
	if err != nil {
		return mobiletransport.SessionRow{}, err
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
		if _, err := m.router.Resolve(sdk.ProviderID(subProvider), subModel); err != nil {
			return mobiletransport.SessionRow{}, fmt.Errorf("sub agent %s/%s is not available: %w", subProvider, subModel, err)
		}
	case subProvider != "" || subModel != "":
		return mobiletransport.SessionRow{}, fmt.Errorf("sub agent %q has no model %q", subProvider, subModel)
	}
	if !mainChanged && !subRouteChanged && subEnabled == nil {
		// Nothing in this request targets the session: neither the main route
		// nor the sub-agent. Report the row instead of rewriting the pins.
		return m.sessionRow(ctx, sessionID)
	}
	if mainChanged {
		if err := m.client.SetSessionRoute(ctx, sessionID, string(provider), model); err != nil {
			return mobiletransport.SessionRow{}, err
		}
	}
	if err := m.client.SetSessionSubAgent(ctx, sessionID, subProvider, subModel, subEnabled, subRouteChanged); err != nil {
		return mobiletransport.SessionRow{}, err
	}
	row, err := m.sessionRow(ctx, sessionID)
	if err != nil {
		return mobiletransport.SessionRow{}, err
	}
	if m.sessions != nil {
		session, err := m.sessions.Resolve(ctx, sdk.Input{SessionID: sessionID})
		if err != nil {
			log.Printf("mobile: apply model choice to open session %s: %v", sessionID, err)
		} else {
			applySessionModel(session, provider, model, m.keysFor(provider))
		}
	}
	return row, nil
}

func (m modelStore) SessionModel(ctx context.Context, sessionID string) (mobiletransport.ModelChoice, bool, error) {
	cfg, err := m.ResolveAgentConfig(ctx, sessionID)
	if err != nil {
		return mobiletransport.ModelChoice{}, false, err
	}
	if !cfg.Pinned && !cfg.SubPinned {
		return mobiletransport.ModelChoice{}, false, nil
	}
	return mobiletransport.ModelChoice{
		Provider: cfg.Provider, Model: cfg.Model,
		SubProvider: cfg.SubProvider, SubModel: cfg.SubModel, SubEnabled: &cfg.SubEnabled,
	}, true, nil
}

// ResolveAgentConfig returns the effective per-session agent setup: session
// pins when present, else the global agent defaults (config:system). This is
// the ACP session-config pattern: each chat carries its own config options.
func (m modelStore) ResolveAgentConfig(ctx context.Context, sessionID string) (mobiletransport.SessionAgentConfig, error) {
	row, found, err := m.client.GetSession(ctx, sessionID)
	if err != nil {
		return mobiletransport.SessionAgentConfig{}, err
	}
	cfg := mobiletransport.SessionAgentConfig{}
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
	return cfg, nil
}

// sessionRouteChange decides what a ModelChoice request does to the main route.
// mainChanged is false for a sub-agent-only request, which is what keeps a
// session's main pin alive when the phone saves sub-agent settings; the router
// fills in the missing half of a half-specified route and rejects a route that
// cannot run. A request with no main fields at all leaves the pin untouched.
func sessionRouteChange(choice mobiletransport.ModelChoice, router *sdk.Router) (bool, sdk.ProviderID, string, error) {
	provider := sdk.ProviderID(choice.Provider)
	model := choice.Model
	if provider == "" && model == "" {
		return false, "", "", nil
	}
	if provider == "" || model == "" {
		// One side only: fill the other from the router so a phone can send just
		// the model, or just the provider, without guessing.
		if provider == "" {
			for _, id := range router.ProviderIDs() {
				if hasModel(router, id, model) {
					provider = id
					break
				}
			}
		} else if models := router.Models(provider); len(models) > 0 {
			model = models[0].ID
		}
	}
	if provider == "" || model == "" {
		return false, "", "", fmt.Errorf("provider %q has no model %q", choice.Provider, choice.Model)
	}
	if _, err := router.Resolve(provider, model); err != nil {
		return false, "", "", fmt.Errorf("%s/%s is not available: %w", provider, model, err)
	}
	return true, provider, model, nil
}

func (m modelStore) sessionRow(ctx context.Context, sessionID string) (mobiletransport.SessionRow, error) {
	row, found, err := m.client.GetSession(ctx, sessionID)
	if err != nil {
		return mobiletransport.SessionRow{}, err
	}
	if !found {
		return mobiletransport.SessionRow{ID: sessionID}, nil
	}
	return mobiletransport.SessionRow{
		ID: row.ID, Title: row.Title, Provider: row.Provider, Model: row.Model,
		SubProvider: row.SubProvider, SubModel: row.SubModel, SubEnabled: row.SubEnabled,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

// keysFor is the key pool the runtime holds for a provider, so a live session
// can be switched to the phone's choice with a usable key.
func (m modelStore) keysFor(provider sdk.ProviderID) *sdk.KeyPool {
	config, err := m.router.Provider(provider)
	if err != nil {
		return nil
	}
	return config.Keys
}

func hasModel(router *sdk.Router, provider sdk.ProviderID, model string) bool {
	for _, candidate := range router.Models(provider) {
		if candidate.ID == model {
			return true
		}
	}
	return false
}

func applySessionModel(session *sdk.Session, provider sdk.ProviderID, model string, keys *sdk.KeyPool) {
	if err := session.SetProvider(provider, keys); err != nil {
		log.Printf("mobile: set provider %s on %s: %v", provider, session.ID(), err)
		return
	}
	if err := session.SetModel(model); err != nil {
		log.Printf("mobile: set model %s on %s: %v", model, session.ID(), err)
	}
}

// historyStore narrows the D1 client to what the phone's history endpoints
// need, so the transport never sees the raw state table.
type historyStore struct{ client *d1store.Client }

func (h historyStore) ListSessions(ctx context.Context, limit int) ([]mobiletransport.SessionRow, error) {
	rows, err := h.client.ListSessions(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]mobiletransport.SessionRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, mobiletransport.SessionRow{
			ID: row.ID, Title: row.Title, Provider: row.Provider, Model: row.Model,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		})
	}
	return out, nil
}

func (h historyStore) CreateSession(ctx context.Context, id, title, model string) (mobiletransport.SessionRow, error) {
	row, err := h.client.CreateSession(ctx, id, title, model)
	if err != nil {
		return mobiletransport.SessionRow{}, err
	}
	return mobiletransport.SessionRow{ID: row.ID, Title: row.Title, Provider: row.Provider, Model: row.Model,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}, nil
}

func (h historyStore) RenameSession(ctx context.Context, id, title string) (mobiletransport.SessionRow, bool, error) {
	row, found, err := h.client.RenameSession(ctx, id, title)
	return mobiletransport.SessionRow{ID: row.ID, Title: row.Title}, found, err
}

func (h historyStore) DeleteSession(ctx context.Context, id string) (bool, error) {
	return h.client.DeleteSession(ctx, id)
}

func (h historyStore) Turns(ctx context.Context, sessionID string, beforeSeq int64, limit int) ([]mobiletransport.TurnRow, error) {
	rows, err := h.client.Turns(ctx, sessionID, beforeSeq, limit)
	if err != nil {
		return nil, err
	}
	out := make([]mobiletransport.TurnRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, mobiletransport.TurnRow{
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

func (h historyStore) Node(ctx context.Context) (mobiletransport.NodeRow, bool, error) {
	node, found, err := h.client.Node(ctx)
	return mobiletransport.NodeRow{TunnelURL: node.TunnelURL, Version: node.Version, Heartbeat: node.Heartbeat}, found, err
}

// d1storeVerifier adapts the D1 client to the transport's Verifier contract:
// a wrong credential is ErrTokenRejected (counted), anything else is a
// transport problem (503, not counted).
type d1storeVerifier struct{ client *d1store.Client }

func (v d1storeVerifier) VerifyToken(ctx context.Context, token string) error {
	err := v.client.VerifyToken(ctx, token)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, d1store.ErrTokenRejected):
		return mobiletransport.ErrTokenRejected
	default:
		return err
	}
}

// Hydrate implements mobiletransport.Hydrator: it runs once, after the first
// phone hands over a verified token, so a daemon that started with an empty
// state directory still comes up with the operator's config and sessions.
func (m *mobileRuntime) Hydrate(ctx context.Context) error {
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
	if m.cfg.syncConfig {
		report, err := m.client.HydrateConfig(ctx, d1store.DefaultConfigFiles(m.stateRoot))
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
	if m.cfg.syncSessions {
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

// PushState uploads the current config and session state. It is best effort:
// a failure here must never interrupt a turn, so the caller only logs it.
func (m *mobileRuntime) PushState(ctx context.Context) {
	if m == nil || m.client == nil {
		return
	}
	if m.cfg.syncConfig {
		report, err := m.client.PushConfig(ctx, d1store.DefaultConfigFiles(m.stateRoot))
		if err != nil {
			log.Printf("mobile: push config to D1: %v", err)
		} else if len(report.PushedConfig) > 0 {
			log.Printf("mobile: pushed config to D1: %s", strings.Join(report.PushedConfig, ", "))
		}
	}
	if m.cfg.syncSessions {
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

// PublishOutput mirrors a finished turn into D1 and then sends it to the
// phones watching that session, so the phone can rebuild the thread after the
// app was closed (REQ-046(5)). D1 is written FIRST: the phone refreshes
// from D1 the moment the done frame arrives, so a display-before-mirror order
// makes the streamed answer vanish for a race window (AXCH-025).
func (m *mobileRuntime) PublishOutput(ctx context.Context, output sdk.Output) {
	if m == nil || m.transport == nil {
		return
	}
	if output.Trace != nil && strings.EqualFold(output.Metadata["trace_actor"], "subagent") {
		return
	}
	text := mobiletransport.FinalText(output)
	if output.SessionID == "" || text == "" {
		return
	}
	// One turn in D1 per turn on the wire. The same answer is reported twice for
	// some providers: once as the content event and again on the terminal one,
	// so the second report only clears the bookkeeping.
	terminal := output.Trace != nil && output.Trace.Stage == sdk.TraceResponse
	key := output.SessionID + "\x00" + text
	if !m.turns.take(key, terminal) {
		return
	}
	jobID := output.Metadata["mobile_job_id"]
	// Wait for this turn's user row first: D1 numbers rows MAX+1, so an answer
	// that lands before its user row steals the lower seq and scrambles the
	// order every client reconstructs. On timeout the answer still goes out
	// (and says so in the log) rather than holding the turn hostage to D1.
	m.waitUserMirror(output.SessionID)
	// The footer is read off the terminal trace: the model that answered, the
	// counts it reported (including cached usage) and how long it took (AX-095).
	var meta d1store.TurnMeta
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
	seq, err := m.client.AppendModelTurn(ctx, output.SessionID, "main", jobID, text, meta)
	if err != nil {
		log.Printf("mobile: mirror turn to D1 session=%s: %v", output.SessionID, err)
		m.turns.forget(key)
		return
	}
	log.Printf("mobile: mirrored answer session=%s seq=%d model=%s in=%d out=%d ms=%d",
		output.SessionID, seq, meta.Model, meta.InputTokens, meta.OutputTokens, meta.DurationMs)
	if err := m.transport.Display(ctx, output); err != nil {
		log.Printf("mobile: display source=%s session=%s: %v", output.Source, output.SessionID, err)
	}
}

// turnMirror keeps one D1 row per finished turn. The sdk reports the same answer
// twice for some providers (a content event, then the terminal one), and a later
// turn may legitimately repeat the same text, so the key is cleared as soon as
// the terminal event has had its say.
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

func (m *mobileRuntime) mirrorUserTurn(input sdk.Input) func(context.Context) error {
	if m == nil || m.client == nil || input.SessionID == "" {
		return nil
	}
	text := inputText(input)
	if text == "" {
		return nil
	}
	return func(ctx context.Context) error {
		gate := make(chan struct{})
		m.mirrorMu.Lock()
		if m.mirrorGates == nil {
			m.mirrorGates = map[string]chan struct{}{}
		}
		m.mirrorGates[input.SessionID] = gate
		m.mirrorMu.Unlock()
		seq, err := m.client.AppendTurnAt(ctx, input.SessionID, "user", "user", "", text)
		m.mirrorMu.Lock()
		if m.mirrorGates[input.SessionID] == gate {
			delete(m.mirrorGates, input.SessionID)
		}
		m.mirrorMu.Unlock()
		close(gate)
		if err != nil {
			return err
		}
		log.Printf("mobile: mirrored user turn session=%s seq=%d", input.SessionID, seq)
		return nil
	}
}

// waitUserMirror blocks until this session's user row has landed, so the
// answer mirror that follows cannot take its seq. A newer turn replaces the
// gate, so waiting on a stale one is impossible: PublishOutput always reads
// the current gate after the turn that produced it.
func (m *mobileRuntime) waitUserMirror(sessionID string) {
	m.mirrorMu.Lock()
	gate := m.mirrorGates[sessionID]
	m.mirrorMu.Unlock()
	if gate == nil {
		return
	}
	select {
	case <-gate:
	case <-time.After(30 * time.Second):
		log.Printf("mobile: user mirror still pending for session=%s; mirroring answer anyway", sessionID)
	}
}

func inputText(input sdk.Input) string {
	var b strings.Builder
	for _, part := range input.Turn.Content {
		b.WriteString(part.Text)
	}
	return strings.TrimSpace(b.String())
}

// sessionDBName mirrors sdk.SessionDBPath naming for the sync layer.
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
