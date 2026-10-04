package openai

import (
	"ai-engine/provider"
	"ai-engine/provider/internal"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// ---- from provider/openai/openai.go ----
type Client struct {
	BaseURL string
	APIKey  string
	Headers map[string]string
	HTTP    *http.Client
}

func New(apiKey string) *Client {
	return &Client{BaseURL: "https://api.openai.com/v1", APIKey: apiKey, HTTP: http.DefaultClient}
}
func (c *Client) WithAPIKey(key string) provider.Provider { cp := *c; cp.APIKey = key; return &cp }
func (c *Client) WithBaseURL(baseURL string) provider.Provider {
	cp := *c
	cp.BaseURL = baseURL
	return &cp
}
func (c *Client) WithHeaders(headers map[string]string) provider.Provider {
	cp := *c
	cp.Headers = cloneHeaders(headers)
	return &cp
}
func (c *Client) ListModels(ctx context.Context, apiKey string) ([]provider.Model, error) {
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if apiKey != "" {
		c = c.WithAPIKey(apiKey).(*Client)
	}
	var r struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := internal.DoJSON(ctx, c.http(), http.MethodGet, c.BaseURL+"/models", c.headers(), nil, &r); err != nil {
		return nil, err
	}
	models := make([]provider.Model, 0, len(r.Data))
	for _, item := range r.Data {
		if item.ID != "" {
			models = append(models, provider.Model{ID: item.ID, Name: item.ID, SupportsStreaming: true, SupportsTemperature: true})
		}
	}
	return models, nil
}
func (c *Client) Name() string { return "openai" }

type ResponsesResponse struct {
	ID     string `json:"id"`
	Model  string `json:"model"`
	Output []struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Status string `json:"status"`
	Usage  struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		TotalTokens  int `json:"total_tokens"`
		InputDetails struct {
			Cached int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
		OutputDetails struct {
			Reasoning int `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
}
type ChatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		PromptDetails    struct {
			Cached int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionDetails struct {
			Reasoning int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

func BuildResponsesRequest(req provider.Request) map[string]any {
	b := map[string]any{"model": req.Model, "stream": req.Stream}
	if req.SystemPrompt != "" {
		b["instructions"] = req.SystemPrompt
	}
	var input []any
	for _, m := range req.Messages {
		switch m.Role {
		case provider.RoleUser, provider.RoleModel:
			if m.Reasoning != nil && m.Reasoning.Text != "" {
				item := map[string]any{"type": "reasoning", "status": "completed", "content": []any{map[string]any{"type": "reasoning_text", "text": m.Reasoning.Text}}}
				if m.Reasoning.ID != "" {
					item["id"] = m.Reasoning.ID
				}
				input = append(input, item)
			}
			text := ""
			for _, p := range m.Content {
				text += p.Text
			}
			if text != "" {
				role := string(m.Role)
				if m.Role == provider.RoleModel {
					role = "assistant"
				}
				input = append(input, map[string]any{"type": "message", "role": role, "content": text})
			}
		case provider.RoleToolCall:
			if m.ToolCall != nil {
				input = append(input, map[string]any{"type": "function_call", "call_id": m.ToolCall.ID, "name": m.ToolCall.Name, "arguments": m.ToolCall.Arguments})
			}
		case provider.RoleToolResult:
			if m.ToolResult != nil {
				input = append(input, map[string]any{"type": "function_call_output", "call_id": m.ToolResult.ID, "output": m.ToolResult.Content})
			}
		}
	}
	b["input"] = input
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": t.InputSchema})
		}
		b["tools"] = tools
	}
	if req.Temperature != nil {
		b["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		b["top_p"] = *req.TopP
	}
	if req.MaxOutputTokens > 0 {
		b["max_output_tokens"] = req.MaxOutputTokens
	}
	// /responses takes the sampling knobs next to reasoning without complaint, so
	// only the parameters it does not have at all are left out.
	if effort := reasoningEffort(req); effort != "" {
		b["reasoning"] = map[string]any{"effort": effort}
	}
	addStreamUsage(b, req.Stream)
	return b
}

// addStreamUsage asks for the token counts on a streamed answer. Without it the
// usage only ever arrives on a non-streamed completion, and a phone that shows
// tokens per second would have nothing to count.
func addStreamUsage(b map[string]any, stream bool) {
	if stream {
		b["stream_options"] = map[string]any{"include_usage": true}
	}
}
func build(req provider.Request) map[string]any { return BuildResponsesRequest(req) }
func BuildChatRequest(req provider.Request) map[string]any {
	b := map[string]any{"model": req.Model, "stream": req.Stream}
	messages := make([]any, 0, len(req.Messages)+1)
	if req.SystemPrompt != "" {
		messages = append(messages, map[string]any{"role": "system", "content": req.SystemPrompt})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case provider.RoleUser, provider.RoleModel:
			content := ""
			for _, p := range m.Content {
				content += p.Text
			}
			if content != "" {
				role := "user"
				if m.Role == provider.RoleModel {
					role = "assistant"
				}
				messages = append(messages, map[string]any{"role": role, "content": content})
			}
		case provider.RoleToolCall:
			if m.ToolCall != nil {
				messages = append(messages, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": m.ToolCall.ID, "type": "function", "function": map[string]any{"name": m.ToolCall.Name, "arguments": m.ToolCall.Arguments}}}})
			}
		case provider.RoleToolResult:
			if m.ToolResult != nil {
				messages = append(messages, map[string]any{"role": "tool", "tool_call_id": m.ToolResult.ID, "content": m.ToolResult.Content})
			}
		}
	}
	b["messages"] = messages
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.InputSchema}})
		}
		b["tools"] = tools
	}
	effort := reasoningEffort(req)
	if effort != "" {
		// A model that thinks before it answers accepts only the default
		// sampling parameters on this endpoint: sending temperature, top_p or
		// either penalty alongside reasoning_effort is refused. Leaving them
		// out is the same as asking for the default.
		b["reasoning_effort"] = effort
		if req.MaxOutputTokens > 0 {
			// max_tokens is the old name and is refused by these models.
			b["max_completion_tokens"] = req.MaxOutputTokens
		}
	} else {
		if req.Temperature != nil {
			b["temperature"] = *req.Temperature
		}
		if req.TopP != nil {
			b["top_p"] = *req.TopP
		}
		if req.PresencePenalty != nil {
			b["presence_penalty"] = *req.PresencePenalty
		}
		if req.FrequencyPenalty != nil {
			b["frequency_penalty"] = *req.FrequencyPenalty
		}
		if req.Seed != nil {
			b["seed"] = *req.Seed
		}
		if len(req.StopSequences) > 0 {
			b["stop"] = req.StopSequences
		}
		if req.MaxOutputTokens > 0 {
			b["max_tokens"] = req.MaxOutputTokens
		}
	}
	addStreamUsage(b, req.Stream)
	return b
}

// reasoningEffort maps the session's thinking level onto what a provider is
// asked for. "none" and "off" both mean "do not ask for reasoning at all", which
// is expressed by leaving the key out entirely rather than by an empty string.
func reasoningEffort(req provider.Request) string {
	switch req.ThinkingLevel {
	case provider.ThinkingLow, provider.ThinkingMedium, provider.ThinkingHigh:
		return string(req.ThinkingLevel)
	default:
		return ""
	}
}

// SamplingKnobs are the parameters OpenAI has no place for but Gemini and
// Anthropic do. They are kept out of the canonical map on purpose: that map is
// sent to OpenAI verbatim as the HTTP body, so an extra key here would be a
// rejected parameter rather than a carried setting. Adapters that want these
// merge this into their own payload instead.
func SamplingKnobs(req provider.Request) map[string]any {
	b := map[string]any{}
	carrySamplingKnobs(b, req)
	return b
}

func carrySamplingKnobs(b map[string]any, req provider.Request) {
	if req.TopK != nil {
		b["top_k"] = *req.TopK
	}
	if len(req.StopSequences) > 0 {
		b["stop"] = req.StopSequences
	}
	if req.PresencePenalty != nil {
		b["presence_penalty"] = *req.PresencePenalty
	}
	if req.FrequencyPenalty != nil {
		b["frequency_penalty"] = *req.FrequencyPenalty
	}
	if req.Seed != nil {
		b["seed"] = *req.Seed
	}
}
func buildChat(req provider.Request) map[string]any { return BuildChatRequest(req) }
func ParseResponsesResponse(r ResponsesResponse) provider.Response {
	out := provider.Response{Provider: "openai", Model: r.Model, Usage: provider.Usage{InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens, TotalTokens: r.Usage.TotalTokens, CacheReadTokens: r.Usage.InputDetails.Cached, ReasoningTokens: r.Usage.OutputDetails.Reasoning, InputIncludesCache: true}, Cache: provider.CacheInfo{Layer: "provider"}}
	out.Cache.Hit = out.Usage.CacheReadTokens > 0
	for _, item := range r.Output {
		switch item.Type {
		case "reasoning":
			for _, p := range item.Content {
				if p.Type == "reasoning_text" && p.Text != "" {
					out.Reasoning = &provider.ReasoningState{ID: item.ID, Text: p.Text}
					break
				}
			}
		case "message":
			for _, p := range item.Content {
				if p.Text != "" {
					out.Content = append(out.Content, provider.ContentPart{Type: provider.ContentText, Text: p.Text})
				}
			}
		case "function_call":
			out.ToolCalls = append(out.ToolCalls, provider.ToolCall{ID: item.CallID, Name: item.Name, Arguments: item.Arguments})
		}
	}
	out.FinishReason = r.Status
	return out
}
func parseResponse(r ResponsesResponse) provider.Response { return ParseResponsesResponse(r) }
func ParseChatResponse(r ChatResponse) provider.Response {
	out := provider.Response{Provider: "openai", Model: r.Model, Usage: provider.Usage{InputTokens: r.Usage.PromptTokens, OutputTokens: r.Usage.CompletionTokens, TotalTokens: r.Usage.TotalTokens, CacheReadTokens: r.Usage.PromptDetails.Cached, ReasoningTokens: r.Usage.CompletionDetails.Reasoning, InputIncludesCache: true}, Cache: provider.CacheInfo{Layer: "provider"}}
	out.Cache.Hit = out.Usage.CacheReadTokens > 0
	if len(r.Choices) == 0 {
		return out
	}
	choice := r.Choices[0]
	if choice.Message.Content != "" {
		out.Content = append(out.Content, provider.ContentPart{Type: provider.ContentText, Text: choice.Message.Content})
	}
	for _, call := range choice.Message.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, provider.ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
	}
	out.FinishReason = choice.FinishReason
	return out
}
func parseChatResponse(r ChatResponse) provider.Response { return ParseChatResponse(r) }
func (c *Client) headers() map[string]string {
	h := map[string]string{"Authorization": "Bearer " + c.APIKey}
	for k, v := range c.Headers {
		if v == "" || strings.EqualFold(k, "Authorization") {
			continue
		}
		h[k] = v
	}
	return h
}
func cloneHeaders(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
func isResponsesModelUnsupported(err error) bool {
	var httpErr *internal.HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	if httpErr.StatusCode == http.StatusNotFound {
		return true
	}
	if httpErr.StatusCode != http.StatusBadRequest {
		return false
	}
	return strings.Contains(httpErr.Body, `"code":"model_not_supported_on_endpoint"`) && strings.Contains(httpErr.Body, `/v1/responses`)
}
func (c *Client) Generate(ctx context.Context, req provider.Request) (provider.Response, error) {
	var r ResponsesResponse
	err := internal.DoJSON(ctx, c.http(), http.MethodPost, c.BaseURL+"/responses", c.headers(), build(req), &r)
	if err == nil {
		return parseResponse(r), nil
	}
	if !isResponsesModelUnsupported(err) {
		return provider.Response{}, err
	}
	var chat ChatResponse
	if chatErr := internal.DoJSON(ctx, c.http(), http.MethodPost, c.BaseURL+"/chat/completions", c.headers(), buildChat(req), &chat); chatErr != nil {
		return provider.Response{}, chatErr
	}
	return parseChatResponse(chat), nil
}

// Stream streams an answer. Two dialects are in play: OpenAI's own Responses
// API and the chat/completions shape every compatible gateway speaks. Which one
// is tried first is decided by the endpoint — a custom base URL is a gateway, so
// it gets chat/completions — and the other one is still tried when the first
// fails before emitting anything, so nothing reaches the caller half-way.
func (c *Client) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	req.Stream = true
	ch := make(chan provider.Event, 16)
	go func() {
		defer close(ch)
		first, second := c.streamResponses, c.streamChat
		if c.prefersChatCompletions() {
			first, second = c.streamChat, c.streamResponses
		}
		emitted := 0
		err := first(ctx, req, ch, &emitted)
		if err == nil {
			return
		}
		if emitted > 0 || ctx.Err() != nil {
			ch <- provider.Event{Type: provider.EventError, Err: err}
			return
		}
		if fallbackErr := second(ctx, req, ch, &emitted); fallbackErr != nil {
			ch <- provider.Event{Type: provider.EventError, Err: fallbackErr}
		}
	}()
	return ch, nil
}

// prefersChatCompletions reports whether this endpoint is an OpenAI-compatible
// gateway rather than OpenAI itself, which is the only place the Responses API
// streaming dialect is the primary one.
func (c *Client) prefersChatCompletions() bool {
	return !strings.Contains(c.BaseURL, "api.openai.com")
}

func (c *Client) streamResponses(ctx context.Context, req provider.Request, ch chan<- provider.Event, emitted *int) error {
	return internal.SSE(ctx, c.http(), http.MethodPost, c.BaseURL+"/responses", c.headers(), build(req), func(data []byte) error {
		var e struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
			Item  struct {
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"item"`
			Response struct {
				Model  string `json:"model"`
				Status string `json:"status"`
				Usage  struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
					TotalTokens  int `json:"total_tokens"`
					InputDetails struct {
						Cached int `json:"cached_tokens"`
					} `json:"input_tokens_details"`
				} `json:"usage"`
			} `json:"response"`
		}
		if json.Unmarshal(data, &e) != nil {
			return nil
		}
		switch e.Type {
		case "ResponsesResponse.output_text.delta":
			if e.Delta != "" {
				*emitted++
				ch <- provider.Event{Type: provider.EventText, Text: e.Delta}
			}
		case "ResponsesResponse.reasoning_summary_text.delta":
			if e.Delta != "" {
				*emitted++
				ch <- provider.Event{Type: provider.EventReasoning, Reasoning: &provider.ReasoningState{Text: e.Delta}}
			}
		case "ResponsesResponse.function_call_arguments.done":
			if e.Item.CallID != "" {
				*emitted++
				ch <- provider.Event{Type: provider.EventToolCall, ToolCall: &provider.ToolCall{ID: e.Item.CallID, Name: e.Item.Name, Arguments: e.Item.Arguments}}
			}
		case "ResponsesResponse.completed":
			*emitted++
			// The finished response carries the token counts, so a streamed turn
			// reports the same usage a non-streamed one does.
			usage := provider.Usage{
				InputTokens:     e.Response.Usage.InputTokens,
				OutputTokens:    e.Response.Usage.OutputTokens,
				TotalTokens:     e.Response.Usage.TotalTokens,
				CacheReadTokens: e.Response.Usage.InputDetails.Cached,
			}
			finished := provider.Response{
				Provider: "openai", Model: e.Response.Model, FinishReason: e.Response.Status,
				Usage: usage, Cache: provider.CacheInfo{Layer: "provider", Hit: usage.CacheReadTokens > 0},
			}
			ch <- provider.Event{Type: provider.EventDone, Response: &finished}
		}
		return nil
	})
}

// streamChat reads the classic OpenAI-compatible SSE dialect: text arrives in
// choices[].delta.content, tool calls in choices[].delta.tool_calls.
func (c *Client) streamChat(ctx context.Context, req provider.Request, ch chan<- provider.Event, emitted *int) error {
	calls := map[int]*provider.ToolCall{}
	var order []int
	// A gateway sends the token counts in a trailing chunk that carries only
	// `usage`, after the choices are done. It is kept here so the closing event
	// can carry the real numbers instead of an estimate.
	var usage provider.Usage
	reason, toolTurn := "", false
	finish := func(reason string) provider.Event {
		return provider.Event{Type: provider.EventDone, Response: &provider.Response{
			Provider: "openai", FinishReason: reason,
			Usage: usage, Cache: provider.CacheInfo{Layer: "provider", Hit: usage.CacheReadTokens > 0},
		}}
	}
	err := internal.SSE(ctx, c.http(), http.MethodPost, c.BaseURL+"/chat/completions", c.headers(), buildChat(req), func(data []byte) error {
		var e struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
				PromptDetails    struct {
					Cached int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if json.Unmarshal(data, &e) != nil {
			return nil
		}
		if e.Usage != nil {
			usage = provider.Usage{
				InputTokens:     e.Usage.PromptTokens,
				OutputTokens:    e.Usage.CompletionTokens,
				TotalTokens:     e.Usage.TotalTokens,
				CacheReadTokens: e.Usage.PromptDetails.Cached,
			}
		}
		if len(e.Choices) == 0 {
			return nil
		}
		choice := e.Choices[0]
		if choice.Delta.Content != "" {
			*emitted++
			ch <- provider.Event{Type: provider.EventText, Text: choice.Delta.Content}
		}
		for _, part := range choice.Delta.ToolCalls {
			call, ok := calls[part.Index]
			if !ok {
				call = &provider.ToolCall{}
				calls[part.Index] = call
				order = append(order, part.Index)
			}
			if part.ID != "" {
				call.ID = part.ID
			}
			if part.Function.Name != "" {
				call.Name = part.Function.Name
			}
			call.Arguments += part.Function.Arguments
		}
		if choice.FinishReason == "tool_calls" {
			for _, index := range order {
				call := *calls[index]
				*emitted++
				ch <- provider.Event{Type: provider.EventToolCall, ToolCall: &call}
			}
			toolTurn = true
			return nil
		}
		if choice.FinishReason != "" {
			reason = choice.FinishReason
		}
		return nil
	})
	if err != nil || toolTurn {
		return err
	}
	// The token counts arrive in the chunk after the closing choice, so `done`
	// is emitted once the stream is over and the real numbers are in hand.
	*emitted++
	ch <- finish(reason)
	return nil
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// ---- from provider/openai/canonical.go ----
// Canonical OpenAI Responses wire is the central interface.
//
// Flow:
//
//	provider.Request --BuildResponsesRequest--> OpenAI Responses map (canonical)
//	    --anthropic.BuildFromOpenAI / gemini.BuildFromOpenAI--> provider-native payload
//
// Native provider responses are converted back through the same OpenAI
// Responses shape (see anthropic.ToOpenAIResponse / gemini.ToOpenAIResponse)
// before becoming provider.Response via ParseResponsesResponse.
//
// Chat Completions (BuildChatRequest/ParseChatResponse) is kept only as a
// fallback for providers that reject /responses.

// Request is the canonical OpenAI Responses request shape used as the
// intermediate representation between provider.Request and provider-native payloads.
type Request struct {
	Model            string         `json:"model"`
	Instructions     string         `json:"instructions,omitempty"`
	Input            []any          `json:"input,omitempty"`
	Tools            []any          `json:"tools,omitempty"`
	Temperature      *float64       `json:"temperature,omitempty"`
	TopP             *float64       `json:"top_p,omitempty"`
	TopK             *float64       `json:"top_k,omitempty"`
	StopSequences    []string       `json:"stop,omitempty"`
	PresencePenalty  *float64       `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64       `json:"frequency_penalty,omitempty"`
	Seed             *int64         `json:"seed,omitempty"`
	MaxOutputTokens  int            `json:"max_output_tokens,omitempty"`
	Reasoning        map[string]any `json:"reasoning,omitempty"`
	Stream           bool           `json:"stream,omitempty"`
}

// Canonical converts an provider.Request into the canonical OpenAI struct form.
// BuildResponsesRequest remains the map-based wire form used for HTTP.
func Canonical(req provider.Request) Request {
	wire := BuildResponsesRequest(req)
	out := Request{Stream: req.Stream}
	if v, _ := wire["model"].(string); v != "" {
		out.Model = v
	}
	if v, _ := wire["instructions"].(string); v != "" {
		out.Instructions = v
	}
	if v, ok := wire["input"].([]any); ok {
		out.Input = v
	}
	if v, ok := wire["tools"].([]any); ok {
		out.Tools = v
	}
	out.Temperature = req.Temperature
	out.TopP = req.TopP
	out.TopK = req.TopK
	out.StopSequences = req.StopSequences
	out.PresencePenalty = req.PresencePenalty
	out.FrequencyPenalty = req.FrequencyPenalty
	out.Seed = req.Seed
	if v, ok := wire["max_output_tokens"].(int); ok {
		out.MaxOutputTokens = v
	}
	if v, ok := wire["reasoning"].(map[string]any); ok {
		out.Reasoning = v
	}
	return out
}

// Helpers for reading the canonical map without touching sdk types.

func stringOf(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func itemsOf(m map[string]any, key string) []any {
	if m == nil {
		return nil
	}
	items, _ := m[key].([]any)
	return items
}

// InstructionsOf returns the canonical "instructions" (system prompt).
func InstructionsOf(openAIReq map[string]any) string { return stringOf(openAIReq, "instructions") }

// ModelOf returns the canonical "model".
func ModelOf(openAIReq map[string]any) string { return stringOf(openAIReq, "model") }

// InputItemsOf returns the canonical "input" items.
func InputItemsOf(openAIReq map[string]any) []any { return itemsOf(openAIReq, "input") }

// ToolsOf returns the canonical "tools" (type=function entries).
func ToolsOf(openAIReq map[string]any) []any { return itemsOf(openAIReq, "tools") }

// ReasoningEffortOf returns the canonical reasoning effort ("low"/"medium"/"high").
func ReasoningEffortOf(openAIReq map[string]any) string {
	if openAIReq == nil {
		return ""
	}
	r, _ := openAIReq["reasoning"].(map[string]any)
	effort, _ := r["effort"].(string)
	return effort
}

// TemperatureOf returns the canonical temperature if present.
func TemperatureOf(openAIReq map[string]any) (float64, bool) {
	return floatOf(openAIReq, "temperature")
}

// FloatOf reads any numeric value off the canonical map, for a key that is not
// an OpenAI parameter but is carried there on its way to another adapter.
func FloatOf(openAIReq map[string]any, key string) (float64, bool) { return floatOf(openAIReq, key) }

func floatOf(openAIReq map[string]any, key string) (float64, bool) {
	if openAIReq == nil {
		return 0, false
	}
	switch v := openAIReq[key].(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	}
	return 0, false
}

// TopPOf returns the canonical top_p if present.
func TopPOf(openAIReq map[string]any) (float64, bool) { return floatOf(openAIReq, "top_p") }

// TopKOf returns the canonical top_k if present. It is not an OpenAI parameter;
// it rides along on the canonical shape so Gemini and Anthropic can read it.
func TopKOf(openAIReq map[string]any) (float64, bool) { return floatOf(openAIReq, "top_k") }

// PresencePenaltyOf returns the canonical presence_penalty if present.
func PresencePenaltyOf(openAIReq map[string]any) (float64, bool) {
	return floatOf(openAIReq, "presence_penalty")
}

// FrequencyPenaltyOf returns the canonical frequency_penalty if present.
func FrequencyPenaltyOf(openAIReq map[string]any) (float64, bool) {
	return floatOf(openAIReq, "frequency_penalty")
}

// SeedOf returns the canonical seed if present.
func SeedOf(openAIReq map[string]any) (int64, bool) {
	if openAIReq == nil {
		return 0, false
	}
	switch v := openAIReq["seed"].(type) {
	case int64:
		return v, true
	case int:
		return int64(v), true
	case float64:
		return int64(v), true
	}
	return 0, false
}

// StopSequencesOf returns the canonical stop sequences if any were set.
func StopSequencesOf(openAIReq map[string]any) []string {
	if openAIReq == nil {
		return nil
	}
	switch v := openAIReq["stop"].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, entry := range v {
			if s, ok := entry.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// MaxOutputTokensOf returns the canonical max_output_tokens if present.
func MaxOutputTokensOf(openAIReq map[string]any) int {
	if openAIReq == nil {
		return 0
	}
	switch v := openAIReq["max_output_tokens"].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// Item helpers: every canonical input item is map[string]any with a "type".

// ItemTypeOf returns the canonical input item type.
func ItemTypeOf(item any) string {
	m, _ := item.(map[string]any)
	return stringOf(m, "type")
}

// ItemMap casts a canonical item to its map form.
func ItemMap(item any) map[string]any {
	m, _ := item.(map[string]any)
	return m
}

// ResponsesResponseFromParts builds an provider.Response through the canonical
// OpenAI Responses output shape: message content + function_call items +
// optional reasoning. Native adapters (anthropic/gemini) convert their wire
// ResponsesResponse into these parts first, then delegate here so provider.Response
// construction stays in one place.
func ResponsesResponseFromParts(model, status string, texts []string, toolCalls []provider.ToolCall, reasoning *provider.ReasoningState, usage provider.Usage) provider.Response {
	r := ResponsesResponse{Model: model, Status: status}
	r.Usage.InputTokens = usage.InputTokens
	r.Usage.OutputTokens = usage.OutputTokens
	r.Usage.TotalTokens = usage.TotalTokens
	r.Usage.InputDetails.Cached = usage.CacheReadTokens
	r.Usage.OutputDetails.Reasoning = usage.ReasoningTokens
	if reasoning != nil && reasoning.Text != "" {
		r.Output = append(r.Output, struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}{Type: "reasoning", ID: reasoning.ID, Content: []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{{Type: "reasoning_text", Text: reasoning.Text}}})
	}
	for _, text := range texts {
		if text == "" {
			continue
		}
		r.Output = append(r.Output, struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}{Type: "message", Content: []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{{Type: "output_text", Text: text}}})
	}
	for _, call := range toolCalls {
		r.Output = append(r.Output, struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}{Type: "function_call", CallID: call.ID, Name: call.Name, Arguments: call.Arguments})
	}
	return ParseResponsesResponse(r)
}
