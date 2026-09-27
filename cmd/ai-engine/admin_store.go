package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Tulipskun/ai-engine/runtime"
	"github.com/Tulipskun/ai-engine/runtime/d1store"
	"github.com/Tulipskun/ai-engine/sdk"
	mobiletransport "github.com/Tulipskun/ai-engine/transport/mobile"
)

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

	manager *runtime.ProviderManager
	// providerConfig is the daemon's in-memory copy of the provider file, kept
	// in step with disk and D1 by saveProvidersLocked.
	providerConfig *runtime.ProviderFileConfig
	client         *d1store.Client

	sessions *runtime.SessionManager
	agent    *sdk.Agent

	// mainRoute is the daemon's boot default, used when the system config leaves
	// the main provider or model empty.
	mainRoute sdk.SessionConfig
}

func newAdminStore(stateRoot string, client *d1store.Client, manager *runtime.ProviderManager,
	providerConfig *runtime.ProviderFileConfig, sessions *runtime.SessionManager, agent *sdk.Agent,
	mainRoute sdk.SessionConfig) *adminStore {
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
func (a *adminStore) probeTools() []sdk.Tool {
	if a.agent != nil && a.agent.Tools != nil {
		if defs := a.agent.Tools.Definitions(); len(defs) > 0 {
			return defs
		}
	}
	return probeTool()
}

// probeTool is the small tool set a health check falls back to.
func probeTool() []sdk.Tool {
	schema := func(props map[string]any, required ...string) map[string]any {
		return map[string]any{
			"type":       "object",
			"properties": props,
			"required":   required,
		}
	}
	return []sdk.Tool{
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
func drainProbe(ch <-chan sdk.Event) error {
	var failure error
	for event := range ch {
		switch event.Type {
		case sdk.EventError:
			if failure == nil && event.Err != nil {
				failure = event.Err
			}
		case sdk.EventText, sdk.EventToolCall, sdk.EventReasoning:
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
func (a *adminStore) WorkingModel(id sdk.ProviderID) string {
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

func (a *adminStore) Providers(context.Context) ([]mobiletransport.ProviderStatus, error) {
	file, err := runtime.LoadProviderFile(a.providerPath)
	if err != nil {
		return nil, err
	}
	out := make([]mobiletransport.ProviderStatus, 0, len(file.Providers))
	for _, p := range file.Providers {
		out = append(out, a.statusOf(p, a.manager))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (a *adminStore) statusOf(p runtime.ProviderFile, manager *runtime.ProviderManager) mobiletransport.ProviderStatus {
	view := mobiletransport.ProviderStatus{
		ID: p.Name, Adapter: p.Adapter, Endpoint: p.HTTPEndpoint,
		FreeOnly: p.FreeOnly, KeyCount: len(p.APIKeys), LastError: a.lastError(p.Name),
		Probed: a.wasProbed(p.Name), WorkingModel: a.WorkingModel(sdk.ProviderID(p.Name)),
	}
	if manager == nil || manager.Rt() == nil || manager.Rt().Router == nil {
		return view
	}
	id := sdk.ProviderID(p.Name)
	view.ModelCount = len(manager.Rt().Router.Models(id))
	view.Reachable = view.LastError == "" && view.ModelCount > 0
	return view
}

func (a *adminStore) AddProvider(ctx context.Context, spec mobiletransport.ProviderSpec) (mobiletransport.ProviderStatus, error) {
	name := strings.TrimSpace(spec.ID)
	if name == "" {
		return mobiletransport.ProviderStatus{}, errors.New("ต้องตั้งชื่อ provider")
	}
	adapter := strings.ToLower(strings.TrimSpace(spec.Adapter))
	switch sdk.AdapterID(adapter) {
	case sdk.AdapterOpenAI, sdk.AdapterAnthropic, sdk.AdapterGemini, sdk.AdapterOpenCode:
	default:
		return mobiletransport.ProviderStatus{}, fmt.Errorf("adapter %q ไม่รู้จัก (ใช้ openai, anthropic, gemini หรือ opencode)", spec.Adapter)
	}
	endpoint := strings.TrimRight(strings.TrimSpace(spec.Endpoint), "/")
	if !strings.HasPrefix(endpoint, "https://") {
		return mobiletransport.ProviderStatus{}, errors.New("endpoint ต้องเป็น https://")
	}
	keys := make([]string, 0, len(spec.Keys))
	for _, k := range spec.Keys {
		if trimmed := strings.TrimSpace(k); trimmed != "" {
			keys = append(keys, trimmed)
		}
	}
	if len(keys) == 0 {
		return mobiletransport.ProviderStatus{}, errors.New("ต้องใส่ API key อย่างน้อยหนึ่งตัว")
	}

	entry := runtime.ProviderFile{
		Name: name, Adapter: adapter, HTTPEndpoint: endpoint,
		APIKeys: keys, FreeOnly: spec.FreeOnly,
	}
	a.fileMu.Lock()
	file, err := runtime.LoadProviderFile(a.providerPath)
	if err != nil {
		a.fileMu.Unlock()
		return mobiletransport.ProviderStatus{}, err
	}
	for _, p := range file.Providers {
		if strings.EqualFold(p.Name, name) {
			a.fileMu.Unlock()
			return mobiletransport.ProviderStatus{}, fmt.Errorf("มี provider %q อยู่แล้ว", name)
		}
	}
	file.Providers = append(file.Providers, entry)
	err = a.saveProvidersLocked(ctx, file)
	a.fileMu.Unlock()
	if err != nil {
		return mobiletransport.ProviderStatus{}, err
	}
	return a.statusOf(entry, a.manager), nil
}

func (a *adminStore) UpdateKeys(ctx context.Context, id string, change mobiletransport.KeyChange) (mobiletransport.ProviderStatus, error) {
	a.fileMu.Lock()
	file, err := runtime.LoadProviderFile(a.providerPath)
	if err != nil {
		a.fileMu.Unlock()
		return mobiletransport.ProviderStatus{}, err
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
		return mobiletransport.ProviderStatus{}, fmt.Errorf("ไม่พบ provider %q", id)
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
			return mobiletransport.ProviderStatus{}, errors.New("key pool ใหม่ว่างเปล่า")
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
			return mobiletransport.ProviderStatus{}, errors.New("ไม่มี key ใหม่")
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
			return mobiletransport.ProviderStatus{}, errors.New("ต้องเหลือ key อย่างน้อยหนึ่งตัว")
		}
		entry.APIKeys = kept
	default:
		a.fileMu.Unlock()
		return mobiletransport.ProviderStatus{}, errors.New("ระบุ add, remove หรือ replace")
	}
	file.Providers[index] = entry
	a.forgetStatus(entry.Name)
	err = a.saveProvidersLocked(ctx, file)
	a.fileMu.Unlock()
	if err != nil {
		return mobiletransport.ProviderStatus{}, err
	}
	return a.statusOf(entry, a.manager), nil
}

func (a *adminStore) RemoveProvider(ctx context.Context, id string) error {
	a.fileMu.Lock()
	defer a.fileMu.Unlock()
	file, err := runtime.LoadProviderFile(a.providerPath)
	if err != nil {
		return err
	}
	kept := make([]runtime.ProviderFile, 0, len(file.Providers))
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
func (a *adminStore) RefreshProviders(ctx context.Context) ([]mobiletransport.ProviderStatus, error) {
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
		go func(config sdk.ProviderConfig) {
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
func (a *adminStore) check(ctx context.Context, config sdk.ProviderConfig) {
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
func (a *adminStore) RefreshProvider(ctx context.Context, id string) (mobiletransport.ProviderStatus, error) {
	if a.manager == nil {
		return mobiletransport.ProviderStatus{}, errors.New("runtime: provider manager is not available")
	}
	configs, err := a.manager.Reload(ctx)
	if err != nil {
		return mobiletransport.ProviderStatus{}, err
	}
	var found *sdk.ProviderConfig
	for i := range configs {
		if strings.EqualFold(string(configs[i].ID), id) {
			found = &configs[i]
			break
		}
	}
	if found == nil {
		return mobiletransport.ProviderStatus{}, fmt.Errorf("ไม่พบ provider %q", id)
	}
	a.check(ctx, *found)
	list, err := a.Providers(ctx)
	if err != nil {
		return mobiletransport.ProviderStatus{}, err
	}
	for _, p := range list {
		if strings.EqualFold(p.ID, id) {
			return p, nil
		}
	}
	return mobiletransport.ProviderStatus{ID: id, Probed: true}, nil
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
func (a *adminStore) probe(ctx context.Context, config sdk.ProviderConfig) error {
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
		session := sdk.NewSession(sdk.SessionConfig{
			ID: "provider-probe-" + string(config.ID), Provider: config.ID, Model: model.ID,
		}, config.Keys)
		// The request must stream: some gateways (OpenCode Zen's free tier)
		// refuse a non-streaming completion outright, so a non-streaming health
		// check would report a working provider as dead.
		ch, err := router.Client.Stream(ctx, session, sdk.Request{
			Model:           model.ID,
			Stream:          true,
			SystemPrompt:    "You are a coding agent. Answer briefly.",
			Messages:        []sdk.Turn{{Role: sdk.RoleUser, Content: []sdk.ContentPart{{Type: sdk.ContentText, Text: "ping"}}}},
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
func probeOrder(models []sdk.Model, working string) []sdk.Model {
	if working == "" {
		return models
	}
	ordered := make([]sdk.Model, 0, len(models))
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
	var statusErr sdk.HTTPStatusError
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

// saveProvidersLocked writes the file, syncs it to D1, and rebuilds the live
// routers so the change takes effect without a restart. The caller holds a.mu.
func (a *adminStore) saveProvidersLocked(ctx context.Context, file runtime.ProviderFileConfig) error {
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.providerPath), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(a.providerPath, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if a.client != nil {
		if err := a.client.Put(ctx, "config:provider", string(raw)); err != nil {
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

func (a *adminStore) Settings(context.Context) (mobiletransport.SettingsView, error) {
	cfg, err := runtime.LoadSystemConfig(a.systemPath)
	if err != nil {
		return mobiletransport.SettingsView{}, err
	}
	view := mobiletransport.SettingsView{
		Main:       mobiletransport.AgentSettings{Provider: cfg.Provider, Model: cfg.Model},
		Sub:        mobiletransport.AgentSettings{Provider: cfg.SubAgent.Provider, Model: cfg.SubAgent.Model},
		SubEnabled: cfg.SubAgent.Enabled,
	}
	if view.Main.Provider == "" {
		view.Main.Provider = string(a.mainRoute.Provider)
	}
	if view.Main.Model == "" {
		view.Main.Model = a.mainRoute.Model
	}
	return view, nil
}

func (a *adminStore) SaveSettings(ctx context.Context, settings mobiletransport.SettingsView) (mobiletransport.SettingsView, error) {
	if err := a.validateRoute(settings.Main); err != nil {
		return mobiletransport.SettingsView{}, fmt.Errorf("main agent: %w", err)
	}
	if err := a.validateRoute(settings.Sub); err != nil && strings.TrimSpace(settings.Sub.Model) != "" {
		return mobiletransport.SettingsView{}, fmt.Errorf("sub agent: %w", err)
	}

	a.fileMu.Lock()
	defer a.fileMu.Unlock()
	cfg, err := runtime.LoadSystemConfig(a.systemPath)
	if err != nil {
		return mobiletransport.SettingsView{}, err
	}
	cfg.Provider = strings.TrimSpace(settings.Main.Provider)
	cfg.Model = strings.TrimSpace(settings.Main.Model)
	cfg.SubAgent.Provider = strings.TrimSpace(settings.Sub.Provider)
	cfg.SubAgent.Model = strings.TrimSpace(settings.Sub.Model)
	if settings.SubEnabled {
		cfg.SubAgent.Enabled = true
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return mobiletransport.SettingsView{}, err
	}
	if err := os.MkdirAll(filepath.Dir(a.systemPath), 0o700); err != nil {
		return mobiletransport.SettingsView{}, err
	}
	if err := os.WriteFile(a.systemPath, append(raw, '\n'), 0o600); err != nil {
		return mobiletransport.SettingsView{}, err
	}
	if a.client != nil {
		if err := a.client.Put(ctx, "config:system", string(raw)); err != nil {
			return mobiletransport.SettingsView{}, err
		}
	}
	a.applySettings(cfg)
	return mobiletransport.SettingsView{
		Main:       mobiletransport.AgentSettings{Provider: cfg.Provider, Model: cfg.Model},
		Sub:        mobiletransport.AgentSettings{Provider: cfg.SubAgent.Provider, Model: cfg.SubAgent.Model},
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
	cfg, err := runtime.LoadSystemConfig(a.systemPath)
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
func (a *adminStore) applySettings(cfg runtime.SystemConfig) {
	if a.agent != nil {
		if cfg.SubAgent.Provider != "" {
			a.agent.SubAgentConfig.Provider = cfg.SubAgent.Provider
		}
		if cfg.SubAgent.Model != "" {
			a.agent.SubAgentConfig.Model = cfg.SubAgent.Model
		}
		a.agent.SubAgentConfig.Enabled = cfg.SubAgent.Enabled
	}
	if a.sessions != nil {
		route := sdk.SessionConfig{Provider: sdk.ProviderID(cfg.Provider), Model: cfg.Model}
		if route.Provider == "" {
			route = a.defaultRoute()
		}
		if a.manager != nil {
			a.sessions.AdoptProviders(a.manager.Rt().ProviderConfigs, route)
		}
	}
	a.mainRoute = sdk.SessionConfig{Provider: sdk.ProviderID(cfg.Provider), Model: cfg.Model}
}

func (a *adminStore) defaultRoute() sdk.SessionConfig {
	return a.mainRoute
}

// validateRoute rejects a pair the router cannot serve, so the phone learns
// immediately instead of on the next turn.
func (a *adminStore) validateRoute(route mobiletransport.AgentSettings) error {
	provider := sdk.ProviderID(strings.TrimSpace(route.Provider))
	model := strings.TrimSpace(route.Model)
	if provider == "" || model == "" {
		return nil
	}
	if a.manager == nil || a.manager.Rt() == nil || a.manager.Rt().Router == nil {
		return errors.New("router ยังไม่พร้อม")
	}
	if _, err := a.manager.Rt().Router.Resolve(provider, model); err != nil {
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
