package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/Tulipskun/ai-engine/sdk"
	"github.com/Tulipskun/ai-engine/sdk/providers/internal"
)

var sessionFormat = regexp.MustCompile(`^ses_f[0-9a-f]{8}ffe[A-Za-z0-9]{14}$`)

func TestSessionIDFormat(t *testing.T) {
	id := SessionIDFor("test-format")
	if !sessionFormat.MatchString(id) {
		t.Fatalf("session id %q does not match opencode shape", id)
	}
	if len(id) != 30 {
		t.Fatalf("session id length = %d, want 30", len(id))
	}
}

func TestSessionIDStablePerKey(t *testing.T) {
	first := SessionIDFor("stable-key")
	for i := 0; i < 5; i++ {
		if got := SessionIDFor("stable-key"); got != first {
			t.Fatalf("session id changed within process: %q vs %q", first, got)
		}
	}
	if other := SessionIDFor("other-key"); other == first {
		t.Fatal("distinct harness sessions share one provider session id")
	}
}

func TestSessionIDEmptyKeyMintsFresh(t *testing.T) {
	if a, b := SessionIDFor(""), SessionIDFor(""); a == b {
		t.Fatal("empty key must not be cached")
	}
}

func checkFingerprint(t *testing.T, h http.Header, sessionKey string) {
	t.Helper()
	if got := h.Get("User-Agent"); got != DefaultUserAgent {
		t.Fatalf("User-Agent=%q, want %q", got, DefaultUserAgent)
	}
	if got := h.Get("HTTP-Referer"); got != Referer {
		t.Fatalf("HTTP-Referer=%q", got)
	}
	if got := h.Get("X-Title"); got != Title {
		t.Fatalf("X-Title=%q", got)
	}
	sid := h.Get("x-opencode-session")
	if !sessionFormat.MatchString(sid) {
		t.Fatalf("x-opencode-session=%q, want opencode shape", sid)
	}
	if sessionKey != "" && sid != SessionIDFor(sessionKey) {
		t.Fatal("session header does not match the harness session mapping")
	}
}

func TestGeneratePrefersResponses(t *testing.T) {
	var gotHeaders http.Header
	var gotBody map[string]any
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		gotHeaders = r.Header
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"id":"resp-1","model":"m","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}`))
	}))
	defer srv.Close()
	c := New("k")
	c.BaseURL = srv.URL
	resp, err := c.Generate(context.Background(), sdk.Request{Model: "m", SessionID: "harness-1", Messages: []sdk.Turn{{Role: sdk.RoleUser, Content: []sdk.ContentPart{{Type: sdk.ContentText, Text: "hi"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "hi" {
		t.Fatalf("content=%+v", resp.Content)
	}
	if len(hits) != 1 || hits[0] != "/responses" {
		t.Fatalf("must use /responses first without chat fallback: %v", hits)
	}
	checkFingerprint(t, gotHeaders, "harness-1")
	if _, ok := gotBody["user"]; ok {
		t.Fatal("request body must not carry a user field (opencode sends the session via header only)")
	}
	if gotBody["model"] != "m" {
		t.Fatalf("model=%v", gotBody["model"])
	}
	if _, ok := gotBody["input"]; !ok {
		t.Fatal("responses body must carry input items")
	}
}

func TestCustomHeadersOverrideExceptSession(t *testing.T) {
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header
		_, _ = w.Write([]byte(`{"id":"resp-1","status":"completed","output":[]}`))
	}))
	defer srv.Close()
	c := New("k").WithHeaders(map[string]string{"User-Agent": "custom/1", "x-opencode-session": "ses_forged"}).(*Client)
	c.BaseURL = srv.URL
	if _, err := c.Generate(context.Background(), sdk.Request{Model: "m", SessionID: "harness-2"}); err != nil {
		t.Fatal(err)
	}
	if got := gotHeaders.Get("User-Agent"); got != "custom/1" {
		t.Fatalf("custom User-Agent lost: %q", got)
	}
	if got := gotHeaders.Get("x-opencode-session"); got != SessionIDFor("harness-2") {
		t.Fatalf("session header was overridden: %q", got)
	}
}

func TestGenerateParsesToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model":"m","status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"bash","arguments":"{\"command\":\"ls\"}"}]}`))
	}))
	defer srv.Close()
	c := New("k")
	c.BaseURL = srv.URL
	resp, err := c.Generate(context.Background(), sdk.Request{Model: "m", SessionID: "tools-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "bash" || resp.ToolCalls[0].ID != "call_1" {
		t.Fatalf("tool calls=%+v", resp.ToolCalls)
	}
}

func TestGenerateFallsBackToChatOnResponses500(t *testing.T) {
	var hits []string
	var chatHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		if r.URL.Path == "/responses" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"error","message":"Internal server error"}}`))
			return
		}
		chatHeaders = r.Header
		_, _ = w.Write([]byte(`{"model":"m","choices":[{"message":{"content":"via-chat"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	c := New("k")
	c.BaseURL = srv.URL
	resp, err := c.Generate(context.Background(), sdk.Request{Model: "m", SessionID: "fallback-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "via-chat" {
		t.Fatalf("content=%+v", resp.Content)
	}
	if len(hits) != 2 || hits[0] != "/responses" || hits[1] != "/chat/completions" {
		t.Fatalf("must fall back to chat after responses 500: %v", hits)
	}
	checkFingerprint(t, chatHeaders, "fallback-1")
}

func TestGenerateNoFallbackOnRateLimit(t *testing.T) {
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer srv.Close()
	c := New("k")
	c.BaseURL = srv.URL
	if _, err := c.Generate(context.Background(), sdk.Request{Model: "m", SessionID: "limited-1"}); err == nil {
		t.Fatal("rate limit must surface, not fall back")
	}
	if len(hits) != 1 || hits[0] != "/responses" {
		t.Fatalf("must not touch chat on 429: %v", hits)
	}
}

func TestListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if r.Header.Get("x-opencode-session") == "" {
			t.Fatal("models request misses session header")
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"a"},{"id":"b"}]}`))
	}))
	defer srv.Close()
	c := New("k")
	c.BaseURL = srv.URL
	models, err := c.ListModels(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "a" || models[1].ID != "b" {
		t.Fatalf("models=%+v", models)
	}
}

func collectStream(t *testing.T, ch <-chan sdk.Event) (string, int, bool) {
	t.Helper()
	var text strings.Builder
	var calls int
	var done bool
	for ev := range ch {
		switch ev.Type {
		case sdk.EventText:
			text.WriteString(ev.Text)
		case sdk.EventToolCall:
			calls++
			if ev.ToolCall.Name != "bash" || ev.ToolCall.ID != "c1" {
				t.Fatalf("tool call=%+v", ev.ToolCall)
			}
		case sdk.EventDone:
			done = true
		case sdk.EventError:
			t.Fatal(ev.Err)
		}
	}
	return text.String(), calls, done
}

func TestStreamResponsesDirect(t *testing.T) {
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"he\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"llo\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.done\",\"item\":{\"call_id\":\"c1\",\"name\":\"bash\",\"arguments\":\"{}\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\"}\n\n")
	}))
	defer srv.Close()
	c := New("k")
	c.BaseURL = srv.URL
	ch, err := c.Stream(context.Background(), sdk.Request{Model: "m", SessionID: "stream-1"})
	if err != nil {
		t.Fatal(err)
	}
	text, calls, done := collectStream(t, ch)
	if text != "hello" || calls != 1 || !done {
		t.Fatalf("text=%q calls=%d done=%v", text, calls, done)
	}
	if len(hits) != 1 || hits[0] != "/responses" {
		t.Fatalf("must stream /responses without chat fallback: %v", hits)
	}
}

func TestStreamFallsBackToChat(t *testing.T) {
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		if r.URL.Path == "/responses" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"type":"error"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"llo\",\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"bash\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c := New("k")
	c.BaseURL = srv.URL
	ch, err := c.Stream(context.Background(), sdk.Request{Model: "m", SessionID: "stream-2"})
	if err != nil {
		t.Fatal(err)
	}
	text, calls, done := collectStream(t, ch)
	if text != "hello" || calls != 1 || !done {
		t.Fatalf("text=%q calls=%d done=%v", text, calls, done)
	}
	if len(hits) != 2 || hits[0] != "/responses" || hits[1] != "/chat/completions" {
		t.Fatalf("must fall back to chat stream: %v", hits)
	}
}

func TestResponsesBodyUsesDeveloperInput(t *testing.T) {
	req := newResponsesBodyRequest()
	body := buildResponsesRequest(req)
	if _, ok := body["instructions"]; ok {
		t.Fatal("responses body must not use the instructions field")
	}
	items, ok := body["input"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("input items = %v", body["input"])
	}
	first, ok := items[0].(map[string]any)
	if !ok || first["role"] != "developer" || first["content"] != "sys" {
		t.Fatalf("first input must be the developer system prompt: %v", items[0])
	}
}

func newResponsesBodyRequest() sdk.Request {
	return sdk.Request{Model: "m", SystemPrompt: "sys", Messages: []sdk.Turn{{Role: sdk.RoleUser, Content: []sdk.ContentPart{{Type: sdk.ContentText, Text: "hi"}}}}}
}

// Zen answers 503 "Endpoint is unavailable" for a model it serves only on
// /chat/completions; that must fall back instead of failing the turn.
func TestShouldTryChatFallsBackOn503(t *testing.T) {
	if !shouldTryChat(&internal.HTTPError{StatusCode: 503, Body: `{"error":{"message":"Upstream request failed: Endpoint is unavailable."}}`}) {
		t.Fatal("503 must fall back to /chat/completions")
	}
	// Zen answers "not this endpoint" on the free tier with a 403 FreeTierError
	// for models it only serves on /chat/completions, so that one does fall back.
	if !shouldTryChat(&internal.HTTPError{StatusCode: 403, Body: `{"error":{"type":"FreeTierError"}}`}) {
		t.Fatal("403 FreeTierError must fall back to /chat/completions")
	}
	// A real auth failure is the same verdict on either endpoint: do not retry.
	if shouldTryChat(&internal.HTTPError{StatusCode: 401, Body: `{"error":{"type":"AuthError"}}`}) {
		t.Fatal("401 must not fall back")
	}
}

// A streamed turn must report the same token counts a non-streamed one does:
// Zen sends them in a trailing chunk that carries only `usage`.
func TestStreamReportsTokenUsageFromTheTrailingChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/responses" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":120,\"completion_tokens\":7,\"total_tokens\":127,\"prompt_tokens_details\":{\"cached_tokens\":80}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c := New("k")
	c.BaseURL = srv.URL
	ch, err := c.Stream(context.Background(), sdk.Request{Model: "m", SessionID: "stream-usage"})
	if err != nil {
		t.Fatal(err)
	}
	var usage sdk.Usage
	for ev := range ch {
		if ev.Type == sdk.EventError {
			t.Fatal(ev.Err)
		}
		if ev.Type == sdk.EventDone && ev.Response != nil {
			usage = ev.Response.Usage
		}
	}
	if usage.InputTokens != 120 || usage.OutputTokens != 7 || usage.CacheReadTokens != 80 {
		t.Fatalf("usage = %+v, want the counts and cached prompt tokens Zen sent in the trailing chunk", usage)
	}
}

func TestStreamResponsesReportsCachedUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"model\":\"m\",\"status\":\"completed\",\"usage\":{\"input_tokens\":90,\"output_tokens\":3,\"total_tokens\":93,\"input_token_details\":{\"cached_tokens\":25}}}}\n\n")
	}))
	defer srv.Close()
	c := New("k")
	c.BaseURL = srv.URL
	ch, err := c.Stream(context.Background(), sdk.Request{Model: "m", SessionID: "stream-responses-cache"})
	if err != nil {
		t.Fatal(err)
	}
	var usage sdk.Usage
	for ev := range ch {
		if ev.Type == sdk.EventError {
			t.Fatal(ev.Err)
		}
		if ev.Type == sdk.EventDone && ev.Response != nil {
			usage = ev.Response.Usage
		}
	}
	if usage.InputTokens != 90 || usage.OutputTokens != 3 || usage.CacheReadTokens != 25 {
		t.Fatalf("usage = %+v, want the Responses usage including cached input", usage)
	}
}

func TestChatRequestAsksForUsageOnAStream(t *testing.T) {
	body := buildChatRequest(sdk.Request{Model: "m", Stream: true, MaxOutputTokens: 8, Tools: probeToolsForTest()})
	opts, ok := body["stream_options"].(map[string]any)
	if !ok || opts["include_usage"] != true {
		t.Fatalf("stream_options = %v, want include_usage", body["stream_options"])
	}
	if body["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v, want auto when tools are sent", body["tool_choice"])
	}
	if body["max_tokens"] != 8 {
		t.Fatalf("max_tokens = %v, want the caller's budget", body["max_tokens"])
	}
}

func probeToolsForTest() []sdk.Tool {
	return []sdk.Tool{{Name: "bash", Description: "run", InputSchema: map[string]any{"type": "object"}}}
}

// The free tier refuses requests that do not carry the client's own tool names,
// so the adapter presents the client's tool set.
func TestRequestPresentsTheClientToolSet(t *testing.T) {
	b := buildChatRequest(sdk.Request{
		Model: "mimo-v2.5-free",
		Tools: []sdk.Tool{
			{Name: "bash", InputSchema: map[string]any{"type": "object"}},
			{Name: "read", InputSchema: map[string]any{"type": "object"}},
		},
	})
	tools, _ := b["tools"].([]any)
	if len(tools) != len(clientToolNames) {
		t.Fatalf("tool set has %d tools, want the client's %d", len(tools), len(clientToolNames))
	}
	got := map[string]string{}
	for _, entry := range tools {
		tool, _ := entry.(map[string]any)
		fn, _ := tool["function"].(map[string]any)
		name, _ := fn["name"].(string)
		raw, _ := json.Marshal(fn["parameters"])
		got[name] = string(raw)
	}
	for _, want := range clientToolNames {
		if _, ok := got[want]; !ok {
			t.Errorf("tool %q missing from the request", want)
		}
	}
	if !strings.Contains(got["bash"], `"object"`) {
		t.Error("the session's own bash schema was not carried over")
	}
}

// A call that comes back under a client name is executed as the session's tool.
func TestToolCallNamesMapBackToTheSession(t *testing.T) {
	// Only the two names this adapter sends can come back; a session tool the
	// adapter never offers stays unreachable rather than being translated.
	tools := []sdk.Tool{{Name: "bash"}, {Name: "read"}}
	for _, tc := range []struct{ client, want string }{
		{"bash", "bash"},
		{"read", "read"},
		{"grep", ""},
		{"todowrite", ""},
		{"websearch", ""},
	} {
		if got := oursToolForName(tc.client, tools); got != tc.want {
			t.Errorf("oursToolForName(%q) = %q, want %q", tc.client, got, tc.want)
		}
		// A call the session cannot run keeps the provider's own name, so the
		// agent can report it as unknown instead of silently dropping it.
		wantBack := tc.want
		if wantBack == "" {
			wantBack = tc.client
		}
		if got := toolNameBack(tc.client, tools); got != wantBack {
			t.Errorf("toolNameBack(%q) = %q, want %q", tc.client, got, wantBack)
		}
	}
}

// Zen closes some turns with an empty 200 stream; that must surface as a
// failure, and the other endpoint gets a chance first.
func TestEmptyResponsesStreamFallsBackThenFails(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "text/event-stream")
		if r.URL.Path == "/chat/completions" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"type":"server_error","message":"Upstream request failed"}}`))
			return
		}
		flusher, _ := w.(http.Flusher)
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer srv.Close()
	client := New("k")
	client.BaseURL = srv.URL
	ch, err := client.Stream(context.Background(), sdk.Request{Model: "muse-spark-1.2-contributor-free", Messages: []sdk.Turn{{Role: sdk.RoleUser, Content: []sdk.ContentPart{{Type: sdk.ContentText, Text: "ping"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	var got error
	for ev := range ch {
		if ev.Type == sdk.EventError {
			got = ev.Err
		}
	}
	if got == nil {
		t.Fatal("an empty turn was reported as a successful one")
	}
	if len(paths) != 2 || paths[0] != "/responses" || paths[1] != "/chat/completions" {
		t.Errorf("endpoints tried = %v, want /responses then /chat/completions", paths)
	}
}

// Instruction files ride in the system message as "Instructions from:" blocks,
// the way the OpenCode client puts them there, and nowhere else.
func TestInstructionFilesJoinTheSystemMessage(t *testing.T) {
	b := buildChatRequest(sdk.Request{
		Model:        "mimo-v2.5-free",
		SystemPrompt: "You are a coding agent.",
		Instructions: []sdk.Instruction{
			{Path: "/root/.config/opencode/AGENTS.md", Text: "global rules\n"},
			{Path: "/work/repo/AGENTS.md", Text: "project rules\n"},
			{Path: "/work/repo/EMPTY.md", Text: "  \n"},
		},
	})
	messages, _ := b["messages"].([]any)
	if len(messages) == 0 {
		t.Fatal("no system message in the request")
	}
	first, _ := messages[0].(map[string]any)
	system, _ := first["content"].(string)
	if !strings.HasPrefix(system, "You are a coding agent.") {
		t.Errorf("the system prompt is no longer first: %.60q", system)
	}
	global := strings.Index(system, "Instructions from: /root/.config/opencode/AGENTS.md")
	project := strings.Index(system, "Instructions from: /work/repo/AGENTS.md")
	if global < 0 || project < 0 {
		t.Fatalf("an instruction file is missing:\n%s", system)
	}
	if global > project {
		t.Error("the global file came after the project one; the client puts it first")
	}
	if !strings.Contains(system, "global rules") || !strings.Contains(system, "project rules") {
		t.Error("the file text did not come along")
	}
	if strings.Contains(system, "EMPTY.md") {
		t.Error("an empty instruction file was sent")
	}
}

// A request without instruction files is exactly what it was before.
func TestNoInstructionFilesLeavesTheSystemPromptAlone(t *testing.T) {
	b := buildChatRequest(sdk.Request{Model: "m", SystemPrompt: "just this"})
	messages, _ := b["messages"].([]any)
	first, _ := messages[0].(map[string]any)
	if system, _ := first["content"].(string); system != "just this" {
		t.Fatalf("system message = %q", system)
	}
}

// Zen moved some models between endpoints with a new 400 shape; that still
// means "try the other endpoint", not a dead key.
func TestShouldTryChatFallsBackOnModelProtocolUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/responses" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"ModelProtocolUnsupported","message":"Model does not support this protocol."}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c := New("k")
	c.BaseURL = srv.URL
	ch, err := c.Stream(context.Background(), sdk.Request{Model: "space-bunny-free", SessionID: "stream-proto"})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for ev := range ch {
		if ev.Type == sdk.EventError {
			t.Fatal(ev.Err)
		}
		if ev.Type == sdk.EventText {
			text += ev.Text
		}
	}
	if text != "hi" {
		t.Fatalf("text = %q, want the answer from the other endpoint", text)
	}
}
