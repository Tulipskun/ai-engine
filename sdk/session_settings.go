package sdk

import (
	"errors"
	"fmt"
	"github.com/Tulipskun/ai-engine/provider"
	"strings"
)

// Settings setters write the session's own top-level fields. Each Discord
// channel owns two sessions (main and sub) with their own settings, so no
// setter needs to know about agent modes (REQ-030, CHANGE-021).

func (s *Session) SetAgentMode(mode provider.AgentMode) error {
	if s == nil {
		return errors.New("sdk: session is nil")
	}
	switch mode {
	case provider.AgentModeMain, provider.AgentModeSub:
		return s.updateConfig(func(config *provider.SessionConfig) error { config.AgentMode = mode; return nil })
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
	return s.updateConfig(func(config *provider.SessionConfig) error { config.Workspace = path; return nil })
}

func (s *Session) SetKeyPool(keys *provider.KeyPool) error {
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

func (s *Session) SetProvider(providerLocal provider.ProviderID, keys *provider.KeyPool) error {
	providerLocal = provider.ProviderID(strings.TrimSpace(string(providerLocal)))
	if providerLocal == "" {
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
	return s.updateConfig(func(config *provider.SessionConfig) error {
		config.Provider = providerLocal
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
	return s.updateConfig(func(config *provider.SessionConfig) error { config.Model = model; return nil })
}
func (s *Session) SetTemperature(temperature float64) error {
	if err := provider.ValidateTemperature(temperature); err != nil {
		return err
	}
	return s.updateConfig(func(config *provider.SessionConfig) error {
		config.Temperature = provider.CloneFloat(&temperature)
		return nil
	})
}
func (s *Session) ClearTemperature() error {
	return s.updateConfig(func(config *provider.SessionConfig) error { config.Temperature = nil; return nil })
}
func (s *Session) SetThinkingLevel(level provider.ThinkingLevel) error {
	if err := provider.ValidateThinkingLevel(level); err != nil {
		return err
	}
	return s.updateConfig(func(config *provider.SessionConfig) error { config.ThinkingLevel = level; return nil })
}
func (s *Session) ClearThinkingLevel() error {
	return s.updateConfig(func(config *provider.SessionConfig) error { config.ThinkingLevel = ""; return nil })
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
	return s.updateConfig(func(config *provider.SessionConfig) error { config.KeyIndex = index; return nil })
}

// SetMaxOutputTokens caps how long an answer may run. Zero means no cap, which
// leaves the limit to the provider's own default rather than inventing one.
func (s *Session) SetMaxOutputTokens(tokens int) error {
	if err := provider.ValidateMaxOutputTokens(tokens); err != nil {
		return err
	}
	return s.updateConfig(func(config *provider.SessionConfig) error { config.MaxOutputTokens = tokens; return nil })
}

// Every setter below validates before it stores, because a knob that a provider
// will reject is better refused here than turned into a failed turn later
// (CHANGE-077).

func (s *Session) SetTopP(v float64) error {
	if err := provider.ValidateTopP(v); err != nil {
		return err
	}
	return s.updateConfig(func(config *provider.SessionConfig) error { config.TopP = provider.CloneFloat(&v); return nil })
}
func (s *Session) ClearTopP() error {
	return s.updateConfig(func(config *provider.SessionConfig) error { config.TopP = nil; return nil })
}

func (s *Session) SetTopK(v float64) error {
	if err := provider.ValidateTopK(v); err != nil {
		return err
	}
	return s.updateConfig(func(config *provider.SessionConfig) error { config.TopK = provider.CloneFloat(&v); return nil })
}
func (s *Session) ClearTopK() error {
	return s.updateConfig(func(config *provider.SessionConfig) error { config.TopK = nil; return nil })
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
	return s.updateConfig(func(config *provider.SessionConfig) error { config.StopSequences = cleaned; return nil })
}
func (s *Session) ClearStopSequences() error {
	return s.updateConfig(func(config *provider.SessionConfig) error { config.StopSequences = nil; return nil })
}

func (s *Session) SetPresencePenalty(v float64) error {
	if err := provider.ValidatePenalty("presence_penalty", v); err != nil {
		return err
	}
	return s.updateConfig(func(config *provider.SessionConfig) error {
		config.PresencePenalty = provider.CloneFloat(&v)
		return nil
	})
}
func (s *Session) ClearPresencePenalty() error {
	return s.updateConfig(func(config *provider.SessionConfig) error { config.PresencePenalty = nil; return nil })
}

func (s *Session) SetFrequencyPenalty(v float64) error {
	if err := provider.ValidatePenalty("frequency_penalty", v); err != nil {
		return err
	}
	return s.updateConfig(func(config *provider.SessionConfig) error {
		config.FrequencyPenalty = provider.CloneFloat(&v)
		return nil
	})
}
func (s *Session) ClearFrequencyPenalty() error {
	return s.updateConfig(func(config *provider.SessionConfig) error { config.FrequencyPenalty = nil; return nil })
}

func (s *Session) SetSeed(seed int64) error {
	return s.updateConfig(func(config *provider.SessionConfig) error { config.Seed = provider.CloneInt64(&seed); return nil })
}
func (s *Session) ClearSeed() error {
	return s.updateConfig(func(config *provider.SessionConfig) error { config.Seed = nil; return nil })
}

func validThinkingLevel(level provider.ThinkingLevel) bool {
	switch level {
	case provider.ThinkingNone, provider.ThinkingLow, provider.ThinkingMedium, provider.ThinkingHigh:
		return true
	default:
		return false
	}
}
func (s *Session) updateConfig(update func(*provider.SessionConfig) error) error {
	if s == nil {
		return errors.New("sdk: session is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	config := s.config.Clone()
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
