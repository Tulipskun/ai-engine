package sdk

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSessionDBListSessions(t *testing.T) {
	db, err := OpenSessionDB(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	older := SessionConfig{ID: "cli:older", Provider: "p", Model: "m", ThinkingLevel: ThinkingNone}
	newer := SessionConfig{ID: "cli:newer", Provider: "p", Model: "m2", ThinkingLevel: ThinkingLow}
	if err := db.SaveSession(older); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := db.SaveSession(newer); err != nil {
		t.Fatal(err)
	}
	if err := db.AppendTurns(newer.ID, []Turn{{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: "hi"}}}}, 0); err != nil {
		t.Fatal(err)
	}
	items, err := db.ListSessions(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d sessions, want 2", len(items))
	}
	if items[0].ID != newer.ID {
		t.Fatalf("newest session = %q, want %q", items[0].ID, newer.ID)
	}
	if items[0].TurnCount != 1 {
		t.Fatalf("turn count = %d, want 1", items[0].TurnCount)
	}
}
