package internal

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultUserAgent is sent on every provider request that does not set its
// own User-Agent (adapter or per-provider headers always win). Go's default
// ("Go-http-client/1.1") is challenged by some gateways' bot rules
// (Cloudflare 403 HTML), which used to surface as multi-KB errors.
const DefaultUserAgent = "ai"

type HTTPError struct {
	StatusCode      int
	Status          string
	Body            string
	RetryAfterDelay time.Duration
}

func (e *HTTPError) Error() string             { return fmt.Sprintf("http %s: %s", e.Status, e.Body) }
func (e *HTTPError) HTTPStatusCode() int       { return e.StatusCode }
func (e *HTTPError) RetryAfter() time.Duration { return e.RetryAfterDelay }

func StatusCode(err error) (int, bool) {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.StatusCode, true
	}
	return 0, false
}

func retryAfter(resp *http.Response) time.Duration {
	value := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

func DoJSON(ctx context.Context, client *http.Client, method, url string, headers map[string]string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", DefaultUserAgent)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return &HTTPError{StatusCode: resp.StatusCode, Status: resp.Status, Body: strings.TrimSpace(string(b)), RetryAfterDelay: retryAfter(resp)}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func SSE(ctx context.Context, client *http.Client, method, url string, headers map[string]string, body any, onData func([]byte) error) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", DefaultUserAgent)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return &HTTPError{StatusCode: resp.StatusCode, Status: resp.Status, Body: strings.TrimSpace(string(b)), RetryAfterDelay: retryAfter(resp)}
	}
	s := bufio.NewScanner(resp.Body)
	s.Buffer(make([]byte, 4096), 4<<20)
	for s.Scan() {
		line := s.Bytes()
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		if err := onData(payload); err != nil {
			return err
		}
	}
	return s.Err()
}
