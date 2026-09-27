package sdk

import (
	"context"
	"errors"
	"testing"
	"time"
)

type retryTestError struct {
	status int
	delay  time.Duration
}

func (e retryTestError) Error() string             { return "rate limited" }
func (e retryTestError) HTTPStatusCode() int       { return e.status }
func (e retryTestError) RetryAfter() time.Duration { return e.delay }

func TestRetryDelayUsesProviderRetryAfter(t *testing.T) {
	if got := retryDelay(retryTestError{status: 429, delay: 5 * time.Second}, 1); got != 5*time.Second {
		t.Fatalf("provider Retry-After delay=%s, want 5s", got)
	}
	if got := retryDelay(retryTestError{status: 429, delay: 120 * time.Second}, 1); got != 96*time.Second {
		t.Fatalf("provider Retry-After delay=%s, want 96s cap", got)
	}
}

func TestRetryDelayFallsBackToDeterministicSchedule(t *testing.T) {
	want := []time.Duration{
		3 * time.Second,
		6 * time.Second,
		12 * time.Second,
		24 * time.Second,
		48 * time.Second,
		96 * time.Second,
		96 * time.Second,
	}
	for attempt, expected := range want {
		if got := retryDelay(retryTestError{status: 429}, attempt+1); got != expected {
			t.Fatalf("attempt %d delay=%s, want %s", attempt+1, got, expected)
		}
	}
}

func TestRetryBackoffResetsAfterSuccessfulRequest(t *testing.T) {
	backoff := retryBackoff{}
	if got := backoff.Delay(retryTestError{status: 429}); got != 3*time.Second {
		t.Fatalf("first retry delay=%s, want 3s", got)
	}
	if got := backoff.Delay(retryTestError{status: 429}); got != 6*time.Second {
		t.Fatalf("second retry delay=%s, want 6s", got)
	}
	backoff.Reset()
	if got := backoff.Delay(retryTestError{status: 429}); got != 3*time.Second {
		t.Fatalf("retry delay after success=%s, want 3s", got)
	}
}

func TestRateLimitDetection(t *testing.T) {
	if !isRateLimitError(retryTestError{status: 429}) {
		t.Fatal("429 was not detected")
	}
	if isRateLimitError(retryTestError{status: 500}) {
		t.Fatal("500 was incorrectly treated as rate limit")
	}
}

// A refused key or a refused client is an answer the provider repeats, so a turn
// must report it instead of retrying it (OpenCode Zen answers a free-tier
// request from outside its own client with 403 and counts every retry).
func TestPolicyRefusalIsNotRetried(t *testing.T) {
	for _, status := range []int{401, 403} {
		err := retryTestError{status: status}
		if !isPolicyRefusal(err) {
			t.Fatalf("%d was not detected as a policy refusal", status)
		}
		if retryableAgentError(context.Background(), err) {
			t.Fatalf("%d was retried, want the turn to report it", status)
		}
	}
	for _, status := range []int{429, 500, 502} {
		if isPolicyRefusal(retryTestError{status: status}) {
			t.Fatalf("%d was treated as a policy refusal", status)
		}
	}
	if !retryableAgentError(context.Background(), retryTestError{status: 500}) {
		t.Fatal("500 must stay retryable")
	}
	if isPolicyRefusal(errors.New("plain failure")) {
		t.Fatal("an error without a status was treated as a policy refusal")
	}
}

func TestWaitRetryHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitRetry(ctx, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestRetryAfterAbortOnDistantRetryAfter(t *testing.T) {
	if !retryAfterAbort(retryTestError{status: 429, delay: 5 * time.Hour}) {
		t.Fatal("5h Retry-After should stop retrying")
	}
	if !retryAfterAbort(retryTestError{status: 429, delay: 24 * time.Hour}) {
		t.Fatal("24h Retry-After should stop retrying")
	}
	if retryAfterAbort(retryTestError{status: 429, delay: 30 * time.Second}) {
		t.Fatal("30s Retry-After should keep retrying")
	}
	if retryAfterAbort(retryTestError{status: 429}) {
		t.Fatal("missing Retry-After should keep retrying")
	}
	if retryAfterAbort(retryTestError{status: 500, delay: 5 * time.Hour}) {
		t.Fatal("Retry-After on non-429 should not abort")
	}
	if retryAfterAbort(nil) {
		t.Fatal("nil error should not abort")
	}
}
