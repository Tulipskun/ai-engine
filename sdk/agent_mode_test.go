package sdk

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestSessionAgentModeRoundTrip(t *testing.T) {
	session := NewSession(SessionConfig{ID: "mode"}, NewKeyPool("key"))
	if got := session.Config().AgentMode; got != "" {
		t.Fatalf("default mode = %q", got)
	}
	if err := session.SetAgentMode(AgentModeSub); err != nil {
		t.Fatal(err)
	}
	if got := session.Config().AgentMode; got != AgentModeSub {
		t.Fatalf("mode = %q", got)
	}
	if err := session.SetAgentMode(AgentModeMain); err != nil {
		t.Fatal(err)
	}
	if got := session.Config().AgentMode; got != AgentModeMain {
		t.Fatalf("mode = %q", got)
	}
	if err := session.SetAgentMode("turbo"); err == nil {
		t.Fatal("invalid mode must fail")
	}
}

// TestSessionAgentModeSelectsRole runs the same shared agent against two
// sessions: the default session plans as the Main Agent while the sub-mode
// session answers as a worker with the full execution tool set and no
// planning prompt wrapper (REQ-029).
func TestSessionAgentModeSelectsRole(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{true: "stream", false: "turn"}[stream], func(t *testing.T) {
			p := &roleTestProvider{agentTestProvider{responses: []Response{
				{Content: []ContentPart{{Type: ContentText, Text: "done"}}},
			}}}
			c, s := newAgentTestSession(p)
			if err := s.SetAgentMode(AgentModeSub); err != nil {
				t.Fatal(err)
			}
			tools := &agentTestTools{definitions: []Tool{{Name: "run_command", Description: "execute shell", InputSchema: map[string]any{"type": "object"}}}}
			a := &Agent{Client: c, Tools: tools, MaxRetries: -1}
			base := "channel system prompt"
			if _, err := a.RunTurn(context.Background(), s, Turn{Role: RoleUser}, Request{Stream: stream, SystemPrompt: base}); err != nil {
				t.Fatal(err)
			}
			if len(p.requests) != 1 {
				t.Fatalf("requests=%d", len(p.requests))
			}
			req := p.requests[0]
			if req.SystemPrompt != base || strings.Contains(req.SystemPrompt, planningSystemInstruction) {
				t.Fatalf("sub prompt wrapped: %q", req.SystemPrompt)
			}
			if !reflect.DeepEqual(req.Tools, tools.Definitions()) {
				t.Fatalf("sub tools=%+v", req.Tools)
			}
		})
	}
}

// TestSubSessionTurnUsesOwnSettings runs one turn on a dedicated sub-mode
// session (its own top-level settings and history) and asserts the request
// carries that session's provider, model, thinking and temperature.
func TestSubSessionTurnUsesOwnSettings(t *testing.T) {
	p := &roleTestProvider{agentTestProvider{responses: []Response{
		{Content: []ContentPart{{Type: ContentText, Text: "done"}}},
	}}}
	c, s := newAgentTestSession(p)
	subKeys := NewKeyPool("key")
	if err := s.SetAgentMode(AgentModeSub); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProvider("test", subKeys); err != nil {
		t.Fatal(err)
	}
	if err := s.SetModel("model"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetThinkingLevel(ThinkingHigh); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTemperature(0.5); err != nil {
		t.Fatal(err)
	}
	tools := &agentTestTools{definitions: []Tool{{Name: "run_command", Description: "execute shell", InputSchema: map[string]any{"type": "object"}}}}
	a := &Agent{Client: c, Tools: tools, MaxRetries: -1}
	if _, err := a.RunTurn(context.Background(), s, Turn{Role: RoleUser}, Request{SystemPrompt: "base"}); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) != 1 {
		t.Fatalf("requests=%d", len(p.requests))
	}
	req := p.requests[0]
	if req.Provider != "test" || req.Model != "model" {
		t.Fatalf("route wrong: %+v", req)
	}
	if req.ThinkingLevel != ThinkingHigh {
		t.Fatalf("thinking wrong: %+v", req.ThinkingLevel)
	}
	if req.Temperature == nil || *req.Temperature != 0.5 {
		t.Fatalf("temperature wrong: %+v", req.Temperature)
	}
	if !reflect.DeepEqual(req.Tools, tools.Definitions()) {
		t.Fatalf("sub turn must carry full tools: %+v", req.Tools)
	}
}
