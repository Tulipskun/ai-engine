package sdk

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Use the same response sequence through both provider paths, including a
// tool-result continuation, so prompt/tool regressions cannot hide after turn 1.
type roleTestProvider struct{ agentTestProvider }

func (p *roleTestProvider) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	resp, err := p.Generate(ctx, req)
	if err != nil {
		return nil, err
	}
	ch := make(chan Event, 1)
	ch <- Event{Type: EventDone, Response: &resp}
	close(ch)
	return ch, nil
}
func (p *roleTestProvider) WithAPIKey(string) Provider { return p }

func TestAgentRoleSeparation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, worker := range []bool{false, true} {
			for _, configured := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%v/worker=%v/subagent=%v", stream, worker, configured), func(t *testing.T) {
					p := &roleTestProvider{agentTestProvider: agentTestProvider{responses: []Response{
						{ToolCalls: []ToolCall{{ID: "direct", Name: "bash", Arguments: `{}`}}},
						{ToolCalls: []ToolCall{{ID: "plan", Name: "plan", Arguments: `{"plan":"inspect"}`}}},
						{ToolCalls: []ToolCall{{ID: "after-plan", Name: "bash", Arguments: `{}`}}},
						{Content: []ContentPart{{Type: ContentText, Text: "done"}}},
					}}}
					if worker {
						p.responses[1] = Response{ToolCalls: []ToolCall{{ID: "worker-again", Name: "bash", Arguments: `{}`}}}
					}
					c, s := newAgentTestSession(p)
					tools := &agentTestTools{definitions: []Tool{
						{Name: "bash", Description: "execute shell", InputSchema: map[string]any{"type": "object"}},
						{Name: "read", Description: "read a file", InputSchema: map[string]any{"type": "object"}},
						{Name: "edit_file", Description: "edit a file", InputSchema: map[string]any{"type": "object"}},
					}}
					a := &Agent{Client: c, Tools: tools, DisablePlanning: worker, SubAgentConfig: SubAgentConfig{Enabled: configured}, MaxRetries: -1}
					base := "You are the worker sub-agent. Use bash to validate.\nAvailable tools:\ncustom context\nProject Requirements (repository source of truth):\nkeep scope\ncustom trailing instruction"
					if !worker {
						base = "Custom main context\n" + base[strings.Index(base, "Available tools:"):]
					}
					_, err := a.RunTurn(context.Background(), s, Turn{Role: RoleUser}, Request{Stream: stream, SystemPrompt: base, Tools: []Tool{{Name: "injected"}}})
					if err != nil {
						t.Fatal(err)
					}
					if len(p.requests) != 4 {
						t.Fatalf("requests=%d", len(p.requests))
					}
					for _, req := range p.requests {
						if worker {
							if req.SystemPrompt != base {
								t.Fatalf("worker prompt changed: %q", req.SystemPrompt)
							}
							if !reflect.DeepEqual(req.Tools, tools.Definitions()) {
								t.Fatalf("worker tools=%+v", req.Tools)
							}
						} else {
							if !strings.HasPrefix(req.SystemPrompt, base) || strings.Count(req.SystemPrompt, planningSystemInstruction) != 1 {
								t.Fatalf("main prompt=%q", req.SystemPrompt)
							}
							want := []string{"plan", "read"}
							if configured {
								want = append(want, "delegate_to_subagent", "stop_subagent", "follow_up_subagent", "continue_subagent", "accept_subagent_result")
							}
							names := make(map[string]bool)
							for _, tool := range req.Tools {
								names[tool.Name] = true
							}
							if len(req.Tools) != len(want) {
								t.Fatalf("main tools=%+v", req.Tools)
							}
							for _, name := range want {
								if !names[name] {
									t.Fatalf("missing %s", name)
								}
							}
						}
					}
					if worker {
						if len(tools.results) != 3 {
							t.Fatalf("worker executions=%d", len(tools.results))
						}
					} else {
						if len(tools.results) != 0 {
							t.Fatal("main executed worker tools")
						}
						rejected := 0
						for _, turn := range s.History() {
							if turn.ToolResult != nil && (turn.ToolResult.ID == "direct" || turn.ToolResult.ID == "after-plan") {
								if !turn.ToolResult.IsError || !strings.Contains(turn.ToolResult.Content, "no write/exec or search tools") {
									t.Fatalf("direct execution not rejected: %+v", turn.ToolResult)
								}
								rejected++
							}
						}
						if rejected != 2 {
							t.Fatalf("rejections=%d", rejected)
						}
					}
				})
			}
		}
	}
}

func TestStreamDoneOnlyContentIsTracedOnce(t *testing.T) {
	p := &roleTestProvider{agentTestProvider: agentTestProvider{responses: []Response{{Content: []ContentPart{{Type: ContentText, Text: "final ไทย🙂 "}}}}}}
	client, session := newAgentTestSession(p)
	agent := &Agent{Client: client}
	var content strings.Builder
	terminal := 0
	_, err := agent.RunTurnWithTrace(context.Background(), session, Turn{Role: RoleUser}, Request{Stream: true}, func(_ context.Context, event TraceEvent) {
		if event.Stage == TraceResponseContent {
			content.WriteString(event.Text)
			if event.Response != nil {
				for _, part := range event.Response.Content {
					content.WriteString(part.Text)
				}
			}
		}
		if event.Stage == TraceResponse {
			terminal++
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if content.String() != "final ไทย🙂 " || terminal != 1 {
		t.Fatalf("content=%q terminal=%d", content.String(), terminal)
	}
}
