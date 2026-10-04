package gateway

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// The bounds a provider accepts. These mirror the ones the SDK enforces, and they
// live here as well because the transport must be able to refuse a bad request
// before it writes anything, and the SDK must be able to refuse it without the
// transport knowing about the SDK (CHANGE-077, REQ-049(1)).
const (
	minTemperature = 0.0
	maxTemperature = 2.0

	minTopP = 0.0
	maxTopP = 1.0

	minTopK = 0.0
	maxTopK = 1.0

	minPenalty = -2.0
	maxPenalty = 2.0
)

// ValidateGenerationSettings checks every knob that was sent. A field left out is
// not checked and not stored, which is how a caller clears a setting: send it
// null, and the stored value goes away.
func ValidateGenerationSettings(g GenerationSettings) error {
	if g.Temperature != nil {
		if err := inRange("temperature", *g.Temperature, minTemperature, maxTemperature); err != nil {
			return err
		}
	}
	if g.TopP != nil {
		if err := inRange("top_p", *g.TopP, minTopP, maxTopP); err != nil {
			return err
		}
	}
	if g.TopK != nil {
		if err := inRange("top_k", *g.TopK, minTopK, maxTopK); err != nil {
			return err
		}
	}
	if g.PresencePenalty != nil {
		if err := inRange("presence_penalty", *g.PresencePenalty, minPenalty, maxPenalty); err != nil {
			return err
		}
	}
	if g.FrequencyPenalty != nil {
		if err := inRange("frequency_penalty", *g.FrequencyPenalty, minPenalty, maxPenalty); err != nil {
			return err
		}
	}
	if g.MaxOutputTokens < 0 {
		return errors.New("max_output_tokens must not be negative")
	}
	if level := strings.TrimSpace(g.ThinkingLevel); level != "" {
		switch level {
		case "none", "low", "medium", "high":
		default:
			return fmt.Errorf("thinking_level must be none, low, medium or high, got %q", level)
		}
	}
	return nil
}

// IsEmpty reports whether nothing was sent, which is how a caller asks to leave
// the stored settings alone rather than clear them.
func (g GenerationSettings) IsEmpty() bool {
	return strings.TrimSpace(g.ThinkingLevel) == "" && g.Temperature == nil &&
		g.TopP == nil && g.TopK == nil && len(g.StopSequences) == 0 &&
		g.PresencePenalty == nil && g.FrequencyPenalty == nil && g.Seed == nil &&
		g.MaxOutputTokens == 0
}

func inRange(name string, v, lo, hi float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("%s must be a number", name)
	}
	if v < lo || v > hi {
		return fmt.Errorf("%s must be between %g and %g", name, lo, hi)
	}
	return nil
}

// MergeKnobNames lists every name Merge understands, so a caller can refuse one
// it does not rather than ignoring it in silence.
func MergeKnobNames() []string {
	return []string{
		"thinking_level", "temperature", "top_p", "top_k", "stop_sequences",
		"presence_penalty", "frequency_penalty", "seed", "max_output_tokens",
	}
}

// ValidateCleared refuses a knob name that would otherwise be silently dropped.
func ValidateCleared(lists ...[]string) error {
	known := map[string]bool{}
	for _, name := range MergeKnobNames() {
		known[name] = true
	}
	for _, list := range lists {
		for _, name := range list {
			if !known[name] {
				return fmt.Errorf("clear_knobs: %q is not a setting", name)
			}
		}
	}
	return nil
}

// Merge writes the knobs the caller actually sent over a stored set. A field that
// was not sent keeps the stored value; a field named in cleared is removed. This
// is what keeps "not mentioned" and "delete it" from being the same request
// (CHANGE-077).
func (g GenerationSettings) Merge(stored GenerationSettings, cleared []string) GenerationSettings {
	out := stored
	if g.ThinkingLevel != "" {
		out.ThinkingLevel = g.ThinkingLevel
	}
	if g.Temperature != nil {
		out.Temperature = g.Temperature
	}
	if g.TopP != nil {
		out.TopP = g.TopP
	}
	if g.TopK != nil {
		out.TopK = g.TopK
	}
	if len(g.StopSequences) > 0 {
		out.StopSequences = g.StopSequences
	}
	if g.PresencePenalty != nil {
		out.PresencePenalty = g.PresencePenalty
	}
	if g.FrequencyPenalty != nil {
		out.FrequencyPenalty = g.FrequencyPenalty
	}
	if g.Seed != nil {
		out.Seed = g.Seed
	}
	if g.MaxOutputTokens > 0 {
		out.MaxOutputTokens = g.MaxOutputTokens
	}
	for _, knob := range cleared {
		switch knob {
		case "thinking_level":
			out.ThinkingLevel = ""
		case "temperature":
			out.Temperature = nil
		case "top_p":
			out.TopP = nil
		case "top_k":
			out.TopK = nil
		case "stop_sequences":
			out.StopSequences = nil
		case "presence_penalty":
			out.PresencePenalty = nil
		case "frequency_penalty":
			out.FrequencyPenalty = nil
		case "seed":
			out.Seed = nil
		case "max_output_tokens":
			out.MaxOutputTokens = 0
		}
	}
	return out
}
