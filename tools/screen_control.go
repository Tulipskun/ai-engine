package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type screenControlRequest struct {
	Goal         string `json:"goal"`
	Role         string `json:"role"`
	Instructions string `json:"instructions"`
}

func screenControlTool() handler {
	client := &http.Client{Timeout: 30 * time.Second}

	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		var in struct {
			Goal string `json:"goal"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return "", fmt.Errorf("screen_control: invalid arguments: %w", err)
		}
		in.Goal = strings.TrimSpace(in.Goal)
		if in.Goal == "" {
			return "", fmt.Errorf("screen_control: goal is required")
		}

		endpoint := strings.TrimSpace(os.Getenv("JEV_LOCAL_URL"))
		if endpoint == "" {
			return "", fmt.Errorf("screen_control: JEV_LOCAL_URL is not configured")
		}

		payload := screenControlRequest{
			Goal:         in.Goal,
			Role:         "screen_controller",
			Instructions: "Control the screen directly. Determine the target position, move or drag the pointer to it, then click or perform the required pointer action. Repeat as needed until the goal is complete. Do not act as a gatekeeper for the main agent.",
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return "", fmt.Errorf("screen_control: encode request: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return "", fmt.Errorf("screen_control: create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("screen_control: local JEV unavailable: %w", err)
		}
		defer resp.Body.Close()

		responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			return "", fmt.Errorf("screen_control: read JEV response: %w", err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", fmt.Errorf("screen_control: local JEV HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(responseBody)))
		}
		if len(bytes.TrimSpace(responseBody)) == 0 {
			return "", fmt.Errorf("screen_control: local JEV returned an empty response")
		}

		return string(responseBody), nil
	}
}
