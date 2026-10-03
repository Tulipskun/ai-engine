package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Tulipskun/ai-engine/runtime"
	"github.com/Tulipskun/ai-engine/sdk"
	"github.com/Tulipskun/ai-engine/transport"
)

var version = "dev"

// main runs the daemon and nothing else (REQ-047, CHANGE-059): the CLI, the
// Discord bot and every maintenance command (update/stop/uninstall/system/
// browser/discord) were removed with their transport. Runtime configuration and
// session state live in Cloudflare D1, so there is nothing left to configure
// from a local command.
func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "daemon":
			args = args[1:]
		case "version", "-version", "--version":
			fmt.Printf("ai %s\n", version)
			return
		default:
			log.Fatalf("ai: unknown command %q — this binary only runs the daemon (try `ai` or `ai daemon`)", args[0])
		}
	}
	if len(args) > 0 {
		log.Fatalf("ai: daemon takes no arguments (got %q)", args[0])
	}
	if err := runDaemon(); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

// runDaemon is the whole product surface: state root, config, provider
// catalogue, sessions, worker tools, the mobile tunnel gateway, and the harness
// loop that ties them together.
func runDaemon() error {
	state, err := stateRoot()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(state, 0o755); err != nil {
		return err
	}
	if err := os.Chdir(state); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx)
}

// stateRoot is where the local materialization of D1 state lives: config files
// and one SQLite file per session (CON-012 keeps that shape; D1 is the
// authoritative copy and this directory is a cache that can be wiped).
func stateRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, ".local", "share", "ai"), nil
	}
	return "", errors.New("ai: cannot resolve the home directory for the state root")
}

func run(ctx context.Context) error {
	state, err := stateRoot()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(state, "data"), 0o755); err != nil {
		return err
	}
	providerConfigPath := filepath.Join(state, runtime.DefaultProviderConfigPath)
	providerFile, err := runtime.LoadProviderFile(providerConfigPath)
	if err != nil {
		return err
	}
	rt, err := runtime.Load(providerConfigPath)
	if err != nil {
		return err
	}
	maxOutputTokens, err := envInt("AI_MAX_OUTPUT_TOKENS")
	if err != nil {
		return err
	}
	sessionDB := filepath.Join(state, "data", "sessions.db")
	providerID := strings.TrimSpace(os.Getenv("AI_PROVIDER"))
	if providerID == "" && len(rt.ProviderConfigs) == 1 {
		providerID = string(rt.ProviderConfigs[0].ID)
	}
	modelID := strings.TrimSpace(os.Getenv("AI_MODEL"))
	systemConfig, err := runtime.LoadSystemConfig(filepath.Join(state, runtime.DefaultSystemConfigPath))
	if err != nil {
		return err
	}
	// The boot session carries the main agent's generation settings. Without this
	// the settings lived in the config file but nothing ever copied them onto a
	// session, so a hand-edited config changed nothing (CHANGE-077).
	baseSession := sdk.SessionConfig{
		Provider:         sdk.ProviderID(providerID),
		Model:            modelID,
		ThinkingLevel:    systemConfig.Settings().ThinkingLevel,
		Temperature:      systemConfig.Settings().Temperature,
		TopP:             systemConfig.Settings().TopP,
		TopK:             systemConfig.Settings().TopK,
		StopSequences:    systemConfig.Settings().StopSequences,
		PresencePenalty:  systemConfig.Settings().PresencePenalty,
		FrequencyPenalty: systemConfig.Settings().FrequencyPenalty,
		Seed:             systemConfig.Settings().Seed,
		MaxOutputTokens:  systemConfig.MaxOutputTokensFor(),
	}
	if maxOutputTokens > 0 {
		baseSession.MaxOutputTokens = maxOutputTokens
	}
	sessions := runtime.NewSessionManagerWithProviders(sessionDB, baseSession, rt.ProviderConfigs)
	defer sessions.Close()
	providerManager := runtime.NewProviderManager(providerConfigPath, rt, providerFile)
	inputConfigPath := filepath.Join(state, transport.DefaultConfigPath)
	transportConfig, err := transport.LoadConfig(inputConfigPath)
	if err != nil {
		return err
	}
	if !transportConfig.Mobile.Enabled {
		return errors.New("mobile transport is disabled; set mobile.enabled in config/entry.json")
	}
	workspace, err := resolveWorkspace()
	if err != nil {
		return err
	}
	agent, err := newAgentWithWorkspaces(rt.Client, workspace, state, systemConfig, func(ctx context.Context) string {
		return sessions.WorkspaceFor(sdk.SessionIDFromContext(ctx))
	})
	if err != nil {
		return err
	}
	providerKeys := make(map[sdk.ProviderID]*sdk.KeyPool, len(rt.ProviderConfigs))
	for _, provider := range rt.ProviderConfigs {
		providerKeys[provider.ID] = provider.Keys
	}
	var sources []sdk.InputSource
	var displays []sdk.Display
	// Mobile transport (REQ-046/REQ-047): the daemon's only gateway. It is
	// reachable exclusively through the Cloudflare quick tunnel, authenticated
	// with the Cloudflare token a phone presents, and it hydrates runtime state
	// from D1 on the first verified connection.
	mobileRT, err := newMobileRuntime(state, mobileSessionDir(state), runtimeMobileConfig{
		cloudflareAPI: transportConfig.Mobile.CloudflareAPI,
		d1Database:    transportConfig.Mobile.D1Database,
		listen:        transportConfig.Mobile.Listen,
		publicListen:  transportConfig.Mobile.PublicListen,
		tunnel:        transportConfig.Mobile.Tunnel,
		cloudflared:   transportConfig.Mobile.Cloudflared,
		syncConfig:    transportConfig.Mobile.SyncConfig,
		syncSessions:  transportConfig.Mobile.SyncSessions,
	}, func(ctx context.Context) error {
		configs, err := providerManager.Reload(ctx)
		if err != nil {
			return err
		}
		sessions.AdoptProviders(configs, sdk.SessionConfig{Provider: sdk.ProviderID(providerID), Model: modelID})
		log.Printf("providers reloaded after D1 hydrate: %d", len(configs))
		return nil
	})
	if err != nil {
		return err
	}
	if mobileRT == nil {
		return errors.New("mobile runtime is not configured")
	}
	models := modelStore{router: rt.Router, client: mobileRT.client, sessions: sessions}
	mobileRT.sessions = sessions
	mobileRT.transport.SetModelStore(models)
	// The phone owns provider keys and which model each agent runs on. The admin
	// store writes config/provider and config/system, which is what the daemon
	// hydrates from, so a restart keeps the choices.
	admin := newAdminStore(state, mobileRT.client, providerManager, &providerFile, sessions, agent,
		sdk.SessionConfig{Provider: sdk.ProviderID(providerID), Model: modelID})
	// The catalogue's default model is the one a health check actually got an
	// answer from, so the phone does not hand the user a model the key refuses.
	models.workingModel = admin.WorkingModel
	mobileRT.transport.SetAdminStore(admin)
	mobileRT.applySystemRoutes = admin.RefreshRoutesFromDisk
	sessions.SetSessionDefaults(func(ctx context.Context, sessionID string) (sdk.ProviderID, string, bool) {
		choice, ok, err := models.SessionModel(ctx, sessionID)
		if err != nil || !ok {
			return "", "", false
		}
		return sdk.ProviderID(choice.Provider), choice.Model, true
	})
	stop, err := mobileRT.transport.StartHTTP(ctx, transportConfig.Mobile.Listen)
	if err != nil {
		return err
	}
	defer stop()
	if transportConfig.Mobile.SyncConfig || transportConfig.Mobile.SyncSessions {
		defer mobileRT.PushState(context.WithoutCancel(ctx))
	}
	// io is the input/output boundary: the transport feeds it inbound events and
	// it publishes canonical output back, mirroring both directions into D1.
	io := newMobileIO(mobileRT)
	mobileRT.transport.SetInputMirror(io.MirrorInput)
	sources = append(sources, mobileRT.transport)
	displays = append(displays, io)
	log.Printf("ai daemon ready: mobile gateway on %s (tunnel=%v d1=%s)",
		transportConfig.Mobile.Listen, transportConfig.Mobile.Tunnel, transportConfig.Mobile.D1Database)
	if len(sources) == 0 {
		return fmt.Errorf("no transports enabled; configure config/entry.json")
	}
	var loop *sdk.HarnessLoop
	inputs, err := sdk.MergeInputSources(ctx, sources...)
	if err != nil {
		return err
	}
	loop = &sdk.HarnessLoop{Agent: agent, Source: sdk.ChannelInputSource{Inputs: inputs}, ResolveSession: sessions.Resolve, BuildRequest: func(context.Context, sdk.Input, *sdk.Session) (sdk.Request, error) {
		// Stream every turn: the phone renders deltas as they arrive, and a
		// provider that cannot stream still answers through the same path.
		return sdk.Request{
			SystemPrompt:    systemPrompt(agent),
			Instructions:    instructionFiles(),
			MaxOutputTokens: maxOutputTokens,
			Stream:          true,
		}, nil
	}, Displays: displays, DisplayTimeout: 10 * time.Second, OnTurnError: func(input sdk.Input, err error) {
		if errors.Is(err, context.Canceled) {
			log.Printf("turn stopped by the phone source=%s session=%s", input.Source, input.SessionID)
			return
		}
		log.Printf("turn failed source=%s session=%s: %v", input.Source, input.SessionID, err)
		mobileRT.transport.ReportTurnError(input.SessionID, turnErrorMessage(err))
	}, ApplySessionConfig: func(sessionID string) {
		// Per-session sub-agent override wins for this turn only (ACP session
		// config pattern): resolve the session pin, else fall back to the
		// global agent defaults the admin store already loaded.
		cfg, err := models.ResolveAgentConfig(context.Background(), sessionID)
		if err != nil {
			log.Printf("mobile: resolve agent config for %s: %v", sessionID, err)
			return
		}
		agent.SubAgentConfig.Provider = cfg.SubProvider
		agent.SubAgentConfig.Model = cfg.SubModel
		agent.SubAgentConfig.Enabled = cfg.SubEnabled
	}}
	mobileRT.transport.SetCancel(func(sessionID string) bool { return loop.CancelTurn(sessionID) })
	// The phone's per-row stop: one sub agent stops without ending the turn that
	// delegated to it (REQ-048(11)).
	mobileRT.transport.SetCancelSubAgent(agent.StopSubAgent)
	// A stopped worker never reaches a terminal trace, so the phone learns about
	// it from the sub agent's final report instead (REQ-048(11)).
	agent.SetSubAgentSinks(func(event sdk.SubAgentEvent) {
		if event.Kind == "final" && event.Parent != nil {
			mobileRT.transport.SubAgentTerminal(event.Parent.ID(), event.JobID, event.Status)
		}
	}, nil)
	return loop.Run(ctx)
}

// turnErrorMessage keeps the phone's copy short: the full error stays in the log
// with the request id and provider detail.
func turnErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}
