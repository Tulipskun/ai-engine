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

// REQ-046(2): two-step access. The tunnel hostname is step one; the Cloudflare
// token on the handshake is step two, verified before the socket is upgraded.
// The daemon owns no credential of its own (CON-012), so these cases pin the
// whole policy: what counts, what locks, and what must never be written down.

type stubVerifier struct {
	token string
	calls int
	err   error
}

func (s *stubVerifier) VerifyToken(_ context.Context, token string) error {
	s.calls++
	if s.err != nil {
		return s.err
	}
	if token == s.token {
		return nil
	}
	return ErrTokenRejected
}

type stubCache struct{ token string }

func (s *stubCache) Get() string        { return s.token }
func (s *stubCache) Adopt(token string) { s.token = token }

func newRequest(token, addr string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/ws", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.RemoteAddr = addr
	return r
}

func TestGateRejectsAMissingOrMalformedHeader(t *testing.T) {
	verifier := &stubVerifier{token: "good"}
	gate := NewGate(GateConfig{Verify: verifier, Cache: &stubCache{}})

	withHeader := func(value string) *http.Request {
		r := newRequest("", "10.0.0.1:1")
		r.Header.Set("Authorization", value)
		return r
	}
	for name, r := range map[string]*http.Request{
		"missing header":  newRequest("", "10.0.0.1:1"),
		"no scheme":       withHeader("good"),
		"wrong scheme":    withHeader("Basic good"),
		"no token":        withHeader("Bearer "),
		"only whitespace": withHeader("Bearer    "),
	} {
		decision := gate.Check(r)
		if decision.Allowed {
			t.Errorf("%s: a handshake without a usable header must be refused", name)
		}
		if decision.Status != http.StatusUnauthorized {
			t.Errorf("%s: want 401, got %d", name, decision.Status)
		}
	}
	// Neither shape is a wrong credential, so neither may count toward a lockout.
	if verifier.calls != 0 {
		t.Errorf("a malformed header must not reach the verifier, got %d calls", verifier.calls)
	}
	if len(gate.fails) != 0 {
		t.Errorf("a missing header must not count as a failed attempt, got %v", gate.fails)
	}
}

func TestGateAcceptsAVerifiedTokenAndTrustsTheCachedOne(t *testing.T) {
	verifier := &stubVerifier{token: "good"}
	cache := &stubCache{}
	gate := NewGate(GateConfig{Verify: verifier, Cache: cache})

	decision := gate.Check(newRequest("good", "10.0.0.2:1"))
	if !decision.Allowed {
		t.Fatalf("a verified token must be allowed: %+v", decision)
	}
	if decision.Token != "good" {
		t.Errorf("the accepted token must be handed to the transport, got %q", decision.Token)
	}

	// A token already verified once is trusted without another Cloudflare round
	// trip, so a phone keeps working through a verifier outage (REQ-046(3)).
	cache.Adopt("good")
	before := verifier.calls
	if again := gate.Check(newRequest("good", "10.0.0.3:1")); !again.Allowed {
		t.Fatalf("a cached token must be allowed: %+v", again)
	}
	if verifier.calls != before {
		t.Error("a cached token must not be verified again")
	}
}

func TestGateLocksOutAfterRepeatedWrongTokens(t *testing.T) {
	verifier := &stubVerifier{token: "good"}
	gate := NewGate(GateConfig{Verify: verifier, Cache: &stubCache{}})
	const addr = "10.0.0.9:1"

	for i := 1; i < defaultMaxFails; i++ {
		decision := gate.Check(newRequest("wrong", addr))
		if decision.Status != http.StatusUnauthorized {
			t.Fatalf("attempt %d: want 401, got %d", i, decision.Status)
		}
	}
	decision := gate.Check(newRequest("wrong", addr))
	if decision.Status != http.StatusTooManyRequests {
		t.Fatalf("attempt %d must lock the address out, got %d", defaultMaxFails, decision.Status)
	}
	if decision.RetryAfter <= 0 {
		t.Error("a lockout must say how long to wait")
	}

	// The lockout is on the address, so even the correct token is now refused
	// without being verified.
	before := verifier.calls
	locked := gate.Check(newRequest("good", addr))
	if locked.Allowed || locked.Status != http.StatusTooManyRequests {
		t.Errorf("a locked address must stay locked, got %+v", locked)
	}
	if verifier.calls != before {
		t.Error("a locked address must not reach the verifier")
	}
	// A different address is unaffected: one client cannot lock out another.
	if other := gate.Check(newRequest("good", "10.0.0.10:1")); !other.Allowed {
		t.Errorf("another address must not be locked out, got %+v", other)
	}
}

func TestGatePenaltyScheduleIsProgressiveAndCapped(t *testing.T) {
	gate := NewGate(GateConfig{Verify: &stubVerifier{}, Cache: &stubCache{}})
	want := []time.Duration{
		30 * time.Second, 60 * time.Second, 120 * time.Second,
		240 * time.Second, 300 * time.Second, 300 * time.Second,
	}
	for i, expect := range want {
		fails := defaultMaxFails + i
		if got := gate.PenaltyFor(fails); got != expect {
			t.Errorf("PenaltyFor(%d) = %v, want %v", fails, got, expect)
		}
	}
	// Below the threshold the base penalty applies and never doubles.
	if got := gate.PenaltyFor(1); got != 30*time.Second {
		t.Errorf("PenaltyFor(1) = %v, want the base penalty", got)
	}
}

func TestGateFailsClosedWhenTheVerifierIsUnreachable(t *testing.T) {
	// A verifier outage is not a wrong credential: it must be 503, must not count,
	// and must never let an unverified socket through (fail closed, REQ-046(2)).
	gate := NewGate(GateConfig{Verify: &stubVerifier{err: errors.New("cloudflare unreachable")}, Cache: &stubCache{}})
	decision := gate.Check(newRequest("anything", "10.0.0.11:1"))
	if decision.Allowed {
		t.Fatal("an unverifiable token must not be allowed")
	}
	if decision.Status != http.StatusServiceUnavailable {
		t.Errorf("want 503, got %d", decision.Status)
	}
	if len(gate.fails) != 0 {
		t.Errorf("an outage must not count as a failed attempt, got %v", gate.fails)
	}
}

func TestGateCountsPerClientAddressBehindTheTunnel(t *testing.T) {
	// After the tunnel the peer is always local cloudflared, so the real client is
	// taken from the forwarding headers instead.
	first := newRequest("x", "127.0.0.1:5000")
	first.Header.Set("CF-Connecting-IP", "203.0.113.7")
	second := newRequest("x", "127.0.0.1:5000")
	second.Header.Set("X-Forwarded-For", "203.0.113.8, 10.0.0.1")

	one, two := clientKey(first), clientKey(second)
	if one != "203.0.113.7" {
		t.Errorf("CF-Connecting-IP must win, got %q", one)
	}
	if two != "203.0.113.8" {
		t.Errorf("the first X-Forwarded-For entry must be used, got %q", two)
	}
	if one == two {
		t.Error("two clients must not share one lockout counter")
	}
}

func TestGateRejectionBodyNeverEchoesTheToken(t *testing.T) {
	verifier := &stubVerifier{token: "good"}
	gate := NewGate(GateConfig{Verify: verifier, Cache: &stubCache{}})
	for i := 0; i < defaultMaxFails; i++ {
		gate.Check(newRequest("super-secret-token", "10.0.0.12:1"))
	}
	recorder := httptest.NewRecorder()
	gate.Write(recorder, Decision{Status: http.StatusTooManyRequests, Reason: "token rejected", RetryAfter: 30 * time.Second})

	body := recorder.Body.String()
	if strings.Contains(body, "super-secret-token") {
		t.Errorf("the rejection body must never echo the token, got %q", body)
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Error("a lockout response must carry Retry-After")
	}
}

func TestWSURLRequiresAnHTTPSQuickTunnel(t *testing.T) {
	got, err := WSURL("https://random-words.trycloudflare.com")
	if err != nil {
		t.Fatalf("WSURL: %v", err)
	}
	if got != "wss://random-words.trycloudflare.com/ws" {
		t.Errorf("unexpected websocket URL %q", got)
	}
	if _, err := WSURL("http://insecure.example"); err == nil {
		t.Error("a non-https tunnel URL must be refused")
	}
}
