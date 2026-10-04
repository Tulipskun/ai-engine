package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Tulipskun/ai-engine/io/gateway"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	publicURL, stopTunnel, err := gateway.RunQuickTunnel(ctx, 8787, "cloudflared")
	if err != nil {
		log.Fatal(err)
	}
	defer stopTunnel()

	log.Printf("tunnel: %s", publicURL)
	<-ctx.Done()
}
