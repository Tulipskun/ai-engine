package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"ai-engine/provider"
)

type capturedRequest struct {
	Header http.Header
	Body   []byte
}

func fakeZen(t *testing.T) (*httptest.Server, *[]capturedRequest) {
	t.Helper()
	var seen []capturedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, capturedRequest{Header: r.Header.Clone(), Body: body})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"gen-1","model":"zen-model","choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","content":"Hello "}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"id":"gen-1","model":"zen-model","choices":[{"index":0,"finish_reason":"stop","delta":{"role":"assistant","content":"there"}}],"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	return server, &seen
}

func providerFor(server *httptest.Server) provider.Provider {
	return provider.Provider{Name: "opencode", Endpoint: server.URL, Keys: []string{"k"}}
}

func toolNames(body []byte) []string {
	var parsed struct {
		Stream bool `json:"stream"`
		Tools  []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(body, &parsed)
	names := make([]string, 0, len(parsed.Tools))
	for _, entry := range parsed.Tools {
		names = append(names, entry.Function.Name)
	}
	return names
}

func hasStream(body []byte) bool {
	var parsed struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &parsed)
	return parsed.Stream
}

func TestFreeTierContractOnComplete(t *testing.T) {
	server, seen := fakeZen(t)
	adapter := New()

	resp, err := adapter.Complete(context.Background(), &provider.Request{
		Model:    "zen-model",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
		Tools:    []provider.ToolDef{{Name: "current_time", Description: "now"}},
	}, providerFor(server), "key")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("requests = %d, want 1", len(*seen))
	}
	got := (*seen)[0]

	if ua := got.Header.Get("User-Agent"); ua != "opencode/1.18.31" {
		t.Errorf("User-Agent = %q", ua)
	}
	session := got.Header.Get("X-Opencode-Session")
	if !regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`).MatchString(session) {
		t.Errorf("session = %q", session)
	}
	if request := got.Header.Get("X-Opencode-Request"); !strings.HasPrefix(request, "msg_") {
		t.Errorf("request = %q", request)
	}
	if client := got.Header.Get("X-Opencode-Client"); client != "cli" {
		t.Errorf("client = %q", client)
	}
	if project := got.Header.Get("X-Opencode-Project"); project != "global" {
		t.Errorf("project = %q", project)
	}
	if !hasStream(got.Body) {
		t.Errorf("stream = false in body: %s", got.Body)
	}

	names := toolNames(got.Body)
	for _, want := range []string{"current_time", "shell", "read"} {
		if !contains(names, want) {
			t.Errorf("tools = %v, missing %q", names, want)
		}
	}

	if resp.Content != "Hello there" {
		t.Errorf("content = %q", resp.Content)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("finish = %q", resp.FinishReason)
	}
	if resp.Usage.TotalTokens != 33 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestStreamKeepsGivenContractTools(t *testing.T) {
	server, seen := fakeZen(t)
	adapter := New()

	var streamed strings.Builder
	_, err := adapter.Stream(context.Background(), &provider.Request{
		Model:    "zen-model",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
		Tools:    []provider.ToolDef{{Name: "bash"}, {Name: "read"}},
	}, providerFor(server), "key", func(delta string) error {
		streamed.WriteString(delta)
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if streamed.String() != "Hello there" {
		t.Errorf("emitted = %q", streamed.String())
	}
	names := toolNames((*seen)[0].Body)
	if strings.Join(names, ",") != "bash,read" {
		t.Errorf("tools = %v, want exactly bash,read", names)
	}
}

func TestSessionStableAndRequestRotates(t *testing.T) {
	server, seen := fakeZen(t)
	adapter := New()
	prov := providerFor(server)

	for i := 0; i < 2; i++ {
		if _, err := adapter.Complete(context.Background(), &provider.Request{
			Model:    "zen-model",
			Messages: []provider.Message{{Role: "user", Content: "hi"}},
		}, prov, "key"); err != nil {
			t.Fatalf("complete %d: %v", i, err)
		}
	}
	first, second := (*seen)[0].Header, (*seen)[1].Header
	if first.Get("X-Opencode-Session") != second.Get("X-Opencode-Session") {
		t.Errorf("session rotated: %q vs %q", first.Get("X-Opencode-Session"), second.Get("X-Opencode-Session"))
	}
	if first.Get("X-Opencode-Request") == second.Get("X-Opencode-Request") {
		t.Errorf("request id reused: %q", first.Get("X-Opencode-Request"))
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
