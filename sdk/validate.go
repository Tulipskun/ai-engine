package sdk

import (
	"errors"
	"fmt"
	"math"
)

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
