package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"ai-engine/db"
	"ai-engine/io/gateway"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfToken := os.Getenv("CF_TOKEN")
	if cfToken == "" {
		log.Fatal("CF_TOKEN is not set")
	}

	valid, err := db.VerifyToken(ctx, cfToken)
	if err != nil {
		log.Fatalf("verify CF token: %v", err)
	}
	if !valid {
		log.Fatal("CF_TOKEN is invalid or inactive")
	}

	publicURL, stopTunnel, err := gateway.RunQuickTunnel(ctx, 8787, "cloudflared")
	if err != nil {
		log.Fatal(err)
	}
	defer stopTunnel()

	log.Printf("tunnel: %s", publicURL)
	<-ctx.Done()
}
