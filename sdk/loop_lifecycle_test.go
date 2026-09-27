package sdk

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

type lifecycleProvider struct {
	mu        sync.Mutex
	delegated bool
}

func (p *lifecycleProvider) Name() string               { return "test" }
func (p *lifecycleProvider) WithAPIKey(string) Provider { return p }
func (p *lifecycleProvider) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	resp, err := p.Generate(ctx, req)
	if err != nil {
		return nil, err
	}
	ch := make(chan Event, len(resp.Content)+len(resp.ToolCalls)+1)
	for _, part := range resp.Content {
		ch <- Event{Type: EventText, Text: part.Text}
	}
	for _, call := range resp.ToolCalls {
		call := call
		ch <- Event{Type: EventToolCall, ToolCall: &call}
	}
	ch <- Event{Type: EventDone, Response: &resp}
	close(ch)
	return ch, nil
}
func (p *lifecycleProvider) Generate(_ context.Context, req Request) (Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if strings.Contains(req.SystemPrompt, "You are the worker sub-agent") {
		return Response{Content: []ContentPart{{Type: ContentText, Text: "worker findings"}}}, nil
	}
	if !p.delegated {
		p.delegated = true
		return Response{ToolCalls: []ToolCall{{ID: "delegate", Name: "delegate_to_subagent", Arguments: `{"task":"investigate"}`}}}, nil
	}
	for _, turn := range req.Messages {
		for _, part := range turn.Content {
			if strings.Contains(part.Text, "<sub agent report") {
				return Response{Content: []ContentPart{{Type: ContentText, Text: "reviewed lifecycle result"}}}, nil
			}
		}
	}
	return Response{Content: []ContentPart{{Type: ContentText, Text: "acknowledged"}}}, nil
}

func TestLoopLifecyclePreservesRouteForAutoReports(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(boolLabel("stream", stream), func(t *testing.T) {
			agent, parent := newSubAgentTest(t, &lifecycleProvider{})
			input := Input{Source: "discord", SessionID: "mapped-project-session", Metadata: map[string]string{"channel_id": "channel-123", "original": "preserve"}, Turn: Turn{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: "new task"}}}}
			var mu sync.Mutex
			reportTurns := 0
			reviewed := make(chan struct{}, 1)
			loop := &HarnessLoop{Agent: agent, Source: staticInputSource{inputs: []Input{input}}, ResolveSession: func(_ context.Context, in Input) (*Session, error) {
				if in.Source != "tool" && in.SessionID != "mapped-project-session" {
					return nil, errorsNew("lost mapped routing alias")
				}
				return parent, nil
			}, BuildRequest: func(_ context.Context, in Input, _ *Session) (Request, error) {
				if in.Source == "discord" && len(in.Turn.Content) > 0 && strings.Contains(in.Turn.Content[0].Text, "<sub agent report") {
					mu.Lock()
					reportTurns++
					if in.Metadata["channel_id"] != "channel-123" || in.Metadata["original"] != "preserve" || in.SessionID != "mapped-project-session" {
						t.Errorf("lifecycle report lost route: %+v", in)
					}
					mu.Unlock()
				}
				return Request{Stream: stream}, nil
			}, Displays: []Display{DisplayFunc(func(_ context.Context, out Output) error {
				if out.Trace != nil && out.Trace.Stage == TraceResponse && out.Trace.Response != nil && responseText(*out.Trace.Response) == "reviewed lifecycle result" {
					select {
					case reviewed <- struct{}{}:
					default:
					}
				}
				return nil
			})}}
			if err := loop.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			select {
			case <-reviewed:
			case <-time.After(5 * time.Second):
				t.Fatal("final report continuation never completed")
			}
			mu.Lock()
			defer mu.Unlock()
			if reportTurns != 1 {
				t.Fatalf("expected exactly one final report continuation, got %d", reportTurns)
			}
		})
	}
}

func errorsNew(text string) error { return &lifecycleError{text} }

type lifecycleError struct{ text string }

func (e *lifecycleError) Error() string { return e.text }

func boolLabel(kind string, v bool) string {
	if v {
		return kind + "=true"
	}
	return kind + "=false"
}

func TestSubAgentFollowUpPreservesOriginalRouteMetadata(t *testing.T) {
	r, events := orchestrationRunner(t, &subAgentImmediateProvider{})
	input := Input{Source: "custom-transport", SessionID: "opaque-alias", Metadata: map[string]string{"channel_id": "original"}}
	traces := make(chan SubAgentEvent, 8)
	r.manager.SetTraceSink(func(event SubAgentEvent) { traces <- event })
	ctx := context.WithValue(context.Background(), lifecycleInputKey{}, input)
	id, err := r.Delegate(ctx, "investigate")
	if err != nil {
		t.Fatal(err)
	}
	awaitReport(t, events, "final")
	deadline := time.After(time.Second)
	captured := false
	for !captured {
		select {
		case event := <-traces:
			captured = event.Input.Source == "custom-transport" && event.Input.Metadata["channel_id"] == "original"
		case <-deadline:
			t.Fatal("worker trace never carried the original route")
		}
	}
	input.Metadata["channel_id"] = "changed"
	retry, err := r.FollowUp(context.Background(), id, "clarify findings")
	if err != nil {
		t.Fatal(err)
	}
	event := awaitReport(t, events, "final")
	if event.Input.Source != "custom-transport" || event.Input.SessionID != "opaque-alias" || event.Input.Metadata["channel_id"] != "original" {
		t.Fatalf("follow-up lost original route: %+v", event.Input)
	}
	if event.JobID != retry {
		t.Fatalf("report for wrong job: %s vs %s", event.JobID, retry)
	}
}
