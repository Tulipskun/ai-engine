package main

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Tulipskun/ai-engine/runtime/d1store"
	"github.com/Tulipskun/ai-engine/sdk"
	mobiletransport "github.com/Tulipskun/ai-engine/transport/mobile"
)

// mobileIO is the daemon's input/output boundary for the phone. One direction
// turns an inbound WebSocket event into a canonical sdk.Input, the other turns a
// canonical sdk.Output back into outbound frames.
//
// Both directions also write to D1, because the app has to be able to rebuild a
// chat after it was closed (REQ-046(5)). The wire format itself is not decided
// here: transport/mobile owns the frames and the handshake, and this type only
// bridges them to the harness.
type mobileIO struct {
	mobile *mobileRuntime

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

func newMobileIO(mobile *mobileRuntime) *mobileIO {
	return &mobileIO{mobile: mobile}
}

// Source makes this an sdk.RoutedDisplay, so sdk.DispatchDisplay hands it only
// the output that belongs to the phone.
func (io *mobileIO) Source() string { return mobiletransport.SourceName }

// ---------------------------------------------------------------- inbound

// MirrorInput is the hook the transport calls for every accepted inbound turn. It
// returns the work to run off the read loop (D1 is a network call), or nil when
// there is nothing worth mirroring.
func (io *mobileIO) MirrorInput(input sdk.Input) func(context.Context) error {
	if io == nil || io.mobile == nil || io.mobile.client == nil || input.SessionID == "" {
		return nil
	}
	text := inputText(input)
	if text == "" {
		return nil
	}
	return func(ctx context.Context) error {
		gate := make(chan struct{})
		io.mirrorMu.Lock()
		if io.mirrorGates == nil {
			io.mirrorGates = map[string]chan struct{}{}
		}
		io.mirrorGates[input.SessionID] = gate
		io.mirrorMu.Unlock()
		seq, err := io.mobile.client.AppendTurnAt(ctx, input.SessionID, "user", "user", "", text)
		io.mirrorMu.Lock()
		if io.mirrorGates[input.SessionID] == gate {
			delete(io.mirrorGates, input.SessionID)
		}
		io.mirrorMu.Unlock()
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
func (io *mobileIO) waitUserMirror(sessionID string) {
	io.mirrorMu.Lock()
	gate := io.mirrorGates[sessionID]
	io.mirrorMu.Unlock()
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

// Display is the harness-facing sink. The transport publishes the frames; this
// call mirrors the finished turn into D1 first and only then lets the phone see
// it, because the phone refreshes from D1 the moment the done frame arrives. A
// display-before-mirror order makes the streamed answer vanish for a race window
// (AXCH-025).
func (io *mobileIO) Display(ctx context.Context, output sdk.Output) error {
	io.publishOutput(ctx, output)
	return nil
}

// publishOutput writes one finished turn to D1 and then to the phone.
func (io *mobileIO) publishOutput(ctx context.Context, output sdk.Output) {
	if io == nil || io.mobile == nil || io.mobile.transport == nil {
		return
	}
	// A worker's own turn is reported through its job, not as a chat message.
	if output.Trace != nil && strings.EqualFold(output.Metadata["trace_actor"], "subagent") {
		return
	}
	text := mobiletransport.FinalText(output)
	if output.SessionID == "" || text == "" {
		return
	}
	// One turn in D1 per turn on the wire.
	terminal := output.Trace != nil && output.Trace.Stage == sdk.TraceResponse
	key := output.SessionID + "\x00" + text
	if !io.turns.take(key, terminal) {
		return
	}
	jobID := output.Metadata["mobile_job_id"]
	io.waitUserMirror(output.SessionID)

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

	seq, err := io.mobile.client.AppendModelTurn(ctx, output.SessionID, "main", jobID, text, meta)
	if err != nil {
		log.Printf("mobile: mirror turn to D1 session=%s: %v", output.SessionID, err)
		io.turns.forget(key)
		return
	}
	log.Printf("mobile: mirrored answer session=%s seq=%d model=%s in=%d out=%d ms=%d",
		output.SessionID, seq, meta.Model, meta.InputTokens, meta.OutputTokens, meta.DurationMs)
	if err := io.mobile.transport.Display(ctx, output); err != nil {
		log.Printf("mobile: display source=%s session=%s: %v", output.Source, output.SessionID, err)
	}
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

func inputText(input sdk.Input) string {
	var b strings.Builder
	for _, part := range input.Turn.Content {
		b.WriteString(part.Text)
	}
	return strings.TrimSpace(b.String())
}
