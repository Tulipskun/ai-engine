package jev

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tulipskun/ai-engine/sdk"
)

func TestAllowBashFromJev(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"safe":{"type":"noul","noul":0.97}}}`))
	}))
	defer srv.Close()
	g := New(Config{Endpoint: srv.URL, Threshold: 0.8})
	ok, err := g.Allow(context.Background(), sdk.Turn{Role: sdk.RoleUser, Content: []sdk.ContentPart{{Type: sdk.ContentText, Text: "edit the project"}}}, sdk.ToolCall{Name: "bash"})
	if err != nil || !ok {
		t.Fatalf("allow=%v err=%v", ok, err)
	}
}

func TestDenyLowConfidenceNoul(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{"safe":{"type":"noul","noul":0.21}}}`))
	}))
	defer srv.Close()
	g := New(Config{Endpoint: srv.URL, Threshold: 0.8})
	ok, err := g.Allow(context.Background(), sdk.Turn{}, sdk.ToolCall{Name: "bash"})
	if err != nil || ok {
		t.Fatalf("allow=%v err=%v", ok, err)
	}
}

func TestNonBashDoesNotCallJev(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer srv.Close()
	g := New(Config{Endpoint: srv.URL})
	ok, err := g.Allow(context.Background(), sdk.Turn{}, sdk.ToolCall{Name: "read"})
	if err != nil || !ok || hits != 0 {
		t.Fatalf("allow=%v err=%v hits=%d", ok, err, hits)
	}
}

// The edge refuses the decision endpoint without the OpenCode client's
// fingerprint, and a refusal makes the guard allow everything. So the headers
// have to actually ride on the request.
func TestDecisionRequestCarriesTheClientFingerprint(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{"answers":{"safe":{"type":"noul","noul":0.9}}}`))
	}))
	defer srv.Close()
	t.Setenv("AI_JEV_ENABLED", "1")
	t.Setenv("AI_JEV_ENDPOINT", srv.URL)
	g := NewFromEnv()
	if g == nil {
		t.Fatal("guard must be on unless explicitly disabled")
	}
	if _, err := g.Allow(context.Background(), sdk.Turn{}, sdk.ToolCall{Name: "bash"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"User-Agent", "X-Opencode-Session", "X-Opencode-Client", "X-Opencode-Project", "Http-Referer"} {
		if got.Get(name) == "" {
			t.Fatalf("request is missing %s; the edge answers 403 and the guard fails open", name)
		}
	}
	if !strings.HasPrefix(got.Get("User-Agent"), "opencode/") {
		t.Fatalf("User-Agent = %q, want the opencode client fingerprint", got.Get("User-Agent"))
	}
	if got.Get("X-Opencode-Session") != got.Get("X-Opencode-Request") {
		t.Fatal("session and request ids must match the client's own pairing")
	}
}

func TestGuardStaysOffWhenTheEnvSaysSo(t *testing.T) {
	for _, off := range []string{"0", "false", "FALSE"} {
		t.Setenv("AI_JEV_ENABLED", off)
		if NewFromEnv() != nil {
			t.Fatalf("AI_JEV_ENABLED=%s must disable the guard", off)
		}
	}
}
