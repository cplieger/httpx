package httpx_test

import (
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

// --- P0: RoundTripper must not mutate the caller's request ---

func TestRetryRoundTripper_does_not_mutate_caller_request(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Body:       io.NopCloser(strings.NewReader("err")),
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

	req, _ := http.NewRequest(http.MethodGet, "http://example.com/test", http.NoBody)
	req.Header.Set("X-Original", "keep")

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()

	// Caller's request must NOT be mutated.
	if req.Header.Get("X-Mutated") != "" {
		t.Errorf("caller's request was mutated: X-Mutated = %q", req.Header.Get("X-Mutated"))
	}
	if req.Header.Get("X-Original") != "keep" {
		t.Errorf("caller's original header lost: X-Original = %q", req.Header.Get("X-Original"))
	}
}

func TestRetryRoundTripper_clone_isolates_body(t *testing.T) {
	var calls atomic.Int32
	var bodies []string
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(req.Body)
		bodies = append(bodies, string(b))
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

	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithMaxRetries(2),
		httpx.WithRetryNonIdempotent(true),
	)

	origBody := strings.NewReader("payload")
	req, _ := http.NewRequest(http.MethodPost, "http://example.com", origBody)
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("payload")), nil
	}
	callerBody := req.Body

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()

	// The caller's req.Body pointer must not have been replaced.
	if req.Body != callerBody {
		t.Error("caller's req.Body was replaced (pointer changed)")
	}

	for i, b := range bodies {
		if b != "payload" {
			t.Errorf("attempt %d body = %q, want %q", i+1, b, "payload")
		}
	}
}

// --- P1: GetBody error must abort cleanly ---

func TestRetryRoundTripper_GetBody_error_aborts(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     http.Header{},
		}, nil
	})

	wantErr := errors.New("body source closed")
	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithMaxRetries(3),
		httpx.WithRetryNonIdempotent(true),
	)

	req, _ := http.NewRequest(http.MethodPost, "http://example.com", strings.NewReader("data"))
	req.GetBody = func() (io.ReadCloser, error) {
		return nil, wantErr
	}

	resp, err := rt.RoundTrip(req) //nolint:bodyclose // error path
	_ = resp
	if err == nil {
		t.Fatal("expected error from GetBody failure")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want wrapping %v", err, wantErr)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1 (abort after GetBody error)", got)
	}
}

// --- P1: Context cancellation mid-backoff must abort promptly ---

func TestRetryRoundTripper_context_cancel_mid_backoff_prompt(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     http.Header{},
		}, nil
	})

	ctx, cancel := context.WithCancel(t.Context())
	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithRTBaseDelay(5*time.Second),
		httpx.WithMaxRetries(10),
	)

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com", http.NoBody)
	start := time.Now()
	resp, err := rt.RoundTrip(req) //nolint:bodyclose // error path
	elapsed := time.Since(start)
	_ = resp

	if err == nil {
		t.Fatal("expected error from context cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("elapsed = %v, want < 500ms (prompt abort)", elapsed)
	}
}

// --- P2: Response body drained on every discarded response ---

func TestRetryRoundTripper_drains_discarded_response_bodies(t *testing.T) {
	var closed atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body: &trackClose{
				Reader:  strings.NewReader("response-body-data"),
				onClose: func() { closed.Add(1) },
			},
			Header: http.Header{},
		}, nil
	})

	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithMaxRetries(3),
	)

	req, _ := http.NewRequest(http.MethodGet, "http://example.com", http.NoBody)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()

	if got := closed.Load(); got != 4 {
		t.Errorf("closed count = %d, want 4 (3 intermediate + 1 final)", got)
	}
}

type trackClose struct {
	io.Reader
	onClose func()
}

func (tc *trackClose) Close() error {
	tc.onClose()
	return nil
}

// --- P2: Backoff.Stop handling ---

func TestRetryRoundTripper_BackoffStop_returns_last_error(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("connection refused")
	})

	bo := &immediateStopBackoff{}
	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithMaxRetries(10),
		httpx.WithBackoff(bo),
	)

	req, _ := http.NewRequest(http.MethodGet, "http://example.com", http.NoBody)
	resp, err := rt.RoundTrip(req) //nolint:bodyclose // error path
	_ = resp
	if err == nil {
		t.Fatal("expected error when backoff stops")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1", got)
	}
}

type immediateStopBackoff struct{}

func (b *immediateStopBackoff) NextBackOff() time.Duration { return httpx.BackoffStop }
func (b *immediateStopBackoff) Reset()                     {}

// --- P0: ParseRetryAfter integer overflow on huge delta-seconds ---

func TestParseRetryAfter_huge_value_no_overflow(t *testing.T) {
	d := httpx.ParseRetryAfter("10000000000")
	if d < 0 {
		t.Fatalf("ParseRetryAfter(10000000000) returned negative: %v", d)
	}
	if d != 60*time.Second {
		t.Errorf("ParseRetryAfter(10000000000) = %v, want 60s (capped)", d)
	}
}

func TestParseRetryAfter_negative_returns_zero(t *testing.T) {
	if d := httpx.ParseRetryAfter("-5"); d != 0 {
		t.Errorf("ParseRetryAfter(-5) = %v, want 0", d)
	}
}

// --- P1: Concurrent use of RetryRoundTripper with shared Backoff must not race ---

func TestRetryRoundTripper_concurrent_shared_backoff_no_race(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		n := calls.Add(1)
		if n%2 == 1 {
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

	bo := httpx.NewExponentialBackoff(httpx.WithInitialInterval(time.Millisecond))
	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithMaxRetries(3),
		httpx.WithBackoff(bo),
	)

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			req, _ := http.NewRequest(http.MethodGet, "http://example.com", http.NoBody)
			resp, err := rt.RoundTrip(req)
			if err == nil && resp != nil {
				resp.Body.Close()
			}
		})
	}
	wg.Wait()
}
