package httpx_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/httpx/v3"
)

// === v3 contract: total-attempts semantics, hard body-overflow, bounded
// === elapsed time, TransportConfig zero-value, NewRetryClient wiring
//
// These tests pin the v3 contract directly (the older files exercise it
// incidentally). They reuse the package-level helper roundTripFunc
// (retry_test.go); nothing here is redeclared.

// TestV3_RoundTripper_MaxAttempts_ExactCounts locks the TransportConfig
// total-attempts model: MaxAttempts in {-1,0,1,2,3} drives EXACTLY {1,3,1,2,3}
// transport calls against an always-retryable upstream. Zero means unset and
// takes DefaultMaxAttempts (3); a negative value means exactly one attempt
// (v2 expressed try-once as WithRTMaxAttempts(0), but a zero struct field
// cannot distinguish absent from zero, so v3 moves try-once to negatives).
func TestV3_RoundTripper_MaxAttempts_ExactCounts(t *testing.T) {
	cases := []struct {
		maxAttempts int
		wantCalls   int32
	}{
		{-1, 1}, // negative: exactly one attempt
		{0, 3},  // zero: unset, takes DefaultMaxAttempts
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
			rt := httpx.NewRetryRoundTripper(transport, httpx.TransportConfig{
				BaseDelay:   time.Microsecond,
				MaxAttempts: tc.maxAttempts,
			})
			req, _ := http.NewRequest(http.MethodGet, "http://example.com/counts", http.NoBody)
			resp, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatalf("RoundTrip = %v, want nil (exhausted retries return the last response)", err)
			}
			resp.Body.Close()
			if got := calls.Load(); got != tc.wantCalls {
				t.Errorf("TransportConfig{MaxAttempts: %d}: transport calls = %d, want %d", tc.maxAttempts, got, tc.wantCalls)
			}
		})
	}
}

// TestV3_Do_MaxAttempts_ExactCounts is the generic-door twin: WithMaxAttempts
// in {0,1,2,3} drives EXACTLY {1,1,2,3} fn invocations when fn always returns
// a transient error. Unlike the TransportConfig struct field, option absence
// is expressible, so WithMaxAttempts(0) keeps its v2 meaning of "exactly one
// attempt" (the below-1 clamp).
func TestV3_Do_MaxAttempts_ExactCounts(t *testing.T) {
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
			_, err := httpx.Do(t.Context(),
				func(_ context.Context) (int, error) {
					calls.Add(1)
					return 0, &httpx.HTTPStatusError{Code: 503} // transient -> retried up to the cap
				}, httpx.WithMaxAttempts(tc.maxAttempts), httpx.WithBaseDelay(time.Microsecond), httpx.WithLabel("counts"))
			if err == nil {
				t.Fatal("Do = nil, want error after exhaustion")
			}
			if got := calls.Load(); got != tc.wantCalls {
				t.Errorf("Do(WithMaxAttempts(%d)): fn calls = %d, want %d", tc.maxAttempts, got, tc.wantCalls)
			}
		})
	}
}

// TestV3_GetBytes_ResponseTooLargeError verifies the hard body-overflow
// contract: a body over WithMaxBodyBytes yields a nil body and
// *ResponseTooLargeError whose Limit reports the cap (no silent truncation); a
// body exactly at the cap succeeds.
func TestV3_GetBytes_ResponseTooLargeError(t *testing.T) {
	const maxBytes = 16

	overTransport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(make([]byte, maxBytes+1))),
			Header:     http.Header{},
		}, nil
	})
	body, err := httpx.GetBytes(t.Context(), &http.Client{Transport: overTransport}, "http://example.com/toolarge",
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
	atBody, atErr := httpx.GetBytes(t.Context(), &http.Client{Transport: atCapTransport}, "http://example.com/atcap",
		httpx.WithBaseDelay(time.Millisecond), httpx.WithMaxBodyBytes(maxBytes))
	if atErr != nil {
		t.Fatalf("at-cap body: err = %v, want nil", atErr)
	}
	if int64(len(atBody)) != maxBytes {
		t.Errorf("at-cap body: len = %d, want %d", len(atBody), int64(maxBytes))
	}
}

// TestV3_RoundTripper_MaxElapsedTime_NotOvershot proves the elapsed-time budget
// is a true ceiling even against a large honored Retry-After: the abort happens
// BEFORE sleeping (so the call returns near-instantly instead of sleeping out
// the 60s Retry-After), the message is the clean nil-lastErr form, and only the
// initial attempt runs.
func TestV3_RoundTripper_MaxElapsedTime_NotOvershot(t *testing.T) {
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
	rt := httpx.NewRetryRoundTripper(transport, httpx.TransportConfig{
		BaseDelay:      time.Millisecond,
		MaxAttempts:    5,
		MaxElapsedTime: budget,
	})
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

// TestV3_GetBytes_MaxBodyBytes_MaxInt64_NoSilentLoss guards the probe-size
// overflow fix: a cap of math.MaxInt64 means "effectively unlimited" and must
// return the full body, not wrap maxBodyBytes+1 negative (which would make
// io.LimitReader read zero bytes and silently return an empty body — the exact
// silent-loss class the hard-overflow error was introduced to eliminate).
func TestV3_GetBytes_MaxBodyBytes_MaxInt64_NoSilentLoss(t *testing.T) {
	const payload = "hello world, not truncated"
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(payload)),
			Header:     http.Header{},
		}, nil
	})
	body, err := httpx.GetBytes(t.Context(), &http.Client{Transport: transport}, "http://example.com/maxint",
		httpx.WithBaseDelay(time.Millisecond), httpx.WithMaxBodyBytes(math.MaxInt64))
	if err != nil {
		t.Fatalf("GetBytes with MaxInt64 cap = %v, want nil", err)
	}
	if string(body) != payload {
		t.Errorf("body = %q (len %d), want %q (a MaxInt64 cap must not wrap and silently truncate)", string(body), len(body), payload)
	}
}

// TestV3_TransportConfig_zero_value_works pins that TransportConfig{} is a
// usable default configuration (the stdlib zero-value idiom): a transient
// failure is retried and the request succeeds. The exact default attempt
// count is pinned by the {0,3} row of the ExactCounts table with a fast
// BaseDelay; this test keeps the pure zero value on a succeed-after-one-503
// upstream so it never sleeps the full default backoff more than once.
func TestV3_TransportConfig_zero_value_works(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
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
	rt := httpx.NewRetryRoundTripper(transport, httpx.TransportConfig{})
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/zero", http.NoBody)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip = %v, want nil", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (zero-value config retries the 503)", resp.StatusCode)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("transport calls = %d, want 2", got)
	}
}

// TestV3_NewRetryClient_nil_policy_panics pins the loud-rejection contract: a
// nil redirect policy is a programmer error (it would silently mean net/http's
// follow-anywhere default — the exact omission the constructor exists to
// prevent), so NewRetryClient panics with a message naming the alternatives.
func TestV3_NewRetryClient_nil_policy_panics(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("NewRetryClient(nil policy) did not panic")
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, "nil redirect policy") {
			t.Errorf("panic = %v, want message naming the nil redirect policy", r)
		}
	}()
	_ = httpx.NewRetryClient(nil, nil, httpx.TransportConfig{})
}

// TestV3_NewRetryClient_wiring pins the constructor's assembly: the returned
// client retries through the transport (a 503-then-200 upstream succeeds), the
// supplied redirect policy is installed and enforced (a cross-host redirect is
// refused under DefaultRedirectPolicy), and no Client.Timeout is set (a
// Client.Timeout above a retrying transport would cap the whole retry
// sequence).
func TestV3_NewRetryClient_wiring(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/flaky":
			if calls.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			fmt.Fprint(w, "ok")
		case "/hop":
			http.Redirect(w, r, "https://evil.example/x", http.StatusFound)
		}
	}))
	t.Cleanup(upstream.Close)

	client := httpx.NewRetryClient(nil, httpx.DefaultRedirectPolicy, httpx.TransportConfig{
		MaxAttempts: 3,
		BaseDelay:   time.Millisecond,
	})

	if client.Timeout != 0 {
		t.Errorf("Client.Timeout = %v, want 0 (total-cap footgun must not be set)", client.Timeout)
	}

	// Retry wiring: the 503 is retried and the second attempt's 200 returned.
	resp, err := client.Get(upstream.URL + "/flaky")
	if err != nil {
		t.Fatalf("GET /flaky = %v, want nil", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "ok" {
		t.Errorf("body = %q, want ok (retry through the transport)", body)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("upstream calls = %d, want 2", got)
	}

	// Policy wiring: a cross-host redirect is refused by DefaultRedirectPolicy.
	respHop, errHop := client.Get(upstream.URL + "/hop")
	if errHop == nil {
		if respHop != nil {
			respHop.Body.Close()
		}
		t.Fatal("GET /hop = nil error, want cross-host redirect refusal")
	}
	if !strings.Contains(errHop.Error(), "refusing redirect") {
		t.Errorf("redirect error = %v, want the DefaultRedirectPolicy refusal", errHop)
	}
}
