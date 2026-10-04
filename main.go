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

	accountID, err := db.FindAccountID(ctx, cfToken)
	if err != nil {
		log.Fatalf("find Cloudflare account: %v", err)
	}

	databaseID, err := db.FindDatabaseID(ctx, cfToken, accountID, "aixodia")
	if err != nil {
		log.Fatalf("find D1 database: %v", err)
	}

	publicURL, stopTunnel, err := gateway.RunQuickTunnel(ctx, 8787, "cloudflared")
	if err != nil {
		log.Fatal(err)
	}
	defer stopTunnel()

	exists, err := db.TableExists(ctx, cfToken, accountID, databaseID, "tunnel")
	if err != nil {
		log.Fatalf("check tunnel table: %v", err)
	}
	if !exists {
		if err := db.CreateTable(ctx, cfToken, accountID, databaseID, "tunnel", "url TEXT"); err != nil {
			log.Fatalf("create tunnel table: %v", err)
		}
	}

	if err := db.Insert(ctx, cfToken, accountID, databaseID, "tunnel", []string{"url"}, []string{publicURL}); err != nil {
		log.Fatalf("insert tunnel URL: %v", err)
	}

	log.Printf("tunnel: %s", publicURL)
	<-ctx.Done()
}
