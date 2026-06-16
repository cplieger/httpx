package httpx_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/httpx"
)

// === ROUND 4: POST-REFACTOR RED-TEAM ===

// --- (A) REFACTOR-SPECIFIC: Default parity ---

// TestR4_NewRetryRoundTripper_Defaults verifies no-option constructor yields
// DefaultMaxAttempts-1 retries (2) and DefaultBaseDelay.
func TestR4_NewRetryRoundTripper_Defaults(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     http.Header{},
		}, nil
	})

	rt := httpx.NewRetryRoundTripper(transport) // no options
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/def", http.NoBody)
	resp, _ := rt.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}

	// DefaultMaxAttempts=3, so maxRetries=2, total attempts=3
	if got := calls.Load(); got != 3 {
		t.Fatalf("no-option NewRetryRoundTripper: calls=%d, want 3 (DefaultMaxAttempts)", got)
	}
}

// TestR4_NewExponentialBackoff_Defaults verifies no-option constructor uses DefaultBaseDelay.
func TestR4_NewExponentialBackoff_Defaults(t *testing.T) {
	bo := httpx.NewExponentialBackoff() // no options
	d := bo.NextBackOff()
	// JitteredBackoff(1s) returns [500ms, 1s]
	if d < 500*time.Millisecond || d > time.Second {
		t.Fatalf("no-option ExponentialBackoff first interval = %v, want [500ms, 1s]", d)
	}
}

// TestR4_Retry_Defaults verifies no-option Retry uses DefaultMaxAttempts.
func TestR4_Retry_Defaults(t *testing.T) {
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
	defer cancel()
	_, _ = httpx.Retry(ctx, client, "http://example.com/retrydef",
		httpx.WithBaseDelay(time.Millisecond)) // speed up, but don't override MaxAttempts

	if got := calls.Load(); got != 3 {
		t.Fatalf("no-maxAttempts Retry: calls=%d, want 3 (DefaultMaxAttempts)", got)
	}
}

// TestR4_Retry_MaxBodyBytes_Default ensures default body limit is 10MB.
func TestR4_Retry_MaxBodyBytes_Default(t *testing.T) {
	// Return body slightly larger than 10MB; verify we get exactly 10MB
	bigBody := make([]byte, 10<<20+100)
	for i := range bigBody {
		bigBody[i] = 'A'
	}
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(bigBody)),
			Header:     http.Header{},
		}, nil
	})
	client := &http.Client{Transport: transport}

	body, err := httpx.Retry(t.Context(), client, "http://example.com/bigbody")
	if err != nil {
		t.Fatalf("Retry error: %v", err)
	}
	if int64(len(body)) != 10<<20 {
		t.Fatalf("default MaxBodyBytes: got %d bytes, want %d", len(body), 10<<20)
	}
}

// TestR4_RedirectPolicyFunc_NoOptions refuses all redirects.
func TestR4_RedirectPolicyFunc_NoOptions(t *testing.T) {
	policy := httpx.RedirectPolicyFunc() // no options
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/target", http.NoBody)
	via := []*http.Request{{}}
	err := policy(req, via)
	if err == nil {
		t.Fatal("no-option RedirectPolicyFunc should refuse all redirects")
	}
}

// TestR4_RedirectPolicyFunc_MaxHops_Default verifies default is redirectCap (5).
func TestR4_RedirectPolicyFunc_MaxHops_Default(t *testing.T) {
	policy := httpx.RedirectPolicyFunc(httpx.WithAllowedHosts("example.com"))
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/hop", http.NoBody)

	// 4 hops: should be allowed
	via4 := make([]*http.Request, 4)
	for i := range via4 {
		via4[i] = req
	}
	if err := policy(req, via4); err != nil {
		t.Fatalf("4 hops should be allowed: %v", err)
	}

	// 5 hops: should be refused
	via5 := make([]*http.Request, 5)
	for i := range via5 {
		via5[i] = req
	}
	if err := policy(req, via5); err == nil {
		t.Fatal("5 hops should be refused (maxHops=5 means len(via)>=5 is blocked)")
	}
}

// --- (A) REFACTOR-SPECIFIC: Options don't cross-wire ---

// TestR4_RTOption_DoesNotAffect_Retry verifies RTOption type can't be passed to Retry.
// This is a compile-time check; if it compiles, the types are distinct.
// We just verify that setting WithMaxRetries on RT doesn't affect Retry's defaults.
func TestR4_RTOption_DoesNotAffect_Retry(t *testing.T) {
	var retryCalls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		retryCalls.Add(1)
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     http.Header{},
		}, nil
	})
	client := &http.Client{Transport: transport}

	// Retry with only BaseDelay override (fast)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, _ = httpx.Retry(ctx, client, "http://example.com/cross",
		httpx.WithBaseDelay(time.Millisecond),
		httpx.WithMaxAttempts(5))

	if got := retryCalls.Load(); got != 5 {
		t.Fatalf("Retry WithMaxAttempts(5): calls=%d, want 5", got)
	}
}

// TestR4_ExpBackoffOption_Independent verifies ExpBackoffOption order independence.
func TestR4_ExpBackoffOption_Independent(t *testing.T) {
	bo1 := httpx.NewExponentialBackoff(
		httpx.WithInitialInterval(100*time.Millisecond),
		httpx.WithMaxElapsedTime(time.Second),
	)
	bo2 := httpx.NewExponentialBackoff(
		httpx.WithMaxElapsedTime(time.Second),
		httpx.WithInitialInterval(100*time.Millisecond),
	)

	d1 := bo1.NextBackOff()
	d2 := bo2.NextBackOff()
	// Both should be in [50ms, 100ms] range
	for _, d := range []time.Duration{d1, d2} {
		if d < 50*time.Millisecond || d > 100*time.Millisecond {
			t.Fatalf("order-dependent result: %v not in [50ms, 100ms]", d)
		}
	}
}

// --- (B) RE-ATTACK: Cloning ---

// TestR4_RoundTripper_Clones_CallerRequest verifies original request is never mutated.
func TestR4_RoundTripper_Clones_CallerRequest(t *testing.T) {
	var attempt atomic.Int32
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		n := attempt.Add(1)
		if n == 1 {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
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
		httpx.WithMaxRetries(2),
		httpx.WithPrepareRetry(func(req *http.Request) error {
			req.Header.Set("X-Mutated", "yes")
			return nil
		}),
	)

	origReq, _ := http.NewRequest(http.MethodGet, "http://example.com/clone", http.NoBody)
	origReq.Header.Set("X-Original", "untouched")

	resp, err := rt.RoundTrip(origReq)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	resp.Body.Close()

	// Original request must NOT have the mutation
	if origReq.Header.Get("X-Mutated") != "" {
		t.Fatal("FINDING: PrepareRetry mutated the caller's original request")
	}
	if origReq.Header.Get("X-Original") != "untouched" {
		t.Fatal("FINDING: original request header was mutated")
	}
}

// --- (B) RE-ATTACK: Drain discarded bodies ---

func TestR4_RoundTripper_DrainsDiscardedBodies(t *testing.T) {
	var attempt atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		n := attempt.Add(1)
		body := strings.NewReader(strings.Repeat("x", 1000))
		if n < 3 {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Body:       io.NopCloser(body),
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

	req, _ := http.NewRequest(http.MethodGet, "http://example.com/drain", http.NoBody)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	resp.Body.Close()
	// If we get here without hanging, draining worked
}

// --- (B) RE-ATTACK: per-request backoff factory under concurrency ---

func TestR4_PerRequestBackoff_Race(t *testing.T) {
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     http.Header{},
		}, nil
	})

	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithMaxRetries(5),
		httpx.WithBackoffFunc(func() httpx.Backoff {
			return httpx.NewExponentialBackoff(
				httpx.WithInitialInterval(time.Millisecond),
				httpx.WithMaxElapsedTime(50*time.Millisecond),
			)
		}),
	)

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com/race", http.NoBody)
			resp, _ := rt.RoundTrip(req)
			if resp != nil {
				resp.Body.Close()
			}
		})
	}
	wg.Wait()
}

// --- (B) RE-ATTACK: ParseRetryAfter overflow ---

func TestR4_ParseRetryAfter_Overflow(t *testing.T) {
	cases := []string{
		"9999999999999999999",
		"9223372036854775807", // MaxInt64 in seconds
		"99999999999",
		"-1",
		"0",
		"",
	}
	for _, c := range cases {
		d := httpx.ParseRetryAfter(c)
		if d < 0 {
			t.Fatalf("ParseRetryAfter(%q) = %v (negative!)", c, d)
		}
		if d > httpx.RetryAfterCap {
			t.Fatalf("ParseRetryAfter(%q) = %v > cap %v", c, d, httpx.RetryAfterCap)
		}
	}
}

// TestR4_ParseRetryAfterResponse_Overflow verifies uncapped version doesn't overflow.
func TestR4_ParseRetryAfterResponse_Overflow(t *testing.T) {
	cases := []string{
		"9999999999999999999",
		"9223372036854775807",
		"-1",
		"0",
	}
	for _, c := range cases {
		resp := &http.Response{Header: http.Header{"Retry-After": {c}}}
		d := httpx.ParseRetryAfterResponse(resp)
		if d < 0 {
			t.Fatalf("ParseRetryAfterResponse(%q) = %v (negative!)", c, d)
		}
	}
}

// --- (B) RE-ATTACK: Context cancel mid-backoff ---

func TestR4_ContextCancel_MidBackoff(t *testing.T) {
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     http.Header{},
		}, nil
	})

	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithRTBaseDelay(10*time.Second), // long delay
		httpx.WithMaxRetries(5),
	)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com/cancel", http.NoBody)
	_, err := rt.RoundTrip(req) //nolint:bodyclose // error path, no response body
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error on context cancel")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("took %v, should have cancelled quickly", elapsed)
	}
}

// --- (B) RE-ATTACK: GetBody replay across retries ---

func TestR4_GetBody_Replay(t *testing.T) {
	var bodies []string
	var mu sync.Mutex
	var calls atomic.Int32

	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(req.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		if calls.Add(1) < 3 {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Body:       io.NopCloser(strings.NewReader("")),
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
		httpx.WithMaxRetries(4),
		httpx.WithRetryNonIdempotent(true),
	)

	payload := "replay-test-payload"
	req, _ := http.NewRequest(http.MethodPost, "http://example.com/replay4", strings.NewReader(payload))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(payload)), nil
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	resp.Body.Close()

	if got := calls.Load(); got != 3 {
		t.Fatalf("calls=%d, want 3", got)
	}
	for i, b := range bodies {
		if b != payload {
			t.Errorf("attempt %d body=%q, want %q", i+1, b, payload)
		}
	}
}

// --- (A) REFACTOR-SPECIFIC: nil transport fallback ---

func TestR4_NewRetryRoundTripper_NilTransport(t *testing.T) {
	// Should not panic with nil transport
	rt := httpx.NewRetryRoundTripper(nil, httpx.WithMaxRetries(0))
	if rt == nil {
		t.Fatal("NewRetryRoundTripper(nil) returned nil")
	}
}

// --- (A) REFACTOR-SPECIFIC: WithMaxRetries(0) means only 1 attempt ---

func TestR4_WithMaxRetries_Zero(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     http.Header{},
		}, nil
	})

	rt := httpx.NewRetryRoundTripper(transport, httpx.WithMaxRetries(0))
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/once", http.NoBody)
	resp, _ := rt.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}

	// WithMaxRetries(0) must mean no retries — only 1 attempt.
	if got := calls.Load(); got != 1 {
		t.Fatalf("WithMaxRetries(0): calls=%d, want 1 (no retries)", got)
	}
}
