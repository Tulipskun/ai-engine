package sdk

import (
	"context"
	"errors"
	"testing"
)

type normalToolTraceProvider struct {
	calls          int
	seenToolResult bool
}

func (p *normalToolTraceProvider) Name() string { return "normal-tool-trace" }

func (p *normalToolTraceProvider) Generate(_ context.Context, req Request) (Response, error) {
	p.calls++
	if p.calls == 1 {
		return Response{ToolCalls: []ToolCall{{ID: "call-1", Name: "read", Arguments: `{"path":"/tmp/example.txt"}`}}}, nil
	}
	for _, turn := range req.Messages {
		if turn.Role == RoleToolResult && turn.ToolResult != nil && turn.ToolResult.ID == "call-1" {
			p.seenToolResult = true
		}
	}
	if !p.seenToolResult {
		return Response{}, errors.New("tool result was not returned to provider")
	}
	return Response{Content: []ContentPart{{Type: ContentText, Text: "done"}}}, nil
}

func (p *normalToolTraceProvider) Stream(context.Context, Request) (<-chan Event, error) {
	return nil, errors.New("stream is not part of this test")
}

func (p *normalToolTraceProvider) WithAPIKey(string) Provider { return p }

func TestAgentNormalGenerateToolTraceLoop(t *testing.T) {
	provider := &normalToolTraceProvider{}
	client, session := newAgentTestSession(provider)
	tools := &agentTestTools{definitions: []Tool{{Name: "read"}}}
	agent := &Agent{Client: client, Tools: tools, MaxRetries: 0, DisablePlanning: true}

	var toolCalls []string
	var toolResults []string
	_, err := agent.RunTurnWithTrace(context.Background(), session, Turn{Role: RoleUser}, Request{}, func(_ context.Context, event TraceEvent) {
		switch event.Stage {
		case TraceToolCall:
			if event.ToolCall != nil {
				toolCalls = append(toolCalls, event.ToolCall.Name)
			}
		case TraceToolResult:
			if event.ToolCall != nil {
				toolResults = append(toolResults, event.ToolCall.Name)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if provider.calls != 2 || !provider.seenToolResult {
		t.Fatalf("provider loop calls=%d seenToolResult=%v", provider.calls, provider.seenToolResult)
	}
	if len(toolCalls) != 1 || toolCalls[0] != "read" {
		t.Fatalf("tool call trace=%v", toolCalls)
	}
	if len(toolResults) != 1 || toolResults[0] != "read" {
		t.Fatalf("tool result trace=%v", toolResults)
	}
}
