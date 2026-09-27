package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Tulipskun/ai-engine/sdk"
	"github.com/Tulipskun/ai-engine/sdk/providers/internal"
	"github.com/Tulipskun/ai-engine/sdk/providers/openai"
)

type Client struct {
	BaseURL string
	APIKey  string
	Headers map[string]string
	HTTP    *http.Client
}

func New(apiKey string) *Client {
	return &Client{BaseURL: "https://generativelanguage.googleapis.com/v1beta", APIKey: apiKey, HTTP: http.DefaultClient}
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
	key := apiKey
	if key == "" {
		key = c.APIKey
	}
	u := c.BaseURL + "/models"
	if key != "" {
		u += "?key=" + url.QueryEscape(key)
	}
	var r struct {
		Models []struct {
			Name        string   `json:"name"`
			DisplayName string   `json:"displayName"`
			Supported   []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := internal.DoJSON(ctx, c.http(), http.MethodGet, u, nil, nil, &r); err != nil {
		return nil, err
	}
	models := make([]sdk.Model, 0, len(r.Models))
	for _, item := range r.Models {
		id := strings.TrimPrefix(item.Name, "models/")
		if id == "" {
			continue
		}
		streaming := false
		for _, method := range item.Supported {
			if method == "streamGenerateContent" {
				streaming = true
				break
			}
		}
		name := item.DisplayName
		if name == "" {
			name = id
		}
		models = append(models, sdk.Model{ID: id, Name: name, SupportsStreaming: streaming, SupportsTools: true, SupportsThinking: true, SupportsTemperature: true})
	}
	return models, nil
}
func (c *Client) Name() string { return "gemini" }

// build converts via the central OpenAI Responses interface:
// sdk.Request -> OpenAI canonical -> Gemini native.
func build(req sdk.Request) map[string]any {
	return BuildFromOpenAI(openai.BuildResponsesRequest(req))
}

// BuildFromOpenAI translates a canonical OpenAI Responses request map
// (see openai.BuildResponsesRequest) into a Gemini generateContent payload.
func BuildFromOpenAI(openAIReq map[string]any) map[string]any {
	b := map[string]any{}
	if sys := openai.InstructionsOf(openAIReq); sys != "" {
		b["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": sys}}}
	}
	var contents []any
	toolNames := map[string]string{}
	for _, item := range openai.InputItemsOf(openAIReq) {
		m := openai.ItemMap(item)
		if m == nil {
			continue
		}
		switch m["type"] {
		case "message":
			role, _ := m["role"].(string)
			gemRole := "user"
			if role == "assistant" {
				gemRole = "model"
			}
			if text, _ := m["content"].(string); text != "" {
				contents = append(contents, map[string]any{"role": gemRole, "parts": []any{map[string]any{"text": text}}})
			}
		case "reasoning":
			// Gemini thought parts are response-only; nothing to replay from history text.
			continue
		case "function_call":
			callID, _ := m["call_id"].(string)
			name, _ := m["name"].(string)
			toolNames[callID] = name
			argsStr, _ := m["arguments"].(string)
			var args any
			_ = json.Unmarshal([]byte(argsStr), &args)
			contents = append(contents, map[string]any{"role": "model", "parts": []any{map[string]any{"functionCall": map[string]any{"name": name, "args": args}}}})
		case "function_call_output":
			callID, _ := m["call_id"].(string)
			output, _ := m["output"].(string)
			name := toolNames[callID]
			if name == "" {
				name = callID
			}
			contents = append(contents, map[string]any{"role": "user", "parts": []any{map[string]any{"functionResponse": map[string]any{"name": name, "response": map[string]any{"content": output}}}}})
		}
	}
	b["contents"] = contents
	cfg := map[string]any{}
	if temp, ok := openai.TemperatureOf(openAIReq); ok {
		cfg["temperature"] = temp
	}
	if maxTokens := openai.MaxOutputTokensOf(openAIReq); maxTokens > 0 {
		cfg["maxOutputTokens"] = maxTokens
	}
	if effort := openai.ReasoningEffortOf(openAIReq); effort != "" && effort != string(sdk.ThinkingNone) {
		cfg["thinkingConfig"] = map[string]any{"thinkingLevel": effort}
	}
	if len(cfg) > 0 {
		b["generationConfig"] = cfg
	}
	var declarations []any
	for _, t := range openai.ToolsOf(openAIReq) {
		tm := openai.ItemMap(t)
		if tm == nil {
			continue
		}
		name, _ := tm["name"].(string)
		desc, _ := tm["description"].(string)
		declarations = append(declarations, map[string]any{"name": name, "description": desc, "parameters": tm["parameters"]})
	}
	if len(declarations) > 0 {
		b["tools"] = []any{map[string]any{"functionDeclarations": declarations}}
	}
	return b
}

type functionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}
type part struct {
	Text         string        `json:"text"`
	FunctionCall *functionCall `json:"functionCall"`
	Thought      bool          `json:"thought"`
}
type candidate struct {
	Content struct {
		Parts []part `json:"parts"`
	} `json:"content"`
	FinishReason string `json:"finishReason"`
}
type response struct {
	Candidates []candidate `json:"candidates"`
	Usage      struct {
		Prompt int `json:"promptTokenCount"`
		Output int `json:"candidatesTokenCount"`
		Total  int `json:"totalTokenCount"`
		Cached int `json:"cachedContentTokenCount"`
	} `json:"usageMetadata"`
}

func (c *Client) endpoint(model string, stream bool) string {
	method := "generateContent"
	if stream {
		method = "streamGenerateContent"
	}
	u := fmt.Sprintf("%s/models/%s:%s", c.BaseURL, url.PathEscape(model), method)
	if stream {
		u += "?alt=sse"
	}
	return u
}
func (c *Client) Generate(ctx context.Context, req sdk.Request) (sdk.Response, error) {
	var r response
	if err := internal.DoJSON(ctx, c.http(), http.MethodPost, c.endpoint(req.Model, false), c.headers(), build(req), &r); err != nil {
		return sdk.Response{}, err
	}
	return ToOpenAIResponse(r, req.Model), nil
}
func parse(r response, model string) sdk.Response { return ToOpenAIResponse(r, model) }
func (c *Client) Stream(ctx context.Context, req sdk.Request) (<-chan sdk.Event, error) {
	req.Stream = true
	ch := make(chan sdk.Event, 16)
	go func() {
		defer close(ch)
		err := internal.SSE(ctx, c.http(), http.MethodPost, c.endpoint(req.Model, true), c.headers(), build(req), func(data []byte) error {
			var r response
			if json.Unmarshal(data, &r) != nil {
				return nil
			}
			if len(r.Candidates) == 0 {
				return nil
			}
			for _, p := range r.Candidates[0].Content.Parts {
				if p.Text != "" {
					if p.Thought {
						ch <- sdk.Event{Type: sdk.EventReasoning, Reasoning: &sdk.ReasoningState{Text: p.Text}}
					} else {
						ch <- sdk.Event{Type: sdk.EventText, Text: p.Text}
					}
				}
				if p.FunctionCall != nil {
					a, _ := json.Marshal(p.FunctionCall.Args)
					ch <- sdk.Event{Type: sdk.EventToolCall, ToolCall: &sdk.ToolCall{ID: fmt.Sprintf("gemini-%s", p.FunctionCall.Name), Name: p.FunctionCall.Name, Arguments: string(a)}}
				}
			}
			if r.Candidates[0].FinishReason != "" {
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
func (c *Client) headers() map[string]string {
	h := map[string]string{"x-goog-api-key": c.APIKey}
	for k, v := range c.Headers {
		if v == "" || strings.EqualFold(k, "x-goog-api-key") {
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

// ToOpenAIResponse converts a native Gemini response into sdk.Response
// through the central OpenAI Responses shape.
func ToOpenAIResponse(r response, model string) sdk.Response {
	usage := sdk.Usage{InputTokens: r.Usage.Prompt, OutputTokens: r.Usage.Output, TotalTokens: r.Usage.Total, CacheReadTokens: r.Usage.Cached}
	if len(r.Candidates) == 0 {
		out := openai.ResponsesResponseFromParts(model, "", nil, nil, nil, usage)
		out.Provider = "gemini"
		out.Cache = sdk.CacheInfo{Layer: "provider", Hit: usage.CacheReadTokens > 0}
		return out
	}
	var texts []string
	var calls []sdk.ToolCall
	var reasoning *sdk.ReasoningState
	for _, p := range r.Candidates[0].Content.Parts {
		if p.Text != "" {
			if p.Thought {
				if reasoning == nil {
					reasoning = &sdk.ReasoningState{}
				}
				reasoning.Text += p.Text
			} else {
				texts = append(texts, p.Text)
			}
		}
		if p.FunctionCall != nil {
			a, _ := json.Marshal(p.FunctionCall.Args)
			calls = append(calls, sdk.ToolCall{ID: fmt.Sprintf("gemini-%s-%d", p.FunctionCall.Name, len(calls)+1), Name: p.FunctionCall.Name, Arguments: string(a)})
		}
	}
	out := openai.ResponsesResponseFromParts(model, r.Candidates[0].FinishReason, texts, calls, reasoning, usage)
	out.Provider = "gemini"
	out.Cache = sdk.CacheInfo{Layer: "provider", Hit: usage.CacheReadTokens > 0}
	return out
}
func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}
