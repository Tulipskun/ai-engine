package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tulipskun/ai-engine/sdk"
)

func TestBuildTranslatesToolHistory(t *testing.T) {
	r := build(sdk.Request{Model: "test", Messages: []sdk.Turn{
		{Role: sdk.RoleUser, Content: []sdk.ContentPart{{Type: sdk.ContentText, Text: "hi"}}},
		{Role: sdk.RoleToolCall, ToolCall: &sdk.ToolCall{ID: "c1", Name: "bash", Arguments: `{"command":"pwd"}`}},
		{Role: sdk.RoleToolResult, ToolResult: &sdk.ToolResult{ID: "c1", Content: "/tmp"}},
	}})
	input := r["input"].([]any)
	if input[1].(map[string]any)["type"] != "function_call" {
		t.Fatal("tool call was not translated")
	}
	if input[2].(map[string]any)["type"] != "function_call_output" {
		t.Fatal("tool result was not translated")
	}
}

func TestBuildReplaysResponsesReasoning(t *testing.T) {
	r := build(sdk.Request{Model: "deepseek-v4-flash", ThinkingLevel: sdk.ThinkingHigh, Messages: []sdk.Turn{
		{Role: sdk.RoleUser, Content: []sdk.ContentPart{{Type: sdk.ContentText, Text: "use the tool"}}},
		{Role: sdk.RoleModel, Reasoning: &sdk.ReasoningState{ID: "rs_123", Text: "think before using the tool"}},
		{Role: sdk.RoleToolCall, ToolCall: &sdk.ToolCall{ID: "c1", Name: "bash", Arguments: `{"command":"pwd"}`}},
		{Role: sdk.RoleToolResult, ToolResult: &sdk.ToolResult{ID: "c1", Content: "/tmp"}},
	}})
	input := r["input"].([]any)
	reasoning, ok := input[1].(map[string]any)
	if !ok || reasoning["type"] != "reasoning" {
		t.Fatalf("reasoning item missing: %#v", input)
	}
	if reasoning["id"] != "rs_123" {
		t.Fatalf("reasoning id=%v", reasoning["id"])
	}
	content := reasoning["content"].([]any)
	part := content[0].(map[string]any)
	if part["type"] != "reasoning_text" || part["text"] != "think before using the tool" {
		t.Fatalf("reasoning content=%#v", part)
	}
}

func TestParseResponsePreservesReasoning(t *testing.T) {
	r := ResponsesResponse{Model: "deepseek-v4-flash", Output: []struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}{
		{Type: "reasoning", ID: "rs_123", Content: []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{{Type: "reasoning_text", Text: "think"}}},
	}}
	got := parseResponse(r)
	if got.Reasoning == nil || got.Reasoning.ID != "rs_123" || got.Reasoning.Text != "think" {
		t.Fatalf("reasoning=%#v", got.Reasoning)
	}
}

func TestParseChatResponsePreservesCachedTokens(t *testing.T) {
	var r ChatResponse
	body := `{"model":"m","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105,"prompt_tokens_details":{"cached_tokens":80}}}`
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatal(err)
	}
	got := parseChatResponse(r)
	if got.Usage.CacheReadTokens != 80 {
		t.Fatalf("CacheReadTokens=%d, want 80", got.Usage.CacheReadTokens)
	}
	if !got.Cache.Hit || got.Cache.Layer != "provider" {
		t.Fatalf("Cache=%+v, want hit on provider layer", got.Cache)
	}
}

func TestGenerateFallsBackToChatCompletions(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/v1/responses" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"model \"qwen3.8-flash\" is not supported on /v1/responses; use /v1/chat/completions instead","code":"model_not_supported_on_endpoint"}}`))
			return
		}
		if r.URL.Path == "/v1/chat/completions" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"qwen3.8-flash","choices":[{"message":{"content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := New("test-key")
	client.BaseURL = server.URL + "/v1"
	client.HTTP = server.Client()
	got, err := client.Generate(context.Background(), sdk.Request{Model: "qwen3.8-flash"})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if got.Model != "qwen3.8-flash" || len(got.Content) != 1 || got.Content[0].Text != "hello" {
		t.Fatalf("unexpected response: %#v", got)
	}
	if len(paths) != 2 || paths[0] != "/v1/responses" || paths[1] != "/v1/chat/completions" {
		t.Fatalf("paths=%v", paths)
	}
}

func TestClientHeadersMergeCustom(t *testing.T) {
	c := &Client{BaseURL: "https://example.invalid/v1", APIKey: "k"}
	p := c.WithHeaders(map[string]string{"X-Title": "ai", "Authorization": "Bearer hacked"})
	got := p.(*Client).headers()
	if got["Authorization"] != "Bearer k" {
		t.Fatalf("auth header must win, got %q", got["Authorization"])
	}
	if got["X-Title"] != "ai" {
		t.Fatalf("custom header missing: %v", got)
	}
}

func TestWithHeadersDeepCopies(t *testing.T) {
	c := &Client{}
	in := map[string]string{"X-Title": "ai"}
	p := c.WithHeaders(in).(*Client)
	in["X-Title"] = "mutated"
	in["X-New"] = "x"
	if p.Headers["X-Title"] != "ai" || len(p.Headers) != 1 {
		t.Fatalf("shared adapter state leaked: %v", p.Headers)
	}
	if len(c.Headers) != 0 {
		t.Fatalf("base adapter mutated: %v", c.Headers)
	}
}

func TestGenerateFallsBackToChatOnResponses404(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/responses" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"404 page not found"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi","tool_calls":[]},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()
	c := &Client{BaseURL: server.URL, APIKey: "k"}
	resp, err := c.Generate(context.Background(), sdk.Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "hi" {
		t.Fatalf("fallback content = %+v (paths %v)", resp.Content, paths)
	}
	if len(paths) != 2 || paths[0] != "/responses" || paths[1] != "/chat/completions" {
		t.Fatalf("paths = %v", paths)
	}
}

func TestGenerateSurfacesChatErrorWhenBoth404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
	}))
	defer server.Close()
	c := &Client{BaseURL: server.URL, APIKey: "k"}
	if _, err := c.Generate(context.Background(), sdk.Request{Model: "m"}); err == nil {
		t.Fatal("expected error when both endpoints 404")
	}
}

// A compatible gateway is reached by a custom base URL, so chat/completions is
// the primary dialect and its deltas must arrive in order.
func TestStreamUsesChatCompletionsForAGateway(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"สวัส\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ดี\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	client := New("test-key").WithBaseURL(server.URL).(*Client)

	events, err := client.Stream(context.Background(), sdk.Request{Model: "m", Messages: []sdk.Turn{{Role: sdk.RoleUser, Content: []sdk.ContentPart{{Type: sdk.ContentText, Text: "hi"}}}}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var text strings.Builder
	done := 0
	for event := range events {
		switch event.Type {
		case sdk.EventText:
			text.WriteString(event.Text)
		case sdk.EventDone:
			done++
		case sdk.EventError:
			t.Fatalf("stream error: %v", event.Err)
		}
	}
	if text.String() != "สวัสดี" {
		t.Fatalf("streamed text = %q, want the chat deltas in order", text.String())
	}
	if done != 1 {
		t.Fatalf("done events = %d, want 1", done)
	}
	if len(seen) != 1 || seen[0] != "/chat/completions" {
		t.Fatalf("requested paths = %v, want the gateway dialect only", seen)
	}
}

// A gateway that only implements the Responses API still has to stream, by way
// of the fallback, rather than answering with one opaque completion.
func TestStreamFallsBackToTheResponsesDialect(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		if r.URL.Path == "/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"ResponsesResponse.output_text.delta\",\"delta\":\"หวัด\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"ResponsesResponse.completed\"}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	client := New("test-key").WithBaseURL(server.URL).(*Client)

	events, err := client.Stream(context.Background(), sdk.Request{Model: "m", Messages: []sdk.Turn{{Role: sdk.RoleUser, Content: []sdk.ContentPart{{Type: sdk.ContentText, Text: "hi"}}}}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var text strings.Builder
	for event := range events {
		if event.Type == sdk.EventText {
			text.WriteString(event.Text)
		}
		if event.Type == sdk.EventError {
			t.Fatalf("stream error: %v", event.Err)
		}
	}
	if text.String() != "หวัด" {
		t.Fatalf("streamed text = %q, want the fallback deltas", text.String())
	}
	if len(seen) != 2 || seen[0] != "/chat/completions" || seen[1] != "/responses" {
		t.Fatalf("requested paths = %v, want the fallback order", seen)
	}
}

func TestStreamChatCollectsToolCallDeltas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","function":{"name":"read","arguments":"{\"pa"}}]}}]}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	client := New("test-key").WithBaseURL(server.URL).(*Client)
	events, err := client.Stream(context.Background(), sdk.Request{Model: "m", Messages: []sdk.Turn{{Role: sdk.RoleUser, Content: []sdk.ContentPart{{Type: sdk.ContentText, Text: "hi"}}}}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var call *sdk.ToolCall
	for event := range events {
		if event.Type == sdk.EventToolCall {
			call = event.ToolCall
		}
		if event.Type == sdk.EventError {
			t.Fatalf("stream error: %v", event.Err)
		}
	}
	if call == nil || call.Name != "read" || call.Arguments != `{"path":"a"}` {
		t.Fatalf("tool call = %+v, want the arguments assembled from the deltas", call)
	}
}

// A streamed gateway reports its token counts in a trailing usage chunk, and the
// closing event is the one that carries them.
func TestStreamChatReportsTokenUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":42,\"completion_tokens\":5,\"total_tokens\":47,\"prompt_tokens_details\":{\"cached_tokens\":7}}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	client := New("test-key").WithBaseURL(server.URL).(*Client)

	events, err := client.Stream(context.Background(), sdk.Request{Model: "m", Messages: []sdk.Turn{{Role: sdk.RoleUser, Content: []sdk.ContentPart{{Type: sdk.ContentText, Text: "hi"}}}}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var usage sdk.Usage
	for event := range events {
		if event.Type == sdk.EventError {
			t.Fatalf("stream error: %v", event.Err)
		}
		if event.Type == sdk.EventDone && event.Response != nil {
			usage = event.Response.Usage
		}
	}
	if usage.InputTokens != 42 || usage.OutputTokens != 5 || usage.CacheReadTokens != 7 {
		t.Fatalf("usage = %+v, want the counts the gateway sent", usage)
	}
}

// The Responses dialect reports usage on the completed event.
func TestStreamResponsesReportsTokenUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat/completions" {
			// A gateway that only speaks the Responses dialect: the client falls
			// back to it, which is where the usage event is.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"ResponsesResponse.output_text.delta\",\"delta\":\"hi\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"ResponsesResponse.completed\",\"response\":{\"model\":\"m\",\"status\":\"completed\",\"usage\":{\"input_tokens\":9,\"output_tokens\":3,\"total_tokens\":12}}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	client := New("test-key")
	client.BaseURL = server.URL

	events, err := client.Stream(context.Background(), sdk.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var usage sdk.Usage
	for event := range events {
		if event.Type == sdk.EventError {
			t.Fatalf("stream error: %v", event.Err)
		}
		if event.Type == sdk.EventDone && event.Response != nil {
			usage = event.Response.Usage
		}
	}
	if usage.InputTokens != 9 || usage.OutputTokens != 3 {
		t.Fatalf("usage = %+v, want the counts on the completed event", usage)
	}
}

func TestStreamRequestAsksForUsage(t *testing.T) {
	if opts, ok := BuildChatRequest(sdk.Request{Model: "m", Stream: true})["stream_options"].(map[string]any); !ok || opts["include_usage"] != true {
		t.Fatal("a streamed chat request must ask for usage")
	}
	if _, ok := BuildChatRequest(sdk.Request{Model: "m"})["stream_options"]; ok {
		t.Fatal("a non-streamed chat request must not ask for stream options")
	}
}
