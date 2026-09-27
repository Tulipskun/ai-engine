package tools

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "strings"
    "time"
)

type screenControlRequest struct {
    SessionID string `json:"session_id"`
    Goal      string `json:"goal"`
}

type screenControlResponse struct {
    OK     bool   `json:"ok"`
    Action string `json:"action,omitempty"`
    Detail string `json:"detail,omitempty"`
    Error  string `json:"error,omitempty"`
}

func screenControlTool() handler {
    client := &http.Client{Timeout: 12 * time.Second}
    return func(ctx context.Context, raw json.RawMessage) (string, error) {
        var req screenControlRequest
        if err := json.Unmarshal(raw, &req); err != nil {
            return "", fmt.Errorf("screen_control: invalid arguments: %w", err)
        }
        req.Goal = strings.TrimSpace(req.Goal)
        if req.Goal == "" {
            return "", fmt.Errorf("screen_control: goal is required")
        }
        payload, err := json.Marshal(req)
        if err != nil { return "", err }
        httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:18790/screen/control", bytes.NewReader(payload))
        if err != nil { return "", err }
        httpReq.Header.Set("Content-Type", "application/json")
        resp, err := client.Do(httpReq)
        if err != nil {
            return "", fmt.Errorf("screen_control unavailable: enable AIxodia Accessibility Service")
        }
        defer resp.Body.Close()
        body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
        if resp.StatusCode < 200 || resp.StatusCode >= 300 {
            return "", fmt.Errorf("screen_control HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
        }
        var out screenControlResponse
        if err := json.Unmarshal(body, &out); err != nil {
            return "", fmt.Errorf("screen_control: invalid response: %w", err)
        }
        if !out.OK {
            if out.Error == "" { out.Error = "screen action failed" }
            return "", fmt.Errorf("screen_control: %s", out.Error)
        }
        result, _ := json.Marshal(out)
        return string(result), nil
    }
}
