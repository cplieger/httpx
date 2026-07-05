package httpx_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/httpx/v2"
)

// bufLogger returns a debug-level text logger writing into buf.
func bufLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func shortOpts() []httpx.Option {
	return []httpx.Option{httpx.WithBaseDelay(time.Millisecond), httpx.WithMaxBodyBytes(10 << 20)}
}

func TestRetry_success_first_try(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	body, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts()...)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %s", body)
	}
}

func TestRetry_error_status_fails_fast(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts()...); err == nil {
		t.Error("expected error for 404")
	}
}

func TestRetry_all_2xx_are_success(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"200 OK", http.StatusOK, "hello"},
		{"201 Created", http.StatusCreated, "created"},
		{"204 No Content", http.StatusNoContent, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.status != http.StatusOK {
					w.WriteHeader(tt.status)
				}
				if tt.body != "" {
					_, _ = w.Write([]byte(tt.body))
				}
			}))
			defer srv.Close()

			body, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts()...)
			if err != nil {
				t.Fatalf("Retry(%d) = %v, want nil", tt.status, err)
			}
			if string(body) != tt.body {
				t.Errorf("Retry(%d) body = %q, want %q", tt.status, body, tt.body)
			}
		})
	}
}

func TestRetry_non_followed_3xx_is_status_error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://example.com/moved")
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer srv.Close()

	client := srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	_, err := httpx.Retry(t.Context(), client, srv.URL, shortOpts()...)
	if err == nil {
		t.Fatal("expected *StatusError for a non-followed 3xx")
	}
	var se *httpx.StatusError
	if !errors.As(err, &se) {
		t.Fatalf("error = %T, want *httpx.StatusError", err)
	}
	if se.Code != http.StatusMovedPermanently {
		t.Errorf("StatusError.Code = %d, want 301", se.Code)
	}
}

func TestRetry_recovers_after_retryable_status(t *testing.T) {
	tests := []struct {
		name       string
		failStatus int
	}{
		{"429 then 200", http.StatusTooManyRequests},
		{"500 then 200", http.StatusInternalServerError},
		{"503 then 200", http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(tt.failStatus)
					_, _ = w.Write([]byte("transient"))
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok-body"))
			}))
			defer srv.Close()

			body, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts()...)
			if err != nil {
				t.Fatalf("Retry after retry = %v, want nil", err)
			}
			if string(body) != "ok-body" {
				t.Errorf("body = %q, want %q", body, "ok-body")
			}
			if got := calls.Load(); got != 2 {
				t.Errorf("server call count = %d, want 2", got)
			}
		})
	}
}

func TestRetry_exhausts_on_persistent_failure(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	body, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts()...)
	if err == nil {
		t.Fatalf("Retry after all retries = nil, want error")
	}
	if body != nil {
		t.Errorf("body = %v, want nil", body)
	}
	if !strings.Contains(err.Error(), "retries exhausted") {
		t.Errorf("error = %v, want containing %q", err, "retries exhausted")
	}
	if !strings.Contains(err.Error(), "HTTP 503") {
		t.Errorf("error = %v, want wrapped HTTP 503 cause", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("server call count = %d, want 3", got)
	}
}

func TestRetry_aborts_on_context_cancellation(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := httpx.Retry(ctx, srv.Client(), srv.URL, httpx.WithBaseDelay(100*time.Millisecond), httpx.WithMaxBodyBytes(10<<20))
	if err == nil {
		t.Fatalf("Retry after ctx cancel = nil, want error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled wrapped", err)
	}
	if got := calls.Load(); got > 2 {
		t.Errorf("server call count = %d, want <= 2", got)
	}
}

func TestRetry_body_size_limit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		buf := make([]byte, 1024)
		for range 11 * 1024 {
			_, _ = w.Write(buf)
		}
	}))
	defer srv.Close()

	// The 11MB body exceeds the 10MB cap. v2 fails loud with
	// *ResponseTooLargeError (no truncated body) and reports the cap via Limit.
	body, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, httpx.WithBaseDelay(time.Millisecond), httpx.WithMaxBodyBytes(10<<20))
	if body != nil {
		t.Errorf("body = %d bytes, want nil (oversize body must not be truncated and returned)", len(body))
	}
	var tooLarge *httpx.ResponseTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("Retry(oversize) error = %v, want *ResponseTooLargeError", err)
	}
	if tooLarge.Limit != 10<<20 {
		t.Errorf("ResponseTooLargeError.Limit = %d, want %d", tooLarge.Limit, int64(10<<20))
	}
}

func TestRetry_honors_retry_after(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	start := time.Now()
	body, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts()...)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Retry = %v, want nil", err)
	}
	if string(body) != "ok" {
		t.Errorf("body = %q, want ok", body)
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("elapsed = %v, want >= 900ms (Retry-After: 1 honored)", elapsed)
	}
}

// TestRetry_honors_retry_after_on_503 asserts the Retry helper waits the
// server-requested Retry-After on a 5xx, not just on 429 (cycle-1 h-f4
// extended honoring to every retryable 5xx via retryAttempt's >=500 branch).
func TestRetry_honors_retry_after_on_503(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run(fmt.Sprintf("%d", status), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(status)
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok"))
			}))
			defer srv.Close()

			start := time.Now()
			body, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts()...)
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("Retry = %v, want nil", err)
			}
			if string(body) != "ok" {
				t.Errorf("body = %q, want ok", body)
			}
			// Retry-After: 1 must be honored on the 5xx, not just on 429.
			if elapsed < 900*time.Millisecond {
				t.Errorf("elapsed = %v, want >= 900ms (Retry-After: 1 honored on %d)", elapsed, status)
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"empty", "", 0},
		{"delta seconds", "5", 5 * time.Second},
		{"delta seconds with whitespace", "  10  ", 10 * time.Second},
		{"delta seconds just under cap", "59", 59 * time.Second},
		{"delta seconds exactly at cap", "60", 60 * time.Second},
		{"delta seconds one over cap", "61", 60 * time.Second},
		{"delta seconds capped at 60", "120", 60 * time.Second},
		{"huge value capped not overflowed", "10000000000", 60 * time.Second},
		{"max int64 seconds capped", "9223372036854775807", 60 * time.Second},
		{"above int64 range treated as malformed", "99999999999999999999", 0},
		{"zero", "0", 0},
		{"negative", "-5", 0},
		{"malformed", "soon", 0},
		{"http-date future capped", now.Add(5 * time.Minute).UTC().Format(http.TimeFormat), 60 * time.Second},
		{"http-date past", now.Add(-time.Hour).UTC().Format(http.TimeFormat), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := httpx.ParseRetryAfter(tt.in)
			if strings.Contains(tt.name, "http-date future") {
				if got < 55*time.Second || got > 60*time.Second {
					t.Errorf("ParseRetryAfter(%q) = %v, want ~60s", tt.in, got)
				}
				return
			}
			if got != tt.want {
				t.Errorf("ParseRetryAfter(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestDrain_small_body swaps slog.Default; not parallel.
func TestDrain_small_body(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(bufLogger(&buf))
	defer slog.SetDefault(prev)

	// A sub-limit body drains via io.CopyN returning io.EOF, which Drain must
	// treat as a clean drain (the !errors.Is(err, io.EOF) guard), never a failure.
	httpx.Drain(io.NopCloser(strings.NewReader("small")))
	if strings.Contains(buf.String(), "failed to drain") {
		t.Errorf("clean small-body drain logged a failure:\n%s", buf.String())
	}
}

func TestRetry_returns_typed_StatusError_on_exhaustion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts()...)
	if err == nil {
		t.Fatal("want error")
	}
	var target *httpx.StatusError
	if !errors.As(err, &target) {
		t.Fatalf("errors.As(*StatusError) = false; err = %v", err)
	}
	if target.Code != 503 {
		t.Errorf("Code = %d, want 503", target.Code)
	}
	if !errors.Is(err, httpx.ErrServerError) {
		t.Error("want errors.Is(ErrServerError)")
	}
}

func TestRetry_returns_typed_StatusError_rate_limited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts()...)
	if err == nil {
		t.Fatal("want error")
	}
	if !errors.Is(err, httpx.ErrRateLimited) {
		t.Errorf("want errors.Is(ErrRateLimited); err = %v", err)
	}
}

func TestRetry_4xx_returns_typed_StatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts()...)
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "retries exhausted") {
		t.Error("4xx should fast-fail without retries exhausted wrap")
	}
	var target *httpx.StatusError
	if !errors.As(err, &target) {
		t.Fatalf("errors.As(*StatusError) = false; err = %v", err)
	}
	if target.Code != 404 {
		t.Errorf("Code = %d, want 404", target.Code)
	}
}

func TestLimitedBody_limits_read(t *testing.T) {
	body := io.NopCloser(strings.NewReader("hello world, this is a long body"))
	resp := &http.Response{Body: body}
	lr := httpx.LimitedBody(resp, 5)
	data, err := io.ReadAll(lr)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("got %q, want %q", data, "hello")
	}
}

func TestLimitedBody_close_propagates(t *testing.T) {
	closed := false
	body := &trackingCloser{Reader: strings.NewReader("data"), onClose: func() { closed = true }}
	resp := &http.Response{Body: body}
	lr := httpx.LimitedBody(resp, 100)
	lr.Close()
	if !closed {
		t.Error("Close did not propagate to underlying body")
	}
}

type trackingCloser struct {
	io.Reader
	onClose func()
}

func (tc *trackingCloser) Close() error {
	tc.onClose()
	return nil
}

func TestRetry_non200_statusCodes(t *testing.T) {
	tests := []struct {
		status int
	}{
		{400}, {403}, {404}, {500}, {503},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d", tt.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			body, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts()...)
			if err == nil {
				t.Fatalf("nil error for status %d", tt.status)
			}
			if body != nil {
				t.Errorf("non-nil body for error status %d", tt.status)
			}
			wantMsg := fmt.Sprintf("HTTP %d", tt.status)
			if !strings.Contains(err.Error(), wantMsg) {
				t.Errorf("error = %v, want containing %q", err, wantMsg)
			}
		})
	}
}

func TestRetry_non_transient_transport_error_fails_fast(t *testing.T) {
	client := &http.Client{
		Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return nil, errors.New("permanent transport failure")
		}),
	}
	_, err := httpx.Retry(t.Context(), client, "http://example.com/test", shortOpts()...)
	if err == nil {
		t.Fatal("expected error for non-transient transport error")
	}
	if strings.Contains(err.Error(), "retries exhausted") {
		t.Error("non-transient transport error should fail fast, not exhaust retries")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestRetry_WithHeaders_appliesHeadersToRequest(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	body, err := httpx.Retry(t.Context(), srv.Client(), srv.URL,
		httpx.WithBaseDelay(time.Millisecond),
		httpx.WithHeaders(func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer token123")
		}),
	)
	if err != nil {
		t.Fatalf("Retry = %v, want nil", err)
	}
	if string(body) != "ok" {
		t.Errorf("body = %q, want ok", body)
	}
	if gotAuth != "Bearer token123" {
		t.Errorf("server saw Authorization = %q, want Bearer token123", gotAuth)
	}
}

func TestRetry_retries_on_transient_transport_error(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{
		Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				return nil, io.ErrUnexpectedEOF
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("ok")),
				Header:     http.Header{},
			}, nil
		}),
	}
	body, err := httpx.Retry(t.Context(), client, "http://example.com/x",
		httpx.WithBaseDelay(time.Millisecond), httpx.WithMaxAttempts(3))
	if err != nil {
		t.Fatalf("Retry = %v, want nil after transient transport error", err)
	}
	if string(body) != "ok" {
		t.Errorf("body = %q, want ok", body)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("transport calls = %d, want 2", got)
	}
}

func TestRetry_create_request_error_fails_fast(t *testing.T) {
	// DEL control character (0x7f) makes http.NewRequestWithContext fail;
	// logSafeError drops the url.Error wrapper so the url-parse cause is returned.
	_, err := httpx.Retry(t.Context(), http.DefaultClient, "http://example.com/\x7f",
		httpx.WithBaseDelay(time.Millisecond))
	if err == nil {
		t.Fatal("Retry with malformed URL = nil, want error")
	}
	if strings.Contains(err.Error(), "retries exhausted") {
		t.Errorf("error = %v, want fast-fail (not retries-exhausted wrap)", err)
	}
	if !strings.Contains(err.Error(), "invalid control character") {
		t.Errorf("error = %v, want URL-parse failure cause", err)
	}
}

func TestRetry_read_response_body_error(t *testing.T) {
	client := &http.Client{
		Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       &errReadCloser{err: errors.New("read boom")},
				Header:     http.Header{},
			}, nil
		}),
	}
	_, err := httpx.Retry(t.Context(), client, "http://example.com/x",
		httpx.WithBaseDelay(time.Millisecond))
	if err == nil {
		t.Fatal("Retry with erroring body = nil, want error")
	}
	if !strings.Contains(err.Error(), "read response") {
		t.Errorf("error = %v, want containing read response", err)
	}
}

// errReadCloser fails every Read and records whether Close was called.
type errReadCloser struct {
	err    error
	closed bool
}

func (e *errReadCloser) Read(_ []byte) (int, error) { return 0, e.err }
func (e *errReadCloser) Close() error               { e.closed = true; return nil }

// errDrainBody fails every Read and is a no-op on Close (for Drain logging).
type errDrainBody struct{}

func (errDrainBody) Read([]byte) (int, error) { return 0, errors.New("read boom") }
func (errDrainBody) Close() error             { return nil }

//nolint:bodyclose // synthetic responses passed to the pure header parser; bodies never read
func TestParseRetryAfterResponse(t *testing.T) {
	t.Parallel()
	resp := func(retryAfter string) *http.Response {
		h := http.Header{}
		if retryAfter != "" {
			h.Set("Retry-After", retryAfter)
		}
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: h}
	}

	if d := httpx.ParseRetryAfterResponse(resp("")); d != 0 {
		t.Errorf("ParseRetryAfterResponse(no header) = %v, want 0", d)
	}
	if d := httpx.ParseRetryAfterResponse(resp("0")); d != 0 {
		t.Errorf("ParseRetryAfterResponse(\"0\") = %v, want 0", d)
	}
	if d := httpx.ParseRetryAfterResponse(resp("-1")); d != 0 {
		t.Errorf("ParseRetryAfterResponse(\"-1\") = %v, want 0", d)
	}
	if d := httpx.ParseRetryAfterResponse(resp("9")); d != 9*time.Second {
		t.Errorf("ParseRetryAfterResponse(\"9\") = %v, want 9s", d)
	}
	// Shared parseRetryAfterValue trims whitespace (delegation, not a per-copy Atoi).
	if d := httpx.ParseRetryAfterResponse(resp("  30  ")); d != 30*time.Second {
		t.Errorf("ParseRetryAfterResponse(\"  30  \") = %v, want 30s", d)
	}
	// Uncapped (unlike ParseRetryAfter): an above-max value caps only at the
	// int64-seconds overflow boundary, computed as maxSecs*time.Second.
	const maxSecs = 9223372036
	if d := httpx.ParseRetryAfterResponse(resp("9999999999")); d != time.Duration(maxSecs)*time.Second {
		t.Errorf("ParseRetryAfterResponse(overflow) = %v, want %v", d, time.Duration(maxSecs)*time.Second)
	}
	future := time.Now().Add(45 * time.Second).UTC().Format(http.TimeFormat)
	if d := httpx.ParseRetryAfterResponse(resp(future)); d <= 0 {
		t.Errorf("ParseRetryAfterResponse(future date) = %v, want > 0", d)
	}
	past := time.Now().Add(-45 * time.Second).UTC().Format(http.TimeFormat)
	if d := httpx.ParseRetryAfterResponse(resp(past)); d != 0 {
		t.Errorf("ParseRetryAfterResponse(past date) = %v, want 0", d)
	}
}

func TestRetry_zero_base_delay_defaults_to_base(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// A zero base delay must be coerced to DefaultBaseDelay (1s), so the pre-retry
	// sleep blows the 100ms deadline and surfaces the context error.
	_, err := httpx.Retry(ctx, srv.Client(), srv.URL, httpx.WithMaxAttempts(2), httpx.WithBaseDelay(0))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Retry(baseDelay=0) = %v, want context.DeadlineExceeded (delay defaulted)", err)
	}
}

func TestRetry_zero_max_body_bytes_defaults_to_full_body(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello world"))
	}))
	defer srv.Close()

	// Zero must be coerced to DefaultMaxBodyBytes, returning the whole body.
	body, err := httpx.Retry(t.Context(), srv.Client(), srv.URL,
		httpx.WithBaseDelay(time.Millisecond), httpx.WithMaxBodyBytes(0))
	if err != nil {
		t.Fatalf("Retry = %v, want nil", err)
	}
	if string(body) != "hello world" {
		t.Errorf("Retry(maxBodyBytes=0) body = %q, want %q", body, "hello world")
	}
}

func TestRetry_does_not_sleep_before_first_attempt(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// The first attempt must NOT sleep, so a fast response returns before the
	// deadline despite a huge base delay.
	body, err := httpx.Retry(ctx, srv.Client(), srv.URL, httpx.WithBaseDelay(10*time.Second))
	if err != nil {
		t.Fatalf("Retry(first attempt) = %v, want nil (no pre-first-attempt sleep)", err)
	}
	if string(body) != "ok" {
		t.Errorf("body = %q, want %q", body, "ok")
	}
}

func TestRetry_fast_response_does_not_log_slow_upstream(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	body, err := httpx.Retry(t.Context(), srv.Client(), srv.URL,
		httpx.WithBaseDelay(time.Millisecond), httpx.WithLogger(bufLogger(&buf)))
	if err != nil {
		t.Fatalf("Retry = %v, want nil", err)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q, want %q", body, "ok")
	}
	// A sub-second response must NOT trigger the >10s "slow upstream" warning.
	if strings.Contains(buf.String(), "slow upstream response") {
		t.Errorf("fast response logged slow-upstream warning:\n%s", buf.String())
	}
}

func TestRetry_retry_debug_log_reports_one_indexed_attempt(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	if _, err := httpx.Retry(t.Context(), srv.Client(), srv.URL,
		httpx.WithBaseDelay(time.Millisecond), httpx.WithLogger(bufLogger(&buf))); err != nil {
		t.Fatalf("Retry = %v, want nil", err)
	}
	logged := buf.String()
	if !strings.Contains(logged, "http request failed, will retry") {
		t.Fatalf("expected retry debug log, got:\n%s", logged)
	}
	// The first failure logs the human (1-indexed) attempt number, attempt+1 == 1.
	if !strings.Contains(logged, "attempt=1") {
		t.Errorf("retry debug log = %q, want attribute attempt=1", logged)
	}
}

// TestDrain_clean_drain_does_not_log_failure swaps slog.Default; not parallel.
func TestDrain_clean_drain_does_not_log_failure(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(bufLogger(&buf))
	defer slog.SetDefault(prev)

	// A body larger than the 64KB drain limit: CopyN returns nil (no EOF).
	httpx.Drain(io.NopCloser(strings.NewReader(strings.Repeat("y", 128<<10))))
	if strings.Contains(buf.String(), "failed to drain") {
		t.Errorf("clean drain logged a failure:\n%s", buf.String())
	}
}

// TestDrain_logs_on_non_eof_read_error swaps slog.Default; not parallel.
func TestDrain_logs_on_non_eof_read_error(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(bufLogger(&buf))
	defer slog.SetDefault(prev)

	httpx.Drain(errDrainBody{})
	if !strings.Contains(buf.String(), "failed to drain response body") {
		t.Errorf("Drain(erroring body) logged %q, want drain-failure debug line", buf.String())
	}
}

func TestDrainClose_closes_body_on_read_error(t *testing.T) {
	errReader := &errReadCloser{err: errors.New("read fail")}
	httpx.DrainClose(errReader)
	if !errReader.closed {
		t.Error("DrainClose did not close body on read error")
	}
}

// TestRetry_maxAttempts_nonpositive_clamps_to_one verifies a degenerate attempt
// count runs fn exactly once (try once), never the old coerce-to-default-3 and
// never a silent zero-attempt no-op.
func TestRetry_maxAttempts_nonpositive_clamps_to_one(t *testing.T) {
	t.Parallel()
	for _, maxAttempts := range []int{0, -1, -100} {
		var calls atomic.Int32
		client := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			calls.Add(1)
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     http.Header{},
			}, nil
		})}
		_, _ = httpx.Retry(t.Context(), client, "http://example.com/retry-edge",
			httpx.WithBaseDelay(time.Millisecond), httpx.WithMaxAttempts(maxAttempts))
		if got := calls.Load(); got != 1 {
			t.Errorf("WithMaxAttempts(%d): calls = %d, want 1 (clamped to a single attempt)", maxAttempts, got)
		}
	}
}

// TestRetry_cancel_during_backoff_aborts_before_next_attempt verifies that a
// context cancelled while Retry is sleeping between attempts aborts the loop
// immediately: the interrupted-sleep error propagates and no further attempt is
// made. The transport cancels the context on the first call so the subsequent
// backoff sleep observes a dead context; the WithHeaders hook counts attempts
// independently of the http.Client's transport-invocation details.
func TestRetry_cancel_during_backoff_aborts_before_next_attempt(t *testing.T) {
	var attempts atomic.Int32
	ctx, cancel := context.WithCancel(t.Context())
	client := &http.Client{
		Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			cancel() // cancel during the post-response backoff window
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     http.Header{},
			}, nil
		}),
	}

	// A one-hour base delay would dominate the run if the sleep were not
	// interrupted; because the context is already cancelled, SleepCtx returns at
	// once and the test stays fast.
	_, err := httpx.Retry(ctx, client, "http://example.com/cancel-during-backoff",
		httpx.WithBaseDelay(time.Hour),
		httpx.WithMaxAttempts(3),
		httpx.WithHeaders(func(*http.Request) { attempts.Add(1) }),
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Retry = %v, want context.Canceled (a cancelled backoff sleep must abort)", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (cancellation during backoff must abort before a second attempt)", got)
	}
}
