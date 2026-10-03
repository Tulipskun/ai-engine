package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Tulipskun/ai-engine/sdk"
)

// Exercise the CLI's real execution registry through a delegated SDK worker.
// No network, subprocess or live transport is used.
type roleCaptureProvider struct{ workerRequests chan sdk.Request }

func (p *roleCaptureProvider) Name() string                   { return "test" }
func (p *roleCaptureProvider) WithAPIKey(string) sdk.Provider { return p }
func (p *roleCaptureProvider) Generate(_ context.Context, req sdk.Request) (sdk.Response, error) {
	worker := !strings.Contains(req.SystemPrompt, "You are the Main Agent")
	if worker {
		p.workerRequests <- req
	}
	call := sdk.ToolCall{ID: "delegate", Name: "delegate_to_subagent", Arguments: `{"task":"read fixture.txt and report its contents"}`}
	if worker {
		call = sdk.ToolCall{ID: "read", Name: "read", Arguments: `{"path":"fixture.txt"}`}
	}
	for _, turn := range req.Messages {
		if turn.ToolResult != nil && turn.ToolResult.ID == call.ID {
			return sdk.Response{Content: []sdk.ContentPart{{Type: sdk.ContentText, Text: turn.ToolResult.Content}}}, nil
		}
	}
	return sdk.Response{ToolCalls: []sdk.ToolCall{call}}, nil
}
func (p *roleCaptureProvider) Stream(ctx context.Context, req sdk.Request) (<-chan sdk.Event, error) {
	resp, err := p.Generate(ctx, req)
	if err != nil {
		return nil, err
	}
	ch := make(chan sdk.Event, 1)
	ch <- sdk.Event{Type: sdk.EventDone, Response: &resp}
	close(ch)
	return ch, nil
}

func TestDelegatedWorkerRetainsRealToolsAndContext(t *testing.T) {
	for _, custom := range []bool{false, true} {
		name := "default"
		if custom {
			name = "custom"
		}
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.MkdirAll(filepath.Join(workspace, "requirements"), 0700); err != nil {
				t.Fatal(err)
			}
			requirement := "REQ-TEST — Preserve the read policy and unrelated project context."
			for path, content := range map[string]string{"requirements/functional.md": requirement, "fixture.txt": "worker read succeeded"} {
				if err := os.WriteFile(filepath.Join(workspace, path), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			router := sdk.NewRouter()
			keys := sdk.NewKeyPool("test-key")
			router.RegisterProvider(sdk.ProviderConfig{ID: "test", BaseURL: "http://test", Keys: keys, Adapter: sdk.AdapterOpenAI})
			router.Register(sdk.ModelRoute{Provider: "test", Model: "model", Adapter: sdk.AdapterOpenAI})
			client := sdk.NewRouterClient(router)
			provider := &roleCaptureProvider{workerRequests: make(chan sdk.Request, 4)}
			client.RegisterAdapter("test", sdk.AdapterOpenAI, provider)
			state := t.TempDir()
			customPrompt := "Custom worker context. Use read to inspect only the assigned file.\nAvailable tools:\nkeep this custom tail"
			if custom {
				if err := os.MkdirAll(filepath.Join(state, "config"), 0700); err != nil {
					t.Fatal(err)
				}
				// A custom configured prompt should not be replaced by the SDK default.
				cfg := `{"sub_agent":{"enabled":true,"SystemPrompt":"Custom worker context. Use read to inspect only the assigned file.\nAvailable tools:\nkeep this custom tail"}}`
				if err := os.WriteFile(filepath.Join(state, "config/system.json"), []byte(cfg), 0600); err != nil {
					t.Fatal(err)
				}
			}
			agent, err := newAgent(client, workspace, state)
			if err != nil {
				t.Fatal(err)
			}
			session, err := sdk.OpenSession(filepath.Join(state, "parent.db"), sdk.SessionConfig{ID: "parent", Provider: "test", Model: "model"}, keys)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			if err := session.SetPlan([]string{"inspect the fixture", "report findings"}); err != nil {
				t.Fatal(err)
			}
			reported := make(chan string, 1)
			agent.SetSubAgentSinks(func(event sdk.SubAgentEvent) {
				if event.Kind == "final" {
					reported <- event.Message()
				}
			}, nil)
			if _, err := agent.RunTurn(context.Background(), session, sdk.Turn{Role: sdk.RoleUser}, sdk.Request{SystemPrompt: defaultSystemPrompt(agent)}); err != nil {
				t.Fatal(err)
			}
			// Async delegation: the final handoff report must arrive on the
			// report sink for the planner session to review (REQ-019, REQ-025).
			var message string
			select {
			case message = <-reported:
			case <-time.After(5 * time.Second):
				t.Fatal("worker final report never arrived")
			}
			if !strings.Contains(message, "worker read succeeded") || !strings.Contains(message, "<sub agent report") {
				t.Fatalf("final report message wrong: %s", message)
			}
			delegateACK := false
			for _, turn := range session.History() {
				if turn.Role == sdk.RoleToolResult && turn.ToolResult != nil && strings.Contains(turn.ToolResult.Content, "sub-agent started: sa-") {
					delegateACK = true
				}
			}
			if !delegateACK {
				t.Fatal("delegate did not return control immediately with a job id")
			}
			if len(provider.workerRequests) != 2 {
				t.Fatalf("worker requests=%d", len(provider.workerRequests))
			}
			for i := 0; i < 2; i++ {
				req := <-provider.workerRequests
				if !reflect.DeepEqual(req.Tools, agent.Tools.Definitions()) {
					t.Fatalf("worker definitions differ from real registry: %+v", req.Tools)
				}
				// The worker receives the real execution registry, including screen_control.
				if len(req.Tools) != 3 {
					t.Fatalf("expected the real execution registry, got %d tools", len(req.Tools))
				}
				for _, tool := range req.Tools {
					switch tool.Name {
					case "plan", "delegate_to_subagent", "stop_subagent", "follow_up_subagent", "continue_subagent", "accept_subagent_result":
						t.Fatalf("worker exposed %s", tool.Name)
					}
				}
				for _, forbidden := range []string{"You are the Main Agent", "do not have execution tools", "First delegate repository investigation"} {
					if strings.Contains(req.SystemPrompt, forbidden) {
						t.Fatalf("worker has main restriction %q: %s", forbidden, req.SystemPrompt)
					}
				}
				for _, want := range []string{requirement, "Current plan:\n1. inspect the fixture\n2. report findings", "Current plan step:\ninspect the fixture"} {
					if !strings.Contains(req.SystemPrompt, want) {
						t.Fatalf("worker context missing %q: %s", want, req.SystemPrompt)
					}
				}
				if custom {
					if !strings.HasPrefix(req.SystemPrompt, customPrompt) {
						t.Fatalf("custom worker context lost: %s", req.SystemPrompt)
					}
				} else {
					for _, want := range []string{"You are the worker sub-agent", "Do not delegate to another agent", "Do not change project scope", "Do not communicate with the end user", "report the result to the planner"} {
						if !strings.Contains(req.SystemPrompt, want) {
							t.Fatalf("worker default missing %q", want)
						}
					}
				}
			}
		})
	}
}
