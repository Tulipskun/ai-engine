package transport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigMissing(t *testing.T) {
	config, err := LoadConfig(filepath.Join(t.TempDir(), "entry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if config.Mobile.Enabled {
		t.Fatalf("unexpected config: %+v", config)
	}
}

func TestLoadConfigReadsMobile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "entry.json")
	data := []byte(`{"mobile":{"enabled":true,"listen":"127.0.0.1:18789","tunnel":true,"d1_database":"aixodia","sync_config":true,"sync_sessions":true}}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !config.Mobile.Enabled || config.Mobile.D1Database != "aixodia" || !config.Mobile.Tunnel ||
		!config.Mobile.SyncConfig || !config.Mobile.SyncSessions {
		t.Fatalf("unexpected config: %+v", config)
	}
}

func TestSaveConfigRoundTripsMobileOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "entry.json")
	want := Config{Mobile: MobileConfig{Enabled: true, D1Database: "aixodia", Tunnel: true}}
	if err := SaveConfig(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mobile != want.Mobile {
		t.Fatalf("round trip changed the config: %+v", got.Mobile)
	}
	// No credential may be written into the entry config (CON-012).
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "token") {
		t.Fatalf("entry config mentions a token: %s", raw)
	}
}
