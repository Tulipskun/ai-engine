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
	config := s.config
	if config.Temperature != nil {
		v := *config.Temperature
		config.Temperature = &v
	}
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
