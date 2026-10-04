package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Tulipskun/ai-engine/agent"
	"github.com/Tulipskun/ai-engine/io"
	"github.com/Tulipskun/ai-engine/io/state"
	"github.com/Tulipskun/ai-engine/provider"
	"github.com/Tulipskun/ai-engine/provider/registry"
	"github.com/Tulipskun/ai-engine/session"
)

// version is stamped at build time (-ldflags "-X main.version=...");
// "dev" marks a binary built by hand. It travels into the D1 nodes row
// so the phone's daemon card names the build that is serving.
var version = "dev"

// stateRoot answers where the daemon's local state lives: the
// ~/.local/share/ai layout REQ-011 mandates. The local files are only
// the materialized copy of the D1 state (CON-012) — a daemon that
// loses its whole state directory comes back once a phone connects and
// hands its token over.
func stateRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "", err
	}
	root := filepath.Join(home, ".local", "share", "ai")
	if err := os.MkdirAll(filepath.Join(root, "config"), 0o700); err != nil {
		return "", err
	}
	if err := os.MkdirAll(mobileSessionDir(root), 0o700); err != nil {
		return "", err
	}
	return root, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root, err := stateRoot()
	if err != nil {
		log.Fatalf("ai-engine: state root: %v", err)
	}
	sessionDir := mobileSessionDir(root)
	providerPath := filepath.Join(root, "config", "provider.json")

	// The one credential the system has (CON-012): the Cloudflare API
	// token from the environment. It is verified once against Cloudflare
	// — which also resolves the account and the D1 database from the
	// token itself (REQ-046(3)) — and then held in RAM only: never in
	// config, on disk or in a log. The first phone that connects hands
	// its own token over through the handshake and replaces this one.
	cfToken := strings.TrimSpace(os.Getenv("CF_TOKEN"))
	if cfToken == "" {
		log.Fatal("ai-engine: CF_TOKEN is not set")
	}
	tokens := state.NewMemoryToken()
	client := state.NewClient(state.DefaultAPIBase, tokens.Get)
	client.SetDatabaseName(state.DefaultDatabaseName)
	if err := client.VerifyToken(ctx, cfToken); err != nil {
		log.Fatalf("ai-engine: CF_TOKEN is invalid or inactive: %v", err)
	}
	tokens.Adopt(cfToken)
	if target, ok := client.ResolvedTarget(); ok {
		log.Printf("ai-engine: D1 ready account=%s database=%s (%s)", target.AccountID, target.Name, target.DatabaseID)
	}

	// Provider runtime: whatever the local copy holds at boot. A fresh
	// daemon starts empty and picks the operator's providers up from the
	// D1 copy once the first verified phone hydrates it (REQ-046(4)).
	providerConfig, err := registry.LoadProviderFile(providerPath)
	if err != nil {
		log.Fatalf("ai-engine: provider config: %v", err)
	}
	runtime, err := registry.Load(providerPath)
	if err != nil {
		log.Fatalf("ai-engine: provider config: %v", err)
	}
	manager := registry.NewProviderManager(providerPath, runtime, providerConfig)

	systemCfg, err := agent.LoadSystemConfig(filepath.Join(root, "config", "system.json"))
	if err != nil {
		log.Fatalf("ai-engine: system config: %v", err)
	}
	// The boot default route: what a chat with no pin of its own runs
	// on until the D1 copy of config/system says otherwise.
	baseRoute := provider.SessionConfig{Provider: provider.ProviderID(systemCfg.Provider), Model: systemCfg.Model}
	sessions := session.NewSessionManagerWithProviders(sessionDir, baseRoute, runtime.ProviderConfigs)
	// A chat's own provider/model pin lives in the D1 sessions table, so
	// a restart keeps routing to what the phone last picked (REQ-046(4)).
	sessions.SetSessionDefaults(func(ctx context.Context, id string) (provider.ProviderID, string, bool) {
		row, found, err := client.GetSession(ctx, id)
		if err != nil || !found || row.Provider == "" || row.Model == "" {
			return "", "", false
		}
		return provider.ProviderID(row.Provider), row.Model, true
	})

	workspace, err := resolveWorkspace()
	if err != nil {
		log.Fatalf("ai-engine: workspace: %v", err)
	}
	ag, err := newAgentWithWorkspaces(runtime.Client, workspace, root, systemCfg, func(ctx context.Context) string {
		// The turn's context names its session, so a chat with its own
		// workspace (REQ-038) resolves it through the session manager.
		return sessions.WorkspaceFor(session.SessionIDFromContext(ctx))
	}, sessions)
	if err != nil {
		log.Fatalf("ai-engine: agent: %v", err)
	}

	admin := newAdminStore(root, client, manager, &providerConfig, sessions, ag, baseRoute)

	mobile, err := newMobileRuntime(root, sessionDir, runtimeMobileConfig{
		listen:       "127.0.0.1:18789",
		tunnel:       true,
		cloudflared:  "cloudflared",
		syncConfig:   true,
		syncSessions: true,
	}, func(ctx context.Context) error {
		// The D1 copy of config/provider is on disk now: re-register
		// every provider and rediscover its models (REQ-046(4)).
		_, err := manager.Reload(ctx)
		return err
	})
	if err != nil {
		log.Fatalf("ai-engine: mobile runtime: %v", err)
	}
	// The verified bootstrap token is the operator's own token — the
	// same one the phone holds — so the gateway adopts it from boot:
	// the tunnel URL must be announced in the D1 nodes row before any
	// phone connects, because the phone reads that row to find the
	// tunnel in the first place. Waiting for the first handshake would
	// deadlock discovery (nobody can connect to an unannounced URL).
	// Still RAM-only, still replaced when a phone hands its token over.
	mobile.tokens.Adopt(cfToken)
	// The D1 copy of config/system is on disk now: push the agent
	// defaults and the session routes it names into the live runtime.
	mobile.applySystemRoutes = admin.RefreshRoutesFromDisk

	// The D1 mirror is the only display the harness needs: it writes a
	// finished turn to D1 before the phone sees it (AXCH-025) and
	// forwards everything — deltas, tool calls, reasoning, a worker's
	// progress and the final answer — to the transport as frames.
	board := newMobileIO(mobile)
	mobile.transport.SetInputMirror(board.MirrorInput)
	mobile.transport.SetModelStore(&modelStore{
		router:       runtime.Router,
		client:       client,
		sessions:     sessions,
		workingModel: admin.WorkingModel,
	})
	mobile.transport.SetAdminStore(admin)

	harness := &agent.HarnessLoop{
		Client: runtime.Client,
		Agent:  ag,
		Source: mobile.transport,
		ResolveSession: func(ctx context.Context, input io.Input) (*session.Session, error) {
			return sessions.Resolve(ctx, input.SessionID)
		},
		BuildRequest: buildRequest(ag),
		Displays:     []io.Display{board},
		OnTurnError: func(input io.Input, err error) {
			log.Printf("ai-engine: turn failed source=%s session=%s: %v", input.Source, input.SessionID, err)
			mobile.transport.ReportTurnError(input.SessionID, err.Error())
		},
	}
	// The phone's stop buttons: one stops the turn a session is
	// running, the other stops one worker and leaves the turn that
	// delegated to it.
	mobile.transport.SetCancel(harness.CancelTurn)
	mobile.transport.SetCancelSubAgent(ag.StopSubAgent)

	go func() {
		if err := harness.Run(ctx); err != nil && ctx.Err() == nil {
			log.Fatalf("ai-engine: harness loop: %v", err)
		}
	}()

	stopHTTP, err := mobile.transport.StartHTTP(ctx, mobile.cfg.listen)
	if err != nil {
		log.Fatal(err)
	}
	defer stopHTTP()

	log.Printf("ai-engine: gateway on %s behind a quick tunnel (tunnel URL is announced in the D1 nodes row)", mobile.cfg.listen)
	<-ctx.Done()
}

// buildRequest is the resolver the harness loop calls before a turn: the
// session's route, the Main Agent prompt and the instruction files. The
// agent fills in the per-attempt tools and the planning prompt itself, so
// the request stays the minimal shape the loop needs.
func buildRequest(ag *agent.Agent) agent.RequestResolver {
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
