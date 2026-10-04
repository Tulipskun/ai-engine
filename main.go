package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"ai-engine/config"
	"ai-engine/db"
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

	config.LoadConfig()

	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"ok":true}`))
		})
		_ = http.ListenAndServe("127.0.0.1:8787", mux)
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

	<-ctx.Done()
}

var tunnelURL = regexp.MustCompile(`https://[A-Za-z0-9.-]+\.trycloudflare\.com`)

// startTunnel publishes localhost:8787 through a Cloudflare quick tunnel
// and returns the random public URL. It needs the cloudflared binary.
func startTunnel(ctx context.Context) (string, error) {
	path, err := exec.LookPath("cloudflared")
	if err != nil {
		return "", errors.New("cloudflared not found in PATH")
	}
	child, cancel := context.WithCancel(context.Background())
	defer cancel()
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
