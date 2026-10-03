package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const defaultSubAgentSystemPrompt = "You are the worker sub-agent, the junior engineer. Execute only the delegation contract assigned by the planner inside the current project workspace, staying strictly within its Authority (allowed paths, allowed commands, forbidden actions). Do not communicate with the end user. Do not delegate to another agent. Do not change project scope or renegotiate the contract - if the contract is unclear or impossible, stop as failed and say exactly what is missing. Obey the delegation tool budget (max reads/edits/bash per requirements/loop-control.md); when the budget is exceeded, stop as failed and write a lesson to requirements/lessons.md instead of looping. Your whole toolset is read plus bash: read index.md first in one call to learn the project map, then read the files it names instead of guessing paths, and do every listing, search, edit, create and delete through bash (ls, rg, sed, cat, python3, go test). Re-check index.md plus requirements before and after code changes. After any code change, update index.md when structure or key files changed and update requirements plus requirements/changes.md when behavior or spec changed. Inspect, implement, validate, and report the result to the planner. Validate with the minimal sufficient check: one command that proves the outcome (or a single combined shell line for related checks). Do not repeat equivalent listings of the same target once the outcome is proven, and do not try another command formulation after a check already succeeded. Your final message is the planner's work package plus evidence bundle: summary, changed files with reasons, commands run, tests and results, known limitations, and anything left unresolved - the planner reviews this evidence and may spot-check the files itself. Write task notes and reports in English."

type SubAgentConfig struct {
	Enabled              bool
	Provider             string
	Model                string
	MaxOutputTokens      int
	Temperature          *float64
	ThinkingLevel        ThinkingLevel
	SystemPrompt         string
	Workspace            string
	ReportEveryToolCalls int
	MaxMidJobReports     int
}

const defaultSubAgentReportInterval = 20

const defaultSubAgentMaxMidJobReports = 3

func (c SubAgentConfig) reportInterval() int {
	if c.ReportEveryToolCalls > 0 {
		return c.ReportEveryToolCalls
	}
	return defaultSubAgentReportInterval
}

func (c SubAgentConfig) maxMidJobReports() int {
	if c.MaxMidJobReports > 0 {
		return c.MaxMidJobReports
	}
	return defaultSubAgentMaxMidJobReports
}

// SubAgentRunner is the planner-facing orchestration surface. Delegation is
// asynchronous: the call returns a job id at once while progress and final
// reports arrive on the planner session automatically. Stop is blocking and
// waits for the real terminal state (REQ-019, REQ-020, REQ-025 flow).
type SubAgentRunner interface {
	Delegate(context.Context, string) (string, error)
	Message(context.Context, string, string) (string, error)
	Status(string) string
	Stop(context.Context, string) (string, error)
	Result(string, string) (string, error)
}

const subAgentReportToolResultRunes = 1000
const subAgentProgressArgsRunes = 80
const subAgentProgressResultRunes = 300

type toolHistoryEntry struct {
	ID        string
	Name      string
	Arguments string
	Result    string
	IsError   bool
	completed bool
}

type subAgentJob struct {
	id                    string
	workerID              string
	revision              uint64
	reviewed              bool
	superseded            bool
	accepted              bool
	continued             bool
	stopRequested         bool
	reportDelivered       bool
	input                 Input
	parent                *Session
	task                  string
	status                string
	started               time.Time
	finished              time.Time
	result                string
	progress              string
	events                []string
	tools                 []toolHistoryEntry
	completedToolCalls    int
	lastReportedToolCount int
	midJobReports         int
	plan                  string
	step                  PlanStep
	planned               bool
	cancel                context.CancelFunc
	done                  chan struct{}
	ctx                   context.Context
}

// SubAgentEvent is one automatic report injected into the planner session:
// Kind "progress" every X completed worker tools, Kind "final" at the
// terminal state (REQ-019, REQ-021).
type SubAgentEvent struct {
	Kind         string
	Parent       *Session
	JobID        string
	Status       string
	Report       string
	PlanStep     PlanStep
	PlanRevision uint64
	Input        Input
	Trace        *TraceEvent
}

func (e SubAgentEvent) Message() string {
	if e.Trace != nil {
		return ""
	}
	label := "investigation"
	if e.PlanStep.Index > 0 {
		label = fmt.Sprintf("plan revision %d step %d", e.PlanRevision, e.PlanStep.Index)
	}
	if e.Kind == "progress" {
		return fmt.Sprintf("<sub agent progress id %s>\n%s\nScope check required: compare this against the delegated task. If the worker is doing extra, missing, or wrong work, call `stop_subagent` (it blocks until stopped) then `follow_up_subagent` with the corrected task. If everything is on scope, reply briefly that work continues and stop calling tools; the next report arrives automatically.", e.JobID, e.Report)
	}
	return fmt.Sprintf("<sub agent report id %s> %s\n%s\nVerify the report against the delegated step, then call `accept_subagent_result` with evidence (planned steps) or use the findings directly (investigations). For failed, blocked, or incomplete work call `follow_up_subagent` with this job id; for new follow-on work call `continue_subagent` with this job id to keep the same worker session.", e.JobID, label, e.Report)
}

type subAgentManager struct {
	mu        sync.RWMutex
	jobs      map[string]*subAgentJob
	agent     *Agent
	cfg       SubAgentConfig
	eventMu   sync.RWMutex
	sink      func(SubAgentEvent)
	traceSink func(SubAgentEvent)
}

func newSubAgentManager(agent *Agent, cfg SubAgentConfig) *subAgentManager {
	return &subAgentManager{jobs: make(map[string]*subAgentJob), agent: agent, cfg: cfg}
}

func (m *subAgentManager) start(parent *Session, task, previous string, input Input) (string, error) {
	m.mu.Lock()
	job, err := m.startLocked(parent, task, previous, input)
	if err != nil {
		m.mu.Unlock()
		return "", err
	}
	m.mu.Unlock()
	go m.run(job)
	return job.id, nil
}

func (m *subAgentManager) startContinue(parent *Session, task, previous string, input Input) (string, error) {
	m.mu.Lock()
	job, err := m.startContinueLocked(parent, task, previous, input)
	if err != nil {
		m.mu.Unlock()
		return "", err
	}
	m.mu.Unlock()
	go m.run(job)
	return job.id, nil
}

func (m *subAgentManager) startLocked(parent *Session, task, previous string, input Input) (*subAgentJob, error) {
	if m == nil || m.agent == nil || parent == nil {
		return nil, errors.New("sdk: sub-agent is not configured")
	}
	task = strings.TrimSpace(task)
	if task == "" {
		return nil, errors.New("sdk: sub-agent task is required")
	}
	var retry *subAgentJob
	if previous != "" {
		retry = m.jobs[previous]
		if retry == nil || retry.parent != parent {
			return nil, errors.New("sdk: sub-agent job not found")
		}
		if retry.status == "running" || retry.accepted || retry.superseded {
			return nil, errors.New("sdk: job is not retryable")
		}
	}
	id := fmt.Sprintf("sa-%d", time.Now().UnixNano())
	plan, step, planned, err := parent.reserveSubAgent(id, retry)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := &subAgentJob{id: id, workerID: parent.ID() + ":subagent:" + id, parent: parent, task: task, status: "running", started: time.Now(), cancel: cancel, step: step, planned: planned, revision: plan.Revision, input: cloneInputRoute(input), done: make(chan struct{})}
	if retry != nil {
		job.workerID = retry.workerID
		job.input = cloneInputRoute(retry.input)
		retry.superseded = true
	}
	if planned {
		job.plan = formatPlan(plan)
	}
	m.jobs[id] = job
	job.ctx = ctx
	return job, nil
}

func (m *subAgentManager) startContinueLocked(parent *Session, task, previous string, input Input) (*subAgentJob, error) {
	if m == nil || m.agent == nil || parent == nil {
		return nil, errors.New("sdk: sub-agent is not configured")
	}
	task = strings.TrimSpace(task)
	if task == "" {
		return nil, errors.New("sdk: sub-agent task is required")
	}
	previous = strings.TrimSpace(previous)
	if previous == "" {
		return nil, errors.New("sdk: continue job_id is required")
	}
	prev := m.jobs[previous]
	if prev == nil || prev.parent != parent {
		return nil, errors.New("sdk: sub-agent job not found")
	}
	if prev.status == "running" {
		return nil, errors.New("sdk: previous job is still running")
	}
	id := fmt.Sprintf("sa-%d", time.Now().UnixNano())
	plan, step, planned, err := parent.reserveSubAgentForContinue(id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := &subAgentJob{id: id, workerID: prev.workerID, parent: parent, task: task, status: "running", started: time.Now(), cancel: cancel, step: step, planned: planned, revision: plan.Revision, input: cloneInputRoute(prev.input), done: make(chan struct{})}
	if planned {
		job.plan = formatPlan(plan)
	}
	prev.continued = true
	m.jobs[id] = job
	job.ctx = ctx
	return job, nil
}

// await blocks until the job reaches its terminal state and returns the
// delivered report; it is reserved for the blocking stop call (REQ-020).
func (m *subAgentManager) await(ctx context.Context, job *subAgentJob) string {
	select {
	case <-job.done:
	case <-ctx.Done():
		m.cancelJob(job)
		<-job.done
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	job.reviewed = true
	return m.reportLocked(job)
}

func (m *subAgentManager) cancelJob(job *subAgentJob) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if job.cancel != nil {
		job.cancel()
	}
}

func (m *subAgentManager) recordToolEvent(job *subAgentJob, event TraceEvent) string {
	switch event.Stage {
	case TraceToolCall, TraceToolRunning:
		if event.ToolCall == nil {
			return ""
		}
		call := *event.ToolCall
		for i := range job.tools {
			if job.tools[i].ID == call.ID {
				return ""
			}
		}
		job.tools = append(job.tools, toolHistoryEntry{ID: call.ID, Name: call.Name, Arguments: call.Arguments})
		if strings.TrimSpace(call.Arguments) != "" {
			return fmt.Sprintf("tool %s started (id=%s) args=%s", call.Name, call.ID, call.Arguments)
		}
		return fmt.Sprintf("tool %s started (id=%s)", call.Name, call.ID)
	case TraceToolResult:
		if event.ToolResult == nil {
			return ""
		}
		res := *event.ToolResult
		name := ""
		for i := range job.tools {
			if job.tools[i].ID == res.ID {
				job.tools[i].Result = res.Content
				job.tools[i].IsError = res.IsError
				job.tools[i].completed = true
				name = job.tools[i].Name
				break
			}
		}
		if name == "" && event.ToolCall != nil {
			name = event.ToolCall.Name
			job.tools = append(job.tools, toolHistoryEntry{ID: res.ID, Name: name, Result: res.Content, IsError: res.IsError, completed: true})
		}
		job.completedToolCalls++
		label := "completed"
		if res.IsError {
			label = "failed"
		}
		if name != "" {
			if res.Content != "" {
				return fmt.Sprintf("tool %s %s (id=%s) result=%s", name, label, res.ID, res.Content)
			}
			return fmt.Sprintf("tool %s %s (id=%s)", name, label, res.ID)
		}
		if res.Content != "" {
			return fmt.Sprintf("tool result %s (id=%s) result=%s", label, res.ID, res.Content)
		}
		return fmt.Sprintf("tool result %s (id=%s)", label, res.ID)
	default:
		return TraceMessage(event)
	}
}

func (m *subAgentManager) run(job *subAgentJob) {
	defer close(job.done)
	resp, err := m.runWorker(job.ctx, job)
	status, result := "completed", responseText(resp)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			status, result = "stopped", "sub-agent was stopped before completion"
		} else {
			status, result = "failed", err.Error()
		}
	}
	m.mu.Lock()
	job.status, job.result, job.finished, job.cancel = status, result, time.Now(), nil
	job.parent.finishSubAgent(job, status)
	job.reviewed = true
	var final string
	var report Input
	deliver := !job.reportDelivered && !job.superseded
	if deliver {
		final = m.reportLocked(job)
		job.reportDelivered = true
		report = cloneInputRoute(job.input)
	}
	m.mu.Unlock()
	if deliver {
		m.eventMu.RLock()
		sink := m.sink
		m.eventMu.RUnlock()
		if sink != nil {
			sink(SubAgentEvent{Kind: "final", Parent: job.parent, JobID: job.id, Status: status, Report: final, PlanStep: job.step, PlanRevision: job.revision, Input: report})
		}
	}
}

func (m *subAgentManager) emitTrace(job *subAgentJob, event TraceEvent) {
	if m == nil || job == nil {
		return
	}
	m.eventMu.RLock()
	sink := m.traceSink
	m.eventMu.RUnlock()
	if sink == nil {
		return
	}
	trace := event
	sink(SubAgentEvent{Parent: job.parent, JobID: job.id, Status: job.status, PlanStep: job.step, PlanRevision: job.revision, Input: cloneInputRoute(job.input), Trace: &trace})
}

// shouldReportProgress is the hybrid milestone plus anomaly gate: a mid-job
// progress report is emitted only for milestone tools (bash, the worker's only
// execution tool since CHANGE-087), for anomalies (two errors in a row, or the
// backstop interval of completed tools since the last report), and only while
// midJobReports stays below the configured cap. The caller must hold m.mu.
func (m *subAgentManager) shouldReportProgress(job *subAgentJob) bool {
	max := m.cfg.maxMidJobReports()
	if job.midJobReports >= max {
		return false
	}
	if job.completedToolCalls <= job.lastReportedToolCount {
		return false
	}
	if isMilestoneTool(lastCompletedToolName(job)) {
		return true
	}
	if lastTwoCompletedAreErrors(job) {
		return true
	}
	return job.completedToolCalls-job.lastReportedToolCount >= m.cfg.reportInterval()
}

func isMilestoneTool(name string) bool {
	switch name {
	case "bash":
		// CHANGE-087: bash is the worker's only execution tool, so every write,
		// edit and check now lands here.
		return true
	default:
		return false
	}
}

func lastCompletedToolName(job *subAgentJob) string {
	for i := len(job.tools) - 1; i >= 0; i-- {
		if job.tools[i].completed {
			return job.tools[i].Name
		}
	}
	return ""
}

func lastTwoCompletedAreErrors(job *subAgentJob) bool {
	seen := 0
	for i := len(job.tools) - 1; i >= 0; i-- {
		if !job.tools[i].completed {
			continue
		}
		if !job.tools[i].IsError {
			return false
		}
		seen++
		if seen >= 2 {
			return true
		}
	}
	return false
}

// emitProgress sends a scope-check report gated by shouldReportProgress
// (REQ-019): milestone tools, anomaly signals, or the backstop interval.
func (m *subAgentManager) emitProgress(job *subAgentJob) {
	m.mu.Lock()
	if job.status != "running" || job.reportDelivered {
		m.mu.Unlock()
		return
	}
	if !m.shouldReportProgress(job) {
		m.mu.Unlock()
		return
	}
	report := m.progressLocked(job)
	job.lastReportedToolCount = job.completedToolCalls
	job.midJobReports++
	input := cloneInputRoute(job.input)
	m.mu.Unlock()
	m.eventMu.RLock()
	sink := m.sink
	m.eventMu.RUnlock()
	if sink == nil {
		return
	}
	sink(SubAgentEvent{Kind: "progress", Parent: job.parent, JobID: job.id, Status: "running", Report: report, PlanStep: job.step, PlanRevision: job.revision, Input: input})
}

func (m *subAgentManager) progressLocked(job *subAgentJob) string {
	stepLabel := "investigation"
	if job.planned {
		stepLabel = fmt.Sprintf("plan revision %d step %d", job.revision, job.step.Index)
	}
	delta := job.completedToolCalls - job.lastReportedToolCount
	if delta < 0 {
		delta = 0
	}
	var b strings.Builder
	fmt.Fprintf(&b, "job=%s status=running completed=%d new=%d (%s)\ntask: %s\n", job.id, job.completedToolCalls, delta, stepLabel, job.task)
	var completed []toolHistoryEntry
	var running []toolHistoryEntry
	for _, tool := range job.tools {
		if tool.completed {
			completed = append(completed, tool)
		} else {
			running = append(running, tool)
		}
	}
	start := len(completed) - delta
	if start < 0 {
		start = 0
	}
	if delta == 0 && len(running) == 0 {
		b.WriteString("tools since last report: none\n")
	} else {
		for i, tool := range completed[start:] {
			status := "ok"
			if tool.IsError {
				status = "error"
			}
			line := fmt.Sprintf("%d. %s [%s]", job.lastReportedToolCount+i+1, tool.Name, status)
			if args := oneLineText(tool.Arguments); args != "" && args != "{}" {
				line += " " + truncateRunes(args, subAgentProgressArgsRunes)
			}
			if res := oneLineText(tool.Result); res != "" {
				line += " result=" + truncateRunes(res, subAgentProgressResultRunes)
			}
			b.WriteString(line + "\n")
		}
		for _, tool := range running {
			line := fmt.Sprintf("running: %s", tool.Name)
			if args := oneLineText(tool.Arguments); args != "" && args != "{}" {
				line += " " + truncateRunes(args, subAgentProgressArgsRunes)
			}
			b.WriteString(line + "\n")
		}
	}
	if job.progress != "" {
		b.WriteString("last: " + truncateRunes(oneLineText(job.progress), subAgentProgressArgsRunes*2))
	}
	return strings.TrimSpace(b.String())
}

func (m *subAgentManager) runWorker(ctx context.Context, job *subAgentJob) (Response, error) {
	parentCfg := job.parent.Config()
	provider := strings.TrimSpace(m.cfg.Provider)
	if provider == "" {
		provider = string(parentCfg.Provider)
	}
	model := strings.TrimSpace(m.cfg.Model)
	if model == "" {
		model = parentCfg.Model
	}
	workspace := strings.TrimSpace(parentCfg.Workspace)
	if workspace == "" {
		workspace = strings.TrimSpace(m.cfg.Workspace)
	}
	ctx = WithWorkspace(ctx, workspace)
	keys := job.parent.keys
	if provider != string(parentCfg.Provider) {
		config, err := m.agent.Client.Router.Provider(ProviderID(provider))
		if err != nil {
			return Response{}, err
		}
		keys = config.Keys
		if keys == nil {
			return Response{}, fmt.Errorf("sdk: sub-agent provider %q has no key pool", provider)
		}
	}
	workerID := job.workerID
	store := job.parent.store
	if store == nil {
		return Response{}, errors.New("sdk: parent session is not persistent")
	}
	workerCfg := SessionConfig{ID: workerID, Provider: ProviderID(provider), Model: model, KeyIndex: parentCfg.KeyIndex, Workspace: workspace}
	workerCfg.inheritGeneration(parentCfg)
	workerCfg.ThinkingLevel = chooseThinking(m.cfg.ThinkingLevel, parentCfg.ThinkingLevel)
	worker, err := OpenSession(SessionDBPath(store.Dir(), workerID), workerCfg, keys)
	if err != nil {
		return Response{}, err
	}
	defer worker.Close()
	if m.cfg.Temperature != nil {
		if err := worker.SetTemperature(*m.cfg.Temperature); err != nil {
			return Response{}, err
		}
	}
	prompt := strings.TrimSpace(m.cfg.SystemPrompt)
	if prompt == "" {
		prompt = defaultSubAgentSystemPrompt
	}
	if job.planned {
		prompt += "\n\nCurrent plan:\n" + job.plan
		prompt += "\n\nCurrent plan step:\n" + job.step.Text
	} else {
		prompt += "\n\nInvestigation mode: inspect the repository and return only the requested findings. Do not modify the project unless the investigation task explicitly requires it."
	}
	if requirements := projectRequirements(workspace); requirements != "" {
		prompt += "\n\nProject requirements from the repository:\n" + requirements
	}
	workerAgent := &Agent{Client: m.agent.Client, Tools: m.agent.Tools, MaxRetries: m.agent.MaxRetries, DisablePlanning: true, SubAgentConfig: SubAgentConfig{Enabled: false}}
	req := Request{Provider: ProviderID(provider), Model: model, SystemPrompt: prompt}
	if m.cfg.MaxOutputTokens > 0 {
		req.MaxOutputTokens = m.cfg.MaxOutputTokens
	}
	trace := func(_ context.Context, event TraceEvent) {
		m.mu.Lock()
		before := job.completedToolCalls
		message := m.recordToolEvent(job, event)
		job.progress = message
		if message != "" {
			job.events = append(job.events, message)
		}
		progress := event.Stage == TraceToolResult && job.completedToolCalls == before+1 && m.shouldReportProgress(job)
		m.mu.Unlock()
		m.emitTrace(job, TraceEvent{Stage: event.Stage, Message: event.Message, Response: cloneResponsePtr(event.Response), ToolCall: cloneToolCallPtr(event.ToolCall), ToolResult: cloneToolResultPtr(event.ToolResult), Text: event.Text, Err: event.Err, RetryAfter: event.RetryAfter, Elapsed: event.Elapsed, RequestStartedMs: event.RequestStartedMs, ProviderAcceptedMs: event.ProviderAcceptedMs, AtMs: event.AtMs})
		if progress {
			m.emitProgress(job)
		}
	}
	return workerAgent.runTurn(ctx, worker, Turn{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: job.task}}}, req, trace, nil)
}

func cloneResponsePtr(in *Response) *Response {
	if in == nil {
		return nil
	}
	out := *in
	out.Content = append([]ContentPart(nil), in.Content...)
	out.ToolCalls = append([]ToolCall(nil), in.ToolCalls...)
	return &out
}

func cloneToolCallPtr(in *ToolCall) *ToolCall {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneToolResultPtr(in *ToolResult) *ToolResult {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func chooseThinking(value, fallback ThinkingLevel) ThinkingLevel {
	if value != "" {
		return value
	}
	return fallback
}

// reportLocked renders the complete handoff report for one job: status,
// summary, and every worker tool with arguments and (length-capped) result.
// The caller must hold m.mu (REQ-019, REQ-034).
func (m *subAgentManager) reportLocked(job *subAgentJob) string {
	stepLabel := "investigation"
	if job.planned {
		stepLabel = fmt.Sprintf("plan revision %d step %d", job.revision, job.step.Index)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "sub-agent %s: status=%s job=%s %s worker=%s\n", job.id, job.status, job.id, stepLabel, job.workerID)
	fmt.Fprintf(&b, "task: %s\n", job.task)
	if len(job.tools) == 0 {
		b.WriteString("tools_used: none\n")
	} else {
		fmt.Fprintf(&b, "tools_used: %d\n", len(job.tools))
		for i, tool := range job.tools {
			fmt.Fprintf(&b, "tool %d: name=%s id=%s\n", i+1, tool.Name, tool.ID)
			if strings.TrimSpace(tool.Arguments) != "" {
				b.WriteString("  args: " + truncateRunes(oneLineText(tool.Arguments), subAgentReportToolResultRunes) + "\n")
			} else {
				b.WriteString("  args: (none)\n")
			}
			if !tool.completed {
				b.WriteString("  result: (pending)\n")
			} else if tool.IsError {
				b.WriteString("  error: " + truncateRunes(tool.Result, subAgentReportToolResultRunes) + "\n")
			} else {
				b.WriteString("  result: " + truncateRunes(tool.Result, subAgentReportToolResultRunes) + "\n")
			}
		}
	}
	if job.result != "" {
		b.WriteString("result: ")
		b.WriteString(job.result)
		b.WriteString("\n")
	}
	b.WriteString("This is the complete handoff report for the job. Verify it, then either accept the step with `accept_subagent_result` (verified success only), retry blocked/failed work with `follow_up_subagent`, or order new work into the same worker session with `continue_subagent`.")
	return strings.TrimSpace(b.String())
}

func truncateRunes(text string, max int) string {
	if max <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max-1]) + "…"
}

func oneLineText(text string) string { return strings.Join(strings.Fields(text), " ") }

// statusText renders one-line job state; kept for non-LLM diagnostics only
// (the model-facing status/history tools were removed with CHANGE-022).
func (m *subAgentManager) statusText(parent *Session, id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[id]
	if job == nil || job.parent != parent {
		return "sub-agent job not found: " + id
	}
	stepLabel := "investigation"
	if job.planned {
		stepLabel = fmt.Sprintf("%d", job.step.Index)
	}
	text := fmt.Sprintf("job=%s status=%s step=%s revision=%d accepted=%t superseded=%t continued=%t worker=%s tools=%d task=%s", job.id, job.status, stepLabel, job.revision, job.accepted, job.superseded, job.continued, job.workerID, len(job.tools), job.task)
	if len(job.tools) > 0 {
		names := make([]string, 0, len(job.tools))
		for _, tool := range job.tools {
			status := "ok"
			if !tool.completed {
				status = "running"
			} else if tool.IsError {
				status = "error"
			}
			names = append(names, tool.Name+"("+status+")")
		}
		text += " tools_used=" + strings.Join(names, ",")
	}
	if job.progress != "" {
		text += " progress=" + job.progress
	}
	return text
}

func (m *subAgentManager) result(parent *Session, id, verification string) (string, error) {
	m.mu.Lock()
	job := m.jobs[strings.TrimSpace(id)]
	if job == nil || job.parent != parent {
		m.mu.Unlock()
		return "", errors.New("sdk: sub-agent job not found: " + strings.TrimSpace(id))
	}
	report := m.reportLocked(job)
	if strings.TrimSpace(verification) == "" {
		m.mu.Unlock()
		return report, nil
	}
	if job.status != "completed" || !job.reviewed || job.accepted || job.superseded || strings.TrimSpace(verification) == "" {
		m.mu.Unlock()
		return report, errors.New("sdk: wait for the final report delivered automatically, then provide verified success before acceptance; failed/incomplete work needs follow-up")
	}
	if err := parent.acceptSubAgent(job); err != nil {
		m.mu.Unlock()
		return report, err
	}
	job.accepted = true
	job.events = append(job.events, "Main accepted verified success: "+verification)
	m.mu.Unlock()
	return report + "\nVerified result accepted; the next plan step is now ready, if any.", nil
}

func (m *subAgentManager) SetEventSink(sink func(SubAgentEvent)) {
	m.eventMu.Lock()
	m.sink = sink
	m.eventMu.Unlock()
}

func (m *subAgentManager) SetTraceSink(sink func(SubAgentEvent)) {
	m.eventMu.Lock()
	m.traceSink = sink
	m.eventMu.Unlock()
}

func formatPlan(plan PlanState) string {
	var b strings.Builder
	for _, step := range plan.Steps {
		b.WriteString(fmt.Sprintf("%d. %s\n", step.Index, step.Text))
	}
	return strings.TrimSpace(b.String())
}

// stop blocks until the job has actually stopped and returns the final
// report inline (REQ-020, REQ-034).
func (m *subAgentManager) stop(ctx context.Context, parent *Session, id string) (string, error) {
	m.mu.Lock()
	job := m.jobs[id]
	if job == nil || job.parent != parent {
		m.mu.Unlock()
		return "", errors.New("sdk: sub-agent job not found: " + id)
	}
	if job.status != "running" {
		m.mu.Unlock()
		return "", errors.New("sdk: sub-agent is not running: " + id)
	}
	job.stopRequested = true
	job.reportDelivered = true
	job.cancel()
	m.mu.Unlock()
	return m.await(ctx, job), nil
}

// RequestStop asks for one job to stop without waiting for it, and reports
// whether there was something to stop. The phone needs this shape: a stop button
// must answer immediately, and the job's own final report (status stopped, with
// the work it did manage) is what tells the phone it really ended. A job that is
// not running is not an error, for the same reason a finished turn is not.
func (m *subAgentManager) RequestStop(parentID, id string) error {
	if m == nil {
		return errors.New("sdk: sub-agent is not configured")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("sdk: sub-agent job id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[id]
	if job == nil {
		return errors.New("sdk: sub-agent job not found: " + id)
	}
	if parentID != "" && job.parent != nil && job.parent.ID() != parentID {
		return errors.New("sdk: sub-agent job not found: " + id)
	}
	if job.status != "running" {
		return nil
	}
	// The report is left undelivered on purpose: the phone's row is waiting for
	// it, and it is what says the worker really stopped. The tool path
	// (`stop`) does the opposite because it hands the report back inline.
	job.stopRequested = true
	job.cancel()
	return nil
}

func (m *subAgentManager) Accept(parent *Session, id, verification string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[id]
	if job == nil || job.parent != parent {
		return errors.New("sdk: sub-agent job not found")
	}
	if job.status != "completed" || !job.reviewed || job.accepted || job.superseded || strings.TrimSpace(verification) == "" {
		return errors.New("sdk: wait for the final report delivered automatically, then provide verified success before acceptance; failed/incomplete work needs follow-up")
	}
	if err := parent.acceptSubAgent(job); err != nil {
		return err
	}
	job.accepted = true
	job.events = append(job.events, "Main accepted verified success: "+verification)
	return nil
}

type subAgentRunner struct {
	manager *subAgentManager
	parent  *Session
}

func (r *subAgentRunner) Delegate(ctx context.Context, task string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	input, _ := ctx.Value(lifecycleInputKey{}).(Input)
	return r.manager.start(r.parent, task, "", input)
}
func (r *subAgentRunner) Status(id string) string { return r.manager.statusText(r.parent, id) }

func (r *subAgentRunner) Stop(ctx context.Context, id string) (string, error) {
	return r.manager.stop(ctx, r.parent, strings.TrimSpace(id))
}

func (r *subAgentRunner) Message(ctx context.Context, id, message string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", errors.New("sdk: delegate message job_id is required")
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return "", errors.New("sdk: delegate message is required")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	input, _ := ctx.Value(lifecycleInputKey{}).(Input)
	return r.manager.startContinue(r.parent, message, id, input)
}

func (r *subAgentRunner) Result(id, verification string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", errors.New("sdk: delegate result job_id is required")
	}
	return r.manager.result(r.parent, id, verification)
}

type subAgentTool struct{ runner SubAgentRunner }

func (t *subAgentTool) Definitions() []Tool {
	return []Tool{
		{Name: "delegate_task", Description: "Delegate a new task to the worker and return immediately with a job id. Progress and the final handoff report arrive automatically.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"task": map[string]any{"type": "string"}}, "required": []string{"task"}}},
		{Name: "delegate_message", Description: "Send follow-on work to a completed worker job, reusing the same worker session and history. Returns immediately with a new job id.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"job_id": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"}}, "required": []string{"job_id", "message"}}},
		{Name: "delegate_status", Description: "Return the current status, progress, worker session and tool history summary for a delegated job.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"job_id": map[string]any{"type": "string"}}, "required": []string{"job_id"}}},
		{Name: "delegate_stop", Description: "Stop a running delegated job. Blocking: waits until the worker really stops and returns its final report.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"job_id": map[string]any{"type": "string"}}, "required": []string{"job_id"}}},
		{Name: "delegate_result", Description: "Return the complete handoff report for a delegated job. Optionally provide verification evidence to explicitly accept a completed plan step.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"job_id": map[string]any{"type": "string"}, "verification": map[string]any{"type": "string"}}, "required": []string{"job_id"}}},
	}
}
func (t *subAgentTool) Execute(ctx context.Context, call ToolCall) ToolResult {
	result := ToolResult{ID: call.ID}
	var input struct {
		Task         string `json:"task"`
		Message      string `json:"message"`
		JobID        string `json:"job_id"`
		Verification string `json:"verification"`
	}
	if err := json.Unmarshal([]byte(call.Arguments), &input); err != nil {
		result.Content = "invalid sub-agent arguments: " + err.Error()
		result.IsError = true
		return result
	}
	if t == nil || t.runner == nil {
		result.Content = "sub-agent is not configured"
		result.IsError = true
		return result
	}
	switch call.Name {
	case "delegate_message":
		id, err := t.runner.Message(ctx, strings.TrimSpace(input.JobID), input.Message)
		if err != nil {
			return ToolResult{ID: call.ID, Content: err.Error(), IsError: true}
		}
		result.Content = "sub-agent follow-up started: " + id + " - progress and final reports will arrive automatically"
		return result
	case "continue_subagent":
		id, err := t.runner.Continue(ctx, strings.TrimSpace(input.JobID), input.Task)
		if err != nil {
			return ToolResult{ID: call.ID, Content: err.Error(), IsError: true}
		}
		result.Content = "sub-agent continued in the same worker session: " + id + " - progress and final reports will arrive automatically"
		return result
	case "accept_subagent_result":
		if err := t.runner.Accept(strings.TrimSpace(input.JobID), input.Verification); err != nil {
			return ToolResult{ID: call.ID, Content: err.Error(), IsError: true}
		}
		result.Content = "Verified result accepted; the next plan step is now ready, if any."
		return result
	case "delegate_to_subagent":
		id, err := t.runner.Delegate(ctx, input.Task)
		if err != nil {
			result.Content = err.Error()
			result.IsError = true
			return result
		}
		result.Content = "sub-agent started: " + id + " - scope-check progress reports follow every few worker tool calls; the complete handoff report arrives when the job ends"
		return result
	case "stop_subagent":
		report, err := t.runner.Stop(ctx, strings.TrimSpace(input.JobID))
		if err != nil {
			result.Content = err.Error()
			result.IsError = true
			return result
		}
		result.Content = report
		return result
	default:
		result.Content = "unknown sub-agent operation"
		result.IsError = true
		return result
	}
}

func responseText(resp Response) string {
	var b strings.Builder
	for _, part := range resp.Content {
		if strings.TrimSpace(part.Text) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(part.Text)
	}
	if b.Len() == 0 {
		return "sub-agent completed without a text result"
	}
	return b.String()
}

// runningJobID reports the parent's currently running job, for diagnostics.
func (m *subAgentManager) runningJobID() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, job := range m.jobs {
		if job.status == "running" {
			return job.id
		}
	}
	return ""
}

// startAndRegister starts a job without waiting; tests use it to exercise
// stop/overlap paths that the async API also exposes, deterministically.
func (m *subAgentManager) startAndRegister(parent *Session, task string) (string, error) {
	return m.start(parent, task, "", Input{})
}
