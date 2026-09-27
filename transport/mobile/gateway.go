package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Tulipskun/ai-engine/sdk"
	"github.com/gorilla/websocket"
)

// Frame kinds on the wire. Additive-only: unknown fields are ignored by both
// sides so an older phone keeps working against a newer daemon.
const (
	FrameHello   = "hello"
	FrameMessage = "message"
	FrameAck     = "ack"
	FrameTrace   = "trace"
	FrameDelta   = "delta"
	FrameCancel  = "cancel"
	FrameDone    = "done"
	FrameError   = "error"
)

// SourceName is the routing label the harness uses. sdk.DispatchDisplay filters
// displays by this, so the mobile client only receives its own output.
const SourceName = "mobile"

type ContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type Inbound struct {
	Type      string        `json:"type"`
	Source    string        `json:"source"`
	SessionID string        `json:"session_id"`
	Role      string        `json:"role"`
	Content   []ContentPart `json:"content"`
	// JobID names one sub agent on a cancel frame: the phone stops one worker
	// without stopping the turn that delegated to it. Empty means the turn.
	JobID       string `json:"job_id,omitempty"`
	ClientMsgID string `json:"client_msg_id"`
}

type ToolCall struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ToolResultView is the short result line the phone shows next to a tool call;
// the full result stays in the session, not in every frame.
type ToolResultView struct {
	ToolCall
	Text    string `json:"text,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
}

// Outbound is what the phone renders. Text, agent attribution and job id let
// the app show main vs sub agent activity both live and after a restart.
type Outbound struct {
	Kind         string          `json:"kind"`
	SessionID    string          `json:"session_id"`
	Role         string          `json:"role"`
	Agent        string          `json:"agent,omitempty"`
	JobID        string          `json:"job_id,omitempty"`
	Stage        string          `json:"stage,omitempty"`
	Seq          int64           `json:"seq,omitempty"`
	Text         string          `json:"text,omitempty"`
	Content      []ContentPart   `json:"content,omitempty"`
	ToolCall     *ToolCall       `json:"tool_call,omitempty"`
	ToolResult   *ToolResultView `json:"tool_result,omitempty"`
	ClientMsgID  string          `json:"client_msg_id,omitempty"`
	InputTokens  int             `json:"input_tokens,omitempty"`
	OutputTokens int             `json:"output_tokens,omitempty"`
	// CacheReadTokens and CacheWriteTokens are the provider's cached usage when
	// it reports them. They are footer data, not live estimates.
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
	// ReasoningMs is the elapsed thinking time recorded for the reasoning event
	// that produced this transient status. Raw reasoning text is never sent.
	ReasoningMs int64 `json:"reasoning_ms,omitempty"`
	// ToolDurationMs is the recorded elapsed time for one tool call/result, from
	// the provider request that produced it until the tool event arrived.
	ToolDurationMs int64 `json:"tool_duration_ms,omitempty"`
	// Model and DurationMs are what the phone puts in the footer of this one
	// message. They are read off the terminal trace rather than the chat's
	// configured route, so the footer names the model that actually answered
	// (AX-095).
	Model      string `json:"model,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
}

// TokenStore is the daemon's volatile credential (d1store.MemoryToken in
// production). It is empty until a phone connects.
type TokenStore interface {
	Get() string
	Adopt(token string)
}

// Hydrator pulls the D1 copy of the runtime state into the local state root
// after a phone hands over a verified token (REQ-046(4)). It runs once per
// process, before the first inbound message is processed.
type Hydrator interface {
	Hydrate(ctx context.Context) error
}

// Config wires the transport.
type Config struct {
	Listen         string // e.g. 127.0.0.1:18789 (the tunnel is the only public ingress)
	PublicListen   string // e.g. 0.0.0.0:18789 for LAN-only operation
	Tunnel         bool
	Cloudflared    string
	Version        string
	Tokens         TokenStore
	MirrorInput    func(sdk.Input) func(context.Context) error
	Verifier       Verifier
	Hydrate        Hydrator
	History        HistoryStore                        // serves the phone's history from D1
	Models         ModelStore                          // provider/model catalogue and per-chat choice
	Admin          AdminStore                          // provider keys and agent settings from the phone
	CancelTurn     func(sessionID string) bool         // stops the turn a phone asked to stop
	CancelSubAgent func(sessionID, jobID string) error // stops one sub agent job, leaving the turn
	ReportError    func(sessionID, message string)     // shows a turn failure on the phone
	Announce       func(context.Context, string) error // publishes the tunnel URL (D1 `nodes`)
	InputBuffer    int
}

const defaultInputBuffer = 64

// subscriber is the write side a live phone connection exposes, narrowed so
// tests can observe the frames the gateway emits.
type subscriber interface {
	WriteMessage(int, []byte) error
	Close() error
}

// SetModelStore attaches the provider/model catalogue after construction. It
// must run before StartHTTP, while the daemon is still single-threaded.
func (t *Transport) SetModelStore(store ModelStore) {
	if t == nil {
		return
	}
	t.cfg.Models = store
}

// SetAdminStore attaches the provider/settings surface after construction, like
// SetModelStore, before the server starts.
func (t *Transport) SetAdminStore(store AdminStore) {
	if t == nil {
		return
	}
	t.cfg.Admin = store
}

// SetErrorReporter attaches the hook that shows a failed turn on the phone, so
// a provider that refuses the request (a dead key, a free tier that only works
// inside another app) is visible instead of a silent retry.
func (t *Transport) SetErrorReporter(report func(sessionID, message string)) {
	if t == nil {
		return
	}
	t.cfg.ReportError = report
}

// ReportTurnError sends one turn failure to the phones watching that session.
func (t *Transport) ReportTurnError(sessionID, message string) {
	if t == nil || sessionID == "" || message == "" {
		return
	}
	t.broadcast(sessionID, Outbound{Kind: FrameError, SessionID: sessionID, Text: message})
}

// SetCancel attaches the stop hook the phone's cancel frame calls.
func (t *Transport) SetCancel(cancel func(string) bool) {
	if t == nil {
		return
	}
	t.cfg.CancelTurn = cancel
}

// SubAgentTerminal reports that one sub agent job reached its end state, so the
// phone settles that row. A worker the phone stopped has no terminal trace of
// its own — the stop is what ended it — so its final report is the only signal.
func (t *Transport) SubAgentTerminal(sessionID, jobID, status string) {
	if t == nil || sessionID == "" || jobID == "" || status == "" {
		return
	}
	stage := status
	switch status {
	case "stopped", "failed", "completed":
	default:
		stage = "completed"
	}
	t.broadcast(sessionID, Outbound{
		Kind: FrameDone, SessionID: sessionID, Role: "system", Agent: "sub",
		JobID: jobID, Stage: "subagent_" + stage,
	})
}

// SetCancelSubAgent attaches the per-sub-agent stop hook. A cancel frame that
// names a job stops that worker only; without it the frame keeps its old
// meaning and stops the whole turn.
func (t *Transport) SetCancelSubAgent(cancel func(sessionID, jobID string) error) {
	if t == nil {
		return
	}
	t.cfg.CancelSubAgent = cancel
}

// Version reports the label this build announces with, so the daemon can write
// it into the D1 `nodes` row without duplicating the config.
func (t *Transport) Version() string {
	if t == nil {
		return ""
	}
	return t.cfg.Version
}

// Transport is both the input source and the display for the mobile client.
type Transport struct {
	cfg      Config
	upgrader websocket.Upgrader
	gate     *Gate

	mu   sync.Mutex
	subs map[string]map[subscriber]struct{}
	seq  int64

	inputs chan sdk.Input

	hydrateOnce sync.Once
	hydratedErr error

	streamMu sync.Mutex
	turns    map[string]*turnProgress // what each in-flight turn already sent
}

func New(cfg Config) *Transport {
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:18789"
	}
	if cfg.InputBuffer <= 0 {
		cfg.InputBuffer = defaultInputBuffer
	}
	t := &Transport{
		cfg:    cfg,
		gate:   NewGate(GateConfig{Verify: cfg.Verifier, Cache: cfg.Tokens}),
		subs:   map[string]map[subscriber]struct{}{},
		inputs: make(chan sdk.Input, cfg.InputBuffer),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin:     func(r *http.Request) bool { return true },
		},
	}
	return t
}

// Source makes the transport a RoutedDisplay so sdk.DispatchDisplay only routes
// mobile output here.
func (t *Transport) Source() string { return SourceName }

// Receive implements sdk.InputSource.
func (t *Transport) Receive(ctx context.Context) (<-chan sdk.Input, error) {
	return t.inputs, nil
}

// Display implements sdk.Display: one canonical sdk.Output becomes frames for
// every phone watching that session. A traced turn arrives as a trace stream
// (sdk.HarnessLoop skips its final output once a trace ran), so the answer is
// read from TraceResponseContent and TraceResponse closes the turn.
// Display implements sdk.Display. One canonical sdk.Output becomes frames for
// every phone watching that session.
//
// A traced turn arrives as a stream (sdk emits one trace per provider event), so
// the mapping has to be exact or the phone shows a half answer:
//
//   - TraceResponseContent with Text and no Response is one streamed chunk -> delta
//   - TraceResponseContent with Response is a provider that did not stream -> message
//   - TraceResponse carries the authoritative text of a streamed turn -> message
//     (only when no deltas were sent) and then done
//   - TraceResponseText is reasoning, not the answer -> a thinking status line
//   - tool stages carry the call and a short result so the app can draw steps
func (t *Transport) Display(ctx context.Context, output sdk.Output) error {
	if output.Source != "" && output.Source != SourceName {
		return nil
	}
	if output.Trace != nil {
		return t.displayTrace(output)
	}
	text := textOf(output)
	if text == "" {
		return nil
	}
	frame := Outbound{
		Kind:             FrameMessage,
		SessionID:        output.SessionID,
		Role:             "model",
		Agent:            agentFor(output),
		JobID:            output.Metadata[jobMetadataKey],
		Text:             text,
		Content:          []ContentPart{{Type: "text", Text: text}},
		InputTokens:      output.Response.Usage.InputTokens,
		OutputTokens:     output.Response.Usage.OutputTokens,
		CacheReadTokens:  output.Response.Usage.CacheReadTokens,
		CacheWriteTokens: output.Response.Usage.CacheWriteTokens,
		Model:            output.Response.Model,
		DurationMs:       turnDurationMs(nil),
	}
	t.broadcast(output.SessionID, frame)
	if frame.InputTokens > 0 || frame.OutputTokens > 0 {
		t.markCounted(output.SessionID, frame.Agent, frame.JobID)
	}
	t.broadcast(output.SessionID, Outbound{Kind: FrameDone, SessionID: output.SessionID, JobID: frame.JobID})
	return nil
}

func (t *Transport) displayTrace(output sdk.Output) error {
	trace := *output.Trace
	agent := traceAgent(output)
	jobID := traceJobID(output)
	switch trace.Stage {
	case sdk.TraceResponseText:
		// Reasoning deltas are progress, not the answer: the phone shows them as
		// a "thinking" line and never stores them as a message.
		if text := strings.TrimSpace(trace.Text); text != "" {
			t.broadcast(output.SessionID, Outbound{
				Kind: FrameTrace, SessionID: output.SessionID, Role: "system", Agent: agent,
				JobID: jobID, Stage: string(sdk.TraceResponseText), Text: "thinking",
				ReasoningMs: trace.TotalElapsed().Milliseconds(),
			})
		}
		return nil

	case sdk.TraceResponseContent:
		if trace.Response != nil {
			// A provider that answered in one shot: this is the whole message.
			// The sdk follows it with TraceResponse carrying the same response,
			// so mark the turn answered and let the terminal event only close it.
			return t.sendAnswer(output, agent, jobID, responseText(trace.Response), false)
		}
		if text := trace.Text; text != "" {
			t.broadcast(output.SessionID, Outbound{
				Kind: FrameDelta, SessionID: output.SessionID, Role: "model", Agent: agent,
				JobID: jobID, Text: text, Content: []ContentPart{{Type: "text", Text: text}},
			})
			t.noteDelta(output.SessionID, agent, jobID)
		}
		return nil

	case sdk.TraceResponse:
		if jobID != "" {
			// A worker's turn ended: the phone's row for that job is done. The
			// answer itself (if any) is handled by the same path as the main
			// agent's, and this frame only carries the end state.
			defer t.broadcast(output.SessionID, Outbound{
				Kind: FrameDone, SessionID: output.SessionID, Role: "system", Agent: agent,
				JobID: jobID, Stage: "subagent_completed",
			})
		}
		text := responseText(trace.Response)
		if text == "" {
			t.broadcast(output.SessionID, Outbound{Kind: FrameDone, SessionID: output.SessionID, JobID: jobID})
			return nil
		}
		// Deltas, or a whole message a moment ago, already reached the phone.
		// The terminal event closes the turn — unless the phone never got the
		// token counts, which only the closing message frame carries. Deltas
		// alone would leave the footer estimating forever, so in that case the
		// authoritative text goes out once more with the real numbers.
		if t.answered(output.SessionID, agent, jobID) {
			counted := t.counted(output.SessionID, agent, jobID)
			t.clearTurn(output.SessionID, agent, jobID)
			usage := trace.Response.Usage
			if trace.Response == nil {
				usage = output.Response.Usage
			}
			if counted || (usage.InputTokens == 0 && usage.OutputTokens == 0) {
				t.broadcast(output.SessionID, Outbound{Kind: FrameDone, SessionID: output.SessionID, JobID: jobID})
				return nil
			}
			return t.sendAnswer(output, agent, jobID, text, true)
		}
		t.sendAnswer(output, agent, jobID, text, true)
		t.clearTurn(output.SessionID, agent, jobID)
		return nil

	case sdk.TraceError:
		if trace.Err != nil && errors.Is(trace.Err, context.Canceled) {
			// The phone asked to stop. A raw "context canceled" provider error
			// would read as a failure, so report it as the stop it was — and a
			// stopped sub agent ends here too, named by its job, so the phone
			// settles that row instead of ending the whole turn on screen.
			stage := "cancelled"
			if jobID != "" {
				stage = "subagent_stopped"
			}
			t.broadcast(output.SessionID, Outbound{
				Kind: FrameDone, SessionID: output.SessionID, Role: "system", Agent: agent,
				JobID: jobID, Stage: stage,
			})
			return nil
		}
		text := strings.TrimSpace(trace.Message)
		if text == "" && trace.Err != nil {
			text = trace.Err.Error()
		}
		if text == "" {
			text = sdk.TraceMessage(trace)
		}
		t.broadcast(output.SessionID, Outbound{
			Kind: FrameError, SessionID: output.SessionID, Agent: agent, JobID: jobID, Text: text,
		})
		if jobID != "" {
			t.broadcast(output.SessionID, Outbound{
				Kind: FrameDone, SessionID: output.SessionID, Role: "system", Agent: agent,
				JobID: jobID, Stage: "subagent_failed",
			})
		}
		return nil

	case sdk.TraceToolCall, sdk.TraceToolRunning:
		call := toolCallView(trace.ToolCall)
		if call == nil {
			return nil
		}
		stage := string(sdk.TraceToolRunning)
		if trace.Stage == sdk.TraceToolCall {
			stage = string(sdk.TraceToolCall)
		}
		t.broadcast(output.SessionID, Outbound{
			Kind: FrameTrace, SessionID: output.SessionID, Role: "system", Agent: agent,
			JobID: jobID, Stage: stage, Text: call.Name, ToolCall: call,
			ToolDurationMs: trace.TotalElapsed().Milliseconds(),
		})
		return nil

	case sdk.TraceToolResult:
		call := toolCallView(trace.ToolCall)
		if call == nil {
			return nil
		}
		view := &ToolResultView{ToolCall: *call}
		if trace.ToolResult != nil {
			view.Text = strings.TrimSpace(trace.ToolResult.Content)
			view.IsError = trace.ToolResult.IsError
		}
		t.broadcast(output.SessionID, Outbound{
			Kind: FrameTrace, SessionID: output.SessionID, Role: "system", Agent: agent,
			JobID: jobID, Stage: string(sdk.TraceToolResult), Text: view.Text, ToolCall: call, ToolResult: view,
			ToolDurationMs: trace.TotalElapsed().Milliseconds(),
		})
		return nil

	default:
		text := strings.TrimSpace(trace.Message)
		if text == "" {
			text = strings.TrimSpace(sdk.TraceMessage(trace))
		}
		if text == "" {
			return nil
		}
		t.broadcast(output.SessionID, Outbound{
			Kind: FrameTrace, SessionID: output.SessionID, Role: "system", Agent: agent,
			JobID: jobID, Stage: string(trace.Stage), Text: text,
		})
		return nil
	}
}

// turnDurationMs is how long the provider took for one answer, taken from the
// timestamps the trace recorded (REQ-033): the request was sent, then the event
// arrived. Never a duration measured while the phone is drawing.
func turnDurationMs(trace *sdk.TraceEvent) int64 {
	if trace == nil {
		return 0
	}
	if trace.RequestStartedMs > 0 && trace.AtMs >= trace.RequestStartedMs {
		return trace.AtMs - trace.RequestStartedMs
	}
	return trace.Elapsed.Milliseconds()
}

// sendAnswer puts one authoritative message on the wire. closeTurn is false when
// a traced turn is still running and its terminal event will close it.
func (t *Transport) sendAnswer(output sdk.Output, agent, jobID, text string, closeTurn bool) error {
	text = strings.TrimSpace(text)
	if text == "" {
		if closeTurn {
			t.broadcast(output.SessionID, Outbound{Kind: FrameDone, SessionID: output.SessionID, JobID: jobID})
		}
		return nil
	}
	frame := Outbound{
		Kind: FrameMessage, SessionID: output.SessionID, Role: "model", Agent: agent,
		JobID: jobID, Text: text, Content: []ContentPart{{Type: "text", Text: text}},
	}
	if output.Trace != nil && output.Trace.Response != nil {
		frame.InputTokens = output.Trace.Response.Usage.InputTokens
		frame.OutputTokens = output.Trace.Response.Usage.OutputTokens
		frame.CacheReadTokens = output.Trace.Response.Usage.CacheReadTokens
		frame.CacheWriteTokens = output.Trace.Response.Usage.CacheWriteTokens
		frame.Model = output.Trace.Response.Model
	}
	frame.DurationMs = turnDurationMs(output.Trace)
	t.broadcast(output.SessionID, frame)
	t.markAnswered(output.SessionID, agent, jobID)
	if frame.InputTokens > 0 || frame.OutputTokens > 0 {
		t.markCounted(output.SessionID, agent, jobID)
	}
	if closeTurn {
		t.broadcast(output.SessionID, Outbound{Kind: FrameDone, SessionID: output.SessionID, JobID: jobID})
	}
	return nil
}

// cancelStage tells the phone whether there was something to stop, so it can say
// "หยุดแล้ว" or "turn นั้นจบไปแล้ว" instead of guessing.
func cancelStage(stopped bool) string {
	if stopped {
		return "cancelled"
	}
	return "already_done"
}

// subAgentStopStage is the same idea for one worker: the phone updates that one
// row instead of ending the whole turn, and a job it cannot find is named so the
// row does not spin forever.
func subAgentStopStage(err error) string {
	switch {
	case err == nil:
		return "subagent_stopping"
	case strings.Contains(err.Error(), "not found"):
		return "subagent_not_found"
	default:
		return "subagent_stop_failed"
	}
}

// cancelSubAgent asks the agent to stop one job and answers immediately. The
// worker's own final report is what proves it stopped; the frame only says the
// request was accepted.
func (t *Transport) cancelSubAgent(conn *websocket.Conn, sessionID, jobID string) {
	stage := "subagent_stop_failed"
	if t.cfg.CancelSubAgent != nil {
		stage = subAgentStopStage(t.cfg.CancelSubAgent(sessionID, jobID))
	}
	frame := Outbound{Kind: FrameDone, SessionID: sessionID, Role: "system", Stage: stage, JobID: jobID}
	t.send(conn, frame)
	t.broadcast(sessionID, frame)
}

func responseText(resp *sdk.Response) string {
	if resp == nil {
		return ""
	}
	return strings.TrimSpace(textOfParts(toContent(resp.Content)))
}

// FinalText is the answer text a phone should keep for this output, whichever
// shape the harness delivered it in. Only terminal events count: a delta or a
// tool step is progress, and mirroring those would duplicate the turn in D1.
func FinalText(output sdk.Output) string {
	if output.Trace == nil {
		return textOf(output)
	}
	switch output.Trace.Stage {
	case sdk.TraceResponseContent:
		if output.Trace.Response != nil {
			return responseText(output.Trace.Response)
		}
		return ""
	case sdk.TraceResponse:
		return responseText(output.Trace.Response)
	}
	return ""
}

func toolCallView(call *sdk.ToolCall) *ToolCall {
	if call == nil || call.Name == "" {
		return nil
	}
	return &ToolCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments}
}

func traceJobID(output sdk.Output) string {
	if id := strings.TrimSpace(output.Metadata[jobMetadataKey]); id != "" {
		return id
	}
	return strings.TrimSpace(output.Metadata["trace_job_id"])
}

// turnProgress remembers what a phone already received for one turn, so the
// terminal event never repeats the answer: deltas count as delivered, and a
// whole message marks the turn answered.
type turnProgress struct {
	streamed bool
	answered bool
	// counted records that a message frame already carried this turn's token
	// counts, so the terminal event does not send the same answer twice.
	counted bool
}

func (t *Transport) turn(sessionID, agent, jobID string) *turnProgress {
	key := sessionKey(sessionID, agent, jobID)
	t.streamMu.Lock()
	defer t.streamMu.Unlock()
	if t.turns == nil {
		t.turns = map[string]*turnProgress{}
	}
	state, ok := t.turns[key]
	if !ok {
		state = &turnProgress{}
		t.turns[key] = state
	}
	return state
}

func (t *Transport) noteDelta(sessionID, agent, jobID string) {
	state := t.turn(sessionID, agent, jobID)
	t.streamMu.Lock()
	state.streamed = true
	t.streamMu.Unlock()
}

func (t *Transport) markAnswered(sessionID, agent, jobID string) {
	state := t.turn(sessionID, agent, jobID)
	t.streamMu.Lock()
	state.answered = true
	t.streamMu.Unlock()
}

// markCounted remembers that the phone already has this turn's token counts.
func (t *Transport) markCounted(sessionID, agent, jobID string) {
	state := t.turn(sessionID, agent, jobID)
	t.streamMu.Lock()
	state.counted = true
	t.streamMu.Unlock()
}

func (t *Transport) counted(sessionID, agent, jobID string) bool {
	state := t.turn(sessionID, agent, jobID)
	t.streamMu.Lock()
	defer t.streamMu.Unlock()
	return state.counted
}

func (t *Transport) answered(sessionID, agent, jobID string) bool {
	state := t.turn(sessionID, agent, jobID)
	t.streamMu.Lock()
	defer t.streamMu.Unlock()
	return state.answered || state.streamed
}

func (t *Transport) clearTurn(sessionID, agent, jobID string) {
	t.streamMu.Lock()
	defer t.streamMu.Unlock()
	delete(t.turns, sessionKey(sessionID, agent, jobID))
}

func sessionKey(sessionID, agent, jobID string) string {
	return sessionID + "\x00" + agent + "\x00" + jobID
}

const (
	agentMetadataKey = "mobile_agent"
	jobMetadataKey   = "mobile_job_id"
)

// PublishTrace streams a progress frame (tool calls, reasoning) to the session.
func (t *Transport) PublishTrace(sessionID, agent, jobID, stage, text string) {
	if sessionID == "" || text == "" {
		return
	}
	t.broadcast(sessionID, Outbound{
		Kind: FrameTrace, SessionID: sessionID, Role: "system", Agent: agent,
		JobID: jobID, Stage: stage, Text: text,
	})
}

// SendInput delivers one inbound frame to the harness.
func (t *Transport) SendInput(ctx context.Context, input sdk.Input) error {
	select {
	case t.inputs <- input:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the transport and disconnects subscribers.
func (t *Transport) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for session, set := range t.subs {
		for c := range set {
			_ = c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, "shutdown"))
			_ = c.Close()
		}
		delete(t.subs, session)
	}
}

// serveWS applies the handshake gate before upgrading.
func (t *Transport) serveWS(w http.ResponseWriter, r *http.Request) {
	decision := t.gate.Check(r)
	if !decision.Allowed {
		t.gate.Write(w, decision)
		return
	}
	// Verified: this is the daemon's only credential, held in memory (CON-012).
	t.cfg.Tokens.Adopt(decision.Token)

	conn, err := t.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	var sessionID string
	defer func() {
		if sessionID == "" {
			return
		}
		t.mu.Lock()
		if set := t.subs[sessionID]; set != nil {
			delete(set, conn)
			if len(set) == 0 {
				delete(t.subs, sessionID)
			}
		}
		t.mu.Unlock()
	}()

	conn.SetReadLimit(1 << 20)
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var in Inbound
		if err := json.Unmarshal(raw, &in); err != nil {
			t.send(conn, Outbound{Kind: FrameError, Text: "bad json"})
			return
		}
		if in.SessionID == "" {
			t.send(conn, Outbound{Kind: FrameError, Text: "session_id required"})
			return
		}
		sessionID = in.SessionID
		t.subscribe(sessionID, conn)

		if in.Type == FrameHello {
			// The first verified connection hydrates runtime state from D1
			// before any turn runs (REQ-046(4)).
			if t.cfg.Hydrate != nil {
				t.hydrateOnce.Do(func() { t.hydratedErr = t.cfg.Hydrate.Hydrate(r.Context()) })
				if t.hydratedErr != nil {
					log.Printf("mobile: hydrate from D1 failed: %v", t.hydratedErr)
					t.send(conn, Outbound{Kind: FrameError, Text: "ดึง state จาก D1 ไม่สำเร็จ: " + t.hydratedErr.Error()})
					return
				}
			}
			t.send(conn, Outbound{Kind: FrameAck, SessionID: sessionID, Role: "system", Stage: "resumed"})
			continue
		}
		if in.Type == FrameCancel {
			// The phone's stop button. A cancel that names a sub agent stops that
			// worker and leaves the turn running; a cancel without a job stops
			// the turn. Neither is an error when there was nothing to stop: the
			// phone may have pressed a moment too late.
			if jobID := strings.TrimSpace(in.JobID); jobID != "" {
				t.cancelSubAgent(conn, sessionID, jobID)
				continue
			}
			stopped := t.cfg.CancelTurn != nil && t.cfg.CancelTurn(sessionID)
			t.send(conn, Outbound{
				Kind: FrameDone, SessionID: sessionID, Role: "system", Stage: cancelStage(stopped),
			})
			t.broadcast(sessionID, Outbound{
				Kind: FrameDone, SessionID: sessionID, Role: "system", Stage: cancelStage(stopped),
			})
			continue
		}
		if in.Type != FrameMessage {
			continue
		}
		text := textOfParts(in.Content)
		if text == "" {
			continue
		}
		t.send(conn, Outbound{Kind: FrameAck, SessionID: sessionID, Role: "system", ClientMsgID: in.ClientMsgID})
		input := sdk.Input{
			Source:    SourceName,
			SessionID: sessionID,
			Turn:      sdk.Turn{Role: sdk.RoleUser, Content: []sdk.ContentPart{{Type: sdk.ContentText, Text: text}}},
			Metadata:  map[string]string{"client_msg_id": in.ClientMsgID},
		}
		select {
		case t.inputs <- input:
			if t.cfg.MirrorInput != nil {
				if mirror := t.cfg.MirrorInput(input); mirror != nil {
					go func() {
						mirrorCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 45*time.Second)
						defer cancel()
						if err := mirror(mirrorCtx); err != nil {
							log.Printf("mobile: mirror user turn to D1: %v", err)
						}
					}()
				}
			}
		default:
			t.send(conn, Outbound{Kind: FrameError, SessionID: sessionID, Text: "busy: input queue full"})
		}
	}
}

func (t *Transport) subscribe(sessionID string, conn subscriber) {
	t.mu.Lock()
	defer t.mu.Unlock()
	set := t.subs[sessionID]
	if set == nil {
		set = map[subscriber]struct{}{}
		t.subs[sessionID] = set
	}
	set[conn] = struct{}{}
}

func (t *Transport) broadcast(sessionID string, frame Outbound) {
	if sessionID == "" {
		return
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		return
	}
	t.mu.Lock()
	conns := make([]subscriber, 0, len(t.subs[sessionID]))
	for c := range t.subs[sessionID] {
		conns = append(conns, c)
	}
	t.mu.Unlock()
	for _, c := range conns {
		_ = c.WriteMessage(websocket.TextMessage, raw)
	}
}

func (t *Transport) send(conn *websocket.Conn, frame Outbound) {
	raw, err := json.Marshal(frame)
	if err != nil {
		return
	}
	_ = conn.WriteMessage(websocket.TextMessage, raw)
}

func textOf(output sdk.Output) string {
	if text := textOfParts(toContent(output.Content)); text != "" {
		return text
	}
	return strings.TrimSpace(textOfContent(output.Content))
}

func textOfContent(parts []sdk.ContentPart) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

func toContent(parts []sdk.ContentPart) []ContentPart {
	out := make([]ContentPart, 0, len(parts))
	for _, p := range parts {
		out = append(out, ContentPart{Type: string(p.Type), Text: p.Text})
	}
	return out
}

func textOfParts(parts []ContentPart) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// agentFor prefers explicit transport metadata (set by the display adapter)
// and falls back to "main" for model output.
func traceAgent(output sdk.Output) string {
	if strings.EqualFold(strings.TrimSpace(output.Metadata["trace_actor"]), "subagent") {
		return "sub"
	}
	return "main"
}

func agentFor(output sdk.Output) string {
	if agent := strings.TrimSpace(output.Metadata[agentMetadataKey]); agent != "" {
		return agent
	}
	return "main"
}

// StartHTTP serves the WebSocket endpoint and, when configured, opens a quick
// tunnel and announces the resulting public URL through the Worker.
func (t *Transport) StartHTTP(ctx context.Context, listen string) (func(), error) {
	if listen == "" {
		listen = t.cfg.Listen
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", t.serveWS)
	if t.cfg.Admin != nil {
		// Provider keys and agent settings are writes, not reads: the phone can
		// set them, and key material never comes back out.
		mux.Handle("/api/providers", NewAdminHandler(t.cfg.Admin, t.gate))
		mux.Handle("/api/providers/", NewAdminHandler(t.cfg.Admin, t.gate))
		mux.Handle("/api/settings", NewAdminHandler(t.cfg.Admin, t.gate))
	}
	if t.cfg.History != nil {
		// One address for the phone: history and the model catalogue come
		// through the tunnel and are answered from D1 plus the live router with
		// the token the daemon already holds, so the app only ever needs the
		// tunnel URL plus its Cloudflare token (REQ-046(3)).
		mux.Handle("/api/", NewHistoryHandler(t.cfg.History, t.gate, t.cfg.Models))
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	server := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	// Bind before the tunnel goes up: a daemon that cannot listen must fail here
	// instead of publishing a URL that answers 502 while logging "ready".
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("mobile: listen %s: %w", listen, err)
	}
	go func() {
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("mobile: serve %s: %v", listen, err)
		}
	}()
	stopServer := func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}
	if !t.cfg.Tunnel {
		return stopServer, nil
	}
	port := portOfListen(listen)
	url, stopTunnel, err := RunQuickTunnel(ctx, port, t.cfg.Cloudflared)
	if err != nil {
		return stopServer, err
	}
	log.Printf("mobile: quick tunnel public URL: %s", url)
	t.announceLoop(ctx, url)
	return func() {
		stopTunnel()
		stopServer()
	}, nil
}

func (t *Transport) announceLoop(ctx context.Context, publicURL string) {
	if t.cfg.Announce == nil || t.cfg.Tokens == nil {
		return
	}
	announce := func() {
		if t.cfg.Tokens.Get() == "" {
			return // no phone connected yet: nothing to authenticate with
		}
		if err := t.cfg.Announce(ctx, publicURL); err != nil {
			log.Printf("mobile: announce tunnel: %v", err)
		}
	}
	go func() {
		announce()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				announce()
			}
		}
	}()
}
