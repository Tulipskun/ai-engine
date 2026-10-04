package io

import (
	"ai-engine/provider"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// ---- from io/io.go ----
// Input is the canonical event entering the Harness from any transport.
type Input struct {
	Source    string
	SessionID string
	Turn      provider.Turn
	Metadata  map[string]string
}

// Output is the canonical result leaving the Harness for one or more displays.
type Output struct {
	Source    string
	SessionID string
	Content   []provider.ContentPart
	Response  provider.Response
	Trace     *TraceEvent
	Metadata  map[string]string
}

type InputSource interface {
	Receive(context.Context) (<-chan Input, error)
}
type Display interface {
	Display(context.Context, Output) error
}
type RoutedDisplay interface {
	Display
	Source() string
}
type DisplayFunc func(context.Context, Output) error

func (f DisplayFunc) Display(ctx context.Context, output Output) error { return f(ctx, output) }

const defaultDisplayTimeout = 10 * time.Second

func DispatchDisplay(parent context.Context, display Display, output Output, timeout time.Duration) {
	if display == nil {
		return
	}
	if routed, ok := display.(RoutedDisplay); ok {
		source := routed.Source()
		if source != "" && source != output.Source {
			return
		}
	}
	if timeout <= 0 {
		timeout = defaultDisplayTimeout
	}
	run := func() {
		defer func() { _ = recover() }()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
		defer cancel()
		if err := display.Display(ctx, output); err != nil {
			log.Printf("sdk: display failed source=%s session=%s: %v", output.Source, output.SessionID, err)
		}
	}
	if output.Trace != nil {
		run()
		return
	}
	go run()
}

// LifecycleInputKey carries the input that started a turn, so delegation
// code running inside the turn can recover which session and route it
// belongs to. The harness writes it, the sub-agent manager reads it, so
// it lives here where both sides already import.
type LifecycleInputKey struct{}

// CloneInputRoute copies the routing identity of an input (source,
// session, metadata) without its turn payload, for continuation turns
// and worker jobs that need the route but carry their own content.
func CloneInputRoute(input Input) Input {
	return Input{Source: input.Source, SessionID: input.SessionID, Metadata: CloneMetadata(input.Metadata)}
}

// CloneMetadata copies a metadata map so a turn cannot reach back through
// it into the settings it was built from.
func CloneMetadata(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// ---- from io/trace.go ----
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
	Stage      TraceStage           `json:"stage"`
	Message    string               `json:"message,omitempty"`
	Response   *provider.Response   `json:"response,omitempty"`
	ToolCall   *provider.ToolCall   `json:"tool_call,omitempty"`
	ToolResult *provider.ToolResult `json:"tool_result,omitempty"`
	Text       string               `json:"text,omitempty"`
	Err        error                `json:"-"`
	RetryAfter time.Duration        `json:"retry_after,omitempty"`
	Elapsed    time.Duration        `json:"elapsed,omitempty"`

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

// ---- from io/input_merge.go ----
// MergeInputSources combines transport input streams into one Harness input stream.
func MergeInputSources(ctx context.Context, sources ...InputSource) (<-chan Input, error) {
	streams := make([]<-chan Input, 0, len(sources))
	for _, source := range sources {
		if source == nil {
			continue
		}
		stream, err := source.Receive(ctx)
		if err != nil {
			return nil, err
		}
		streams = append(streams, stream)
	}
	out := make(chan Input)
	var wg sync.WaitGroup
	wg.Add(len(streams))
	for _, stream := range streams {
		go func(stream <-chan Input) {
			defer wg.Done()
			for {
				select {
				case input, ok := <-stream:
					if !ok {
						return
					}
					select {
					case out <- input:
					case <-ctx.Done():
						return
					}
				case <-ctx.Done():
					return
				}
			}
		}(stream)
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out, nil
}

// ChannelInputSource adapts an existing canonical input channel to InputSource.
type ChannelInputSource struct {
	Inputs <-chan Input
}

func (s ChannelInputSource) Receive(context.Context) (<-chan Input, error) {
	return s.Inputs, nil
}

// ---- from io/entry_config.go ----
// MobileConfig points the daemon at the local listener a Cloudflare quick tunnel
// publishes (REQ-046, REQ-047). It carries no credential: the Cloudflare token
// arrives in the phone's Authorization header, is held in memory only, and is
// used straight against Cloudflare's API — the account and database are
// discovered from that token, so no ids are configured here (CON-012).
type MobileConfig struct {
	Enabled       bool   `json:"enabled"`
	CloudflareAPI string `json:"cloudflare_api,omitempty"`
	D1Database    string `json:"d1_database,omitempty"`
	Listen        string `json:"listen"`
	PublicListen  string `json:"public_listen"`
	Tunnel        bool   `json:"tunnel"`
	Cloudflared   string `json:"cloudflared"`
	SyncConfig    bool   `json:"sync_config"`
	SyncSessions  bool   `json:"sync_sessions"`
}

// Config is the whole entry config: one gateway, mobile over tunnel. The
// Discord and CLI blocks were removed with their transports (CHANGE-059).
type Config struct {
	Mobile MobileConfig `json:"mobile"`
}

const DefaultConfigPath = "config/entry.json"

func LoadConfig(path string) (Config, error) {
	if path == "" {
		path = DefaultConfigPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("transport: read entry config %q: %w", path, err)
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return Config{}, fmt.Errorf("transport: decode entry config %q: %w", path, err)
	}
	return config, nil
}
