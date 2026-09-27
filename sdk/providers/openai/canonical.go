package openai

import (
	"github.com/Tulipskun/ai-engine/sdk"
)

// Canonical OpenAI Responses wire is the central interface.
//
// Flow:
//
//	sdk.Request --BuildResponsesRequest--> OpenAI Responses map (canonical)
//	    --anthropic.BuildFromOpenAI / gemini.BuildFromOpenAI--> provider-native payload
//
// Native provider responses are converted back through the same OpenAI
// Responses shape (see anthropic.ToOpenAIResponse / gemini.ToOpenAIResponse)
// before becoming sdk.Response via ParseResponsesResponse.
//
// Chat Completions (BuildChatRequest/ParseChatResponse) is kept only as a
// fallback for providers that reject /responses.

// Request is the canonical OpenAI Responses request shape used as the
// intermediate representation between sdk.Request and provider-native payloads.
type Request struct {
	Model           string         `json:"model"`
	Instructions    string         `json:"instructions,omitempty"`
	Input           []any          `json:"input,omitempty"`
	Tools           []any          `json:"tools,omitempty"`
	Temperature     *float64       `json:"temperature,omitempty"`
	MaxOutputTokens int            `json:"max_output_tokens,omitempty"`
	Reasoning       map[string]any `json:"reasoning,omitempty"`
	Stream          bool           `json:"stream,omitempty"`
}

// Canonical converts an sdk.Request into the canonical OpenAI struct form.
// BuildResponsesRequest remains the map-based wire form used for HTTP.
func Canonical(req sdk.Request) Request {
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
	if openAIReq == nil {
		return 0, false
	}
	switch v := openAIReq["temperature"].(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	}
	return 0, false
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

// ResponsesResponseFromParts builds an sdk.Response through the canonical
// OpenAI Responses output shape: message content + function_call items +
// optional reasoning. Native adapters (anthropic/gemini) convert their wire
// ResponsesResponse into these parts first, then delegate here so sdk.Response
// construction stays in one place.
func ResponsesResponseFromParts(model, status string, texts []string, toolCalls []sdk.ToolCall, reasoning *sdk.ReasoningState, usage sdk.Usage) sdk.Response {
	r := ResponsesResponse{Model: model, Status: status}
	r.Usage.InputTokens = usage.InputTokens
	r.Usage.OutputTokens = usage.OutputTokens
	r.Usage.TotalTokens = usage.TotalTokens
	r.Usage.InputDetails.Cached = usage.CacheReadTokens
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
