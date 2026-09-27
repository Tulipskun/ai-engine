package mobile

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

var trycloudflareURL = regexp.MustCompile(`https://[A-Za-z0-9.-]+\.trycloudflare\.com`)

func portOfListen(listen string) int {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return 18789
	}
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 {
		return 18789
	}
	return n
}

// RunQuickTunnel publishes a localhost listener through a Cloudflare quick
// tunnel — no account, no port forwarding. The public hostname is random, which
// is the first authentication step; the D1 token in the handshake header is the
// second (REQ-046(2)).
//
// --metrics 127.0.0.1:0 is deliberate: some hosts cannot resolve the hostname
// "localhost", and cloudflared then aborts before the tunnel is registered
// (Cloudflare error 1033).
func RunQuickTunnel(ctx context.Context, port int, binary string) (string, func(), error) {
	if binary == "" {
		binary = "cloudflared"
	}
	path, err := exec.LookPath(binary)
	if err != nil {
		return "", nil, fmt.Errorf("mobile: cloudflared not found in PATH: %w", err)
	}
	child, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(child, path, "tunnel", "--no-autoupdate",
		"--metrics", "127.0.0.1:0", "--url", fmt.Sprintf("http://127.0.0.1:%d", port))
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return "", nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return "", nil, err
	}
	found := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 64*1024), 64*1024)
		for scanner.Scan() {
			if match := trycloudflareURL.FindString(scanner.Text()); match != "" {
				select {
				case found <- match:
				default:
				}
			}
		}
	}()
	var public string
	select {
	case public = <-found:
	case <-time.After(45 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		cancel()
		return "", nil, errors.New("mobile: timed out waiting for the trycloudflare URL")
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		cancel()
		return "", nil, ctx.Err()
	}
	return public, func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		cancel()
	}, nil
}

// WSURL converts a public tunnel URL into the WebSocket endpoint the phone
// dials.
func WSURL(tunnelURL string) (string, error) {
	parsed, err := url.Parse(tunnelURL)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("mobile: expected an https tunnel URL, got %q", tunnelURL)
	}
	return "wss://" + parsed.Host + "/ws", nil
}
