package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Tulipskun/ai-engine/db"
	"github.com/Tulipskun/ai-engine/io/gateway"
	"github.com/Tulipskun/ai-engine/io/state"
)

type cfTokenVerifier struct {
	client *state.Client
}

func (v cfTokenVerifier) VerifyToken(ctx context.Context, token string) error {
	return v.client.VerifyToken(ctx, token)
}

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

	tokens := state.NewMemoryToken()
	tokens.Adopt(cfToken)
	d1 := state.NewClient(state.DefaultAPIBase, tokens.Get)
	d1.SetDatabaseName("aixodia")

	transport := gateway.New(gateway.Config{
		Listen:      "127.0.0.1:8787",
		Tunnel:      true,
		Cloudflared: "cloudflared",
		Tokens:      tokens,
		Verifier:    cfTokenVerifier{client: d1},
		Announce: func(ctx context.Context, publicURL string) error {
			return db.SetTunnelURL(ctx, cfToken, accountID, databaseID, publicURL)
		},
	})

	stopHTTP, err := transport.StartHTTP(ctx, "127.0.0.1:8787")
	if err != nil {
		log.Fatal(err)
	}
	defer stopHTTP()

	log.Printf("ai-engine: HTTP/WebSocket listening on 127.0.0.1:8787")
	<-ctx.Done()
}
