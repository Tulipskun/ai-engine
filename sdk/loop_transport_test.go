package sdk

import (
	"context"
	"sync"
	"testing"
	"time"
)

type routedTestDisplay struct {
	source string
	mu     sync.Mutex
	seen   []Output
}

func (d *routedTestDisplay) Source() string { return d.source }

func (d *routedTestDisplay) Display(_ context.Context, output Output) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen = append(d.seen, output)
	return nil
}

func (d *routedTestDisplay) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}

func TestLoopRoutesOutputOnlyToMatchingSource(t *testing.T) {
	keys := NewKeyPool("test-key")
	session := NewSession(SessionConfig{ID: "discord:channel:1", Provider: ProviderOpenRouter, Model: "model", KeyIndex: 0}, keys)
	discord := &routedTestDisplay{source: "discord"}
	telegram := &routedTestDisplay{source: "telegram"}
	loop := &HarnessLoop{
		Client:         newLoopTestClient(),
		ResolveSession: func(context.Context, Input) (*Session, error) { return session, nil },
		Displays:       []Display{discord, telegram},
		DisplayTimeout: time.Second,
	}

	if err := loop.Handle(context.Background(), Input{
		Source:    "discord",
		SessionID: session.ID(),
		Turn:      Turn{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: "hello"}}},
	}); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(time.Second)
	for discord.count() == 0 {
		select {
		case <-deadline:
			t.Fatal("discord display was not dispatched")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if got := telegram.count(); got != 0 {
		t.Fatalf("telegram display received discord output: %d", got)
	}
}

func TestLoopSerializesConcurrentTurnsForSameSession(t *testing.T) {
	keys := NewKeyPool("test-key")
	session := NewSession(SessionConfig{ID: "web:conversation:1", Provider: ProviderOpenRouter, Model: "model", KeyIndex: 0}, keys)
	loop := &HarnessLoop{
		Client:         newLoopTestClient(),
		ResolveSession: func(context.Context, Input) (*Session, error) { return session, nil },
	}

	const turns = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < turns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := loop.Handle(context.Background(), Input{
				Source:    "web",
				SessionID: session.ID(),
				Turn:      Turn{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: "hello"}}},
			}); err != nil {
				t.Errorf("Handle() error = %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	history := session.History()
	if got, want := len(history), turns*2; got != want {
		t.Fatalf("history length = %d, want %d", got, want)
	}
	for i, turn := range history {
		want := RoleUser
		if i%2 == 1 {
			want = RoleModel
		}
		if turn.Role != want {
			t.Fatalf("history[%d].Role = %q, want %q; history=%+v", i, turn.Role, want, history)
		}
	}
}
