package httpx_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/httpx"
)

// === ROUND 3: FINAL ADVERSARIAL SWEEP ===

// --- Verify round-1/2 fixes ---

// Verify req.Clone fix: concurrent RoundTrip calls must not share request state.
func TestR3_Clone_ConcurrentRoundTrips_NoRace(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		_ = req.Header.Get("X-Attempt")
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Header:     http.Header{},
		}, nil
	})

	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithMaxRetries(1),
		httpx.WithPrepareRetry(func(req *http.Request) error {
			req.Header.Set("X-Attempt", "retry")
			return nil
		}),
	)

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			req, _ := http.NewRequest(http.MethodGet, "http://example.com/r3", http.NoBody)
			req.Header.Set("X-Original", "val")
			resp, err := rt.RoundTrip(req)
			if err == nil && resp != nil {
				resp.Body.Close()
			}
		})
	}
	wg.Wait()
}

// Verify ParseRetryAfter cap: values near int64 overflow boundary.
func TestR3_ParseRetryAfter_OverflowBoundary(t *testing.T) {
	cases := []string{
		"9223372036",
		"92233720368",
		"999999999999999999",
		"18446744073709551615",
	}
	for _, c := range cases {
		d := httpx.ParseRetryAfter(c)
		if d < 0 {
			t.Fatalf("ParseRetryAfter(%q) negative: %v", c, d)
		}
		if d > httpx.RetryAfterCap {
			t.Fatalf("ParseRetryAfter(%q) = %v, exceeds cap %v", c, d, httpx.RetryAfterCap)
		}
	}
}

// Verify per-request backoff factory: concurrent calls each get an independent
// ExponentialBackoff instance (no shared mutable state, no mutex needed).
func TestR3_PerRequestBackoff_Concurrent(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		n := calls.Add(1)
		if n%3 != 0 {
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
		httpx.WithMaxRetries(5),
		httpx.WithBackoffFunc(func() httpx.Backoff {
			return httpx.NewExponentialBackoff(
				httpx.WithInitialInterval(time.Millisecond),
				httpx.WithMaxElapsedTime(50*time.Millisecond),
			)
		}),
	)

	var wg sync.WaitGroup
	for range 30 {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com/bo", http.NoBody)
			resp, err := rt.RoundTrip(req)
			if err == nil && resp != nil {
				resp.Body.Close()
			}
		})
	}
	wg.Wait()
}

// --- Body replay >2 attempts + GetBody error mid-sequence ---

func TestR3_BodyReplay_FiveAttempts(t *testing.T) {
	var calls atomic.Int32
	var bodies []string
	var mu sync.Mutex
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(req.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		if calls.Add(1) < 5 {
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
		httpx.WithMaxRetries(5),
		httpx.WithRetryNonIdempotent(true),
	)

	req, _ := http.NewRequest(http.MethodPost, "http://example.com/replay", strings.NewReader("body5"))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("body5")), nil
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()

	if got := calls.Load(); got != 5 {
		t.Fatalf("calls = %d, want 5", got)
	}
	for i, b := range bodies {
		if b != "body5" {
			t.Errorf("attempt %d body = %q, want %q", i+1, b, "body5")
		}
	}
}

func TestR3_GetBody_ErrorMidSequence(t *testing.T) {
	var calls atomic.Int32
	var getBodyCalls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     http.Header{},
		}, nil
	})

	wantErr := errors.New("stream exhausted")
	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithMaxRetries(5),
		httpx.WithRetryNonIdempotent(true),
	)

	req, _ := http.NewRequest(http.MethodPost, "http://example.com/mid", strings.NewReader("init"))
	req.GetBody = func() (io.ReadCloser, error) {
		n := getBodyCalls.Add(1)
		if n >= 2 {
			return nil, wantErr
		}
		return io.NopCloser(strings.NewReader("init")), nil
	}

	_, err := rt.RoundTrip(req) //nolint:bodyclose // error path, no response body
	if err == nil {
		t.Fatal("expected error from GetBody failure mid-sequence")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want wrapping %v", err, wantErr)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("transport calls = %d, want 2", got)
	}
}

// --- All error-classification branches ---

func TestR3_IsTransient_AllBranches(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"permanent", httpx.Permanent(errors.New("x")), false},
		{"auth", &httpx.AuthError{Msg: "bad"}, false},
		{"ratelimit", &httpx.RateLimitError{Msg: "slow"}, false},
		{"context.Canceled", context.Canceled, false},
		{"context.DeadlineExceeded", context.DeadlineExceeded, false},
		{"transient-true", &httpx.HTTPStatusError{Code: 503}, true},
		{"transient-false", &httpx.HTTPStatusError{Code: 400}, false},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"wrapped permanent", fmt.Errorf("wrap: %w", httpx.Permanent(errors.New("x"))), false},
		{"wrapped auth", fmt.Errorf("wrap: %w", &httpx.AuthError{Msg: "x"}), false},
		{"wrapped ratelimit", fmt.Errorf("wrap: %w", &httpx.RateLimitError{Msg: "x"}), false},
		{"wrapped transient-true", fmt.Errorf("wrap: %w", &httpx.HTTPStatusError{Code: 502}), true},
		{"wrapped context.Canceled", fmt.Errorf("wrap: %w", context.Canceled), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := httpx.IsTransient(tt.err); got != tt.want {
				t.Errorf("IsTransient(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// --- Timer/goroutine leaks under cancellation ---

func TestR3_SleepCtx_NoTimerLeak(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	before := runtime.NumGoroutine()
	for range 100 {
		_ = httpx.SleepCtx(ctx, time.Hour)
	}
	runtime.GC()
	time.Sleep(10 * time.Millisecond)
	after := runtime.NumGoroutine()

	if after-before > 5 {
		t.Errorf("goroutine leak: before=%d, after=%d", before, after)
	}
}

func TestR3_RoundTripper_CancelDuringBackoff_NoLeak(t *testing.T) {
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     http.Header{},
		}, nil
	})

	before := runtime.NumGoroutine()

	for range 20 {
		ctx, cancel := context.WithCancel(t.Context())
		rt := httpx.NewRetryRoundTripper(transport,
			httpx.WithRTBaseDelay(time.Hour),
			httpx.WithMaxRetries(10),
		)
		go func() {
			time.Sleep(time.Millisecond)
			cancel()
		}()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com/leak", http.NoBody)
		resp, _ := rt.RoundTrip(req)
		if resp != nil {
			resp.Body.Close()
		}
	}

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	after := runtime.NumGoroutine()

	if after-before > 5 {
		t.Errorf("goroutine leak after cancellation: before=%d, after=%d", before, after)
	}
}

// --- Zero/negative config ---

func TestR3_ZeroConfig_Defaults(t *testing.T) {
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

	// All zero values — should use defaults via NewRetryRoundTripper.
	rt := httpx.NewRetryRoundTripper(transport)
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/zero", http.NoBody)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("zero-config RoundTrip error: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestR3_NegativeConfig(t *testing.T) {
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Header:     http.Header{},
		}, nil
	})

	// Negative MaxRetries, negative BaseDelay.
	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithMaxRetries(-5),
		httpx.WithRTBaseDelay(-time.Second),
	)
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/neg", http.NoBody)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("negative-config RoundTrip error: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestR3_JitteredBackoff_ZeroNegative(t *testing.T) {
	if d := httpx.JitteredBackoff(0); d != 0 {
		t.Errorf("JitteredBackoff(0) = %v, want 0", d)
	}
	if d := httpx.JitteredBackoff(-time.Second); d != -time.Second {
		t.Errorf("JitteredBackoff(-1s) = %v, want -1s", d)
	}
}

func TestR3_SafeDouble_ZeroNegative(t *testing.T) {
	if d := httpx.SafeDouble(0); d != 0 {
		t.Errorf("SafeDouble(0) = %v, want 0", d)
	}
	if d := httpx.SafeDouble(-time.Second); d != -time.Second {
		t.Errorf("SafeDouble(-1s) = %v, want -1s", d)
	}
}

func TestR3_SleepCtx_ZeroNegative(t *testing.T) {
	start := time.Now()
	if err := httpx.SleepCtx(t.Context(), 0); err != nil {
		t.Errorf("SleepCtx(0) = %v", err)
	}
	if err := httpx.SleepCtx(t.Context(), -time.Second); err != nil {
		t.Errorf("SleepCtx(-1s) = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("zero/negative sleep took %v", elapsed)
	}
}

// --- Concurrency on every shared field ---

func TestR3_ConcurrentAccess_AllFields(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		n := calls.Add(1)
		if n%2 == 1 {
			return nil, io.ErrUnexpectedEOF // transient
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Header:     http.Header{},
		}, nil
	})

	var onRetryCalls atomic.Int32
	var prepareCalls atomic.Int32

	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithMaxRetries(3),
		httpx.WithRetryNonIdempotent(true),
		httpx.WithOnRetry(func(_ int, _ *http.Request, _ *http.Response, _ error) {
			onRetryCalls.Add(1)
		}),
		httpx.WithPrepareRetry(func(req *http.Request) error {
			prepareCalls.Add(1)
			req.Header.Set("X-Refreshed", "yes")
			return nil
		}),
		httpx.WithCheckRetry(func(_ context.Context, resp *http.Response, err error) (bool, error) {
			if err != nil {
				return httpx.IsTransient(err), nil
			}
			if resp != nil && resp.StatusCode >= 500 {
				return true, nil
			}
			return false, nil
		}),
	)

	var wg sync.WaitGroup
	for range 30 {
		wg.Go(func() {
			req, _ := http.NewRequest(http.MethodGet, "http://example.com/all", http.NoBody)
			resp, err := rt.RoundTrip(req)
			if err == nil && resp != nil {
				resp.Body.Close()
			}
		})
	}
	wg.Wait()

	if onRetryCalls.Load() == 0 {
		t.Error("OnRetry was never called")
	}
	if prepareCalls.Load() == 0 {
		t.Error("PrepareRetry was never called")
	}
}

// --- RetryWithBackoff: zero/negative maxRetries ---

func TestR3_RetryWithBackoff_ZeroMaxRetries(t *testing.T) {
	_, err := httpx.RetryWithBackoff(t.Context(), 0, time.Millisecond, "test", func(_ context.Context) (int, error) {
		t.Fatal("fn should not be called with maxRetries=0")
		return 0, nil
	})
	if err != nil {
		t.Errorf("RetryWithBackoff(maxRetries=0) = %v, want nil", err)
	}
}

func TestR3_RetryWithBackoff_NegativeMaxRetries(t *testing.T) {
	_, err := httpx.RetryWithBackoff(t.Context(), -1, time.Millisecond, "test", func(_ context.Context) (int, error) {
		t.Fatal("fn should not be called with negative maxRetries")
		return 0, nil
	})
	if err != nil {
		t.Errorf("RetryWithBackoff(maxRetries=-1) = %v, want nil", err)
	}
}

// --- RetryOnRateLimit: zero/negative maxAttempts ---

func TestR3_RetryOnRateLimit_ZeroMaxAttempts(t *testing.T) {
	err := httpx.RetryOnRateLimit(t.Context(), 0, time.Second, func(_ context.Context) error {
		t.Fatal("fn should not be called with maxAttempts=0")
		return nil
	})
	if err != nil {
		t.Errorf("RetryOnRateLimit(maxAttempts=0) = %v, want nil", err)
	}
}

// --- Retry (HTTP GET): zero/negative opts ---

func TestR3_Retry_ZeroOpts(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Header:     http.Header{},
		}, nil
	})
	client := &http.Client{Transport: transport}

	body, err := httpx.Retry(t.Context(), client, "http://example.com/z")
	if err != nil {
		t.Fatalf("Retry with zero opts: %v", err)
	}
	if string(body) != "ok" {
		t.Errorf("body = %q, want %q", body, "ok")
	}
}

// --- ExponentialBackoff: MaxElapsedTime ---

func TestR3_ExponentialBackoff_MaxElapsedTime(t *testing.T) {
	bo := httpx.NewExponentialBackoff(
		httpx.WithInitialInterval(time.Millisecond),
		httpx.WithMaxElapsedTime(10*time.Millisecond),
	)
	time.Sleep(15 * time.Millisecond)
	if d := bo.NextBackOff(); d != httpx.BackoffStop {
		t.Errorf("expected BackoffStop after MaxElapsedTime, got %v", d)
	}
}

// --- Permanent error wrapping ---

func TestR3_Permanent_Nil(t *testing.T) {
	if err := httpx.Permanent(nil); err != nil {
		t.Errorf("Permanent(nil) = %v, want nil", err)
	}
}

func TestR3_IsPermanent_Wrapped(t *testing.T) {
	inner := httpx.Permanent(errors.New("inner"))
	wrapped := fmt.Errorf("outer: %w", inner)
	if !httpx.IsPermanent(wrapped) {
		t.Error("IsPermanent(wrapped permanent) = false, want true")
	}
}

// --- HTTPStatusError interface compliance ---

func TestR3_HTTPStatusError_Methods(t *testing.T) {
	tests := []struct {
		code      int
		transient bool
		server    bool
		client    bool
	}{
		{400, false, false, true},
		{404, false, false, true},
		{499, false, false, true},
		{500, false, true, false},
		{502, true, true, false},
		{503, true, true, false},
		{504, true, true, false},
		{501, false, true, false},
	}
	for _, tt := range tests {
		e := &httpx.HTTPStatusError{Code: tt.code}
		if e.IsTransient() != tt.transient {
			t.Errorf("HTTPStatusError{%d}.IsTransient() = %v", tt.code, e.IsTransient())
		}
		if e.IsServerError() != tt.server {
			t.Errorf("HTTPStatusError{%d}.IsServerError() = %v", tt.code, e.IsServerError())
		}
		if e.IsClientError() != tt.client {
			t.Errorf("HTTPStatusError{%d}.IsClientError() = %v", tt.code, e.IsClientError())
		}
	}
}

// --- defaultCheckRetry branches ---

func TestR3_DefaultCheckRetry_via_RoundTripper(t *testing.T) {
	nonRetryable := []int{200, 400, 401, 403, 404, 500}
	for _, code := range nonRetryable {
		var calls atomic.Int32
		transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			calls.Add(1)
			return &http.Response{
				StatusCode: code,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     http.Header{},
			}, nil
		})
		rt := httpx.NewRetryRoundTripper(transport, httpx.WithRTBaseDelay(time.Millisecond), httpx.WithMaxRetries(2))
		req, _ := http.NewRequest(http.MethodGet, "http://example.com", http.NoBody)
		resp, _ := rt.RoundTrip(req)
		if resp != nil {
			resp.Body.Close()
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("code %d: calls = %d, want 1 (no retry)", code, got)
		}
	}

	retryable := []int{429, 502, 503, 504}
	for _, code := range retryable {
		var calls atomic.Int32
		transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			calls.Add(1)
			return &http.Response{
				StatusCode: code,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     http.Header{},
			}, nil
		})
		rt := httpx.NewRetryRoundTripper(transport, httpx.WithRTBaseDelay(time.Millisecond), httpx.WithMaxRetries(2))
		req, _ := http.NewRequest(http.MethodGet, "http://example.com", http.NoBody)
		resp, _ := rt.RoundTrip(req)
		if resp != nil {
			resp.Body.Close()
		}
		if got := calls.Load(); got != 3 {
			t.Errorf("code %d: calls = %d, want 3 (retried)", code, got)
		}
	}
}

// --- RedactTransportError edge cases ---

func TestR3_RedactTransportError_Edges(t *testing.T) {
	if err := httpx.RedactTransportError(nil, "p", "s"); err != nil {
		t.Errorf("nil input: %v", err)
	}
	e := errors.New("some error")
	if got := httpx.RedactTransportError(e, "", ""); got.Error() != "some error" {
		t.Errorf("empty secret: %v", got)
	}
	if got := httpx.RedactTransportError(e, "pfx", "absent"); !strings.Contains(got.Error(), "pfx") {
		t.Errorf("prefix missing: %v", got)
	}
}

// --- DrainClose edge ---

func TestR3_DrainClose_NilBody(t *testing.T) {
	errReader := &errReadCloser{err: errors.New("read fail")}
	httpx.DrainClose(errReader)
	if !errReader.closed {
		t.Error("DrainClose did not close body on read error")
	}
}

type errReadCloser struct {
	err    error
	closed bool
}

func (e *errReadCloser) Read(_ []byte) (int, error) { return 0, e.err }
func (e *errReadCloser) Close() error               { e.closed = true; return nil }
