package httpx_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/httpx/v2"
)

// testBackoff is a simple Backoff implementation for testing.
type testBackoff struct {
	delays []time.Duration
	idx    int
}

func (b *testBackoff) NextBackOff() time.Duration {
	if b.idx >= len(b.delays) {
		return httpx.BackoffStop
	}
	d := b.delays[b.idx]
	b.idx++
	return d
}

func (b *testBackoff) Reset() { b.idx = 0 }

func TestRetryRoundTripper_custom_Backoff(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	bo := &testBackoff{delays: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}}
	rt := httpx.NewRetryRoundTripper(srv.Client().Transport,
		httpx.WithRTMaxAttempts(6),
		httpx.WithBackoffFunc(func() httpx.Backoff { return bo }),
	)
	client := rt.StandardClient()

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("calls = %d, want 3", got)
	}
}

func TestRetryRoundTripper_Backoff_Stop(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	bo := &testBackoff{delays: []time.Duration{time.Millisecond}}
	rt := httpx.NewRetryRoundTripper(srv.Client().Transport,
		httpx.WithRTMaxAttempts(11),
		httpx.WithBackoffFunc(func() httpx.Backoff { return bo }),
	)
	client := rt.StandardClient()

	resp, err := client.Get(srv.URL)
	if err != nil {
		// Backoff stop returns error.
		return
	}
	if resp != nil {
		resp.Body.Close()
	}
	if got := calls.Load(); got > 3 {
		t.Errorf("calls = %d, want <= 3 with Backoff stop", got)
	}
}

func TestRetryRoundTripper_BackoffStop_returns_last_transport_error(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, io.ErrUnexpectedEOF
	})
	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithRTMaxAttempts(6),
		httpx.WithBackoffFunc(func() httpx.Backoff { return &testBackoff{} }),
	)
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/x", http.NoBody)
	resp, err := rt.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("RoundTrip = nil, want last transport error on BackoffStop")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("error = %v, want wrapping io.ErrUnexpectedEOF", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("transport calls = %d, want 1", got)
	}
}

func TestExponentialBackoff_zero_value_is_usable(t *testing.T) {
	t.Parallel()
	var b httpx.ExponentialBackoff
	got := b.NextBackOff()
	if got == httpx.BackoffStop {
		t.Fatal("zero-value ExponentialBackoff.NextBackOff() = BackoffStop, want usable delay")
	}
	if got < httpx.DefaultBaseDelay/2 || got > httpx.DefaultBaseDelay {
		t.Errorf("zero-value NextBackOff() = %v, want [%v, %v]",
			got, httpx.DefaultBaseDelay/2, httpx.DefaultBaseDelay)
	}
}

func TestRetryRoundTripper_MaxElapsedTime(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport,
		httpx.WithRTBaseDelay(50*time.Millisecond),
		httpx.WithRTMaxAttempts(11),
		httpx.WithRTMaxElapsedTime(80*time.Millisecond),
	)
	client := rt.StandardClient()

	_, err := client.Get(srv.URL) //nolint:bodyclose // error path returns no body
	if err == nil {
		t.Fatal("expected error from MaxElapsedTime")
	}
	if !strings.Contains(err.Error(), "max elapsed time") {
		t.Errorf("error = %v, want containing 'max elapsed time'", err)
	}
	// The always-503 server drives the lastErr==nil branch of sleepBeforeRetry,
	// which must return a clean message (no fmt.Errorf("...: %w", nil) artifact).
	if strings.Contains(err.Error(), "<nil>") || strings.Contains(err.Error(), "%!w") {
		t.Errorf("clean nil-lastErr error expected, got %v", err)
	}
	if got := calls.Load(); got > 4 {
		t.Errorf("calls = %d, want <= 4 with MaxElapsedTime", got)
	}
}

func TestRetryRoundTripper_MaxElapsedTime_wraps_transport_error(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, io.ErrUnexpectedEOF
	})
	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithRTBaseDelay(time.Hour),
		httpx.WithRTMaxAttempts(3),
		httpx.WithRTMaxElapsedTime(time.Millisecond),
	)
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/elapsed-transport-err", http.NoBody)
	resp, err := rt.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("RoundTrip = nil, want max-elapsed-time error")
	}
	if !strings.Contains(err.Error(), "max elapsed time") {
		t.Errorf("error = %v, want containing 'max elapsed time'", err)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("error = %v, want errors.Is(err, io.ErrUnexpectedEOF)", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("transport calls = %d, want 1", got)
	}
}

// TestRetryRoundTripper_zero_base_delay_defaults_to_base: a zero base delay must
// default to DefaultBaseDelay (1s); the single retry sleep blows the 100ms
// deadline. A boundary mutant that keeps the delay at 0 runs the retry instantly.
func TestRetryRoundTripper_zero_base_delay_defaults_to_base(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport,
		httpx.WithRTBaseDelay(0), httpx.WithRTMaxAttempts(2))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)

	resp, err := rt.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Errorf("RoundTrip(baseDelay=0) err = nil, want context deadline (delay defaulted)")
	}
}

// TestRetryRoundTripper_default_backoff_doubles_between_retries: with doubling,
// the three jittered sleeps total >= 60+120+240 = 420ms (> 400ms deadline);
// without doubling they total <= 120*3 = 360ms. A mutant that skips SafeDouble
// finishes within the deadline and returns 503/nil.
func TestRetryRoundTripper_default_backoff_doubles_between_retries(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport,
		httpx.WithRTBaseDelay(120*time.Millisecond), httpx.WithRTMaxAttempts(4))
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)

	resp, err := rt.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Errorf("RoundTrip(default backoff) err = nil, want context deadline (backoff must double)")
	}
}

// TestRetryRoundTripper_zero_max_elapsed_does_not_cap: maxElapsedTime==0 means
// no cap, so the retry reaches 200. A mutant treating 0 as "cap enabled" aborts.
func TestRetryRoundTripper_zero_max_elapsed_does_not_cap(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport,
		httpx.WithRTBaseDelay(time.Millisecond), httpx.WithRTMaxAttempts(3))
	resp, err := rt.StandardClient().Get(srv.URL)
	if err != nil {
		t.Fatalf("Get = %v, want nil", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (no elapsed-time cap)", resp.StatusCode)
	}
}

// TestRetryRoundTripper_large_max_elapsed_allows_fast_retry: elapsed (~ms) is far
// below the 10s cap, so the retry reaches 200.
func TestRetryRoundTripper_large_max_elapsed_allows_fast_retry(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithRTMaxAttempts(3),
		httpx.WithRTMaxElapsedTime(10*time.Second))
	resp, err := rt.StandardClient().Get(srv.URL)
	if err != nil {
		t.Fatalf("Get = %v, want nil", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (within elapsed cap)", resp.StatusCode)
	}
}

// TestRetryRoundTripper_429_without_retry_after_uses_backoff: with no Retry-After,
// ParseRetryAfter==0 must NOT override the huge jittered backoff, so the retry
// sleep blows the 100ms deadline.
func TestRetryRoundTripper_429_without_retry_after_uses_backoff(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport,
		httpx.WithRTBaseDelay(10*time.Second), httpx.WithRTMaxAttempts(2))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)

	resp, err := rt.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Errorf("RoundTrip(429 no Retry-After) err = nil, want context deadline (backoff used)")
	}
}

// TestRetryRoundTripper_sleep_error_aborts_with_bare_context_error: a
// deadline-interrupted sleep must abort RoundTrip and surface the bare context
// error, not a transport *url.Error from another attempt against the dead context.
func TestRetryRoundTripper_sleep_error_aborts_with_bare_context_error(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport,
		httpx.WithRTBaseDelay(10*time.Second), httpx.WithRTMaxAttempts(2))
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)

	resp, err := rt.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("RoundTrip err = nil, want context error from interrupted sleep")
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		t.Errorf("RoundTrip err = %T (%v), want bare context error, not *url.Error", err, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("RoundTrip err = %v, want context.DeadlineExceeded", err)
	}
}
