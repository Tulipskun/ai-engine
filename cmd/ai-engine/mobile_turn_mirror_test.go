package main

import (
	"testing"
	"time"
)

// A streamed answer is reported by the sdk as a content event and again on the
// terminal one: D1 must still end up with a single row for the turn.
func TestTurnMirrorAdmitsOneRowPerTurn(t *testing.T) {
	var mirror turnMirror
	key := "work-1\x00คำตอบเดียว"
	if !mirror.take(key, false) {
		t.Fatal("the first report of a turn must be admitted")
	}
	if mirror.take(key, true) {
		t.Fatal("the terminal repeat must be dropped")
	}
	if !mirror.take(key, false) {
		t.Fatal("a later turn with the same text must be admitted again")
	}
}

func TestTurnMirrorForgetsAFailedWrite(t *testing.T) {
	var mirror turnMirror
	key := "work-2\x00ok"
	if !mirror.take(key, false) {
		t.Fatal("first report must be admitted")
	}
	mirror.forget(key)
	if !mirror.take(key, false) {
		t.Fatal("a failed write must not block the retry")
	}
}

// The answer mirror must wait for the user mirror of its turn: D1 numbers
// rows MAX+1, so an answer that lands first steals the lower seq and the phone
// (which numbers its pending row from its own counter) drops the answer on a
// seq collision it can never recover from.
func TestWaitUserMirrorWaitsForTheGate(t *testing.T) {
	var m mobileRuntime
	released := make(chan struct{})
	go func() { m.waitUserMirror("s1"); close(released) }()
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("wait with no gate must return at once")
	}

	m.mirrorMu.Lock()
	if m.mirrorGates == nil {
		m.mirrorGates = map[string]chan struct{}{}
	}
	gate := make(chan struct{})
	m.mirrorGates["s1"] = gate
	m.mirrorMu.Unlock()
	waited := make(chan struct{})
	go func() { m.waitUserMirror("s1"); close(waited) }()
	select {
	case <-waited:
		t.Fatal("wait returned while the gate was still open")
	case <-time.After(100 * time.Millisecond):
	}
	close(gate)
	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("wait did not return after the gate closed")
	}
}
