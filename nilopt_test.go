package httpx_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/httpx/v2"
)

// Nil-option guards (a nil functional option must be skipped, not panic) and the
// negative-attempt clamp edge case.

func TestRetry_nil_option_is_skipped(t *testing.T) {
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Header:     http.Header{},
		}, nil
	})
	client := &http.Client{Transport: transport}
	var nilOpt httpx.Option
	if _, err := httpx.Retry(context.Background(), client, "http://example.com/nilopt", nilOpt); err != nil {
		t.Fatalf("nil Option caused error: %v", err)
	}
}

func TestRetryRoundTripper_nil_option_is_skipped(t *testing.T) {
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Header:     http.Header{},
		}, nil
	})
	var nilOpt httpx.RTOption
	rt := httpx.NewRetryRoundTripper(transport, nilOpt)
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/nilrtopt", http.NoBody)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("nil RTOption caused error: %v", err)
	}
	resp.Body.Close()
}

func TestExponentialBackoff_nil_option_is_skipped(t *testing.T) {
	var nilOpt httpx.ExpBackoffOption
	bo := httpx.NewExponentialBackoff(nilOpt)
	if d := bo.NextBackOff(); d < 0 {
		t.Fatalf("negative backoff: %v", d)
	}
}

func TestRedirectPolicyFunc_nil_option_still_refuses(t *testing.T) {
	var nilOpt httpx.RedirectOption
	policy := httpx.RedirectPolicyFunc(nilOpt)
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/nilredir", http.NoBody)
	if err := policy(req, nil); err == nil {
		t.Fatal("nil RedirectOption should still refuse (no hosts configured)")
	}
}

// TestRetryRoundTripper_negative_max_attempts_clamps_to_one covers the negative
// edge (the {0,1,2,3} exact-count table lives in v2_test.go): -1 clamps to a
// single attempt, never a silent no-op and never the old coerce-to-default-3.
func TestRetryRoundTripper_negative_max_attempts_clamps_to_one(t *testing.T) {
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
		httpx.WithRTMaxAttempts(-1),
		httpx.WithRTBaseDelay(time.Millisecond),
	)
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/neg", http.NoBody)
	resp, _ := rt.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("WithRTMaxAttempts(-1): calls=%d, want 1 (clamped to a single attempt)", got)
	}
}
