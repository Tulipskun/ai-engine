package sdk

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestCancelTurnStopsTheTurnInFlight(t *testing.T) {
	started := make(chan struct{})
	router := NewRouter()
	router.Register(ModelRoute{Provider: ProviderOpenRouter, Model: "model", Adapter: AdapterOpenAI})
	client := NewRouterClient(router)
	client.RegisterAdapter(AdapterOpenAI, blockingProvider{started: started, once: &sync.Once{}})
	session := NewSession(SessionConfig{ID: "work-1", Provider: ProviderOpenRouter, Model: "model"}, NewKeyPool("k"))
	loop := &HarnessLoop{
		Client:         client,
		Agent:          &Agent{Client: client},
		ResolveSession: func(context.Context, Input) (*Session, error) { return session, nil },
		BuildRequest: func(context.Context, Input, *Session) (Request, error) {
			return Request{Model: "model", Stream: true}, nil
		},
	}
	errs := make(chan error, 1)
	go func() {
		errs <- loop.Entry(context.Background(), Input{SessionID: "work-1", Turn: Turn{Role: RoleUser}})
	}()
	select {
	case <-started:
	case err := <-errs:
		t.Fatalf("turn failed before the provider was called: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("turn never started")
	}
	if !loop.Busy("work-1") {
		t.Fatal("Busy should be true while a turn is in flight")
	}
	if !loop.CancelTurn("work-1") {
		t.Fatal("CancelTurn should report the turn it stopped")
	}
	if loop.Busy("work-1") {
		t.Fatal("Busy should be false once the turn is cancelled")
	}
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("turn error = %v, want context.Canceled", err)
	}
	if loop.CancelTurn("work-1") {
		t.Fatal("cancelling twice should report that nothing was running")
	}
}

// blockingProvider stands in for a provider call that runs until the turn's
// context is cancelled, which is exactly what the stop button does.
type blockingProvider struct {
	started chan struct{}
	once    *sync.Once
}

func (p blockingProvider) Stream(ctx context.Context, _ Request) (<-chan Event, error) {
	ch := make(chan Event)
	go func() {
		defer close(ch)
		p.once.Do(func() { close(p.started) })
		<-ctx.Done()
		ch <- Event{Type: EventError, Err: ctx.Err()}
	}()
	return ch, nil
}

func (blockingProvider) Generate(context.Context, Request) (Response, error) {
	return Response{Content: []ContentPart{{Type: ContentText, Text: "unused"}}}, nil
}

func (blockingProvider) Name() string { return "blocking" }
