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
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

var (
	cfToken    string
	accountID  string
	databaseID string
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfToken = strings.TrimSpace(os.Getenv("CF_TOKEN"))
	if cfToken == "" {
		log.Fatal("CF_TOKEN is not set")
	}

	if !verifyToken(cfToken) {
		log.Fatal("CF_TOKEN is invalid")
	}

	loadConfig()

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

	if err := saveTunnel(url); err != nil {
		log.Fatalf("save tunnel: %v", err)
	}
	log.Print("tunnel url saved")

	<-ctx.Done()
}

// loadConfig prepares ~/.local/share/ai. Files the phone has not pushed
// yet simply do not exist on first run.
func loadConfig() {
	home, _ := os.UserHomeDir()
	root := filepath.Join(home, ".local", "share", "ai")
	_ = os.MkdirAll(filepath.Join(root, "config"), 0o700)
	_ = os.MkdirAll(filepath.Join(root, "data", "sessions"), 0o700)
}

// verifyToken checks the token with Cloudflare and discovers the account
// and the aixodia database from it. False means bad token, nothing
// visible, or Cloudflare unreachable — fail closed either way.
func verifyToken(token string) bool {
	var out struct {
		Success bool `json:"success"`
		Result  struct {
			Status string `json:"status"`
		} `json:"result"`
	}
	if err := cfGet("/user/tokens/verify", token, &out); err != nil {
		return false
	}
	if !out.Success || out.Result.Status != "active" {
		return false
	}
	account, database, err := discoverDB(token)
	if err != nil {
		return false
	}
	accountID, databaseID = account, database
	return true
}

// saveTunnel replaces the single row in the tunnel table with url.
func saveTunnel(url string) error {
	if strings.TrimSpace(url) == "" {
		return fmt.Errorf("tunnel url is empty")
	}
	stmts := []struct {
		sql    string
		params []string
	}{
		{"CREATE TABLE IF NOT EXISTS tunnel (url TEXT)", nil},
		{"DELETE FROM tunnel", nil},
		{"INSERT INTO tunnel (url) VALUES (?)", []string{url}},
	}
	for _, s := range stmts {
		if err := d1query(s.sql, s.params); err != nil {
			return err
		}
	}
	return nil
}

var cfHTTP = &http.Client{Timeout: 15 * time.Second}

func cfGet(path, token string, out any) error {
	req, err := http.NewRequest(http.MethodGet, "https://api.cloudflare.com/client/v4"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := cfHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s -> HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func discoverDB(token string) (string, string, error) {
	var accounts struct {
		Success bool `json:"success"`
		Result  []struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := cfGet("/accounts", token, &accounts); err != nil {
		return "", "", err
	}
	if !accounts.Success || len(accounts.Result) == 0 {
		return "", "", fmt.Errorf("token sees no account")
	}
	account := accounts.Result[0].ID

	var databases struct {
		Success bool `json:"success"`
		Result  []struct {
			UUID string `json:"uuid"`
			Name string `json:"name"`
		} `json:"result"`
	}
	if err := cfGet("/accounts/"+account+"/d1/database", token, &databases); err != nil {
		return "", "", err
	}
	if !databases.Success || len(databases.Result) == 0 {
		return "", "", fmt.Errorf("account has no D1 database")
	}
	database := databases.Result[0].UUID
	for _, d := range databases.Result {
		if d.Name == "aixodia" {
			database = d.UUID
		}
	}
	return account, database, nil
}

func d1query(sql string, params []string) error {
	body, err := json.Marshal(map[string]any{"sql": sql, "params": params})
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/d1/database/%s/query",
		accountID, databaseID)
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cfHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		Success bool `json:"success"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !out.Success {
		msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if len(out.Errors) > 0 {
			msg = out.Errors[0].Message
		}
		return fmt.Errorf("query failed: %s", msg)
	}
	return nil
}

var tunnelURL = regexp.MustCompile(`https://[A-Za-z0-9.-]+\.trycloudflare\.com`)

// startTunnel publishes localhost:8787 through a Cloudflare quick tunnel
// and returns the random public URL. It needs the cloudflared binary.
// The tunnel dies with ctx.
func startTunnel(ctx context.Context) (string, error) {
	path, err := exec.LookPath("cloudflared")
	if err != nil {
		return "", errors.New("cloudflared not found in PATH")
	}
	cmd := exec.Command(path, "tunnel", "--no-autoupdate",
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
	}
}
