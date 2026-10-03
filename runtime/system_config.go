package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Tulipskun/ai-engine/provider"
	"os"
	"strings"

	"github.com/Tulipskun/ai-engine/sdk"
)

type SubAgentConfig = sdk.SubAgentConfig

// SystemConfig is the boot configuration. The generation knobs are top-level
// fields for the main agent and live under sub_agent for the worker, because
// each agent runs on its own settings; anything left unset stays unset so the
// provider keeps its own default (CHANGE-077).
type SystemConfig struct {
	SystemPrompt string `json:"system_prompt"`
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	// Generation holds the main agent's knobs. max_output_tokens stays a sibling
	// field because it predates this and is read by the boot path directly.
	Generation      provider.GenerationSettings `json:"generation"`
	MaxOutputTokens int                         `json:"max_output_tokens"`
	Workspace       string                      `json:"workspace"`
	SubAgent        SubAgentConfig              `json:"sub_agent"`
}

// Settings returns the generation knobs as they are stored, so the boot path can
// seed a session from this without knowing how the file spelled them.
func (c SystemConfig) Settings() provider.GenerationSettings { return c.Generation }

// MaxOutputTokensFor returns the cap that applies to the main agent, preferring
// the generation block and falling back to the older top-level field.
func (c SystemConfig) MaxOutputTokensFor() int {
	if c.MaxOutputTokens > 0 {
		return c.MaxOutputTokens
	}
	return c.Generation.MaxOutputTokens
}

const DefaultSystemConfigPath = "config/system.json"

func LoadSystemConfig(path string) (SystemConfig, error) {
	if path == "" {
		path = DefaultSystemConfigPath
	}
	var cfg SystemConfig
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			cfg.SubAgent.Enabled = true
			return cfg, nil
		}
		return SystemConfig{}, fmt.Errorf("system: read config %q: %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return SystemConfig{}, fmt.Errorf("system: decode config %q: %w", path, err)
	}
	if cfg.MaxOutputTokens < 0 {
		return SystemConfig{}, fmt.Errorf("system: decode config %q: max_output_tokens must not be negative", path)
	}
	if err := validateGeneration(path, cfg.Generation); err != nil {
		return SystemConfig{}, err
	}
	if err := validateSubAgentGeneration(path, cfg.SubAgent); err != nil {
		return SystemConfig{}, err
	}
	cfg.SystemPrompt = strings.TrimSpace(cfg.SystemPrompt)
	cfg.Provider = strings.TrimSpace(cfg.Provider)
	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.Workspace = strings.TrimSpace(cfg.Workspace)
	if cfg.SubAgent.Provider == "" && cfg.SubAgent.Model == "" && cfg.SubAgent.MaxOutputTokens == 0 && cfg.SubAgent.Temperature == nil && cfg.SubAgent.ThinkingLevel == "" {
		cfg.SubAgent.Enabled = true
	}
	return cfg, nil
}

// validateGeneration holds a hand-edited config to the same bounds a value
// arriving over the wire is held to, so the two cannot disagree about what is
// allowed (CHANGE-077).
func validateGeneration(path string, g provider.GenerationSettings) error {
	if g.Temperature != nil {
		if err := provider.ValidateTemperature(*g.Temperature); err != nil {
			return fmt.Errorf("system: decode config %q: %w", path, err)
		}
	}
	if g.TopP != nil {
		if err := provider.ValidateTopP(*g.TopP); err != nil {
			return fmt.Errorf("system: decode config %q: %w", path, err)
		}
	}
	if g.TopK != nil {
		if err := provider.ValidateTopK(*g.TopK); err != nil {
			return fmt.Errorf("system: decode config %q: %w", path, err)
		}
	}
	if g.PresencePenalty != nil {
		if err := provider.ValidatePenalty("presence_penalty", *g.PresencePenalty); err != nil {
			return fmt.Errorf("system: decode config %q: %w", path, err)
		}
	}
	if g.FrequencyPenalty != nil {
		if err := provider.ValidatePenalty("frequency_penalty", *g.FrequencyPenalty); err != nil {
			return fmt.Errorf("system: decode config %q: %w", path, err)
		}
	}
	if g.ThinkingLevel != "" {
		if err := provider.ValidateThinkingLevel(g.ThinkingLevel); err != nil {
			return fmt.Errorf("system: decode config %q: %w", path, err)
		}
	}
	if g.MaxOutputTokens < 0 {
		return fmt.Errorf("system: decode config %q: max_output_tokens must not be negative", path)
	}
	return nil
}

func validateSubAgentGeneration(path string, sub sdk.SubAgentConfig) error {
	return validateGeneration(path, provider.GenerationSettings{
		ThinkingLevel:   sub.ThinkingLevel,
		Temperature:     sub.Temperature,
		MaxOutputTokens: sub.MaxOutputTokens,
	})
}

// dirOf returns the directory part of a config path. CHANGE-087 moved it here
// when the browser config that used to own it was deleted.
func dirOf(path string) string {
	i := strings.LastIndex(path, "/")
	if i < 0 {
		return "."
	}
	if i == 0 {
		return "/"
	}
	return path[:i]
}

func SaveSystemConfig(path string, cfg SystemConfig) error {
	if path == "" {
		path = DefaultSystemConfigPath
	}
	cfg.SystemPrompt = strings.TrimSpace(cfg.SystemPrompt)
	cfg.Provider = strings.TrimSpace(cfg.Provider)
	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.Workspace = strings.TrimSpace(cfg.Workspace)
	cfg.SubAgent.Provider = strings.TrimSpace(cfg.SubAgent.Provider)
	cfg.SubAgent.Model = strings.TrimSpace(cfg.SubAgent.Model)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("system: encode config: %w", err)
	}
	if err := os.MkdirAll(dirOf(path), 0o700); err != nil {
		return fmt.Errorf("system: create config directory: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("system: write config %q: %w", path, err)
	}
	return nil
}
