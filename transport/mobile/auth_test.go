package mobile

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type stubVerifier struct {
	down    bool
	rejects bool
	calls   int
}

func (v *stubVerifier) VerifyToken(_ context.Context, token string) error {
	v.calls++
	switch {
	case v.down:
		return errors.New("worker unreachable")
	case v.rejects || token != "good-token":
		return ErrTokenRejected
	default:
		return nil
	}
}

func handshake(t *testing.T, url, auth string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request error: %v", err)
	}
	return resp
}

type recordingTokens struct{ value string }

func (r *recordingTokens) Get() string        { return r.value }
func (r *recordingTokens) Adopt(token string) { r.value = token }

func TestHandshakeRejectsMissingHeader(t *testing.T) {
	verifier := &stubVerifier{}
	tokens := &recordingTokens{}
	transport := New(Config{Tokens: tokens, Verifier: verifier})
	server := httptest.NewServer(http.HandlerFunc(transport.serveWS))
	defer server.Close()

	resp := handshake(t, server.URL, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if verifier.calls != 0 {
		t.Fatalf("verifier called %d times; a missing header must not count as a login", verifier.calls)
	}
	if tokens.value != "" {
		t.Fatal("token adopted without a verified handshake")
	}
}

func TestHandshakeRejectsMalformedHeader(t *testing.T) {
	transport := New(Config{Tokens: &recordingTokens{}, Verifier: &stubVerifier{}})
	server := httptest.NewServer(http.HandlerFunc(transport.serveWS))
	defer server.Close()
	for _, header := range []string{"Token abc", "Bearer", "Bearer   "} {
		resp := handshake(t, server.URL, header)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("header %q: status = %d, want 401", header, resp.StatusCode)
		}
	}
}

func TestWrongTokenLocksOutAndEscalates(t *testing.T) {
	verifier := &stubVerifier{rejects: true}
	transport := New(Config{Tokens: &recordingTokens{}, Verifier: verifier})
	server := httptest.NewServer(http.HandlerFunc(transport.serveWS))
	defer server.Close()

	for i := 1; i <= 4; i++ {
		resp := handshake(t, server.URL, "Bearer wrong")
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i, resp.StatusCode)
		}
		if got := resp.Header.Get("Retry-After"); got != "" {
			t.Fatalf("attempt %d: Retry-After = %q, want none before the limit", i, got)
		}
	}
	fifth := handshake(t, server.URL, "Bearer wrong")
	defer fifth.Body.Close()
	if fifth.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("5th attempt: status = %d, want 429", fifth.StatusCode)
	}
	if got := fifth.Header.Get("Retry-After"); got != "30" {
		t.Fatalf("5th attempt: Retry-After = %q, want 30", got)
	}

	// Even a valid token stays locked out...
	valid := handshake(t, server.URL, "Bearer good-token")
	valid.Body.Close()
	if valid.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("valid token during lockout: status = %d, want 429", valid.StatusCode)
	}
	// ...and so does a different invalid token, otherwise rotating credentials
	// would dodge the rule.
	other := handshake(t, server.URL, "Bearer another-bad-token")
	other.Body.Close()
	if other.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("lockout dodged by rotating token: status = %d", other.StatusCode)
	}

	// Schedule: 30 → 60 → 120 → 240 → 300 (cap).
	want := []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second, 240 * time.Second, 300 * time.Second, 300 * time.Second}
	for i, expect := range want {
		if got := transport.gate.PenaltyFor(5 + i); got != expect {
			t.Fatalf("penalty after %d failures = %s, want %s", 6+i, got, expect)
		}
	}

	// Another address is unaffected.
	otherReq := httptest.NewRequest(http.MethodGet, "/ws", nil)
	otherReq.Header.Set("Authorization", "Bearer wrong")
	otherReq.Header.Set("CF-Connecting-IP", "9.9.9.9")
	if d := transport.gate.Check(otherReq); d.Status != http.StatusUnauthorized {
		t.Fatalf("other address: status = %d, want 401", d.Status)
	}
}

func TestVerifierOutageFailsClosedWithoutCounting(t *testing.T) {
	verifier := &stubVerifier{down: true}
	transport := New(Config{Tokens: &recordingTokens{}, Verifier: verifier})
	server := httptest.NewServer(http.HandlerFunc(transport.serveWS))
	defer server.Close()

	for i := 0; i < 8; i++ {
		resp := handshake(t, server.URL, "Bearer good-token")
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("attempt %d: status = %d, want 503", i, resp.StatusCode)
		}
	}
	verifier.down = false
	if d := transport.gate.Check(fakeRequest("1.1.1.1", "Bearer good-token")); !d.Allowed {
		t.Fatalf("after recovery: %+v, want allowed (an outage must not lock a client out)", d)
	}
}

func TestRejectionBodyNeverContainsToken(t *testing.T) {
	transport := New(Config{Tokens: &recordingTokens{}, Verifier: &stubVerifier{rejects: true}})
	recorder := httptest.NewRecorder()
	transport.gate.Write(recorder, transport.gate.Check(fakeRequest("1.1.1.1", "Bearer super-secret")))
	if strings.Contains(recorder.Body.String(), "super-secret") {
		t.Fatalf("token leaked into the response: %s", recorder.Body.String())
	}
}

func TestValidHandshakeAdoptsTokenOnce(t *testing.T) {
	tokens := &recordingTokens{}
	transport := New(Config{Tokens: tokens, Verifier: &stubVerifier{}})
	request := fakeRequest("1.1.1.1", "Bearer good-token")
	decision := transport.gate.Check(request)
	if !decision.Allowed || decision.Token != "good-token" {
		t.Fatalf("decision = %+v, want allowed", decision)
	}
	tokens.Adopt(decision.Token)
	if tokens.value != "good-token" {
		t.Fatalf("token store = %q", tokens.value)
	}
	// Success resets the counter for that address.
	transport.gate.fail("1.1.1.1")
	transport.gate.succeed("1.1.1.1")
	if got := transport.gate.Fails("1.1.1.1"); got != 0 {
		t.Fatalf("fails after success = %d, want 0", got)
	}
}

func TestWSURLConversion(t *testing.T) {
	got, err := WSURL("https://abc-def.trycloudflare.com")
	if err != nil {
		t.Fatal(err)
	}
	if got != "wss://abc-def.trycloudflare.com/ws" {
		t.Fatalf("WSURL = %q", got)
	}
	if _, err := WSURL("http://abc-def.trycloudflare.com"); err == nil {
		t.Fatal("expected an error for a non-https tunnel URL")
	}
}

func fakeRequest(ip, auth string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/ws", nil)
	r.Header.Set("Authorization", auth)
	r.Header.Set("CF-Connecting-IP", ip)
	return r
}

type ramCache struct{ token string }

func (c *ramCache) Get() string        { return c.token }
func (c *ramCache) Adopt(token string) { c.token = token }

type countingVerifier struct {
	calls  int
	reject bool
}

func (v *countingVerifier) VerifyToken(context.Context, string) error {
	v.calls++
	if v.reject {
		return ErrTokenRejected
	}
	return nil
}

func TestGateReusesCachedTokenWithoutAskingTheWorkerAgain(t *testing.T) {
	verifier := &countingVerifier{}
	cache := &ramCache{}
	gate := NewGate(GateConfig{Verify: verifier, Cache: cache})

	first := gate.Check(integrationRequest("good-token"))
	if !first.Allowed {
		t.Fatalf("first handshake = %+v, want allowed", first)
	}
	if verifier.calls != 1 {
		t.Fatalf("verifier calls after first handshake = %d, want 1", verifier.calls)
	}
	cache.Adopt("good-token")
	second := gate.Check(integrationRequest("good-token"))
	if !second.Allowed || second.Reason != "cached token" {
		t.Fatalf("second handshake = %+v, want allowed from cache", second)
	}
	if verifier.calls != 1 {
		t.Fatalf("verifier calls = %d, want the cached token to skip the Worker", verifier.calls)
	}
}

func TestGateStillRejectsOtherTokensWhenOneIsCached(t *testing.T) {
	verifier := &countingVerifier{reject: true}
	cache := &ramCache{token: "good-token"}
	gate := NewGate(GateConfig{Verify: verifier, Cache: cache})

	decision := gate.Check(integrationRequest("wrong-token"))
	if decision.Allowed || decision.Status != http.StatusUnauthorized {
		t.Fatalf("wrong token decision = %+v, want 401", decision)
	}
	if verifier.calls != 1 {
		t.Fatalf("verifier calls = %d, want the wrong token checked", verifier.calls)
	}
	for i := 0; i < defaultMaxFails-1; i++ {
		gate.Check(integrationRequest("wrong-token"))
	}
	locked := gate.Check(integrationRequest("wrong-token"))
	if locked.Status != http.StatusTooManyRequests {
		t.Fatalf("after %d failures status = %d, want 429", defaultMaxFails, locked.Status)
	}
}

func TestGateDoesNotCacheAnUnverifiedToken(t *testing.T) {
	verifier := &countingVerifier{reject: true}
	cache := &ramCache{}
	gate := NewGate(GateConfig{Verify: verifier, Cache: cache})

	if gate.Check(integrationRequest("guess")).Allowed {
		t.Fatal("rejected token was allowed")
	}
	if got := cache.Get(); got != "" {
		t.Fatalf("cache = %q, want it to stay empty", got)
	}
}
