package runtime

import (
	"context"
	"testing"

	"github.com/Tulipskun/ai-engine/sdk"
)

func TestSessionManagerReusesSessionByID(t *testing.T) {
	manager := NewSessionManager(t.TempDir()+"/sessions.db", sdk.SessionConfig{Provider: sdk.ProviderOpenRouter, Model: "model"}, sdk.NewKeyPool("key"))
	first, err := manager.Resolve(context.Background(), sdk.Input{SessionID: "discord:channel:1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Resolve(context.Background(), sdk.Input{SessionID: "discord:channel:1"})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("same SessionID created multiple session objects")
	}
}

func TestSessionManagerUsesInputSessionID(t *testing.T) {
	manager := NewSessionManager(t.TempDir()+"/sessions.db", sdk.SessionConfig{Provider: sdk.ProviderOpenRouter, Model: "model"}, sdk.NewKeyPool("key"))
	session, err := manager.Resolve(context.Background(), sdk.Input{SessionID: "telegram:chat:42"})
	if err != nil {
		t.Fatal(err)
	}
	if session.ID() != "telegram:chat:42" {
		t.Fatalf("session ID = %q", session.ID())
	}
}

func TestSessionManagerAdoptsProvidersAndRepointsStaleSessions(t *testing.T) {
	manager := NewSessionManager(t.TempDir()+"/sessions.db", sdk.SessionConfig{}, nil)
	stale, err := manager.Resolve(context.Background(), sdk.Input{SessionID: "work-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := stale.Config().Provider; got != "" {
		t.Fatalf("session provider before adopt = %q, want empty", got)
	}

	manager.AdoptProviders(
		[]sdk.ProviderConfig{{ID: "NousResearch", Keys: sdk.NewKeyPool("key")}},
		sdk.SessionConfig{Provider: "NousResearch", Model: "meituan/longcat-2.0:free"},
	)

	if got := stale.Config().Provider; got != "NousResearch" {
		t.Fatalf("cached session provider = %q, want NousResearch", got)
	}
	if got := stale.Config().Model; got != "meituan/longcat-2.0:free" {
		t.Fatalf("cached session model = %q, want the reloaded model", got)
	}
	reopened, err := manager.Resolve(context.Background(), sdk.Input{SessionID: "work-2"})
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Config().Provider; got != "NousResearch" {
		t.Fatalf("session opened after adopt = %q, want NousResearch", got)
	}
}

func TestSessionManagerPrefersTheStoredProviderAndModel(t *testing.T) {
	dir := t.TempDir()
	keys := sdk.NewKeyPool("key")
	manager := NewSessionManagerWithProviders(dir+"/sessions.db", sdk.SessionConfig{Provider: "boot", Model: "boot-model"},
		[]sdk.ProviderConfig{{ID: "boot", Keys: keys}, {ID: "picked", Keys: sdk.NewKeyPool("other")}})
	manager.SetSessionDefaults(func(_ context.Context, sessionID string) (sdk.ProviderID, string, bool) {
		if sessionID != "work-7" {
			return "", "", false
		}
		return "picked", "chosen-model", true
	})

	session, err := manager.Resolve(context.Background(), sdk.Input{SessionID: "work-7"})
	if err != nil {
		t.Fatal(err)
	}
	if got := session.Config().Provider; got != "picked" {
		t.Fatalf("provider = %q, want the phone's choice", got)
	}
	if got := session.Config().Model; got != "chosen-model" {
		t.Fatalf("model = %q, want the phone's choice", got)
	}

	// A session the phone never configured keeps the boot default.
	other, err := manager.Resolve(context.Background(), sdk.Input{SessionID: "work-8"})
	if err != nil {
		t.Fatal(err)
	}
	if got := other.Config().Provider; got != "boot" {
		t.Fatalf("provider = %q, want the boot default", got)
	}
}

func TestSessionManagerIgnoresAStoredProviderItCannotRouteTo(t *testing.T) {
	manager := NewSessionManager(t.TempDir()+"/sessions.db", sdk.SessionConfig{Provider: "boot", Model: "m"}, sdk.NewKeyPool("boot-key"))
	manager.SetSessionDefaults(func(context.Context, string) (sdk.ProviderID, string, bool) {
		return "deleted-provider", "gone", true
	})
	session, err := manager.Resolve(context.Background(), sdk.Input{SessionID: "work-9"})
	if err != nil {
		t.Fatal(err)
	}
	if got := session.Config().Provider; got != "boot" {
		t.Fatalf("provider = %q, want the boot default for an unknown provider", got)
	}
}

func TestSessionManagerForgetReopensTheNextTurnFromDefaults(t *testing.T) {
	manager := NewSessionManager(t.TempDir()+"/sessions.db", sdk.SessionConfig{Provider: "boot", Model: "m"}, sdk.NewKeyPool("boot-key"))
	first, err := manager.Resolve(context.Background(), sdk.Input{SessionID: "work-10"})
	if err != nil {
		t.Fatal(err)
	}
	manager.Forget("work-10")
	manager.Forget("")
	second, err := manager.Resolve(context.Background(), sdk.Input{SessionID: "work-10"})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("forgetting a session must not return the cached object")
	}
	if got := second.Config().Provider; got != "boot" {
		t.Fatalf("provider = %q, want the current default after forget", got)
	}
}
