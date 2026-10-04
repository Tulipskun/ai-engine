package provider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// ---- from provider/types.go ----
type Role string

const (
	RoleUser       Role = "user"
	RoleModel      Role = "model"
	RoleToolCall   Role = "tool_call"
	RoleToolResult Role = "tool_result"
)

type ContentType string

const ContentText ContentType = "text"

type ContentPart struct {
	Type ContentType `json:"type"`
	Text string      `json:"text,omitempty"`
}
type Message struct {
	Role    Role          `json:"role"`
	Content []ContentPart `json:"content,omitempty"`
}
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema,omitempty"`
}
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type ToolResult struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}
type Turn struct {
	Role       Role            `json:"role"`
	Content    []ContentPart   `json:"content,omitempty"`
	ToolCall   *ToolCall       `json:"tool_call,omitempty"`
	ToolResult *ToolResult     `json:"tool_result,omitempty"`
	Reasoning  *ReasoningState `json:"reasoning,omitempty"`
}

type ThinkingLevel string

const (
	ThinkingNone   ThinkingLevel = "none"
	ThinkingLow    ThinkingLevel = "low"
	ThinkingMedium ThinkingLevel = "medium"
	ThinkingHigh   ThinkingLevel = "high"
)

// Instruction is one instruction file handed to the provider, the way the
// OpenCode client hands over AGENTS.md: the path it came from and the text.
// Only the OpenCode adapter renders it (as an "Instructions from:" block);
// other providers see the system prompt alone, as before.
type Instruction struct {
	Path string
	Text string
}

type Request struct {
	Provider        ProviderID    `json:"provider,omitempty"`
	SessionID       string        `json:"session_id,omitempty"`
	SystemPrompt    string        `json:"system_prompt,omitempty"`
	Instructions    []Instruction `json:"instructions,omitempty"`
	Messages        []Turn        `json:"messages,omitempty"`
	Tools           []Tool        `json:"tools,omitempty"`
	Model           string        `json:"model"`
	Temperature     *float64      `json:"temperature,omitempty"`
	ThinkingLevel   ThinkingLevel `json:"thinking_level,omitempty"`
	MaxOutputTokens int           `json:"max_output_tokens,omitempty"`
	// The rest of the knobs a provider accepts. Every one of them is optional
	// and stays nil when the user never set it, because an omitted knob lets the
	// provider apply its own default while a zero we invented is a real value the
	// model was asked to obey (CHANGE-077).
	TopP             *float64 `json:"top_p,omitempty"`
	TopK             *float64 `json:"top_k,omitempty"`
	StopSequences    []string `json:"stop_sequences,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	Seed             *int64   `json:"seed,omitempty"`
	Stream           bool     `json:"stream,omitempty"`
}

type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
	// ReasoningTokens are the output tokens a model spent thinking before it
	// wrote the answer, counted separately by OpenAI and Gemini. Anthropic
	// reports thinking time in milliseconds but never a token count, so this
	// stays 0 there rather than being invented.
	ReasoningTokens int `json:"reasoning_tokens"`
	// InputIncludesCache records how the provider counted the input. OpenAI
	// and Gemini report a prompt total that already contains the cached part;
	// Anthropic reports cache separately from input. Without this the phone
	// cannot tell "63701 total, 63424 of it cached" from "63701 fresh plus
	// 63424 cached", and the two do not look the same on purpose.
	InputIncludesCache bool `json:"input_includes_cache"`
}

// FreshInputTokens is the input that was not served from cache, which is the
// number a reader compares against the cache count.
func (u Usage) FreshInputTokens() int {
	if u.InputIncludesCache {
		if n := u.InputTokens - u.CacheReadTokens; n > 0 {
			return n
		}
		return 0
	}
	return u.InputTokens
}

// TotalInputTokens is the whole prompt however the provider chose to split it.
func (u Usage) TotalInputTokens() int {
	if u.InputIncludesCache {
		return u.InputTokens
	}
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

type CacheInfo struct {
	Hit   bool   `json:"hit"`
	Layer string `json:"layer,omitempty"`
}
type Response struct {
	Provider     string          `json:"provider"`
	Model        string          `json:"model"`
	Content      []ContentPart   `json:"content,omitempty"`
	ToolCalls    []ToolCall      `json:"tool_calls,omitempty"`
	Reasoning    *ReasoningState `json:"reasoning,omitempty"`
	FinishReason string          `json:"finish_reason,omitempty"`
	Usage        Usage           `json:"usage"`
	Cache        CacheInfo       `json:"cache"`
}

type EventType string

const (
	EventText      EventType = "text"
	EventToolCall  EventType = "tool_call"
	EventReasoning EventType = "reasoning"
	EventDone      EventType = "done"
	EventError     EventType = "error"
)

type Event struct {
	Type      EventType       `json:"type"`
	Text      string          `json:"text,omitempty"`
	ToolCall  *ToolCall       `json:"tool_call,omitempty"`
	Reasoning *ReasoningState `json:"reasoning,omitempty"`
	Response  *Response       `json:"response,omitempty"`
	Err       error           `json:"-"`
}

type Provider interface {
	Name() string
	Generate(context.Context, Request) (Response, error)
	Stream(context.Context, Request) (<-chan Event, error)
}
type ProviderID string
type AdapterID string

const (
	AdapterOpenAI    AdapterID = "openai"
	AdapterAnthropic AdapterID = "anthropic"
	AdapterGemini    AdapterID = "gemini"
	AdapterOpenCode  AdapterID = "opencode"
)

type Model struct {
	ID                  string `json:"id"`
	Name                string `json:"name,omitempty"`
	SupportsTools       bool   `json:"supports_tools"`
	SupportsThinking    bool   `json:"supports_thinking"`
	SupportsTemperature bool   `json:"supports_temperature"`
	SupportsStreaming   bool   `json:"supports_streaming"`
	// The remaining knobs, reported per model so the phone can hide what this
	// particular model would refuse (CHANGE-077).
	SupportsTopP             bool `json:"supports_top_p"`
	SupportsTopK             bool `json:"supports_top_k"`
	SupportsStopSequences    bool `json:"supports_stop_sequences"`
	SupportsPresencePenalty  bool `json:"supports_presence_penalty"`
	SupportsFrequencyPenalty bool `json:"supports_frequency_penalty"`
	SupportsSeed             bool `json:"supports_seed"`
}
type ProviderConfig struct {
	ID         ProviderID        `json:"id"`
	BaseURL    string            `json:"base_url"`
	Keys       *KeyPool          `json:"-"`
	Adapter    AdapterID         `json:"adapter"`
	RotateKeys bool              `json:"rotate_keys,omitempty"`
	FreeOnly   bool              `json:"free_only,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
}
type ModelRoute struct {
	Provider ProviderID `json:"provider"`
	Model    string     `json:"model"`
	Adapter  AdapterID  `json:"adapter"`
}
type SessionConfig struct {
	ID            string        `json:"id"`
	Provider      ProviderID    `json:"provider"`
	Model         string        `json:"model"`
	KeyIndex      int           `json:"key_index"`
	ThinkingLevel ThinkingLevel `json:"thinking_level,omitempty"`
	Temperature   *float64      `json:"temperature,omitempty"`
	AgentMode     AgentMode     `json:"agent_mode,omitempty"`
	Workspace     string        `json:"workspace,omitempty"`
	// Same optional knobs as Request, kept per session so a conversation can
	// come back the way it was asked to answer (CHANGE-077).
	TopP             *float64 `json:"top_p,omitempty"`
	TopK             *float64 `json:"top_k,omitempty"`
	StopSequences    []string `json:"stop_sequences,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	Seed             *int64   `json:"seed,omitempty"`
	// MaxOutputTokens is 0 for "no cap", which is also what leaves the limit to
	// the provider's own default (CHANGE-077).
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
}

// applyGeneration fills the knobs the caller left unset from the session's own
// settings. A value the turn carries itself always wins, so one turn can ask for
// something different without changing the conversation. This is the only place
// the two structs are reconciled, which is what keeps a newly added knob from
// being silently dropped on one of the paths (CHANGE-077).
//
// Everything is copied rather than pointed at, so a request can never reach back
// through one of these fields into the settings it was built from.
func (c SessionConfig) applyGeneration(req *Request) {
	if req == nil {
		return
	}
	if req.ThinkingLevel == "" {
		req.ThinkingLevel = c.ThinkingLevel
	}
	if req.Temperature == nil {
		req.Temperature = CloneFloat(c.Temperature)
	}
	if req.TopP == nil {
		req.TopP = CloneFloat(c.TopP)
	}
	if req.TopK == nil {
		req.TopK = CloneFloat(c.TopK)
	}
	if len(req.StopSequences) == 0 && len(c.StopSequences) > 0 {
		req.StopSequences = append([]string(nil), c.StopSequences...)
	}
	if req.PresencePenalty == nil {
		req.PresencePenalty = CloneFloat(c.PresencePenalty)
	}
	if req.FrequencyPenalty == nil {
		req.FrequencyPenalty = CloneFloat(c.FrequencyPenalty)
	}
	if req.Seed == nil {
		req.Seed = CloneInt64(c.Seed)
	}
	if req.MaxOutputTokens == 0 {
		req.MaxOutputTokens = c.MaxOutputTokens
	}
}

// Generation returns a copy of the session's knobs, for handing a worker the
// same generation settings its parent runs on.
func (c SessionConfig) Generation() GenerationSettings {
	return GenerationSettings{
		ThinkingLevel:    c.ThinkingLevel,
		Temperature:      c.Temperature,
		TopP:             c.TopP,
		TopK:             c.TopK,
		StopSequences:    c.StopSequences,
		PresencePenalty:  c.PresencePenalty,
		FrequencyPenalty: c.FrequencyPenalty,
		Seed:             c.Seed,
	}
}

// inheritGeneration copies the generation knobs onto c from a session the caller
// is about to spawn work for. A worker answers the same conversation as its
// parent, so it runs on the same knobs unless its own config says otherwise —
// and listing them here once is what stops a newly added knob from being left
// behind on the worker path (CHANGE-077).
func (c *SessionConfig) InheritGeneration(parent SessionConfig) {
	c.ThinkingLevel = parent.ThinkingLevel
	c.Temperature = CloneFloat(parent.Temperature)
	c.TopP = CloneFloat(parent.TopP)
	c.TopK = CloneFloat(parent.TopK)
	c.PresencePenalty = CloneFloat(parent.PresencePenalty)
	c.FrequencyPenalty = CloneFloat(parent.FrequencyPenalty)
	c.Seed = CloneInt64(parent.Seed)
	if parent.StopSequences != nil {
		c.StopSequences = append([]string(nil), parent.StopSequences...)
	} else {
		c.StopSequences = nil
	}
	if parent.MaxOutputTokens > 0 {
		c.MaxOutputTokens = parent.MaxOutputTokens
	}
}

// clone returns a copy that shares no pointer and no slice backing with c, so
// a caller holding one can never reach back into the session's own settings.
// Every knob is a pointer or a slice, so a shallow copy would alias them all.
func (c SessionConfig) Clone() SessionConfig {
	out := c
	out.Temperature = CloneFloat(c.Temperature)
	out.TopP = CloneFloat(c.TopP)
	out.TopK = CloneFloat(c.TopK)
	out.PresencePenalty = CloneFloat(c.PresencePenalty)
	out.FrequencyPenalty = CloneFloat(c.FrequencyPenalty)
	out.Seed = CloneInt64(c.Seed)
	if c.StopSequences != nil {
		out.StopSequences = append([]string(nil), c.StopSequences...)
	}
	return out
}

func CloneFloat(v *float64) *float64 {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

func CloneInt64(v *int64) *int64 {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

// GenerationSettings is the transport- and config-facing shape of the knobs a
// provider accepts. Pointers are how "not set" is told apart from zero.
type GenerationSettings struct {
	ThinkingLevel    ThinkingLevel `json:"thinking_level,omitempty"`
	Temperature      *float64      `json:"temperature,omitempty"`
	TopP             *float64      `json:"top_p,omitempty"`
	TopK             *float64      `json:"top_k,omitempty"`
	StopSequences    []string      `json:"stop_sequences,omitempty"`
	PresencePenalty  *float64      `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64      `json:"frequency_penalty,omitempty"`
	Seed             *int64        `json:"seed,omitempty"`
	// MaxOutputTokens is 0 for "no cap", which leaves the limit to the provider.
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
}

// AgentMode selects how a session answers: AgentModeMain plans through the
// Main Agent (planning prompt plus orchestration and read-only context tools,
// no write/exec tools) while AgentModeSub answers as a worker with the full
// execution tool set.
// The zero value behaves as AgentModeMain so stored sessions keep working.
type AgentMode string

const (
	AgentModeMain AgentMode = "main"
	AgentModeSub  AgentMode = "sub"
)

type HTTPStatusError interface {
	error
	HTTPStatusCode() int
}
type RetryAfterError interface {
	error
	RetryAfter() time.Duration
}

type ModelLister interface {
	ListModels(context.Context, string) ([]Model, error)
}
type EndpointProvider interface{ WithBaseURL(string) Provider }

// ---- from provider/keys.go ----
type KeyPool struct {
	mu      sync.Mutex
	keys    []string
	current int
}

func NewKeyPool(keys ...string) *KeyPool {
	cleaned := make([]string, 0, len(keys))
	for _, k := range keys {
		if k = strings.TrimSpace(k); k != "" {
			cleaned = append(cleaned, k)
		}
	}
	return &KeyPool{keys: cleaned}
}
func (p *KeyPool) Len() int                 { p.mu.Lock(); defer p.mu.Unlock(); return len(p.keys) }
func (p *KeyPool) Current() (string, error) { return p.At(p.IndexOfCurrent()) }
func (p *KeyPool) At(index int) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.keys) == 0 {
		return "", errors.New("sdk: no API keys configured")
	}
	if index < 0 || index >= len(p.keys) {
		return "", errors.New("sdk: API key index out of range")
	}
	return p.keys[index], nil
}
func (p *KeyPool) IndexOfCurrent() int { p.mu.Lock(); defer p.mu.Unlock(); return p.current }
func (p *KeyPool) Rotate() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.keys) == 0 {
		return "", errors.New("sdk: no API keys configured")
	}
	p.current = (p.current + 1) % len(p.keys)
	return p.keys[p.current], nil
}

// ---- from provider/capabilities.go ----
// No provider publishes what a model accepts. There is no capability endpoint to
// ask, so what is known has to be carried here, and the honest way to say so is
// to start from what the adapter can express and narrow it per model family
// (CHANGE-077, REQ-049(3)).
//
// Two rules cause most of the confusion and are encoded as such:
//
//   - A model that thinks before it answers takes only the default sampling
//     parameters. Asking for a temperature, a top_p or either penalty is refused
//     by OpenAI on chat/completions, and by Anthropic whenever extended thinking
//     is enabled. So a reasoning family does not support temperature.
//   - A model that cannot think does not support a reasoning level. Reporting it
//     as capable is how a setting gets accepted here and then dropped there.

// adapterKnobs is what each adapter's translation is able to express at all,
// before any model is considered. It is here rather than in the four adapters so
// there is one answer to compare against, and so a knob cannot be claimed by one
// adapter and quietly missing from another.
var adapterKnobs = map[AdapterID]Model{
	AdapterOpenAI: {
		SupportsTopP: true, SupportsStopSequences: true,
		SupportsPresencePenalty: true, SupportsFrequencyPenalty: true, SupportsSeed: true,
	},
	AdapterOpenCode: {
		SupportsTopP: true, SupportsStopSequences: true,
		SupportsPresencePenalty: true, SupportsFrequencyPenalty: true, SupportsSeed: true,
	},
	AdapterAnthropic: {
		SupportsTopP: true, SupportsTopK: true, SupportsStopSequences: true,
	},
	AdapterGemini: {
		SupportsTopP: true, SupportsTopK: true, SupportsStopSequences: true,
	},
}

// ModelCapabilities narrows what an adapter can express down to what one model
// accepts. The base comes from the adapter, because an adapter is the honest
// answer to "what can this integration say at all"; the rules below only remove
// what this particular model would refuse.
func ModelCapabilities(adapter AdapterID, base Model) Model {
	baseline, ok := adapterKnobs[adapter]
	if !ok {
		baseline = adapterKnobs[AdapterOpenAI]
	}
	base.SupportsTopP = baseline.SupportsTopP
	base.SupportsTopK = baseline.SupportsTopK
	base.SupportsStopSequences = baseline.SupportsStopSequences
	base.SupportsPresencePenalty = baseline.SupportsPresencePenalty
	base.SupportsFrequencyPenalty = baseline.SupportsFrequencyPenalty
	base.SupportsSeed = baseline.SupportsSeed

	base.SupportsThinking = base.SupportsThinking || thinksByFamily(base.ID)
	if base.SupportsThinking {
		// Reasoning pins the sampling knobs, so a temperature is not one of the
		// things this model can be told.
		base.SupportsTemperature = false
	}
	base.SupportsTools = base.SupportsTools || base.SupportsThinking
	return base
}

// thinksByFamily names the model families that expose a reasoning control.
// Matching is on the part of the id before any date or size suffix, because
// providers append those themselves: "gpt-5-2025-08-07" is still a gpt-5.
func thinksByFamily(modelID string) bool {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" {
		return false
	}
	for _, prefix := range []string{
		"o1", "o3", "o4", // OpenAI reasoning families
		"gpt-5", "gpt-oss", // and the current GPT line
		"claude-3-7", "claude-4", // Anthropic extended thinking arrived at 3.7
		"claude-opus-4", "claude-sonnet-4", "claude-haiku-4",
		"gemini-2.5", // Gemini thinking budget
		"deepseek-r1", "deepseek-reasoner", "qwq",
	} {
		if id == prefix || strings.HasPrefix(id, prefix+"-") {
			return true
		}
	}
	return false
}

// ---- from provider/validate.go ----
// The bounds a provider will accept, in one place. The session setters and the
// config loader both go through here, so a value hand-edited into config is held
// to the same rule as one that arrived over the wire, and the two cannot drift
// apart (CHANGE-077).

const (
	MinTemperature = 0.0
	MaxTemperature = 2.0

	MinTopP = 0.0
	MaxTopP = 1.0

	MinTopK = 0.0
	MaxTopK = 1.0

	MinPenalty = -2.0
	MaxPenalty = 2.0
)

// ValidateTemperature rejects a value outside the range, and rejects NaN and
// Inf because both survive a range comparison and would reach the body as
// something a provider cannot read.
func ValidateTemperature(v float64) error {
	return checkRange("temperature", v, MinTemperature, MaxTemperature)
}

// ValidateTopP rejects a nucleus value outside 0..1.
func ValidateTopP(v float64) error { return checkRange("top_p", v, MinTopP, MaxTopP) }

// ValidateTopK rejects a top-k outside 0..1.
func ValidateTopK(v float64) error { return checkRange("top_k", v, MinTopK, MaxTopK) }

// ValidatePenalty rejects a presence or frequency penalty outside -2..2.
func ValidatePenalty(name string, v float64) error {
	return checkRange(name, v, MinPenalty, MaxPenalty)
}

// ValidateMaxOutputTokens rejects a negative cap. Zero is allowed and means the
// provider keeps its own limit.
func ValidateMaxOutputTokens(v int) error {
	if v < 0 {
		return errors.New("sdk: max output tokens must not be negative")
	}
	return nil
}

// ValidateThinkingLevel rejects a level no provider is asked for.
func ValidateThinkingLevel(level ThinkingLevel) error {
	if !validThinkingLevel(level) {
		return fmt.Errorf("sdk: invalid thinking level %q", level)
	}
	return nil
}

// checkRange rejects a value no provider would accept.
func checkRange(name string, v, lo, hi float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("sdk: %s must be a number", name)
	}
	if v < lo || v > hi {
		return fmt.Errorf("sdk: %s must be between %g and %g, got %g", name, lo, hi, v)
	}
	return nil
}

func validThinkingLevel(level ThinkingLevel) bool {
	switch level {
	case ThinkingNone, ThinkingLow, ThinkingMedium, ThinkingHigh:
		return true
	default:
		return false
	}
}

// ---- from provider/reasoning.go ----
// ReasoningState preserves provider-native thinking/reasoning data across an
// agent tool loop. Providers that require their reasoning text to be echoed
// back can use this field without exposing it as normal model content.
type ReasoningState struct {
	ID   string `json:"id,omitempty"`
	Text string `json:"text,omitempty"`
}
