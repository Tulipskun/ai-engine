package transport

import (
	"os"
	"path/filepath"
	"testing"
)

// The daemon refuses to start without config/entry.json, and no example shipped
// with it, so a first run failed by talking about a transport rather than about
// the missing file. LoadConfig answers an empty config for a missing file and
// main refuses to start on one, which makes an example that parses into nothing
// just as useless as none at all (CHANGE-077).

func repoExample(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", ".config", name)
	if _, err := os.Stat(path); err != nil {
		t.Skipf("example file is not in this checkout: %v", err)
	}
	return path
}

func TestTheEntryExampleIsAConfigThatBoots(t *testing.T) {
	cfg, err := LoadConfig(repoExample(t, "entry.example.json"))
	if err != nil {
		t.Fatalf("the shipped example does not parse: %v", err)
	}
	if !cfg.Mobile.Enabled {
		t.Error("the example leaves mobile disabled, so copying it still refuses to start")
	}
	if cfg.Mobile.Listen == "" {
		t.Error("the example has no listen address")
	}
	if cfg.Mobile.Cloudflared == "" {
		t.Error("the example names no cloudflared, so the tunnel has nothing to run")
	}
	// CON-012 gives the system exactly one secret and it arrives from the phone,
	// so an example carrying an id or a token would be teaching the wrong thing.
	if cfg.Mobile.CloudflareAPI != "" || cfg.Mobile.D1Database != "" {
		t.Error("the example carries a Cloudflare id or token, which must never be held on disk")
	}
	if !cfg.Mobile.SyncConfig || !cfg.Mobile.SyncSessions {
		t.Error("the example turns off the D1 sync that holds the runtime state")
	}
}
