package sdk

import (
	"context"
	"errors"
	"testing"
	"time"
)

type failingDisplay struct {
	called chan struct{}
}

func (d failingDisplay) Display(context.Context, Output) error {
	close(d.called)
	return errors.New("display failed")
}

func TestDispatchDisplayDoesNotBlockCallerOnDisplayFailure(t *testing.T) {
	called := make(chan struct{})
	d := failingDisplay{called: called}
	output := Output{SessionID: "s1"}

	start := time.Now()
	DispatchDisplay(context.Background(), d, output, 50*time.Millisecond)
	if time.Since(start) > 20*time.Millisecond {
		t.Fatal("display dispatch blocked the caller")
	}

	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("display was not dispatched")
	}
}

func TestDispatchDisplayRecoversFromPanic(t *testing.T) {
	d := DisplayFunc(func(context.Context, Output) error { panic("boom") })
	DispatchDisplay(context.Background(), d, Output{SessionID: "s1"}, time.Second)
}
