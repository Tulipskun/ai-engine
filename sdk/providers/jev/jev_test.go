package jev

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	if err != nil || !ok { t.Fatalf("allow=%v err=%v", ok, err) }
}

func TestDenyLowConfidenceNoul(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{"safe":{"type":"noul","noul":0.21}}}`))
	}))
	defer srv.Close()
	g := New(Config{Endpoint: srv.URL, Threshold: 0.8})
	ok, err := g.Allow(context.Background(), sdk.Turn{}, sdk.ToolCall{Name: "bash"})
	if err != nil || ok { t.Fatalf("allow=%v err=%v", ok, err) }
}

func TestNonBashDoesNotCallJev(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer srv.Close()
	g := New(Config{Endpoint: srv.URL})
	ok, err := g.Allow(context.Background(), sdk.Turn{}, sdk.ToolCall{Name: "read"})
	if err != nil || !ok || hits != 0 { t.Fatalf("allow=%v err=%v hits=%d", ok, err, hits) }
}
