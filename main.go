package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"ai-engine/db"
	"ai-engine/io"
	"ai-engine/io/gateway"
	"ai-engine/provider"
	"ai-engine/provider/registry"
	"ai-engine/session"
)

// version is stamped at build time (-ldflags "-X main.version=...");
// "dev" marks a binary built by hand. It travels into the handover record
// so a D1 query names the build that is actually serving.
var version = "dev"

// main is the composition root and nothing else: verify the one credential,
// build the leaf runtimes from their packages, hand them to the gateway,
// and run the harness loop until a signal arrives.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root, err := gateway.StateRoot()
	if err != nil {
		log.Fatalf("ai-engine: state root: %v", err)
	}
	sessionDir := gateway.MobileSessionDir(root)
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
	tokens := db.NewMemoryToken()
	client := db.NewClient(db.DefaultAPIBase, tokens.Get)
	client.SetDatabaseName(db.DefaultDatabaseName)
	if err := client.VerifyToken(ctx, cfToken); err != nil {
		log.Fatalf("ai-engine: CF_TOKEN is invalid or inactive: %v", err)
	}
	tokens.Adopt(cfToken)
	if target, ok := client.ResolvedTarget(); ok {
		log.Printf("ai-engine: D1 ready account=%s database=%s (%s)", target.AccountID, target.Name, target.DatabaseID)
	}

	// Provider runtime comes from the D1 providers table; the local file
	// is only the materialized cache the loader reads.
	if err := client.EnsureProvidersTable(ctx); err != nil {
		log.Fatalf("ai-engine: ensure providers table: %v", err)
	}
	rows, err := client.ListProviders(ctx)
	if err != nil {
		log.Fatalf("ai-engine: list providers from D1: %v", err)
	}
	if len(rows) == 0 {
		rows = seedProvidersFromBlob(ctx, client)
	}
	if len(rows) > 0 {
		file := gateway.ProviderRowsToFile(rows)
		if err := registry.SaveProviderFile(providerPath, file); err != nil {
			log.Fatalf("ai-engine: materialize provider config: %v", err)
		}
		log.Printf("ai-engine: provider runtime from D1 providers table (%d providers)", len(rows))
	}
	providerConfig, err := registry.LoadProviderFile(providerPath)
	if err != nil {
		log.Fatalf("ai-engine: provider config: %v", err)
	}
	runtime, err := registry.Load(providerPath)
	if err != nil {
		log.Fatalf("ai-engine: provider config: %v", err)
	}
	manager := registry.NewProviderManager(providerPath, runtime, providerConfig)

	systemCfg, err := session.LoadSystemConfig(filepath.Join(root, "config", "system.json"))
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

	workspace, err := gateway.ResolveWorkspace()
	if err != nil {
		log.Fatalf("ai-engine: workspace: %v", err)
	}
	ag, err := gateway.NewAgentWithWorkspaces(runtime.Client, workspace, root, systemCfg, func(ctx context.Context) string {
		// The turn's context names its session, so a chat with its own
		// workspace (REQ-038) resolves it through the session manager.
		return sessions.WorkspaceFor(session.SessionIDFromContext(ctx))
	}, sessions)
	if err != nil {
		log.Fatalf("ai-engine: agent: %v", err)
	}

	mobile, err := gateway.NewMobileRuntime(root, sessionDir, gateway.RuntimeMobileConfig{
		Listen:       "127.0.0.1:18789",
		Tunnel:       true,
		Cloudflared:  "cloudflared",
		SyncConfig:   true,
		SyncSessions: true,
		Version:      version,
	}, func(ctx context.Context) error {
		// The D1 copy of the providers is in the table now: re-register
		// every provider and rediscover its models (REQ-046(4)).
		_, err := manager.Reload(ctx)
		return err
	})
	if err != nil {
		log.Fatalf("ai-engine: mobile runtime: %v", err)
	}
	// The verified bootstrap token is the operator's own token — the
	// same one the phone holds — so the gateway adopts it from boot:
	// the tunnel URL must be announced in the D1 tunnel table before
	// any phone connects, because the phone reads that table to find
	// the tunnel in the first place. Still RAM-only, still replaced
	// when a phone hands its token over.
	mobile.Tokens.Adopt(cfToken)
	// One call wires the live collaborators into the transport and
	// returns the single display the harness needs.
	board := mobile.Attach(gateway.AttachParams{
		Router:         runtime.Router,
		Sessions:       sessions,
		Agent:          ag,
		Manager:        manager,
		ProviderConfig: &providerConfig,
		MainRoute:      baseRoute,
		StateRoot:      root,
		Client:         client,
	})

	harness := &session.HarnessLoop{
		Client: runtime.Client,
		Agent:  ag,
		Source: mobile.Transport,
		ResolveSession: func(ctx context.Context, input io.Input) (*session.Session, error) {
			return sessions.Resolve(ctx, input.SessionID)
		},
		BuildRequest: gateway.NewRequestResolver(ag),
		Displays:     []io.Display{board},
		OnTurnError: func(input io.Input, err error) {
			log.Printf("ai-engine: turn failed source=%s session=%s: %v", input.Source, input.SessionID, err)
			mobile.Transport.ReportTurnError(input.SessionID, err.Error())
		},
	}
	// The phone's stop buttons: one stops the turn a session is
	// running, the other stops one worker and leaves the turn that
	// delegated to it.
	mobile.Transport.SetCancel(harness.CancelTurn)
	mobile.Transport.SetCancelSubAgent(ag.StopSubAgent)

	go func() {
		if err := harness.Run(ctx); err != nil && ctx.Err() == nil {
			log.Fatalf("ai-engine: harness loop: %v", err)
		}
	}()

	stopHTTP, err := mobile.Transport.StartHTTP(ctx, mobile.ListenAddr())
	if err != nil {
		log.Fatal(err)
	}
	defer stopHTTP()

	log.Printf("ai-engine: gateway on %s behind a quick tunnel (tunnel URL is announced in the D1 tunnel table)", mobile.ListenAddr())
	<-ctx.Done()
}

// seedProvidersFromBlob moves the retired config:provider blob into the
// providers table once, on the first boot after the move. The blob itself
// is left alone: the migration only reads, so a failed seed can always be
// retried on the next boot.
func seedProvidersFromBlob(ctx context.Context, client *db.Client) []db.ProviderRow {
	raw, found, err := client.Get(ctx, "config:provider")
	if err != nil || !found {
		return nil
	}
	var file registry.ProviderFileConfig
	if err := json.Unmarshal([]byte(raw), &file); err != nil || len(file.Providers) == 0 {
		return nil
	}
	rows := gateway.ProviderFileToRows(file)
	if err := client.PutProviders(ctx, rows); err != nil {
		log.Printf("ai-engine: seed providers table: %v", err)
		return nil
	}
	log.Printf("ai-engine: seeded providers table from config:provider (%d providers)", len(rows))
	return rows
}
