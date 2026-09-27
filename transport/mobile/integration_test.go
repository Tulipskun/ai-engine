package mobile

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestIntegrationWorkerHandshake is an opt-in check against a real AIxodia
// Worker. It is skipped unless both variables are set, so CI never depends on
// a live account and no credential is ever committed:
//
//	AIXODIA_WORKER=https://aixodia.<subdomain>.workers.dev
//	AIXODIA_TOKEN=<D1 access token>
//
// It proves the two-step rule against the real service: a wrong token is
// rejected as a bad credential (counted), and a valid one is accepted.
func TestIntegrationWorkerHandshake(t *testing.T) {
	base := os.Getenv("AIXODIA_WORKER")
	token := os.Getenv("AIXODIA_TOKEN")
	if base == "" || token == "" {
		t.Skip("set AIXODIA_WORKER and AIXODIA_TOKEN to run the live handshake check")
	}
	verifier := workerVerifier{base: base}
	transport := New(Config{Tokens: &stubTokens{}, Verifier: verifier})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := verifier.VerifyToken(ctx, token+"definitely-wrong"); err == nil || !isRejected(err) {
		t.Fatalf("wrong token error = %v, want a rejected credential", err)
	}
	if err := verifier.VerifyToken(ctx, token); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	decision := transport.gate.Check(integrationRequest(token))
	if !decision.Allowed {
		t.Fatalf("valid handshake not allowed: %+v", decision)
	}
	decision = transport.gate.Check(integrationRequest("nope"))
	if decision.Allowed || decision.Status != http.StatusUnauthorized {
		t.Fatalf("wrong token decision = %+v, want 401", decision)
	}
}

type stubTokens struct{}

func (stubTokens) Get() string  { return "" }
func (stubTokens) Adopt(string) {}

type workerVerifier struct{ base string }

func (v workerVerifier) VerifyToken(ctx context.Context, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.base+"/api/ping", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrTokenRejected
	case http.StatusOK:
		return nil
	default:
		return &unexpectedStatus{code: resp.StatusCode}
	}
}

type unexpectedStatus struct{ code int }

func (e *unexpectedStatus) Error() string {
	return "mobile: unexpected ping status"
}

func isRejected(err error) bool {
	if err == nil {
		return false
	}
	if err == ErrTokenRejected {
		return true
	}
	type stringer interface{ Error() string }
	if s, ok := err.(stringer); ok {
		return contains(s.Error(), ErrTokenRejected.Error())
	}
	return false
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func integrationRequest(token string) *http.Request {
	req, _ := http.NewRequest(http.MethodGet, "http://mobile.invalid/ws", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("CF-Connecting-IP", "127.0.0.1")
	return req
}
