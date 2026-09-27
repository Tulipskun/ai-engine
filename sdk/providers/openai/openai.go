package openai

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Tulipskun/ai-engine/sdk"
	"github.com/Tulipskun/ai-engine/sdk/providers/internal"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	BaseURL string
	APIKey  string
	Headers map[string]string
	HTTP    *http.Client
}

func New(apiKey string) *Client {
	return &Client{BaseURL: "https://api.openai.com/v1", APIKey: apiKey, HTTP: http.DefaultClient}
}
func (c *Client) WithAPIKey(key string) sdk.Provider      { cp := *c; cp.APIKey = key; return &cp }
func (c *Client) WithBaseURL(baseURL string) sdk.Provider { cp := *c; cp.BaseURL = baseURL; return &cp }
func (c *Client) WithHeaders(headers map[string]string) sdk.Provider {
	cp := *c
	cp.Headers = cloneHeaders(headers)
	return &cp
}
func (c *Client) ListModels(ctx context.Context, apiKey string) ([]sdk.Model, error) {
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
	models := make([]sdk.Model, 0, len(r.Data))
	for _, item := range r.Data {
		if item.ID != "" {
			models = append(models, sdk.Model{ID: item.ID, Name: item.ID, SupportsStreaming: true, SupportsTemperature: true})
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
	} `json:"usage"`
}

func BuildResponsesRequest(req sdk.Request) map[string]any {
	b := map[string]any{"model": req.Model, "stream": req.Stream}
	if req.SystemPrompt != "" {
		b["instructions"] = req.SystemPrompt
	}
	var input []any
	for _, m := range req.Messages {
		switch m.Role {
		case sdk.RoleUser, sdk.RoleModel:
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
				if m.Role == sdk.RoleModel {
					role = "assistant"
				}
				input = append(input, map[string]any{"type": "message", "role": role, "content": text})
			}
		case sdk.RoleToolCall:
			if m.ToolCall != nil {
				input = append(input, map[string]any{"type": "function_call", "call_id": m.ToolCall.ID, "name": m.ToolCall.Name, "arguments": m.ToolCall.Arguments})
			}
		case sdk.RoleToolResult:
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
	if req.MaxOutputTokens > 0 {
		b["max_output_tokens"] = req.MaxOutputTokens
	}
	if req.ThinkingLevel != "" && req.ThinkingLevel != sdk.ThinkingNone {
		b["reasoning"] = map[string]any{"effort": string(req.ThinkingLevel)}
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
func build(req sdk.Request) map[string]any { return BuildResponsesRequest(req) }
func BuildChatRequest(req sdk.Request) map[string]any {
	b := map[string]any{"model": req.Model, "stream": req.Stream}
	messages := make([]any, 0, len(req.Messages)+1)
	if req.SystemPrompt != "" {
		messages = append(messages, map[string]any{"role": "system", "content": req.SystemPrompt})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case sdk.RoleUser, sdk.RoleModel:
			content := ""
			for _, p := range m.Content {
				content += p.Text
			}
			if content != "" {
				role := "user"
				if m.Role == sdk.RoleModel {
					role = "assistant"
				}
				messages = append(messages, map[string]any{"role": role, "content": content})
			}
		case sdk.RoleToolCall:
			if m.ToolCall != nil {
				messages = append(messages, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": m.ToolCall.ID, "type": "function", "function": map[string]any{"name": m.ToolCall.Name, "arguments": m.ToolCall.Arguments}}}})
			}
		case sdk.RoleToolResult:
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
	if req.Temperature != nil {
		b["temperature"] = *req.Temperature
	}
	if req.MaxOutputTokens > 0 {
		b["max_tokens"] = req.MaxOutputTokens
	}
	addStreamUsage(b, req.Stream)
	return b
}
func buildChat(req sdk.Request) map[string]any { return BuildChatRequest(req) }
func ParseResponsesResponse(r ResponsesResponse) sdk.Response {
	out := sdk.Response{Provider: "openai", Model: r.Model, Usage: sdk.Usage{InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens, TotalTokens: r.Usage.TotalTokens, CacheReadTokens: r.Usage.InputDetails.Cached}, Cache: sdk.CacheInfo{Layer: "provider"}}
	out.Cache.Hit = out.Usage.CacheReadTokens > 0
	for _, item := range r.Output {
		switch item.Type {
		case "reasoning":
			for _, p := range item.Content {
				if p.Type == "reasoning_text" && p.Text != "" {
					out.Reasoning = &sdk.ReasoningState{ID: item.ID, Text: p.Text}
					break
				}
			}
		case "message":
			for _, p := range item.Content {
				if p.Text != "" {
					out.Content = append(out.Content, sdk.ContentPart{Type: sdk.ContentText, Text: p.Text})
				}
			}
		case "function_call":
			out.ToolCalls = append(out.ToolCalls, sdk.ToolCall{ID: item.CallID, Name: item.Name, Arguments: item.Arguments})
		}
	}
	out.FinishReason = r.Status
	return out
}
func parseResponse(r ResponsesResponse) sdk.Response { return ParseResponsesResponse(r) }
func ParseChatResponse(r ChatResponse) sdk.Response {
	out := sdk.Response{Provider: "openai", Model: r.Model, Usage: sdk.Usage{InputTokens: r.Usage.PromptTokens, OutputTokens: r.Usage.CompletionTokens, TotalTokens: r.Usage.TotalTokens, CacheReadTokens: r.Usage.PromptDetails.Cached}, Cache: sdk.CacheInfo{Layer: "provider"}}
	out.Cache.Hit = out.Usage.CacheReadTokens > 0
	if len(r.Choices) == 0 {
		return out
	}
	choice := r.Choices[0]
	if choice.Message.Content != "" {
		out.Content = append(out.Content, sdk.ContentPart{Type: sdk.ContentText, Text: choice.Message.Content})
	}
	for _, call := range choice.Message.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, sdk.ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
	}
	out.FinishReason = choice.FinishReason
	return out
}
func parseChatResponse(r ChatResponse) sdk.Response { return ParseChatResponse(r) }
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
func (c *Client) Generate(ctx context.Context, req sdk.Request) (sdk.Response, error) {
	var r ResponsesResponse
	err := internal.DoJSON(ctx, c.http(), http.MethodPost, c.BaseURL+"/responses", c.headers(), build(req), &r)
	if err == nil {
		return parseResponse(r), nil
	}
	if !isResponsesModelUnsupported(err) {
		return sdk.Response{}, err
	}
	var chat ChatResponse
	if chatErr := internal.DoJSON(ctx, c.http(), http.MethodPost, c.BaseURL+"/chat/completions", c.headers(), buildChat(req), &chat); chatErr != nil {
		return sdk.Response{}, chatErr
	}
	return parseChatResponse(chat), nil
}

// Stream streams an answer. Two dialects are in play: OpenAI's own Responses
// API and the chat/completions shape every compatible gateway speaks. Which one
// is tried first is decided by the endpoint — a custom base URL is a gateway, so
// it gets chat/completions — and the other one is still tried when the first
// fails before emitting anything, so nothing reaches the caller half-way.
func (c *Client) Stream(ctx context.Context, req sdk.Request) (<-chan sdk.Event, error) {
	req.Stream = true
	ch := make(chan sdk.Event, 16)
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
			ch <- sdk.Event{Type: sdk.EventError, Err: err}
			return
		}
		if fallbackErr := second(ctx, req, ch, &emitted); fallbackErr != nil {
			ch <- sdk.Event{Type: sdk.EventError, Err: fallbackErr}
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

func (c *Client) streamResponses(ctx context.Context, req sdk.Request, ch chan<- sdk.Event, emitted *int) error {
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
				ch <- sdk.Event{Type: sdk.EventText, Text: e.Delta}
			}
		case "ResponsesResponse.reasoning_summary_text.delta":
			if e.Delta != "" {
				*emitted++
				ch <- sdk.Event{Type: sdk.EventReasoning, Reasoning: &sdk.ReasoningState{Text: e.Delta}}
			}
		case "ResponsesResponse.function_call_arguments.done":
			if e.Item.CallID != "" {
				*emitted++
				ch <- sdk.Event{Type: sdk.EventToolCall, ToolCall: &sdk.ToolCall{ID: e.Item.CallID, Name: e.Item.Name, Arguments: e.Item.Arguments}}
			}
		case "ResponsesResponse.completed":
			*emitted++
			// The finished response carries the token counts, so a streamed turn
			// reports the same usage a non-streamed one does.
			usage := sdk.Usage{
				InputTokens:     e.Response.Usage.InputTokens,
				OutputTokens:    e.Response.Usage.OutputTokens,
				TotalTokens:     e.Response.Usage.TotalTokens,
				CacheReadTokens: e.Response.Usage.InputDetails.Cached,
			}
			finished := sdk.Response{
				Provider: "openai", Model: e.Response.Model, FinishReason: e.Response.Status,
				Usage: usage, Cache: sdk.CacheInfo{Layer: "provider", Hit: usage.CacheReadTokens > 0},
			}
			ch <- sdk.Event{Type: sdk.EventDone, Response: &finished}
		}
		return nil
	})
}

// streamChat reads the classic OpenAI-compatible SSE dialect: text arrives in
// choices[].delta.content, tool calls in choices[].delta.tool_calls.
func (c *Client) streamChat(ctx context.Context, req sdk.Request, ch chan<- sdk.Event, emitted *int) error {
	calls := map[int]*sdk.ToolCall{}
	var order []int
	// A gateway sends the token counts in a trailing chunk that carries only
	// `usage`, after the choices are done. It is kept here so the closing event
	// can carry the real numbers instead of an estimate.
	var usage sdk.Usage
	reason, toolTurn := "", false
	finish := func(reason string) sdk.Event {
		return sdk.Event{Type: sdk.EventDone, Response: &sdk.Response{
			Provider: "openai", FinishReason: reason,
			Usage: usage, Cache: sdk.CacheInfo{Layer: "provider", Hit: usage.CacheReadTokens > 0},
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
			usage = sdk.Usage{
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
			ch <- sdk.Event{Type: sdk.EventText, Text: choice.Delta.Content}
		}
		for _, part := range choice.Delta.ToolCalls {
			call, ok := calls[part.Index]
			if !ok {
				call = &sdk.ToolCall{}
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
				ch <- sdk.Event{Type: sdk.EventToolCall, ToolCall: &call}
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
