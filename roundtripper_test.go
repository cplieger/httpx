package httpx_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/httpx/v3"
)

func TestRetryRoundTripper_success_no_retry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport, httpx.TransportConfig{BaseDelay: time.Millisecond})
	client := &http.Client{Transport: rt}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestRetryRoundTripper_retries_on_503(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport, httpx.TransportConfig{BaseDelay: time.Millisecond, MaxAttempts: 4})
	client := &http.Client{Transport: rt}

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

func TestRetryRoundTripper_retries_on_429_with_retry_after(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport, httpx.TransportConfig{BaseDelay: time.Millisecond, MaxAttempts: 3})
	client := &http.Client{Transport: rt}

	start := time.Now()
	resp, err := client.Get(srv.URL)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("elapsed = %v, want >= 900ms (Retry-After honored)", elapsed)
	}
}

// TestRetryRoundTripper_honors_retry_after_on_503 asserts the RoundTripper
// honors Retry-After on a 5xx, not just on 429 (cycle-1 h-f4 broadened the
// sleepBeforeRetry override to any retryable response: 429/502/503/504).
func TestRetryRoundTripper_honors_retry_after_on_503(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"502", http.StatusBadGateway},
		{"503", http.StatusServiceUnavailable},
		{"504", http.StatusGatewayTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(tt.status)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			rt := httpx.NewRetryRoundTripper(srv.Client().Transport, httpx.TransportConfig{BaseDelay: time.Millisecond, MaxAttempts: 3})
			client := &http.Client{Transport: rt}

			start := time.Now()
			resp, err := client.Get(srv.URL)
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200", resp.StatusCode)
			}
			// Retry-After: 1 must be honored on the 5xx, not just on 429.
			if elapsed < 900*time.Millisecond {
				t.Errorf("elapsed = %v, want >= 900ms (Retry-After honored on %s)", elapsed, tt.name)
			}
		})
	}
}

func TestRetryRoundTripper_no_retry_on_POST_without_opt_in(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport, httpx.TransportConfig{BaseDelay: time.Millisecond, MaxAttempts: 4})
	client := &http.Client{Transport: rt}

	req, _ := http.NewRequest(http.MethodPost, srv.URL, http.NoBody)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1 (no retry for POST without opt-in)", got)
	}
}

func TestRetryRoundTripper_POST_with_GetBody_and_opt_in(t *testing.T) {
	var calls atomic.Int32
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	bodyContent := "hello-body"
	rt := httpx.NewRetryRoundTripper(srv.Client().Transport, httpx.TransportConfig{BaseDelay: time.Millisecond, MaxAttempts: 4, RetryNonIdempotent: true})
	client := &http.Client{Transport: rt}

	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(bodyContent))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(bodyContent)), nil
	}
	resp, err := client.Do(req)
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
	for i, b := range bodies {
		if b != bodyContent {
			t.Errorf("attempt %d body = %q, want %q", i+1, b, bodyContent)
		}
	}
}

func TestRetryRoundTripper_POST_without_GetBody_no_retry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport, httpx.TransportConfig{BaseDelay: time.Millisecond, MaxAttempts: 4, RetryNonIdempotent: true})
	client := &http.Client{Transport: rt}

	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("data"))
	req.GetBody = nil
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1 (no retry without GetBody)", got)
	}
}

func TestRetryRoundTripper_DELETE_no_body_with_opt_in(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport, httpx.TransportConfig{BaseDelay: time.Millisecond, MaxAttempts: 3, RetryNonIdempotent: true})
	client := &http.Client{Transport: rt}

	req, _ := http.NewRequest(http.MethodDelete, srv.URL, http.NoBody)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("calls = %d, want 2", got)
	}
}

func TestRetryRoundTripper_PUT_with_bytes_buffer_GetBody(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if string(b) != "payload" {
			t.Errorf("body = %q, want %q", b, "payload")
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	payload := []byte("payload")
	rt := httpx.NewRetryRoundTripper(srv.Client().Transport, httpx.TransportConfig{BaseDelay: time.Millisecond, MaxAttempts: 3, RetryNonIdempotent: true})
	client := &http.Client{Transport: rt}

	req, _ := http.NewRequest(http.MethodPut, srv.URL, bytes.NewReader(payload))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(payload)), nil
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestRetryRoundTripper_no_retry_on_4xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport, httpx.TransportConfig{BaseDelay: time.Millisecond, MaxAttempts: 4})
	client := &http.Client{Transport: rt}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1 (no retry for 404)", got)
	}
}

func TestRetryRoundTripper_OnRetry_hook(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var hookCalls int
	rt := httpx.NewRetryRoundTripper(srv.Client().Transport, httpx.TransportConfig{BaseDelay: time.Millisecond, MaxAttempts: 4, OnRetry: func(attempt int, _ *http.Request, resp *http.Response, _ error) {
		hookCalls++
		if resp == nil {
			t.Error("OnRetry: resp should not be nil for HTTP error")
		}
		if attempt != hookCalls {
			t.Errorf("OnRetry: attempt = %d, want %d", attempt, hookCalls)
		}
	}})
	client := &http.Client{Transport: rt}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if hookCalls != 2 {
		t.Errorf("OnRetry called %d times, want 2", hookCalls)
	}
}

func TestRetryRoundTripper_CheckRetry_custom(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport, httpx.TransportConfig{BaseDelay: time.Millisecond, MaxAttempts: 4, CheckRetry: func(_ context.Context, _ *http.Response, _ error) (bool, error) {
		return false, nil
	}})
	client := &http.Client{Transport: rt}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1 (custom policy says no retry)", got)
	}
}

func TestRetryRoundTripper_CheckRetry_error_shortcircuit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	wantErr := errors.New("policy abort")
	rt := httpx.NewRetryRoundTripper(srv.Client().Transport, httpx.TransportConfig{BaseDelay: time.Millisecond, MaxAttempts: 4, CheckRetry: func(_ context.Context, _ *http.Response, _ error) (bool, error) {
		return false, wantErr
	}})
	client := &http.Client{Transport: rt}

	resp, err := client.Get(srv.URL) //nolint:bodyclose // error path, no body
	_ = resp
	if err == nil {
		t.Fatal("expected error from CheckRetry")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want wrapping %v", err, wantErr)
	}
}

func TestRetryRoundTripper_PrepareRetry_hook(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if r.Header.Get("X-Refreshed") != "true" {
			t.Error("PrepareRetry did not set header")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport, httpx.TransportConfig{BaseDelay: time.Millisecond, MaxAttempts: 3, PrepareRetry: func(req *http.Request) error {
		req.Header.Set("X-Refreshed", "true")
		return nil
	}})
	client := &http.Client{Transport: rt}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestRetryRoundTripper_PrepareRetry_error_aborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	wantErr := errors.New("token refresh failed")
	rt := httpx.NewRetryRoundTripper(srv.Client().Transport, httpx.TransportConfig{BaseDelay: time.Millisecond, MaxAttempts: 3, PrepareRetry: func(_ *http.Request) error {
		return wantErr
	}})
	client := &http.Client{Transport: rt}

	resp, err := client.Get(srv.URL) //nolint:bodyclose // error path, no body
	_ = resp
	if err == nil {
		t.Fatal("expected error from PrepareRetry")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want wrapping %v", err, wantErr)
	}
}

func TestRetryRoundTripper_context_cancellation(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport, httpx.TransportConfig{BaseDelay: 100 * time.Millisecond, MaxAttempts: 11})
	client := &http.Client{Transport: rt}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)
	resp, err := client.Do(req) //nolint:bodyclose // error path, no body
	_ = resp
	if err == nil {
		t.Fatal("expected error from context cancellation")
	}
	if got := calls.Load(); got > 3 {
		t.Errorf("calls = %d, want <= 3 with context cancellation", got)
	}
}

func TestRetryRoundTripper_exhausts_retries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport, httpx.TransportConfig{BaseDelay: time.Millisecond, MaxAttempts: 3})
	client := &http.Client{Transport: rt}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (last response)", resp.StatusCode)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("calls = %d, want 3 (1 initial + 2 retries)", got)
	}
}

func TestRetryRoundTripper_HEAD_retried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport, httpx.TransportConfig{BaseDelay: time.Millisecond, MaxAttempts: 3})
	client := &http.Client{Transport: rt}

	resp, err := client.Head(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("calls = %d, want 2", got)
	}
}

func TestRetryRoundTripper_PermanentError_stops_retry(t *testing.T) {
	var calls atomic.Int32
	permErr := httpx.Permanent(errors.New("do not retry"))
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, permErr
	})

	rt := httpx.NewRetryRoundTripper(transport, httpx.TransportConfig{BaseDelay: time.Millisecond, MaxAttempts: 6})
	client := &http.Client{Transport: rt}

	_, err := client.Get("http://example.com/test") //nolint:bodyclose // error path returns no body
	if err == nil {
		t.Fatal("expected error")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1 (PermanentError should not retry)", got)
	}
}

func TestRetryRoundTripper_GetBody_error_aborts_retry(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     http.Header{},
		}, nil
	})
	rt := httpx.NewRetryRoundTripper(transport, httpx.TransportConfig{BaseDelay: time.Millisecond, MaxAttempts: 4, RetryNonIdempotent: true})
	req, _ := http.NewRequest(http.MethodPost, "http://example.com/x", strings.NewReader("payload"))
	wantErr := errors.New("rewind boom")
	req.GetBody = func() (io.ReadCloser, error) { return nil, wantErr }
	resp, err := rt.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("RoundTrip with failing GetBody = nil, want error")
	}
	if !strings.Contains(err.Error(), "rewind request body") {
		t.Errorf("error = %v, want containing rewind request body", err)
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want wrapping the GetBody error", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("transport calls = %d, want 1", got)
	}
}

func TestRetryRoundTripper_cancel_after_retryable_response_skips_onRetry(t *testing.T) {
	var calls atomic.Int32
	var onRetryCalls atomic.Int32
	ctx, cancel := context.WithCancel(t.Context())
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls.Add(1)
		cancel()
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     http.Header{},
		}, nil
	})
	rt := httpx.NewRetryRoundTripper(transport, httpx.TransportConfig{BaseDelay: time.Hour, MaxAttempts: 3, OnRetry: func(int, *http.Request, *http.Response, error) {
		onRetryCalls.Add(1)
	}})
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com/cancel", http.NoBody)
	resp, err := rt.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RoundTrip = %v, want context.Canceled", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("transport calls = %d, want 1", got)
	}
	if got := onRetryCalls.Load(); got != 0 {
		t.Errorf("OnRetry calls = %d, want 0 (no retry once context is cancelled)", got)
	}
}

// TestRetryRoundTripper_defaultCheckRetry_status_table pins the built-in policy:
// 429/502/503/504 retry; 200/4xx and (notably) 500 do NOT. The 500 exclusion is
// a documented divergence from the one-shot Retry helper.
func TestRetryRoundTripper_defaultCheckRetry_status_table(t *testing.T) {
	t.Parallel()
	statusCalls := func(t *testing.T, code, wantCalls int) {
		t.Helper()
		var calls atomic.Int32
		transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			calls.Add(1)
			return &http.Response{
				StatusCode: code,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     http.Header{},
			}, nil
		})
		rt := httpx.NewRetryRoundTripper(transport, httpx.TransportConfig{BaseDelay: time.Millisecond, MaxAttempts: 3})
		req, _ := http.NewRequest(http.MethodGet, "http://example.com", http.NoBody)
		resp, _ := rt.RoundTrip(req)
		if resp != nil {
			resp.Body.Close()
		}
		if got := int(calls.Load()); got != wantCalls {
			t.Errorf("code %d: calls = %d, want %d", code, got, wantCalls)
		}
	}
	for _, code := range []int{200, 400, 401, 403, 404, 500} {
		statusCalls(t, code, 1) // not retried
	}
	for _, code := range []int{429, 502, 503, 504} {
		statusCalls(t, code, 3) // retried to exhaustion
	}
}

// TestRetryRoundTripper_nil_callbacks_no_panic verifies that explicitly passing
// nil hooks (WithPrepareRetry(nil)/WithOnRetry(nil)/WithCheckRetry(nil)) is safe:
// nil onRetry/prepareRetry are skipped and a nil checkRetry falls back to the
// default policy.
func TestRetryRoundTripper_nil_callbacks_no_panic(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		if calls.Add(1) < 2 {
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

	rt := httpx.NewRetryRoundTripper(transport, httpx.TransportConfig{BaseDelay: time.Millisecond, MaxAttempts: 3, PrepareRetry: nil, OnRetry: nil, CheckRetry: nil})
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/nilcb", http.NoBody)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("nil callbacks caused error: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestRetryRoundTripper_nil_transport_does_not_panic verifies the constructor
// falls back to http.DefaultTransport when next is nil.
func TestRetryRoundTripper_nil_transport_does_not_panic(t *testing.T) {
	t.Parallel()
	rt := httpx.NewRetryRoundTripper(nil, httpx.TransportConfig{MaxAttempts: 1})
	if rt == nil {
		t.Fatal("NewRetryRoundTripper(nil, httpx.TransportConfig{}) returned nil")
	}
}

// TestRetryRoundTripper_maxAttempts_zero_with_custom_checkRetry verifies the
// attempt clamp dominates: even an always-retry custom policy cannot force a
// second attempt when maxAttempts clamps to 1.
func TestRetryRoundTripper_maxAttempts_zero_with_custom_checkRetry(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, io.ErrUnexpectedEOF
	})
	rt := httpx.NewRetryRoundTripper(transport, httpx.TransportConfig{MaxAttempts: -1, CheckRetry: func(_ context.Context, _ *http.Response, _ error) (bool, error) {
		return true, nil // always retry — but a negative MaxAttempts means one attempt
	}})
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/zero-custom", http.NoBody)
	_, err := rt.RoundTrip(req) //nolint:bodyclose // error path, no response body
	if err == nil {
		t.Fatal("expected error from the single clamped attempt")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("TransportConfig{MaxAttempts: -1}+custom CheckRetry: calls=%d, want 1 (a single attempt)", got)
	}
}

// TestRetryRoundTripper_drains_every_discarded_response_body verifies each
// discarded retry response is drained-and-closed: 3 intermediate + 1 final.
func TestRetryRoundTripper_drains_every_discarded_response_body(t *testing.T) {
	t.Parallel()
	var closed atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body: &trackingCloser{
				Reader:  strings.NewReader("response-body-data"),
				onClose: func() { closed.Add(1) },
			},
			Header: http.Header{},
		}, nil
	})

	rt := httpx.NewRetryRoundTripper(transport, httpx.TransportConfig{BaseDelay: time.Millisecond, MaxAttempts: 4})
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
