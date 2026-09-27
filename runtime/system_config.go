package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Tulipskun/ai-engine/sdk"
)

type SubAgentConfig = sdk.SubAgentConfig

type SystemConfig struct {
	SystemPrompt    string         `json:"system_prompt"`
	Provider        string         `json:"provider"`
	Model           string         `json:"model"`
	MaxOutputTokens int            `json:"max_output_tokens"`
	Workspace       string         `json:"workspace"`
	SubAgent        SubAgentConfig `json:"sub_agent"`
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
	cfg.SystemPrompt = strings.TrimSpace(cfg.SystemPrompt)
	cfg.Provider = strings.TrimSpace(cfg.Provider)
	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.Workspace = strings.TrimSpace(cfg.Workspace)
	if cfg.SubAgent.Provider == "" && cfg.SubAgent.Model == "" && cfg.SubAgent.MaxOutputTokens == 0 && cfg.SubAgent.Temperature == nil && cfg.SubAgent.ThinkingLevel == "" {
		cfg.SubAgent.Enabled = true
	}
	return cfg, nil
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
