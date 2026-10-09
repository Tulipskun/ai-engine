package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"ai-engine/provider"
)

type Tool struct {
	Def provider.ToolDef
	Run func(ctx context.Context, args string) (string, error)
}

func Builtin() map[string]Tool {
	return map[string]Tool{
		"current_time": {
			Def: provider.ToolDef{
				Name:        "current_time",
				Description: "Returns the current date and time, optionally in a specific IANA time zone.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"timezone": map[string]any{
							"type":        "string",
							"description": "IANA time zone name, for example Asia/Bangkok. Defaults to UTC.",
						},
					},
				},
			},
			Run: currentTime,
		},
		"fetch_url": {
			Def: provider.ToolDef{
				Name:        "fetch_url",
				Description: "Fetches an http or https URL with GET and returns the response body as text.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"url": map[string]any{
							"type":        "string",
							"description": "Absolute http or https URL to fetch.",
						},
					},
					"required": []any{"url"},
				},
			},
			Run: fetchURL,
		},
	}
}

func currentTime(ctx context.Context, args string) (string, error) {
	input := struct {
		Timezone string `json:"timezone"`
	}{}
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &input); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}
	location := time.UTC
	if input.Timezone != "" {
		parsed, err := time.LoadLocation(input.Timezone)
		if err != nil {
			return "", fmt.Errorf("unknown time zone %q", input.Timezone)
		}
		location = parsed
	}
	return time.Now().In(location).Format(time.RFC3339), nil
}

func fetchURL(ctx context.Context, args string) (string, error) {
	input := struct {
		URL string `json:"url"`
	}{}
	if err := json.Unmarshal([]byte(args), &input); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	parsed, err := url.Parse(strings.TrimSpace(input.URL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("url must be absolute http or https")
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: safePublicDialContext,
		},
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			if err := validatePublicURL(request.URL); err != nil {
				return err
			}
			return nil
		},
	}
	if err := validatePublicURL(parsed); err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", provider.UserAgent)

	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, 128*1024))
	if err != nil {
		return "", err
	}
	text := string(body)
	if len(text) > 32*1024 {
		text = text[:32*1024] + "\n[truncated]"
	}
	return fmt.Sprintf("HTTP %d\n%s", response.StatusCode, text), nil
}


// validatePublicURL rejects literal private and local addresses before dialing.
// safePublicDialContext repeats the check after DNS resolution to prevent
// DNS rebinding from reaching local services or cloud metadata endpoints.
func validatePublicURL(target *url.URL) error {
	if target == nil || (target.Scheme != "http" && target.Scheme != "https") || target.Hostname() == "" {
		return fmt.Errorf("url must be absolute http or https")
	}
	host := strings.TrimSuffix(strings.ToLower(target.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return fmt.Errorf("local and private network URLs are not allowed")
	}
	if ip := net.ParseIP(host); ip != nil && !isPublicIP(ip) {
		return fmt.Errorf("local and private network URLs are not allowed")
	}
	return nil
}

func safePublicDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if _, err := strconv.Atoi(port); err != nil {
		return nil, fmt.Errorf("invalid port")
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("hostname resolved to no addresses")
	}
	for _, entry := range ips {
		if !isPublicIP(entry.IP) {
			return nil, fmt.Errorf("local and private network URLs are not allowed")
		}
	}
	dialer := net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	var lastErr error
	for _, entry := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(entry.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func isPublicIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	// Carrier-grade NAT and IPv4 special-use ranges are not public destinations.
	if v4 := ip.To4(); v4 != nil {
		blocked := []string{"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4"}
		for _, cidr := range blocked {
			_, network, _ := net.ParseCIDR(cidr)
			if network.Contains(v4) {
				return false
			}
		}
	}
	return true
}
