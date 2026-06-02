package httpx_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/httpx"
)

// === ROUND 5: Nil-option panic regression + edge-case option values ===

// --- Nil option guards (P0 regression) ---

func TestR5_NilOption_Retry(t *testing.T) {
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Header:     http.Header{},
		}, nil
	})
	client := &http.Client{Transport: transport}
	var nilOpt httpx.Option
	_, err := httpx.Retry(context.Background(), client, "http://example.com/nilopt", nilOpt)
	if err != nil {
		t.Fatalf("nil Option caused error: %v", err)
	}
}

func TestR5_NilOption_RTOption(t *testing.T) {
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

func TestR5_NilOption_ExpBackoffOption(t *testing.T) {
	var nilOpt httpx.ExpBackoffOption
	bo := httpx.NewExponentialBackoff(nilOpt)
	d := bo.NextBackOff()
	if d < 0 {
		t.Fatalf("negative backoff: %v", d)
	}
}

func TestR5_NilOption_RedirectOption(t *testing.T) {
	var nilOpt httpx.RedirectOption
	policy := httpx.RedirectPolicyFunc(nilOpt)
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/nilredir", http.NoBody)
	err := policy(req, nil)
	if err == nil {
		t.Fatal("nil RedirectOption should still refuse (no hosts configured)")
	}
}

// --- WithMaxRetries(negative) handling ---

func TestR5_WithMaxRetries_Negative(t *testing.T) {
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
		httpx.WithMaxRetries(-1),
		httpx.WithRTBaseDelay(time.Millisecond),
	)
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/neg", http.NoBody)
	resp, _ := rt.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	// Negative should fallback to default (2 retries = 3 attempts)
	if got := calls.Load(); got != 3 {
		t.Fatalf("WithMaxRetries(-1): calls=%d, want 3 (default)", got)
	}
}

// --- WithBaseDelay(0) and WithRTBaseDelay(0) fallback to default ---

func TestR5_WithBaseDelay_Zero(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls.Add(1)
		if calls.Load() < 2 {
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
	client := &http.Client{Transport: transport}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// WithBaseDelay(0) should not hang (falls back to default)
	_, err := httpx.Retry(ctx, client, "http://example.com/zerodelay",
		httpx.WithBaseDelay(0), httpx.WithMaxAttempts(2))
	if err != nil {
		t.Fatalf("WithBaseDelay(0): %v", err)
	}
}

func TestR5_WithRTBaseDelay_Zero(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls.Add(1)
		if calls.Load() < 2 {
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
		httpx.WithRTBaseDelay(0),
		httpx.WithMaxRetries(2),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com/zerortdelay", http.NoBody)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("WithRTBaseDelay(0): %v", err)
	}
	resp.Body.Close()
}

// --- WithMaxRetries(0) round-1 fix verification ---

func TestR5_WithMaxRetries_Zero_Verify(t *testing.T) {
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
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/zeroretry", http.NoBody)
	resp, _ := rt.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("WithMaxRetries(0): calls=%d, want 1", got)
	}
}
