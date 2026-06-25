package httpx_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/httpx/v2"
)

// === v2 API: total-attempts semantics, hard body-overflow, bounded elapsed ===
//
// These tests pin the v2 contract directly (the older red-team/mutant files
// exercise it incidentally). They reuse the package-level helper roundTripFunc
// (retry_test.go); nothing here is redeclared.

// TestV2_RoundTripper_MaxAttempts_ExactCounts locks the WithRTMaxAttempts
// total-attempts model and the degenerate clamp: maxAttempts in {0,1,2,3}
// drives EXACTLY {1,1,2,3} transport calls against an always-retryable upstream.
// This is the highest-risk invariant of the rewrite (an unchanged `<=` loop fed
// a total would run attempts+1 times).
func TestV2_RoundTripper_MaxAttempts_ExactCounts(t *testing.T) {
	cases := []struct {
		maxAttempts int
		wantCalls   int32
	}{
		{0, 1}, // clamps to 1
		{1, 1},
		{2, 2},
		{3, 3},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("maxAttempts=%d", tc.maxAttempts), func(t *testing.T) {
			var calls atomic.Int32
			transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Body:       io.NopCloser(strings.NewReader("")),
					Header:     http.Header{},
				}, nil
			})
			rt := httpx.NewRetryRoundTripper(transport,
				httpx.WithRTBaseDelay(time.Microsecond),
				httpx.WithRTMaxAttempts(tc.maxAttempts),
			)
			req, _ := http.NewRequest(http.MethodGet, "http://example.com/counts", http.NoBody)
			resp, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatalf("RoundTrip = %v, want nil (exhausted retries return the last response)", err)
			}
			resp.Body.Close()
			if got := calls.Load(); got != tc.wantCalls {
				t.Errorf("WithRTMaxAttempts(%d): transport calls = %d, want %d", tc.maxAttempts, got, tc.wantCalls)
			}
		})
	}
}

// TestV2_RetryWithBackoff_MaxAttempts_ExactCounts is the generic-helper twin of
// the RoundTripper count test: maxAttempts in {0,1,2,3} drives EXACTLY {1,1,2,3}
// fn invocations when fn always returns a transient error.
func TestV2_RetryWithBackoff_MaxAttempts_ExactCounts(t *testing.T) {
	cases := []struct {
		maxAttempts int
		wantCalls   int32
	}{
		{0, 1}, // clamps to 1
		{1, 1},
		{2, 2},
		{3, 3},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("maxAttempts=%d", tc.maxAttempts), func(t *testing.T) {
			var calls atomic.Int32
			_, err := httpx.RetryWithBackoff(t.Context(), tc.maxAttempts, time.Microsecond, "counts",
				func(_ context.Context) (int, error) {
					calls.Add(1)
					return 0, &httpx.HTTPStatusError{Code: 503} // transient -> retried up to the cap
				})
			if err == nil {
				t.Fatal("RetryWithBackoff = nil, want error after exhaustion")
			}
			if got := calls.Load(); got != tc.wantCalls {
				t.Errorf("RetryWithBackoff(maxAttempts=%d): fn calls = %d, want %d", tc.maxAttempts, got, tc.wantCalls)
			}
		})
	}
}

// TestV2_Retry_ResponseTooLargeError verifies the hard body-overflow contract:
// a body over WithMaxBodyBytes yields a nil body and *ResponseTooLargeError
// whose Limit reports the cap (no silent truncation); a body exactly at the cap
// succeeds.
func TestV2_Retry_ResponseTooLargeError(t *testing.T) {
	const maxBytes = 16

	overTransport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(make([]byte, maxBytes+1))),
			Header:     http.Header{},
		}, nil
	})
	body, err := httpx.Retry(t.Context(), &http.Client{Transport: overTransport}, "http://example.com/toolarge",
		httpx.WithBaseDelay(time.Millisecond), httpx.WithMaxBodyBytes(maxBytes))
	if body != nil {
		t.Errorf("over-cap body = %d bytes, want nil", len(body))
	}
	var tooLarge *httpx.ResponseTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("over-cap error = %v, want *ResponseTooLargeError", err)
	}
	if tooLarge.Limit != maxBytes {
		t.Errorf("ResponseTooLargeError.Limit = %d, want %d", tooLarge.Limit, int64(maxBytes))
	}

	// A body exactly at the cap must NOT error (the +1 probe read finds no overflow).
	atCapTransport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(make([]byte, maxBytes))),
			Header:     http.Header{},
		}, nil
	})
	atBody, atErr := httpx.Retry(t.Context(), &http.Client{Transport: atCapTransport}, "http://example.com/atcap",
		httpx.WithBaseDelay(time.Millisecond), httpx.WithMaxBodyBytes(maxBytes))
	if atErr != nil {
		t.Fatalf("at-cap body: err = %v, want nil", atErr)
	}
	if int64(len(atBody)) != maxBytes {
		t.Errorf("at-cap body: len = %d, want %d", len(atBody), int64(maxBytes))
	}
}

// TestV2_RoundTripper_MaxElapsedTime_NotOvershot proves the elapsed-time budget
// is a true ceiling even against a large honored Retry-After: the abort happens
// BEFORE sleeping (so the call returns near-instantly instead of sleeping out
// the 60s Retry-After), the message is the clean nil-lastErr form, and only the
// initial attempt runs.
func TestV2_RoundTripper_MaxElapsedTime_NotOvershot(t *testing.T) {
	const budget = 50 * time.Millisecond
	var calls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls.Add(1)
		h := http.Header{}
		h.Set("Retry-After", "60") // capped at RetryAfterCap (60s) — dwarfs the 50ms budget
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     h,
		}, nil
	})
	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithRTMaxAttempts(5),
		httpx.WithRTMaxElapsedTime(budget),
	)
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/budget", http.NoBody)

	start := time.Now()
	_, err := rt.RoundTrip(req) //nolint:bodyclose // budget-abort path returns no usable body
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected max-elapsed-time error")
	}
	if !strings.Contains(err.Error(), "max elapsed time") {
		t.Errorf("error = %v, want containing 'max elapsed time'", err)
	}
	// The honored 60s Retry-After must NOT be slept; aborting on the budget
	// returns immediately. A generous ceiling avoids CI flakes.
	if elapsed > 5*time.Second {
		t.Errorf("elapsed = %v, want << 60s (must not sleep past the budget)", elapsed)
	}
	// lastErr is nil on the 503-response path: the message must be clean, with
	// no fmt.Errorf("...: %w", nil) artifact.
	if strings.Contains(err.Error(), "<nil>") || strings.Contains(err.Error(), "%!w") {
		t.Errorf("error = %v, want clean nil-lastErr message", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("transport calls = %d, want 1 (aborted before the first retry sleep)", got)
	}
}

// TestV2_Retry_MaxBodyBytes_MaxInt64_NoSilentLoss guards the probe-size overflow
// fix (L1): a cap of math.MaxInt64 means "effectively unlimited" and must return
// the full body, not wrap maxBodyBytes+1 negative (which would make
// io.LimitReader read zero bytes and silently return an empty body — the exact
// silent-loss class the hard-overflow error was introduced to eliminate).
func TestV2_Retry_MaxBodyBytes_MaxInt64_NoSilentLoss(t *testing.T) {
	const payload = "hello world, not truncated"
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(payload)),
			Header:     http.Header{},
		}, nil
	})
	body, err := httpx.Retry(t.Context(), &http.Client{Transport: transport}, "http://example.com/maxint",
		httpx.WithBaseDelay(time.Millisecond), httpx.WithMaxBodyBytes(math.MaxInt64))
	if err != nil {
		t.Fatalf("Retry with MaxInt64 cap = %v, want nil", err)
	}
	if string(body) != payload {
		t.Errorf("body = %q (len %d), want %q (a MaxInt64 cap must not wrap and silently truncate)", string(body), len(body), payload)
	}
}
