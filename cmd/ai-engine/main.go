package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Tulipskun/ai-engine/runtime"
	"github.com/Tulipskun/ai-engine/sdk"
	"github.com/Tulipskun/ai-engine/tools"
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
	sessions := runtime.NewSessionManagerWithProviders(sessionDB, sdk.SessionConfig{Provider: sdk.ProviderID(providerID), Model: modelID}, rt.ProviderConfigs)
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
	agent, err := newAgentWithWorkspaces(rt.Client, workspace, state, func(ctx context.Context) string {
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
	sources = append(sources, mobileRT.transport)
	displays = append(displays, mobileDisplayAdapter{mobile: mobileRT})
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

func newAgent(client *sdk.RouterClient, workspace, state string) (*sdk.Agent, error) {
	return newAgentWithWorkspaces(client, workspace, state, nil)
}

func newAgentWithWorkspaces(client *sdk.RouterClient, workspace, state string, workspaceFor func(context.Context) string) (*sdk.Agent, error) {
	registry, err := tools.NewRegistry(workspace)
	if err != nil {
		return nil, err
	}
	if workspaceFor != nil {
		registry.SetWorkspaceResolver(workspaceFor)
	}
	agent := &sdk.Agent{Client: client, Tools: registry}
	cfg, err := runtime.LoadSystemConfig(filepath.Join(state, runtime.DefaultSystemConfigPath))
	if err != nil {
		return nil, err
	}
	agent.SubAgentConfig = sdk.SubAgentConfig{
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
	return agent, nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func systemPrompt(agent *sdk.Agent) string {
	base := ""
	if value := strings.TrimSpace(os.Getenv("AI_SYSTEM_PROMPT")); value != "" {
		base = value
	} else if state, err := stateRoot(); err == nil {
		if cfg, err := runtime.LoadSystemConfig(filepath.Join(state, runtime.DefaultSystemConfigPath)); err == nil {
			base = cfg.SystemPrompt
		}
	}
	if base == "" {
		base = defaultSystemPrompt(agent)
	}
	workspace, err := resolveWorkspace()
	if err == nil {
		if requirements := projectRequirements(workspace); requirements != "" {
			base += "\n\nProject Requirements (repository source of truth):\n" + requirements
		}
	}
	return base
}

// instructionFiles are the project's instruction files, found the way the
// OpenCode client finds them: the global one first, then AGENTS.md from the
// working directory upwards. Only the OpenCode adapter sends them on; every
// other provider keeps the system prompt alone.
func instructionFiles() []sdk.Instruction {
	var out []sdk.Instruction
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
		out = append(out, sdk.Instruction{Path: path, Text: string(raw)})
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		add(filepath.Join(home, ".config", "opencode", "AGENTS.md"))
	}
	dir, err := resolveWorkspace()
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

func systemPromptSource() string {
	if state, err := stateRoot(); err == nil {
		path := filepath.Join(state, runtime.DefaultSystemConfigPath)
		if cfg, err := runtime.LoadSystemConfig(path); err == nil && cfg.SystemPrompt != "" {
			return path
		}
	}
	return "built-in default"
}

func defaultSystemPrompt(agent *sdk.Agent) string {
	var b strings.Builder
	b.WriteString("You are the Main Agent: the senior engineer, not a courier. Think in this context: analyze the goal, read code and context yourself with your one read tool (read index.md first - it is the map of the project - then read only the files it names; you cannot list, search or run anything), make the design calls, and break the work into minimal ordered steps. You never write, edit, run, or browse yourself; delegate execution to the worker sub-agent, and never pass the user's raw wording through as a worker task - write every delegated task as a scoped English engineering contract (Objective, Non-goals, Authority with allowed paths/commands/forbidden actions, Expected tests, Required evidence, Acceptance criteria) with the tool budget and validation you expect. All planner-to-worker traffic is in English regardless of the user's language; answer the user in their language.\n")
	b.WriteString("Stay strictly within the user's requested goal and scope. Do not start unrelated improvements, features, cleanup, or investigations.\n")
	b.WriteString("If the user's message needs no tools, no repository context, and no task to complete - a greeting, thanks, acknowledgement, or a question answerable directly from the conversation - reply in the user's language immediately with no tool calls: do not create a plan, do not delegate, do not investigate. Planning and delegation start only when there is real work to do.\n")
	b.WriteString("Before creating the plan, read context yourself with your read tools, but only when the task needs repository context - inspect the relevant source and requirements first (index.md is the map, then read the files it names), then use what you learned to write a precise contract. For a trivial task that needs no repository context, skip that investigation and make a minimal one-step plan. Keep every plan to the fewest steps that cover the goal. You have no write, exec, list or search tools - a bash call is rejected; the worker does the hands-on work.\n")
	b.WriteString("Create one ordered execution plan. The plan is the authoritative sequence of steps. Delegate only the current step at a time, and write each delegated task as an engineering contract so the worker validates with the minimal sufficient check only.\n")
	b.WriteString("When the worker reports a tool, command, build, test, or edit failure, analyze its report and delegate diagnosis and repair within the current step. A failure is not a reason to abandon the task or move to an unrelated step.\n")
	b.WriteString("For implementation work, delegate the current plan step to `delegate_to_subagent`; the call returns control to you at once with a job id. Progress reports arrive automatically after every few completed worker tool calls - use each one for a quick scope check (over/under/off-target work): if wrong, call `stop_subagent` (it blocks until stopped) and then `follow_up_subagent` with the corrected task; if correct, reply briefly and stop calling tools so the next report arrives on its own. A complete handoff report (terminal status, final summary, every worker tool with arguments and result) arrives when the job ends; review the work package against its evidence - spot-check by reading files yourself when needed - and accept a step only with verification evidence. Verify, don't trust - there is no status or history polling. Retry failed, blocked, or incomplete work using `follow_up_subagent` in the same worker session. Order new follow-on work into the same worker session with `continue_subagent`. Call `accept_subagent_result` with verification evidence before delegating the next step. `stop_subagent` waits until the worker has actually stopped and returns its final report. The worker has a separate session and never communicates with the user.\n")
	b.WriteString("Only mark a step complete after verifying that its intended result is actually achieved. After the final goal is complete, stop and send the final result.\n")
	b.WriteString("After tool results, summarize briefly what you did. Match the user's language.\n")
	return b.String()
}

func resolveWorkspace() (string, error) {
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
