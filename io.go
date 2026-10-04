package main

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Tulipskun/ai-engine/io"
	"github.com/Tulipskun/ai-engine/io/gateway"
	"github.com/Tulipskun/ai-engine/io/state"
)

// mobileIO is the daemon's input/output boundary for the phone. One direction
// turns an inbound WebSocket event into a canonical b.Input, the other turns a
// canonical b.Output back into outbound frames.
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

// Source makes this an agent.RoutedDisplay, so agent.DispatchDisplay hands it only
// the output that belongs to the phone.
func (b *mobileIO) Source() string { return gateway.SourceName }

// ---------------------------------------------------------------- inbound

// MirrorInput is the hook the transport calls for every accepted inbound turn. It
// returns the work to run off the read loop (D1 is a network call), or nil when
// there is nothing worth mirroring.
func (b *mobileIO) MirrorInput(input io.Input) func(context.Context) error {
	if b == nil || b.mobile == nil || b.mobile.client == nil || input.SessionID == "" {
		return nil
	}
	text := inputText(input)
	if text == "" {
		return nil
	}
	return func(ctx context.Context) error {
		gate := make(chan struct{})
		b.mirrorMu.Lock()
		if b.mirrorGates == nil {
			b.mirrorGates = map[string]chan struct{}{}
		}
		b.mirrorGates[input.SessionID] = gate
		b.mirrorMu.Unlock()
		seq, err := b.mobile.client.AppendTurnAt(ctx, input.SessionID, "user", "user", "", text)
		b.mirrorMu.Lock()
		if b.mirrorGates[input.SessionID] == gate {
			delete(b.mirrorGates, input.SessionID)
		}
		b.mirrorMu.Unlock()
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
func (b *mobileIO) waitUserMirror(sessionID string) {
	b.mirrorMu.Lock()
	gate := b.mirrorGates[sessionID]
	b.mirrorMu.Unlock()
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

// Display is the harness-facing sink. A finished main turn is
// mirrored into D1 before the phone is told about it, because the
// app refreshes from D1 the moment the done frame arrives — a
// display-before-mirror order makes the streamed answer vanish for
// a race window (AXCH-025). The output is then forwarded to the
// transport in every shape it arrives: deltas, tool calls,
// reasoning, a worker's progress and the final answer, so the
// phone sees the turn live, not only its ending.
func (b *mobileIO) Display(ctx context.Context, output io.Output) error {
	if b == nil || b.mobile == nil || b.mobile.transport == nil {
		return nil
	}
	b.mirrorOutput(ctx, output)
	return b.mobile.transport.Display(ctx, output)
}

// mirrorOutput writes one finished turn to D1. A worker's own turn
// is reported through its job frames, not as a chat message, so it
// is not mirrored. A failure here is logged and never swallows the
// answer on the phone: the turn still displays, it only does not
// survive a restart.
func (b *mobileIO) mirrorOutput(ctx context.Context, output io.Output) {
	if b == nil || b.mobile == nil || b.mobile.transport == nil {
		return
	}
	// A worker's own turn is reported through its job, not as a chat message.
	if output.Trace != nil && strings.EqualFold(output.Metadata["trace_actor"], "subagent") {
		return
	}
	text := gateway.FinalText(output)
	if output.SessionID == "" || text == "" {
		return
	}
	// One turn in D1 per turn on the wire.
	terminal := output.Trace != nil && output.Trace.Stage == io.TraceResponse
	key := output.SessionID + "\x00" + text
	if !b.turns.take(key, terminal) {
		return
	}
	jobID := output.Metadata["mobile_job_id"]
	b.waitUserMirror(output.SessionID)

	// The footer is read off the terminal trace: the model that answered, the
	// counts it reported (including cached usage) and how long it took (AX-095).
	var meta state.TurnMeta
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

	seq, err := b.mobile.client.AppendModelTurn(ctx, output.SessionID, "main", jobID, text, meta)
	if err != nil {
		log.Printf("mobile: mirror turn to D1 session=%s: %v", output.SessionID, err)
		b.turns.forget(key)
		return
	}
	log.Printf("mobile: mirrored answer session=%s seq=%d model=%s in=%d out=%d ms=%d",
		output.SessionID, seq, meta.Model, meta.InputTokens, meta.OutputTokens, meta.DurationMs)
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

func inputText(input io.Input) string {
	var sb strings.Builder
	for _, part := range input.Turn.Content {
		sb.WriteString(part.Text)
	}
	return strings.TrimSpace(sb.String())
}
