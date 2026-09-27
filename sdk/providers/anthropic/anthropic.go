package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Tulipskun/ai-engine/sdk"
	"github.com/Tulipskun/ai-engine/sdk/providers/internal"
	"github.com/Tulipskun/ai-engine/sdk/providers/openai"
)

type Client struct {
	BaseURL, APIKey, APIVersion string
	Headers                     map[string]string
	HTTP                        *http.Client
}

func New(apiKey string) *Client {
	return &Client{BaseURL: "https://api.anthropic.com/v1", APIKey: apiKey, APIVersion: "2023-06-01", HTTP: http.DefaultClient}
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
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := internal.DoJSON(ctx, c.http(), http.MethodGet, c.BaseURL+"/models", c.headers(), nil, &r); err != nil {
		return nil, err
	}
	models := make([]sdk.Model, 0, len(r.Data))
	for _, item := range r.Data {
		if item.ID == "" {
			continue
		}
		name := item.DisplayName
		if name == "" {
			name = item.ID
		}
		models = append(models, sdk.Model{ID: item.ID, Name: name, SupportsStreaming: true, SupportsTools: true, SupportsTemperature: true})
	}
	return models, nil
}
func (c *Client) Name() string { return "anthropic" }

// build converts via the central OpenAI Responses interface:
// sdk.Request -> OpenAI canonical -> Anthropic native.
func build(req sdk.Request) map[string]any {
	return BuildFromOpenAI(openai.BuildResponsesRequest(req))
}

// BuildFromOpenAI translates a canonical OpenAI Responses request map
// (see openai.BuildResponsesRequest) into an Anthropic /messages payload.
func BuildFromOpenAI(openAIReq map[string]any) map[string]any {
	model := openai.ModelOf(openAIReq)
	maxTokens := openai.MaxOutputTokensOf(openAIReq)
	b := map[string]any{"model": model, "max_tokens": maxTokens}
	if sys := openai.InstructionsOf(openAIReq); sys != "" {
		b["system"] = sys
	}
	if temp, ok := openai.TemperatureOf(openAIReq); ok {
		b["temperature"] = temp
	}
	if effort := openai.ReasoningEffortOf(openAIReq); effort != "" && effort != string(sdk.ThinkingNone) {
		bud := map[string]int{"low": 2048, "medium": 4096, "high": 8192}[effort]
		b["thinking"] = map[string]any{"type": "enabled", "budget_tokens": bud}
		if maxTokens <= bud {
			b["max_tokens"] = bud + max(1024, maxTokens)
		}
	}
	var msgs []any
	for _, item := range openai.InputItemsOf(openAIReq) {
		m := openai.ItemMap(item)
		if m == nil {
			continue
		}
		switch m["type"] {
		case "message":
			role, _ := m["role"].(string)
			if role == "assistant" {
				// Canonical uses "assistant"; Anthropic native is also "assistant".
			} else {
				role = "user"
			}
			text, _ := m["content"].(string)
			msgs = append(msgs, map[string]any{"role": role, "content": text})
		case "reasoning":
			// Anthropic history carries thinking at request level; nothing to replay per-message.
			continue
		case "function_call":
			callID, _ := m["call_id"].(string)
			name, _ := m["name"].(string)
			argsStr, _ := m["arguments"].(string)
			var args any
			_ = json.Unmarshal([]byte(argsStr), &args)
			msgs = append(msgs, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": callID, "name": name, "input": args}}})
		case "function_call_output":
			callID, _ := m["call_id"].(string)
			output, _ := m["output"].(string)
			msgs = append(msgs, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": callID, "content": output}}})
		}
	}
	b["messages"] = msgs
	var tools []any
	for _, t := range openai.ToolsOf(openAIReq) {
		tm := openai.ItemMap(t)
		if tm == nil {
			continue
		}
		name, _ := tm["name"].(string)
		desc, _ := tm["description"].(string)
		tools = append(tools, map[string]any{"name": name, "description": desc, "input_schema": tm["parameters"]})
	}
	if len(tools) > 0 {
		b["tools"] = tools
	}
	if stream, _ := openAIReq["stream"].(bool); stream {
		b["stream"] = true
	}
	return b
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

type response struct {
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	Content    []struct {
		Type, Text, ID, Name string
		Input                json.RawMessage `json:"input"`
	} `json:"content"`
	Usage struct {
		Input         int `json:"input_tokens"`
		OutputTokens  int `json:"output_tokens"`
		CacheRead     int `json:"cache_read_input_tokens"`
		CacheCreation int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

func (c *Client) headers() map[string]string {
	h := map[string]string{"x-api-key": c.APIKey, "anthropic-version": c.APIVersion}
	for k, v := range c.Headers {
		if v == "" || strings.EqualFold(k, "x-api-key") {
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
func (c *Client) Generate(ctx context.Context, req sdk.Request) (sdk.Response, error) {
	var r response
	if err := internal.DoJSON(ctx, c.http(), http.MethodPost, c.BaseURL+"/messages", c.headers(), build(req), &r); err != nil {
		return sdk.Response{}, err
	}
	return ToOpenAIResponse(r), nil
}
func (c *Client) Stream(ctx context.Context, req sdk.Request) (<-chan sdk.Event, error) {
	req.Stream = true
	ch := make(chan sdk.Event, 16)
	go func() {
		defer close(ch)
		type toolState struct{ id, name, args string }
		tools := map[int]*toolState{}
		var reasoningID string
		err := internal.SSE(ctx, c.http(), http.MethodPost, c.BaseURL+"/messages", c.headers(), build(req), func(data []byte) error {
			var e struct {
				Type  string `json:"type"`
				Index int    `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					Thinking    string `json:"thinking"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
				ContentBlock struct {
					Type     string `json:"type"`
					ID       string `json:"id"`
					Name     string `json:"name"`
					Thinking string `json:"thinking"`
				} `json:"content_block"`
			}
			if json.Unmarshal(data, &e) != nil {
				return nil
			}
			switch e.Type {
			case "content_block_start":
				switch e.ContentBlock.Type {
				case "tool_use":
					tools[e.Index] = &toolState{id: e.ContentBlock.ID, name: e.ContentBlock.Name}
				case "thinking":
					reasoningID = e.ContentBlock.ID
					if e.ContentBlock.Thinking != "" {
						ch <- sdk.Event{Type: sdk.EventReasoning, Reasoning: &sdk.ReasoningState{ID: reasoningID, Text: e.ContentBlock.Thinking}}
					}
				}
			case "content_block_delta":
				switch e.Delta.Type {
				case "text_delta":
					if e.Delta.Text != "" {
						ch <- sdk.Event{Type: sdk.EventText, Text: e.Delta.Text}
					}
				case "thinking_delta":
					if e.Delta.Thinking != "" {
						ch <- sdk.Event{Type: sdk.EventReasoning, Reasoning: &sdk.ReasoningState{ID: reasoningID, Text: e.Delta.Thinking}}
					}
				case "input_json_delta":
					if state := tools[e.Index]; state != nil {
						state.args += e.Delta.PartialJSON
					}
				}
			case "content_block_stop":
				if state := tools[e.Index]; state != nil {
					ch <- sdk.Event{Type: sdk.EventToolCall, ToolCall: &sdk.ToolCall{ID: state.id, Name: state.name, Arguments: state.args}}
					delete(tools, e.Index)
				}
			case "message_stop":
				ch <- sdk.Event{Type: sdk.EventDone}
			}
			return nil
		})
		if err != nil {
			ch <- sdk.Event{Type: sdk.EventError, Err: err}
		}
	}()
	return ch, nil
}
func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// ToOpenAIResponse converts a native Anthropic response into sdk.Response
// through the central OpenAI Responses shape.
func ToOpenAIResponse(r response) sdk.Response {
	var texts []string
	var calls []sdk.ToolCall
	for _, p := range r.Content {
		switch p.Type {
		case "text":
			texts = append(texts, p.Text)
		case "tool_use":
			calls = append(calls, sdk.ToolCall{ID: p.ID, Name: p.Name, Arguments: string(p.Input)})
		}
	}
	usage := sdk.Usage{InputTokens: r.Usage.Input, OutputTokens: r.Usage.OutputTokens, TotalTokens: r.Usage.Input + r.Usage.OutputTokens, CacheReadTokens: r.Usage.CacheRead, CacheWriteTokens: r.Usage.CacheCreation}
	out := openai.ResponsesResponseFromParts(r.Model, r.StopReason, texts, calls, nil, usage)
	out.Provider = "anthropic"
	out.Cache = sdk.CacheInfo{Layer: "provider", Hit: usage.CacheReadTokens > 0}
	return out
}
