package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Tulipskun/ai-engine/sdk"
	"github.com/Tulipskun/ai-engine/sdk/providers/opencode"
)

const (
	DefaultEndpoint = "https://opencode.ai/zen/v1/systemone"
	DefaultModel    = "jev-1.13-free"
)

type Config struct {
	Endpoint  string
	Model     string
	APIKey    string
	Client    *http.Client
	Threshold float64
	// Headers ride on every decision request. Without the OpenCode client's
	// fingerprint the edge answers 403 code 1010 and the guard would silently
	// allow everything, so the caller passes opencode.FreeTierHeaders.
	Headers map[string]string
}

type Guard struct{ cfg Config }

func New(cfg Config) *Guard {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		cfg.Endpoint = DefaultEndpoint
	}
	if strings.TrimSpace(cfg.Model) == "" {
		cfg.Model = DefaultModel
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 4 * time.Second}
	}
	if cfg.Threshold <= 0 || cfg.Threshold > 1 {
		cfg.Threshold = 0.80
	}
	return &Guard{cfg: cfg}
}

func NewFromEnv() *Guard {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("AI_JEV_ENABLED")), "0") ||
		strings.EqualFold(strings.TrimSpace(os.Getenv("AI_JEV_ENABLED")), "false") {
		return nil
	}
	return New(Config{
		Endpoint: strings.TrimSpace(os.Getenv("AI_JEV_ENDPOINT")),
		Model:    strings.TrimSpace(os.Getenv("AI_JEV_MODEL")),
		APIKey:   strings.TrimSpace(os.Getenv("AI_JEV_API_KEY")),
		// One stable id for the process: the edge only checks that the header
		// is present, and a fresh id per call would look unlike a real client.
		Headers: opencode.FreeTierHeaders(opencode.SessionIDFor("jev-guard")),
	})
}

func (g *Guard) Allow(ctx context.Context, user sdk.Turn, call sdk.ToolCall) (bool, error) {
	if g == nil || strings.TrimSpace(call.Name) != "bash" {
		return true, nil
	}
	body := map[string]any{
		"model": g.cfg.Model,
		"state": "User request:\n" + turnText(user) + "\n\nTool call:\nname=" + call.Name + "\narguments=" + call.Arguments,
		"questions": map[string]any{
			"safe": map[string]any{
				"type":         "noul",
				"instructions": "Is this exact tool call appropriate to execute for the user's request? Answer no for destructive actions, credential or secret exfiltration, unrelated system changes, network abuse, or actions outside the requested scope. Answer yes for ordinary, scoped development work.",
			},
		},
	}
	data, err := json.Marshal(body)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.cfg.Endpoint, bytes.NewReader(data))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range g.cfg.Headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	if g.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+g.cfg.APIKey)
	}
	resp, err := g.cfg.Client.Do(req)
	if err != nil {
		return true, nil
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return false, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return true, nil
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return true, nil
	}
	answers, _ := parsed["answers"].(map[string]any)
	safe, _ := answers["safe"].(map[string]any)
	noul, ok := safe["noul"].(float64)
	if !ok {
		return true, nil
	}
	return noul >= g.cfg.Threshold, nil
}

func turnText(t sdk.Turn) string {
	var b strings.Builder
	for _, part := range t.Content {
		b.WriteString(part.Text)
	}
	if b.Len() == 0 {
		return "(no user text)"
	}
	return b.String()
}

var _ sdk.ToolGuard = (*Guard)(nil)
