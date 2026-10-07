package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", provider.UserAgent)

	response, err := provider.HTTP.Do(request)
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
