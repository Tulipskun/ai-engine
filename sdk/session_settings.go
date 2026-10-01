package sdk

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// Settings setters write the session's own top-level fields. Each Discord
// channel owns two sessions (main and sub) with their own settings, so no
// setter needs to know about agent modes (REQ-030, CHANGE-021).

func (s *Session) SetAgentMode(mode AgentMode) error {
	if s == nil {
		return errors.New("sdk: session is nil")
	}
	switch mode {
	case AgentModeMain, AgentModeSub:
		return s.updateConfig(func(config *SessionConfig) error { config.AgentMode = mode; return nil })
	default:
		return fmt.Errorf("sdk: invalid agent mode %q", mode)
	}
}

// SetWorkspace pins the session's working directory. Empty clears it back to
// the process default. Values must be absolute; existence checks belong to the
// transport that received the user command (REQ-038).
func (s *Session) SetWorkspace(path string) error {
	if s == nil {
		return errors.New("sdk: session is nil")
	}
	path = strings.TrimSpace(path)
	if path != "" && !strings.HasPrefix(path, "/") && !strings.Contains(path, ":\\") && !strings.Contains(path, ":/") {
		return fmt.Errorf("sdk: workspace must be absolute: %q", path)
	}
	return s.updateConfig(func(config *SessionConfig) error { config.Workspace = path; return nil })
}

func (s *Session) SetKeyPool(keys *KeyPool) error {
	if s == nil {
		return errors.New("sdk: session is nil")
	}
	if keys == nil {
		return errors.New("sdk: provider has no key pool")
	}
	if _, err := keys.At(0); err != nil {
		return err
	}
	s.mu.Lock()
	s.keys = keys
	s.mu.Unlock()
	return nil
}

func (s *Session) SetProvider(provider ProviderID, keys *KeyPool) error {
	provider = ProviderID(strings.TrimSpace(string(provider)))
	if provider == "" {
		return errors.New("sdk: provider is required")
	}
	if keys == nil {
		return errors.New("sdk: provider has no key pool")
	}
	if _, err := keys.At(0); err != nil {
		return err
	}
	if err := s.SetKeyPool(keys); err != nil {
		return err
	}
	return s.updateConfig(func(config *SessionConfig) error {
		config.Provider = provider
		config.Model = ""
		config.KeyIndex = 0
		return nil
	})
}
func (s *Session) SetModel(model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return errors.New("sdk: model is required")
	}
	return s.updateConfig(func(config *SessionConfig) error { config.Model = model; return nil })
}
func (s *Session) SetTemperature(temperature float64) error {
	if math.IsNaN(temperature) || math.IsInf(temperature, 0) {
		return errors.New("sdk: temperature must be finite")
	}
	return s.updateConfig(func(config *SessionConfig) error { v := temperature; config.Temperature = &v; return nil })
}
func (s *Session) ClearTemperature() error {
	return s.updateConfig(func(config *SessionConfig) error { config.Temperature = nil; return nil })
}
func (s *Session) SetThinkingLevel(level ThinkingLevel) error {
	if !validThinkingLevel(level) {
		return fmt.Errorf("sdk: invalid thinking level %q", level)
	}
	return s.updateConfig(func(config *SessionConfig) error { config.ThinkingLevel = level; return nil })
}
func (s *Session) ClearThinkingLevel() error {
	return s.updateConfig(func(config *SessionConfig) error { config.ThinkingLevel = ""; return nil })
}
func (s *Session) SetKeyIndex(index int) error {
	if index < 0 {
		return errors.New("sdk: API key index out of range")
	}
	if s == nil {
		return errors.New("sdk: session is nil")
	}
	s.mu.RLock()
	keys := s.keys
	s.mu.RUnlock()
	if keys == nil {
		return errors.New("sdk: session has no key pool")
	}
	if _, err := keys.At(index); err != nil {
		return err
	}
	return s.updateConfig(func(config *SessionConfig) error { config.KeyIndex = index; return nil })
}

// SetMaxOutputTokens caps how long an answer may run. Zero means no cap, which
// leaves the limit to the provider's own default rather than inventing one.
func (s *Session) SetMaxOutputTokens(tokens int) error {
	if tokens < 0 {
		return errors.New("sdk: max output tokens must not be negative")
	}
	return s.updateConfig(func(config *SessionConfig) error { config.MaxOutputTokens = tokens; return nil })
}

// Every setter below validates before it stores, because a knob that a provider
// will reject is better refused here than turned into a failed turn later
// (CHANGE-077).

func (s *Session) SetTopP(v float64) error {
	if err := checkRange("top_p", v, 0, 1); err != nil {
		return err
	}
	return s.updateConfig(func(config *SessionConfig) error { config.TopP = cloneFloat(&v); return nil })
}
func (s *Session) ClearTopP() error {
	return s.updateConfig(func(config *SessionConfig) error { config.TopP = nil; return nil })
}

func (s *Session) SetTopK(v float64) error {
	if err := checkRange("top_k", v, 0, 1); err != nil {
		return err
	}
	return s.updateConfig(func(config *SessionConfig) error { config.TopK = cloneFloat(&v); return nil })
}
func (s *Session) ClearTopK() error {
	return s.updateConfig(func(config *SessionConfig) error { config.TopK = nil; return nil })
}

func (s *Session) SetStopSequences(seq []string) error {
	cleaned := make([]string, 0, len(seq))
	for _, entry := range seq {
		if entry = strings.TrimSpace(entry); entry != "" {
			cleaned = append(cleaned, entry)
		}
	}
	if len(cleaned) == 0 {
		return s.ClearStopSequences()
	}
	return s.updateConfig(func(config *SessionConfig) error { config.StopSequences = cleaned; return nil })
}
func (s *Session) ClearStopSequences() error {
	return s.updateConfig(func(config *SessionConfig) error { config.StopSequences = nil; return nil })
}

func (s *Session) SetPresencePenalty(v float64) error {
	if err := checkRange("presence_penalty", v, -2, 2); err != nil {
		return err
	}
	return s.updateConfig(func(config *SessionConfig) error { config.PresencePenalty = cloneFloat(&v); return nil })
}
func (s *Session) ClearPresencePenalty() error {
	return s.updateConfig(func(config *SessionConfig) error { config.PresencePenalty = nil; return nil })
}

func (s *Session) SetFrequencyPenalty(v float64) error {
	if err := checkRange("frequency_penalty", v, -2, 2); err != nil {
		return err
	}
	return s.updateConfig(func(config *SessionConfig) error { config.FrequencyPenalty = cloneFloat(&v); return nil })
}
func (s *Session) ClearFrequencyPenalty() error {
	return s.updateConfig(func(config *SessionConfig) error { config.FrequencyPenalty = nil; return nil })
}

func (s *Session) SetSeed(seed int64) error {
	return s.updateConfig(func(config *SessionConfig) error { config.Seed = cloneInt64(&seed); return nil })
}
func (s *Session) ClearSeed() error {
	return s.updateConfig(func(config *SessionConfig) error { config.Seed = nil; return nil })
}

// checkRange rejects a value no provider would accept, and rejects NaN and Inf
// too: they survive a range comparison and would reach the HTTP body as garbage.
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
func (s *Session) updateConfig(update func(*SessionConfig) error) error {
	if s == nil {
		return errors.New("sdk: session is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	config := s.config.clone()
	if err := update(&config); err != nil {
		return err
	}
	if s.store != nil {
		if err := s.store.SaveSession(config); err != nil {
			return err
		}
	}
	s.config = config
	return nil
}
