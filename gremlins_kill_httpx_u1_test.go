package httpx

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// errGkHttpxU1Sentinel is the "prior attempt" error handed to
// sleepBeforeRetry. The original code returns the context error from the
// interrupted sleep (not this sentinel); we assert against the context error,
// so this value only documents that the sleep error is what surfaces.
var errGkHttpxU1Sentinel = errors.New("gk_httpx_u1 prior-attempt failure")

// TestGkHttpxU1_sleepBeforeRetry_surfaces_sleep_error kills the
// CONDITIONALS_NEGATION mutant at roundtripper.go:325:47:
//
//	if sleepErr := SleepCtx(ctx, wait); sleepErr != nil {  // != -> ==
//		return sleepErr
//	}
//	return nil
//
// The unexported sleepBeforeRetry is called directly (internal test package)
// with an ALREADY-CANCELLED context and a positive backoff. JitteredBackoff
// yields wait > 0, so SleepCtx enters its select, observes the closed
// ctx.Done() immediately (the timer never fires), and returns context.Canceled.
//
//   - original (`sleepErr != nil`): true  -> returns context.Canceled (non-nil)
//   - mutant   (`sleepErr == nil`): false -> falls through -> returns nil
//
// Asserting the result IS the cancellation fails only against the mutant.
//
// rt is the zero value, so maxElapsedTime==0 (the `> 0` guard short-circuits
// the timing-dependent L295 check) and onRetry/resp/bo are nil — the only
// branch with an observable effect here is L325, which isolates this mutant.
// This is deterministic: a pre-cancelled context makes SleepCtx return without
// waiting for the ~500ms-1s timer, so there is no scheduler race.
func TestGkHttpxU1_sleepBeforeRetry_surfaces_sleep_error(t *testing.T) {
	rt := &RetryRoundTripper{} // zero value: maxElapsedTime=0, onRetry=nil

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel: SleepCtx returns ctx.Err() immediately

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://gk-httpx-u1.invalid", http.NoBody)
	if err != nil {
		t.Fatalf("setup: NewRequestWithContext = %v, want nil", err)
	}

	got := rt.sleepBeforeRetry(ctx, 1, req, nil, errGkHttpxU1Sentinel, time.Second, nil, time.Now())

	if got == nil {
		t.Fatalf("sleepBeforeRetry(cancelled ctx) = nil, want context cancellation error")
	}
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("sleepBeforeRetry(cancelled ctx) = %v, want errors.Is(err, context.Canceled)", got)
	}
}

func TestRetryRoundTripper_transport_defaults_when_next_nil(t *testing.T) {
	rt := &RetryRoundTripper{} // zero value: next == nil
	if got := rt.transport(); got != http.DefaultTransport {
		t.Errorf("(&RetryRoundTripper{}).transport() = %v, want http.DefaultTransport", got)
	}
}
