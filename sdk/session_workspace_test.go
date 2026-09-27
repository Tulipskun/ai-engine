package sdk

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionWorkspacePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenSessionDB(filepath.Join(dir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keys := NewKeyPool("k")
	path := filepath.Join(dir, "session.db")
	session, err := OpenSession(path, SessionConfig{ID: "ch1", Provider: "test", Model: "m"}, keys)
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	if err := session.SetWorkspace(work); err != nil {
		t.Fatal(err)
	}
	if err := session.SetAgentMode(AgentModeSub); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSession(path, SessionConfig{ID: "ch1"}, keys)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	config := reopened.Config()
	if config.Workspace != work {
		t.Fatalf("workspace lost on reopen: %q", config.Workspace)
	}
	if config.AgentMode != AgentModeSub {
		t.Fatalf("agent mode lost on reopen: %q", config.AgentMode)
	}
}

func TestSetWorkspaceRequiresAbsolutePath(t *testing.T) {
	session := NewSession(SessionConfig{ID: "s"}, NewKeyPool("k"))
	if err := session.SetWorkspace("relative/dir"); err == nil {
		t.Fatal("relative workspace accepted")
	}
	if err := session.SetWorkspace(""); err != nil {
		t.Fatalf("clearing workspace rejected: %v", err)
	}
}

func TestSubAgentReportIntervalDefaults(t *testing.T) {
	if got := (SubAgentConfig{}).reportInterval(); got != 20 {
		t.Fatalf("default interval=%d", got)
	}
	if got := (SubAgentConfig{ReportEveryToolCalls: 3}).reportInterval(); got != 3 {
		t.Fatalf("configured interval=%d", got)
	}
	if got := (SubAgentConfig{}).maxMidJobReports(); got != 3 {
		t.Fatalf("default max mid-job reports=%d", got)
	}
}

func TestWorkspaceContextRoundTrip(t *testing.T) {
	ws := filepath.Join(os.TempDir(), "ai-ws-test")
	ctx := WithWorkspace(t.Context(), ws)
	if got := WorkspaceFromContext(ctx); got != ws {
		t.Fatalf("workspace ctx=%q want=%q", got, ws)
	}
	if got := WorkspaceFromContext(t.Context()); got != "" {
		t.Fatalf("empty ctx=%q", got)
	}
	if strings.TrimSpace(WorkspaceFromContext(nil)) != "" {
		t.Fatal("nil ctx must yield empty workspace")
	}
}
