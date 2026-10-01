package sdk

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// A pool of several keys was held in memory and never turned: RotateAPIKey had no
// caller outside tests, so one dead key ended the whole turn even though the next
// key would have worked (CHANGE-077).

type statusError struct{ code int }

func (e statusError) Error() string       { return http.StatusText(e.code) }
func (e statusError) HTTPStatusCode() int { return e.code }
func (e statusError) Temporary() bool     { return true }

// keyRecordingProvider answers a 401 for the first keys and then works, so the
// test can see which keys the turn was actually presented with.
type keyRecordingProvider struct {
	refuseFirst int
	asked       []string
	calls       int
}

func (p *keyRecordingProvider) Name() string { return "test" }

func (p *keyRecordingProvider) Generate(_ context.Context, _ Request) (Response, error) {
	p.calls++
	if p.calls <= p.refuseFirst {
		return Response{}, statusError{code: http.StatusUnauthorized}
	}
	return Response{Content: []ContentPart{{Type: ContentText, Text: "answered"}}}, nil
}

func (p *keyRecordingProvider) Stream(context.Context, Request) (<-chan Event, error) {
	return nil, errors.New("not implemented")
}
func (p *keyRecordingProvider) WithAPIKey(string) Provider { return p }

// rotatingSession wires a provider that reads the key the session would send, so
// the rotation is observable rather than assumed.
func rotatingSession(t *testing.T, refuseFirst int, keys ...string) (*Agent, *keyRecordingProvider, *Session) {
	t.Helper()
	p := &keyRecordingProvider{refuseFirst: refuseFirst}
	pool := NewKeyPool(keys...)
	r := NewRouter()
	r.RegisterProvider(ProviderConfig{ID: "test", BaseURL: "http://test", Keys: pool, Adapter: AdapterOpenAI})
	r.Register(ModelRoute{Provider: "test", Model: "model", Adapter: AdapterOpenAI})
	c := NewRouterClient(r)
	c.RegisterAdapter(AdapterOpenAI, keyWatching{inner: p})
	s := NewSession(SessionConfig{ID: "s", Provider: "test", Model: "model", KeyIndex: 0}, pool)
	return &Agent{Client: c, Tools: &agentTestTools{}, MaxRetries: 6, DisablePlanning: true}, p, s
}

// keyWatching records the key the router handed it, which is how a real adapter
// learns the credential, so the rotation is observed rather than assumed.
type keyWatching struct {
	inner *keyRecordingProvider
	key   string
}

func (k keyWatching) Name() string { return "test" }
func (k keyWatching) Generate(ctx context.Context, req Request) (Response, error) {
	k.inner.asked = append(k.inner.asked, k.key)
	return k.inner.Generate(ctx, req)
}
func (k keyWatching) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	return k.inner.Stream(ctx, req)
}
func (k keyWatching) WithAPIKey(key string) Provider {
	return keyWatching{inner: k.inner, key: key}
}

func TestARejectedKeyMovesOnToTheNextOne(t *testing.T) {
	a, p, s := rotatingSession(t, 2, "dead-1", "dead-2", "live")
	resp, err := a.RunTurn(context.Background(), s, Turn{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: "hi"}}}, Request{})
	if err != nil {
		t.Fatalf("the turn failed instead of moving past the dead keys: %v", err)
	}
	if len(resp.Content) == 0 {
		t.Fatal("no answer came back")
	}
	if len(p.asked) != 3 {
		t.Fatalf("the turn was presented with %d keys: %v", len(p.asked), p.asked)
	}
	if p.asked[2] != "live" {
		t.Fatalf("the working key was never reached: %v", p.asked)
	}
}

// Every key is tried once; the turn then fails for real rather than cycling.
func TestRotationStopsOnceEveryKeyHasBeenTried(t *testing.T) {
	a, p, s := rotatingSession(t, 2, "dead-1", "dead-2")
	_, err := a.RunTurn(context.Background(), s, Turn{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: "hi"}}}, Request{})
	if err == nil {
		t.Fatal("the turn succeeded although every key refused it")
	}
	if len(p.asked) != 2 {
		t.Fatalf("keys presented = %d (%v), want one attempt per key", len(p.asked), p.asked)
	}
}

// A 403 is a verdict about the caller or the tier, not about the key. REQ-048(9)
// records that every retry spends quota the tier is already refusing.
func TestAForbiddenRequestIsNotRetriedOnAnotherKey(t *testing.T) {
	a, _, s := rotatingSession(t, 0, "live-1", "live-2")
	forbidAll := &forbiddingProvider{}
	a.Client.RegisterAdapter(AdapterOpenAI, forbidAll)
	_, err := a.RunTurn(context.Background(), s, Turn{Role: RoleUser, Content: []ContentPart{{Type: ContentText, Text: "hi"}}}, Request{})
	if err == nil {
		t.Fatal("a forbidden request was reported as a success")
	}
	if forbidAll.calls != 1 {
		t.Fatalf("the forbidden request was tried %d times", forbidAll.calls)
	}
}

type forbiddingProvider struct{ calls int }

func (f *forbiddingProvider) Name() string { return "test" }
func (f *forbiddingProvider) Generate(context.Context, Request) (Response, error) {
	f.calls++
	return Response{}, statusError{code: http.StatusForbidden}
}
func (f *forbiddingProvider) Stream(context.Context, Request) (<-chan Event, error) {
	return nil, errors.New("not implemented")
}
func (f *forbiddingProvider) WithAPIKey(string) Provider { return f }

// The decision itself, independent of the loop.
func TestKeyRejectionIsToldApartFromAPolicyRefusal(t *testing.T) {
	if !isKeyRejection(statusError{code: http.StatusUnauthorized}) {
		t.Error("a 401 was not treated as a rejected key")
	}
	for _, code := range []int{http.StatusForbidden, http.StatusTooManyRequests, http.StatusNotFound, http.StatusInternalServerError} {
		if isKeyRejection(statusError{code: code}) {
			t.Errorf("a %d was treated as a rejected key", code)
		}
	}
	if isKeyRejection(errors.New("no status at all")) {
		t.Error("an error with no status was treated as a rejected key")
	}
}

func TestASessionWithOneKeyHasNothingToRotate(t *testing.T) {
	session := newRotatingSession(t, NewKeyPool("only"))
	defer session.Close()
	if rotateToNextKey(session, 1) {
		t.Error("a single-key pool reported somewhere to rotate to")
	}
}

func TestASessionWithNoPoolHasNothingToRotate(t *testing.T) {
	if rotateToNextKey(&Session{}, 1) {
		t.Error("a session with no key pool reported somewhere to rotate to")
	}
}

func TestRetryableStatusExcludesTheRefusals(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		if RetryableHTTPStatus(code) {
			t.Errorf("a %d was called retryable", code)
		}
	}
	for _, code := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway} {
		if !RetryableHTTPStatus(code) {
			t.Errorf("a %d was called final", code)
		}
	}
}

func newRotatingSession(t *testing.T, pool *KeyPool) *Session {
	t.Helper()
	session, err := OpenSession(t.TempDir()+"/s.db", SessionConfig{ID: "s1", Provider: ProviderOpenRouter, Model: "m"}, pool)
	if err != nil {
		t.Fatal(err)
	}
	return session
}
