package openai

import (
	"ai-engine/provider"
)

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
