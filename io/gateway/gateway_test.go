package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"ai-engine/config"
	"ai-engine/provider"
	"ai-engine/provider/registry"
	"ai-engine/session"
	"ai-engine/tools"
)

type memStore struct {
	mu       sync.Mutex
	sessions map[string]*session.Session
	turns    map[string][]session.Turn
}

func newMemStore() *memStore {
	return &memStore{sessions: map[string]*session.Session{}, turns: map[string][]session.Turn{}}
}

func (m *memStore) Create(params session.NewSession) (*session.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	created := &session.Session{
		ID:          "s1",
		Title:       params.Title,
		Provider:    params.Provider,
		Model:       params.Model,
		SubProvider: params.SubProvider,
		SubModel:    params.SubModel,
		SubEnabled:  params.SubEnabled,
		Temperature: params.Temperature,
		TopP:        params.TopP,
		MaxTokens:   params.MaxTokens,
	}
	m.sessions[created.ID] = created
	return created, nil
}

func (m *memStore) Get(id string) (*session.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	found, ok := m.sessions[id]
	if !ok {
		return nil, io.EOF
	}
	return found, nil
}

func (m *memStore) List() ([]session.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]session.Session, 0, len(m.sessions))
	for _, item := range m.sessions {
		out = append(out, *item)
	}
	return out, nil
}

func (m *memStore) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	delete(m.turns, id)
	return nil
}

func (m *memStore) SaveConfig(id string, saved session.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	target, ok := m.sessions[id]
	if !ok {
		return io.EOF
	}
	target.Provider = saved.Provider
	target.Model = saved.Model
	if saved.Title != "" {
		target.Title = saved.Title
	}
	return nil
}

func (m *memStore) Turns(id string) ([]session.Turn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]session.Turn(nil), m.turns[id]...), nil
}

func (m *memStore) History(id string) ([]provider.Message, error) {
	turns, err := m.Turns(id)
	if err != nil {
		return nil, err
	}
	messages := make([]provider.Message, 0, len(turns))
	for _, turn := range turns {
		if turn.Role == "system" || turn.Role == "user" || turn.Role == "assistant" {
			messages = append(messages, provider.Message{Role: turn.Role, Content: turn.Text})
		}
	}
	return messages, nil
}

func (m *memStore) AppendTurns(id string, turns []session.TurnInput) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, turn := range turns {
		m.turns[id] = append(m.turns[id], session.Turn{
			SessionID:    id,
			Seq:          len(m.turns[id]) + 1,
			Role:         turn.Role,
			Text:         turn.Text,
			Model:        turn.Model,
			InputTokens:  turn.InputTokens,
			OutputTokens: turn.OutputTokens,
			DurationMS:   turn.DurationMS,
		})
	}
	return nil
}

func fakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, `{"error":{"message":"bad key"}}`, http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		request := struct {
			Stream bool `json:"stream"`
		}{}
		_ = json.Unmarshal(body, &request)

		answered := strings.Contains(string(body), `"tool_call_id"`)
		if request.Stream {
			if answered {
				streamFake(w, "done", nil, "stop")
			} else {
				streamFake(w, "", []string{"current_time"}, "tool_calls")
			}
			return
		}
		if answered {
			writeFake(w, "done", nil, "")
			return
		}
		writeFake(w, "", []string{"current_time"}, "thinking")
	}))
}

func streamFake(w http.ResponseWriter, content string, toolCalls []string, finish string) {
	w.Header().Set("Content-Type", "text/event-stream")
	if content != "" || len(toolCalls) > 0 {
		delta := map[string]any{"role": "assistant"}
		if content != "" {
			delta["content"] = content
		}
		if len(toolCalls) > 0 {
			calls := make([]map[string]any, 0, len(toolCalls))
			for _, name := range toolCalls {
				calls = append(calls, map[string]any{
					"id":       "call_1",
					"type":     "function",
					"index":    0,
					"function": map[string]any{"name": name, "arguments": "{}"},
				})
			}
			delta["tool_calls"] = calls
		}
		chunk := map[string]any{
			"id": "chatcmpl-fake",
			"choices": []map[string]any{
				{"index": 0, "delta": delta, "finish_reason": nil},
			},
		}
		encoded, _ := json.Marshal(chunk)
		_, _ = w.Write([]byte("data: " + string(encoded) + "\n\n"))
	}
	final := map[string]any{
		"id": "chatcmpl-fake",
		"choices": []map[string]any{
			{"index": 0, "delta": map[string]any{}, "finish_reason": finish},
		},
	}
	encoded, _ := json.Marshal(final)
	_, _ = w.Write([]byte("data: " + string(encoded) + "\n\n"))
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
}

func writeFake(w http.ResponseWriter, content string, toolCalls []string, finish string) {
	w.Header().Set("Content-Type", "application/json")
	message := map[string]any{"role": "assistant", "content": content}
	if len(toolCalls) > 0 {
		calls := make([]map[string]any, 0, len(toolCalls))
		for i, name := range toolCalls {
			calls = append(calls, map[string]any{
				"id":       "call_1",
				"type":     "function",
				"index":    i,
				"function": map[string]any{"name": name, "arguments": "{}"},
			})
		}
		message["tool_calls"] = calls
		finish = "tool_calls"
	}
	payload := map[string]any{
		"id":      "chatcmpl-fake",
		"model":   "test-model",
		"choices": []map[string]any{{"message": message, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7},
	}
	_ = json.NewEncoder(w).Encode(payload)
}

func newTestGateway(t *testing.T, store SessionStore) (*Gateway, *httptest.Server) {
	t.Helper()
	upstream := fakeUpstream(t)
	t.Cleanup(upstream.Close)
	config.Providers = []config.Provider{{
		ID:      "9",
		Name:    "mock",
		APIURL:  upstream.URL,
		Keys:    []string{"test-key"},
		Adapter: "openai",
	}}
	return New(store, registry.New(), tools.Builtin()), upstream
}

func postJSON(t *testing.T, handler http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestChatCompletionRunsToolAndReturnsAnswer(t *testing.T) {
	gateway, _ := newTestGateway(t, newMemStore())

	recorder := postJSON(t, gateway.Handler(), "/v1/chat/completions",
		`{"model":"mock/test-model","messages":[{"role":"user","content":"what time is it"}]}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	var payload struct {
		Provider string `json:"provider"`
		Choices  []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Provider != "mock" {
		t.Errorf("provider = %q, want mock", payload.Provider)
	}
	if got := payload.Choices[0].Message.Content; got != "done" {
		t.Errorf("content = %q, want done", got)
	}
	if got := payload.Usage.TotalTokens; got != 14 {
		t.Errorf("total tokens = %d, want 14 (two rounds)", got)
	}
}

func TestChatCompletionStreams(t *testing.T) {
	gateway, _ := newTestGateway(t, newMemStore())

	recorder := postJSON(t, gateway.Handler(), "/v1/chat/completions",
		`{"model":"mock/test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "chat.completion.chunk") {
		t.Errorf("missing chunk objects:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("missing [DONE]:\n%s", body)
	}
	if strings.Count(body, "\n\n") < 3 {
		t.Errorf("expected several SSE events:\n%s", body)
	}
}

func TestChatRejectsUnknownModel(t *testing.T) {
	gateway, _ := newTestGateway(t, newMemStore())

	recorder := postJSON(t, gateway.Handler(), "/v1/chat/completions",
		`{"model":"nope","messages":[{"role":"user","content":"hi"}]}`)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "prefix it with a provider") {
		t.Errorf("body = %s", recorder.Body.String())
	}
}

func TestChatWithSessionPersistsTurns(t *testing.T) {
	store := newMemStore()
	gateway, _ := newTestGateway(t, store)

	created := postJSON(t, gateway.Handler(), "/v1/sessions", `{"title":"t","model":"mock/test-model"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", created.Code, created.Body.String())
	}

	recorder := postJSON(t, gateway.Handler(), "/v1/chat/completions",
		`{"session_id":"s1","messages":[{"role":"user","content":"hello"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("chat status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	turns, _ := store.Turns("s1")
	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2 (user + assistant)", len(turns))
	}
	if turns[0].Role != "user" || turns[0].Text != "hello" {
		t.Errorf("first turn = %+v", turns[0])
	}
	if turns[1].Role != "assistant" || turns[1].Text != "done" {
		t.Errorf("second turn = %+v", turns[1])
	}
	if store.sessions["s1"].Model != "test-model" {
		t.Errorf("session model = %q, want test-model", store.sessions["s1"].Model)
	}
}

func TestMergeHistoryAppendsOnlyNewMessages(t *testing.T) {
	stored := []provider.Message{
		{Role: "user", Content: "one"},
		{Role: "assistant", Content: "two"},
	}
	incoming := []provider.Message{
		{Role: "user", Content: "one"},
		{Role: "assistant", Content: "two"},
		{Role: "user", Content: "three"},
	}

	full, delta := mergeHistory(stored, incoming)
	if len(full) != 3 {
		t.Errorf("full = %d messages, want 3", len(full))
	}
	if len(delta) != 1 || delta[0].Content != "three" {
		t.Errorf("delta = %+v, want only the new message", delta)
	}
}
