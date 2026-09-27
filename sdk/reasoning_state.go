package sdk

// ReasoningState preserves provider-native thinking/reasoning data across an
// agent tool loop. Providers that require their reasoning text to be echoed
// back can use this field without exposing it as normal model content.
type ReasoningState struct {
	ID   string `json:"id,omitempty"`
	Text string `json:"text,omitempty"`
}
