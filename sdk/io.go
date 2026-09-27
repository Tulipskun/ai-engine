package sdk

import (
	"context"
	"log"
	"time"
)

// Input is the canonical event entering the Harness from any transport.
type Input struct {
	Source    string
	SessionID string
	Turn      Turn
	Metadata  map[string]string
}

// Output is the canonical result leaving the Harness for one or more displays.
type Output struct {
	Source    string
	SessionID string
	Content   []ContentPart
	Response  Response
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
