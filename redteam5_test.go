package httpx_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/httpx"
)

// === ROUND 5 CONVERGENCE: Final adversarial sweep ===

// --- req.Clone deep isolation: verify Header map is not shared ---

func TestR5_Clone_HeaderMapIsolation(t *testing.T) {
	var attempt atomic.Int32
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		n := attempt.Add(1)
		// On retry attempts, verify the header is NOT inherited from prior attempt mutations
		if n > 1 {
			if req.Header.Get("X-Attempt-Marker") != "" {
				// This would be a bug: mutations from transport leak across retries
				t.Error("FINDING: header mutation from transport leaked across retries")
			}
		}
		// Mutate the header in transport (simulating something reading it)
		req.Header.Set("X-Attempt-Marker", "set-by-transport")

		if n < 3 {
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     http.Header{},
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Header:     http.Header{},
		}, nil
	})

	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithMaxRetries(3),
	)
	origReq, _ := http.NewRequest(http.MethodGet, "http://example.com/headerisolation", http.NoBody)
	resp, err := rt.RoundTrip(origReq)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()

	// Original request must remain clean
	if origReq.Header.Get("X-Attempt-Marker") != "" {
		t.Fatal("FINDING: transport-side header mutation leaked to original request")
	}
}

// --- Per-request backoff factory under aggressive concurrency ---

func TestR5_PerRequestBackoff_Aggressive(t *testing.T) {
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     http.Header{},
		}, nil
	})

	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithMaxRetries(10),
		httpx.WithBackoffFunc(func() httpx.Backoff {
			return httpx.NewExponentialBackoff(
				httpx.WithInitialInterval(time.Millisecond),
				httpx.WithMaxElapsedTime(20*time.Millisecond),
			)
		}),
	)

	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com/agg-race", http.NoBody)
			resp, _ := rt.RoundTrip(req)
			if resp != nil {
				resp.Body.Close()
			}
		})
	}
	wg.Wait()
}

// --- ParseRetryAfter boundary: exactly at cap boundary ---

func TestR5_ParseRetryAfter_ExactBoundary(t *testing.T) {
	// Exactly 60 (cap) — should return cap
	d := httpx.ParseRetryAfter("60")
	if d != httpx.RetryAfterCap {
		t.Fatalf("ParseRetryAfter(60) = %v, want %v", d, httpx.RetryAfterCap)
	}
	// 61 — should return cap
	d = httpx.ParseRetryAfter("61")
	if d != httpx.RetryAfterCap {
		t.Fatalf("ParseRetryAfter(61) = %v, want %v", d, httpx.RetryAfterCap)
	}
	// 59 — should return 59s
	d = httpx.ParseRetryAfter("59")
	if d != 59*time.Second {
		t.Fatalf("ParseRetryAfter(59) = %v, want %v", d, 59*time.Second)
	}
}

// --- WithMaxRetries(0) with WithCheckRetry custom policy ---

func TestR5_WithMaxRetries_Zero_CustomCheckRetry(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, io.ErrUnexpectedEOF // transient error
	})

	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithMaxRetries(0),
		httpx.WithCheckRetry(func(_ context.Context, _ *http.Response, _ error) (bool, error) {
			return true, nil // always retry — but maxRetries=0 should prevent it
		}),
	)
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/zero-custom", http.NoBody)
	_, err := rt.RoundTrip(req) //nolint:bodyclose // error path, no response body
	if err == nil {
		t.Fatal("expected error with 0 retries")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("WithMaxRetries(0)+custom CheckRetry: calls=%d, want 1", got)
	}
}

// --- Body drain: verify huge body doesn't block retry (cap at drainLimit) ---

func TestR5_Drain_HugeBody_NoBlock(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		n := calls.Add(1)
		if n < 2 {
			// Return a "huge" body (1MB) that the drainer should cap
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Body:       io.NopCloser(io.LimitReader(neverEnding('x'), 1<<20)),
				Header:     http.Header{},
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("done")),
			Header:     http.Header{},
		}, nil
	})

	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithMaxRetries(2),
	)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com/hugedrain", http.NoBody)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	resp.Body.Close()
}

// neverEnding is an io.Reader that yields b forever.
type neverEnding byte

func (b neverEnding) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

// --- Retry function: WithMaxAttempts(0) and negative should fallback to default ---

func TestR5_Retry_MaxAttempts_ZeroAndNegative(t *testing.T) {
	for _, maxAttempts := range []int{0, -1, -100} {
		var calls atomic.Int32
		transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			calls.Add(1)
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     http.Header{},
			}, nil
		})
		client := &http.Client{Transport: transport}
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		_, _ = httpx.Retry(ctx, client, "http://example.com/retry-edge",
			httpx.WithBaseDelay(time.Millisecond),
			httpx.WithMaxAttempts(maxAttempts))
		cancel()
		// Should fallback to DefaultMaxAttempts (3)
		if got := calls.Load(); got != 3 {
			t.Fatalf("WithMaxAttempts(%d): calls=%d, want 3 (default)", maxAttempts, got)
		}
	}
}

// --- MaxElapsedTime: verify it aborts instead of continuing ---

func TestR5_MaxElapsedTime_Abort(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls.Add(1)
		time.Sleep(5 * time.Millisecond)
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     http.Header{},
		}, nil
	})

	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithMaxRetries(100), // lots of retries allowed
		httpx.WithRTMaxElapsedTime(15*time.Millisecond),
	)

	req, _ := http.NewRequest(http.MethodGet, "http://example.com/elapsed", http.NoBody)
	_, err := rt.RoundTrip(req) //nolint:bodyclose // error path, no response body
	if err == nil {
		t.Fatal("expected max elapsed time error")
	}
	// Should have made only a few calls, not 100
	if got := calls.Load(); got > 10 {
		t.Fatalf("MaxElapsedTime didn't abort early: %d calls", got)
	}
}
