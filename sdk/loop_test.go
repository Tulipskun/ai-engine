package sdk

import (
	"context"
	"testing"
	"time"
)

type loopTestProvider struct{}

func (loopTestProvider) Name() string { return "test" }
func (loopTestProvider) Generate(context.Context, Request) (Response, error) {
	return Response{Content: []ContentPart{{Type: ContentText, Text: "response"}}}, nil
}
func (loopTestProvider) Stream(context.Context, Request) (<-chan Event, error) {
	ch := make(chan Event, 1)
	ch <- Event{Type: EventDone}
	close(ch)
	return ch, nil
}
func newLoopTestClient() *RouterClient {
	router := NewRouter()
	router.Register(ModelRoute{Provider: ProviderOpenRouter, Model: "model", Adapter: AdapterOpenAI})
	client := NewRouterClient(router)
	client.RegisterAdapter(AdapterOpenAI, loopTestProvider{})
	return client
}

func TestLoopCommitsSessionBeforeDisplay(t *testing.T) {
	keys := NewKeyPool("test-key")
	session := NewSession(SessionConfig{ID: "s1", Provider: ProviderOpenRouter, Model: "model", KeyIndex: 0}, keys)
	seen := make(chan []Turn, 1)
	display := DisplayFunc(func(_ context.Context, _ Output) error { seen <- session.History(); return nil })
	loop := &HarnessLoop{Client: newLoopTestClient(), ResolveSession: func(context.Context, Input) (*Session, error) { return session, nil }, Displays: []Display{display}, DisplayTimeout: time.Second}
	input := Input{Source: "discord", SessionID: "s1", Turn: Turn{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: "input"}}}}
	if err := loop.Handle(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	select {
	case history := <-seen:
		if len(history) != 2 || history[0].Role != RoleUser || history[1].Role != RoleModel {
			t.Fatalf("display observed uncommitted history: %+v", history)
		}
	case <-time.After(time.Second):
		t.Fatal("display was not dispatched")
	}
}

func TestLoopToolResultDoesNotDeadlock(t *testing.T) {
	session := NewSession(SessionConfig{ID: "s1", Provider: "test", Model: "model", KeyIndex: 0}, NewKeyPool("test-key"))
	provider := &agentTestProvider{responses: []Response{{Content: []ContentPart{{Type: ContentText, Text: `{"steps":[{"goal":"use tool"}]}`}}}, {ToolCalls: []ToolCall{{ID: "1", Name: "echo", Arguments: "{}"}}}, {Content: []ContentPart{{Type: ContentText, Text: "finished"}}}}}
	router := NewRouter()
	router.Register(ModelRoute{Provider: "test", Model: "model", Adapter: AdapterOpenAI})
	client := NewRouterClient(router)
	client.RegisterAdapter(AdapterOpenAI, provider)
	tools := &agentTestTools{definitions: []Tool{{Name: "echo"}}}
	loop := &HarnessLoop{Client: client, Agent: &Agent{Client: client, Tools: tools, MaxRetries: 0}, ResolveSession: func(context.Context, Input) (*Session, error) { return session, nil }}
	done := make(chan error, 1)
	go func() {
		done <- loop.Entry(context.Background(), Input{Source: "test", SessionID: "s1", Turn: Turn{Role: RoleUser}})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("tool result re-entered the session lock")
	}
}

func TestLoopEmitsFinalResponseTrace(t *testing.T) {
	session := NewSession(SessionConfig{ID: "s1", Provider: "test", Model: "model", KeyIndex: 0}, NewKeyPool("test-key"))
	provider := &tracedLoopTestProvider{}
	router := NewRouter()
	router.Register(ModelRoute{Provider: "test", Model: "model", Adapter: AdapterOpenAI})
	client := NewRouterClient(router)
	client.RegisterAdapter(AdapterOpenAI, provider)
	events := make(chan TraceEvent, 8)
	display := DisplayFunc(func(_ context.Context, output Output) error {
		if output.Trace != nil {
			events <- *output.Trace
		}
		return nil
	})
	loop := &HarnessLoop{Client: client, Agent: &Agent{Client: client, MaxRetries: 0}, ResolveSession: func(context.Context, Input) (*Session, error) { return session, nil }, Displays: []Display{display}, DisplayTimeout: time.Second}
	if err := loop.Handle(context.Background(), Input{Source: "discord", SessionID: "s1", Turn: Turn{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: "input"}}}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	var sawContent, sawFinal bool
	var finalCount int
	for {
		select {
		case event := <-events:
			if event.Stage == TraceResponseContent && event.Response != nil && len(event.Response.Content) > 0 && event.Response.Content[0].Text == "response" {
				sawContent = true
			}
			if event.Stage == TraceResponse {
				finalCount++
				if event.Response == nil || len(event.Response.Content) != 1 || event.Response.Content[0].Text != "response" {
					t.Fatalf("final response trace missing content: %+v", event.Response)
				}
				sawFinal = true
			}
			if sawContent && sawFinal {
				if finalCount != 1 {
					t.Fatalf("final response trace emitted %d times", finalCount)
				}
				return
			}
		case <-deadline:
			t.Fatalf("content trace=%v final trace=%v (final x%d)", sawContent, sawFinal, finalCount)
		}
	}
}

type tracedLoopTestProvider struct{}

func (tracedLoopTestProvider) Name() string { return "test" }
func (tracedLoopTestProvider) Generate(context.Context, Request) (Response, error) {
	return Response{Content: []ContentPart{{Type: ContentText, Text: "response"}}}, nil
}
func (tracedLoopTestProvider) Stream(context.Context, Request) (<-chan Event, error) {
	ch := make(chan Event, 1)
	ch <- Event{Type: EventDone}
	close(ch)
	return ch, nil
}
func (tracedLoopTestProvider) WithAPIKey(string) Provider { return tracedLoopTestProvider{} }

func TestLoopContinuesWhenDisplayFails(t *testing.T) {
	keys := NewKeyPool("test-key")
	session := NewSession(SessionConfig{ID: "s1", Provider: ProviderOpenRouter, Model: "model", KeyIndex: 0}, keys)
	loop := &HarnessLoop{Client: newLoopTestClient(), ResolveSession: func(context.Context, Input) (*Session, error) { return session, nil }, Displays: []Display{DisplayFunc(func(context.Context, Output) error { return context.Canceled })}, DisplayTimeout: time.Second}
	if err := loop.Handle(context.Background(), Input{SessionID: "s1", Turn: Turn{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: "input"}}}}); err != nil {
		t.Fatal(err)
	}
	if got := session.History(); len(got) != 2 {
		t.Fatalf("display failure changed core processing: %+v", got)
	}
}
