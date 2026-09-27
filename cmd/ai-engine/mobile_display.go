package main

import (
	"context"

	"github.com/Tulipskun/ai-engine/sdk"
)

// mobileDisplayAdapter keeps the harness-facing display contract narrow: the
// transport publishes frames, and the same call mirrors the finished turn into
// D1 so the phone can rebuild the thread after the app was closed
// (REQ-046(5)). It implements sdk.RoutedDisplay, so it only receives output
// whose source is "mobile".
type mobileDisplayAdapter struct{ mobile *mobileRuntime }

func (a mobileDisplayAdapter) Source() string { return "mobile" }

func (a mobileDisplayAdapter) Display(ctx context.Context, output sdk.Output) error {
	a.mobile.PublishOutput(ctx, output)
	return nil
}
