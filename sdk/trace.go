package sdk

import (
	"context"
	"strings"
	"time"
)

type TraceStage string

const (
	TraceRequest         TraceStage = "request"
	TraceProviderReady   TraceStage = "provider_ready"
	TraceResponseText    TraceStage = "response_text"
	TraceResponseContent TraceStage = "response_content"
	TraceToolCall        TraceStage = "tool_call"
	TraceToolRunning     TraceStage = "tool_running"
	TraceToolResult      TraceStage = "tool_result"
	TraceResponse        TraceStage = "response"
	TraceRetryWait       TraceStage = "retry_wait"
	TraceError           TraceStage = "error"
)

type TraceEvent struct {
	Stage      TraceStage    `json:"stage"`
	Message    string        `json:"message,omitempty"`
	Response   *Response     `json:"response,omitempty"`
	ToolCall   *ToolCall     `json:"tool_call,omitempty"`
	ToolResult *ToolResult   `json:"tool_result,omitempty"`
	Text       string        `json:"text,omitempty"`
	Err        error         `json:"-"`
	RetryAfter time.Duration `json:"retry_after,omitempty"`
	Elapsed    time.Duration `json:"elapsed,omitempty"`

	// RequestStartedMs is the Unix millisecond timestamp of the moment the
	// provider request that produced this event was sent. ProviderAcceptedMs
	// is the moment the provider accepted it (0 before that happens).
	// Durations shown to users are derived by subtracting these recorded
	// timestamps in the display, never time.Since at render time (REQ-033).
	RequestStartedMs   int64 `json:"request_started_ms,omitempty"`
	ProviderAcceptedMs int64 `json:"provider_accepted_ms,omitempty"`
	AtMs               int64 `json:"at_ms,omitempty"`
}

// ProviderLatency reports how long the provider took to accept the request
// that produced this event, derived from the recorded timestamps.
func (e TraceEvent) ProviderLatency() time.Duration {
	if e.RequestStartedMs <= 0 || e.ProviderAcceptedMs <= 0 || e.ProviderAcceptedMs < e.RequestStartedMs {
		return 0
	}
	return time.Duration(e.ProviderAcceptedMs-e.RequestStartedMs) * time.Millisecond
}

// TotalElapsed reports the time from sending the producing request until this
// event happened, derived from the recorded timestamps (REQ-033).
func (e TraceEvent) TotalElapsed() time.Duration {
	if e.RequestStartedMs <= 0 || e.AtMs <= 0 || e.AtMs < e.RequestStartedMs {
		return 0
	}
	return time.Duration(e.AtMs-e.RequestStartedMs) * time.Millisecond
}

type TraceFunc func(context.Context, TraceEvent)

func TraceMessage(event TraceEvent) string {
	if event.Message != "" {
		return event.Message
	}
	switch event.Stage {
	case TraceRequest:
		return "sending request to provider"
	case TraceProviderReady:
		return "provider accepted request; processing"
	case TraceResponseText:
		return "provider thinking"
	case TraceResponseContent:
		return "provider sending content"
	case TraceToolCall:
		return "tool call received"
	case TraceToolRunning:
		return "tool execution started"
	case TraceToolResult:
		if event.ToolResult != nil && event.ToolResult.IsError {
			return "tool execution failed"
		}
		return "tool execution succeeded"
	case TraceResponse:
		return "provider response complete"
	case TraceRetryWait:
		return "waiting before retry"
	case TraceError:
		return "provider request failed"
	default:
		return ""
	}
}

func ResponseText(resp Response) string {
	var b strings.Builder
	for _, part := range resp.Content {
		if part.Type == ContentText {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}
