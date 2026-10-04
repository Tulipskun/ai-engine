package io

import (
	"ai-engine/provider"
	"context"
	"log"
	"time"
)

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
