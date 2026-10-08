package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"ai-engine/config"
	"ai-engine/db"
	"ai-engine/io/gateway"
	"ai-engine/io/mobile"
	"ai-engine/provider/registry"
	"ai-engine/session"
	"ai-engine/tools"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	token := strings.TrimSpace(os.Getenv("CF_TOKEN"))
	if token == "" {
		log.Fatal("CF_TOKEN is not set")
	}

	if !db.Verify(token) {
		log.Fatal("CF_TOKEN is invalid")
	}

	startConfigReloader(ctx)

	gw := gateway.New(session.NewD1(), registry.New(), tools.Builtin()).Handler()
	model := strings.TrimSpace(os.Getenv("AIXODIA_MODEL"))
	if model == "" {
		model = "Opencode/big-pickle"
	}
	mux := http.NewServeMux()
	mux.Handle("/ws", mobile.Handler(token, chatViaGateway(gw, model)))
	mux.Handle("/", gw)
	server := &http.Server{
		Addr:    "127.0.0.1:8787",
		Handler: mux,
	}
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http: %v", err)
			stop()
		}
	}()

	url, err := startTunnel(ctx)
	if err != nil {
		log.Fatalf("tunnel: %v", err)
	}
	log.Printf("tunnel url: %s", url)

	if err := db.SaveTunnel(url); err != nil {
		log.Fatalf("save tunnel: %v", err)
	}
	log.Print("tunnel url saved")
	go startHeartbeat(ctx, url)

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
}

func startConfigReloader(ctx context.Context) {
	if err := config.LoadConfig(); err != nil {
		log.Printf("config: %v", err)
		return
	}
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := config.LoadConfig(); err != nil {
					log.Printf("config: %v", err)
				}
			}
		}
	}()
}

var tunnelURL = regexp.MustCompile(`https://[A-Za-z0-9.-]+\.trycloudflare\.com`)

// startTunnel publishes localhost:8787 through a Cloudflare quick tunnel
// and returns the random public URL. It needs the cloudflared binary.
func startTunnel(ctx context.Context) (string, error) {
	path, err := exec.LookPath("cloudflared")
	if err != nil {
		return "", errors.New("cloudflared not found in PATH")
	}
	child := context.Background()
	cmd := exec.CommandContext(child, path, "tunnel", "--no-autoupdate",
		"--metrics", "127.0.0.1:0", "--url", "http://127.0.0.1:8787")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	found := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 64*1024), 64*1024)
		for scanner.Scan() {
			if match := tunnelURL.FindString(scanner.Text()); match != "" {
				select {
				case found <- match:
				default:
				}
			}
		}
	}()
	select {
	case url := <-found:
		go func() {
			<-ctx.Done()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}()
		return url, nil
	case <-time.After(45 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return "", fmt.Errorf("timed out waiting for the trycloudflare URL")
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return "", ctx.Err()
	}
}

// chatViaGateway answers one message by calling the HTTP chat route in-process,
// so the WebSocket path shares routing, history, and persistence with the API.
func chatViaGateway(h http.Handler, model string) mobile.ChatFunc {
	return func(sessionID, text string) (string, int, int, error) {
		body, err := json.Marshal(map[string]any{
			"session_id": sessionID,
			"model":      model,
			"messages":   []map[string]string{{"role": "user", "content": text}},
		})
		if err != nil {
			return "", 0, 0, err
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			return "", 0, 0, fmt.Errorf("chat %d: %s", rec.Code, strings.TrimSpace(rec.Body.String()))
		}
		var out struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			return "", 0, 0, err
		}
		if len(out.Choices) == 0 {
			return "", 0, 0, fmt.Errorf("chat: empty choices")
		}
		return out.Choices[0].Message.Content, out.Usage.PromptTokens, out.Usage.CompletionTokens, nil
	}
}

// startHeartbeat keeps the single `nodes` row ('ai') current so the app can
// find this daemon: tunnel URL and a heartbeat every 30 seconds.
func startHeartbeat(ctx context.Context, url string) {
	beat := func() {
		_, err := db.Query(
			"INSERT INTO nodes (id, tunnel_url, version, heartbeat) VALUES ('ai', ?, ?, ?) "+
				"ON CONFLICT(id) DO UPDATE SET tunnel_url = excluded.tunnel_url, version = excluded.version, heartbeat = excluded.heartbeat",
			url, "ai-engine/1.0", time.Now().Unix())
		if err != nil {
			log.Printf("heartbeat: %v", err)
		}
	}
	beat()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			beat()
		}
	}
}
