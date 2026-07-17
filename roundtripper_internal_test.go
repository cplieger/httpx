package httpx

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// White-box tests for RetryRoundTripper unexported helpers that are not
// reachable through the public constructor (NewRetryRoundTripper always sets a
// transport), so they can only be verified against the package internals.

// TestRetryRoundTripper_sleepBeforeRetry_surfaces_sleep_error pins that an
// interrupted backoff sleep aborts the pre-retry step by returning the context
// error. The unexported helper is called directly with an already-cancelled
// context and a positive backoff, so SleepCtx returns context.Canceled without
// waiting for the timer (deterministic, no scheduler race).
func TestRetryRoundTripper_sleepBeforeRetry_surfaces_sleep_error(t *testing.T) {
	rt := &RetryRoundTripper{} // zero value: maxElapsedTime=0, onRetry=nil

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.invalid", http.NoBody)
	if err != nil {
		t.Fatalf("setup: NewRequestWithContext = %v, want nil", err)
	}

	got := rt.sleepBeforeRetry(ctx, 1, req, nil, errors.New("prior attempt failed"), time.Second, time.Now())
	if got == nil {
		t.Fatal("sleepBeforeRetry(cancelled ctx) = nil, want context cancellation error")
	}
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("sleepBeforeRetry(cancelled ctx) = %v, want errors.Is(err, context.Canceled)", got)
	}
}

// TestRetryRoundTripper_transport_defaults_when_next_nil pins the nil-transport
// fallback in transport(): a zero-value RetryRoundTripper (next == nil) routes
// through http.DefaultTransport.
func TestRetryRoundTripper_transport_defaults_when_next_nil(t *testing.T) {
	rt := &RetryRoundTripper{} // zero value: next == nil
	if got := rt.transport(); got != http.DefaultTransport {
		t.Errorf("(&RetryRoundTripper{}).transport() = %v, want http.DefaultTransport", got)
	}
}
