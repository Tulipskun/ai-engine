package sdk

import (
	"context"
	"time"
)

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
	Stream          bool          `json:"stream,omitempty"`
}

type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
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
	ProviderOpenRouter ProviderID = "openrouter"
	ProviderOpenCode   ProviderID = "opencode"
	AdapterOpenAI      AdapterID  = "openai"
	AdapterAnthropic   AdapterID  = "anthropic"
	AdapterGemini      AdapterID  = "gemini"
	AdapterOpenCode    AdapterID  = "opencode"
)

type Model struct {
	ID                  string `json:"id"`
	Name                string `json:"name,omitempty"`
	SupportsTools       bool   `json:"supports_tools"`
	SupportsThinking    bool   `json:"supports_thinking"`
	SupportsTemperature bool   `json:"supports_temperature"`
	SupportsStreaming   bool   `json:"supports_streaming"`
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

type RetryPolicy struct {
	MaxAttempts    int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 3, InitialBackoff: 3 * time.Second, MaxBackoff: maxRetryCooldown}
}

type HTTPStatusError interface {
	error
	HTTPStatusCode() int
}
type RetryAfterError interface {
	error
	RetryAfter() time.Duration
}

func (r Request) RequestProvider() ProviderID { return ProviderID(r.Provider) }

type ModelLister interface {
	ListModels(context.Context, string) ([]Model, error)
}
type EndpointProvider interface{ WithBaseURL(string) Provider }
