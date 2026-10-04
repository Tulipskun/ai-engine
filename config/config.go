// Package config prepares the local state root: ~/.local/share/ai.
// Files the phone has not pushed yet simply do not exist on first run;
// nothing here fails the boot for that.
package config

import (
	"os"
	"path/filepath"
)

// Root is the state directory. ProviderPath and SystemPath are where the
// provider list and the agent defaults materialize once the phone pushes
// them.
var (
	Root         string
	ProviderPath string
	SystemPath   string
	SessionDir   string
)

// LoadConfig resolves the paths and creates the directories. Missing files
// are fine — they arrive later from the phone.
func LoadConfig() {
	home, _ := os.UserHomeDir()
	Root = filepath.Join(home, ".local", "share", "ai")
	ProviderPath = filepath.Join(Root, "config", "provider.json")
	SystemPath = filepath.Join(Root, "config", "system.json")
	SessionDir = filepath.Join(Root, "data", "sessions")
	_ = os.MkdirAll(filepath.Join(Root, "config"), 0o700)
	_ = os.MkdirAll(SessionDir, 0o700)
}
