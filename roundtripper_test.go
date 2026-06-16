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

	"github.com/cplieger/httpx"
)

func TestRetryRoundTripper_success_no_retry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport, httpx.WithRTBaseDelay(time.Millisecond))
	client := rt.StandardClient()

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

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport, httpx.WithRTBaseDelay(time.Millisecond), httpx.WithMaxRetries(3))
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

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport, httpx.WithRTBaseDelay(time.Millisecond), httpx.WithMaxRetries(2))
	client := rt.StandardClient()

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

func TestRetryRoundTripper_no_retry_on_POST_without_opt_in(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport, httpx.WithRTBaseDelay(time.Millisecond), httpx.WithMaxRetries(3))
	client := rt.StandardClient()

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
	rt := httpx.NewRetryRoundTripper(srv.Client().Transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithMaxRetries(3),
		httpx.WithRetryNonIdempotent(true),
	)
	client := rt.StandardClient()

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

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithMaxRetries(3),
		httpx.WithRetryNonIdempotent(true),
	)
	client := rt.StandardClient()

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

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithMaxRetries(2),
		httpx.WithRetryNonIdempotent(true),
	)
	client := rt.StandardClient()

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
	rt := httpx.NewRetryRoundTripper(srv.Client().Transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithMaxRetries(2),
		httpx.WithRetryNonIdempotent(true),
	)
	client := rt.StandardClient()

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

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport, httpx.WithRTBaseDelay(time.Millisecond), httpx.WithMaxRetries(3))
	client := rt.StandardClient()

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
	rt := httpx.NewRetryRoundTripper(srv.Client().Transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithMaxRetries(3),
		httpx.WithOnRetry(func(attempt int, _ *http.Request, resp *http.Response, _ error) {
			hookCalls++
			if resp == nil {
				t.Error("OnRetry: resp should not be nil for HTTP error")
			}
			if attempt != hookCalls {
				t.Errorf("OnRetry: attempt = %d, want %d", attempt, hookCalls)
			}
		}),
	)
	client := rt.StandardClient()

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

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithMaxRetries(3),
		httpx.WithCheckRetry(func(_ context.Context, _ *http.Response, _ error) (bool, error) {
			return false, nil
		}),
	)
	client := rt.StandardClient()

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
	rt := httpx.NewRetryRoundTripper(srv.Client().Transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithMaxRetries(3),
		httpx.WithCheckRetry(func(_ context.Context, _ *http.Response, _ error) (bool, error) {
			return false, wantErr
		}),
	)
	client := rt.StandardClient()

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

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithMaxRetries(2),
		httpx.WithPrepareRetry(func(req *http.Request) error {
			req.Header.Set("X-Refreshed", "true")
			return nil
		}),
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
}

func TestRetryRoundTripper_PrepareRetry_error_aborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	wantErr := errors.New("token refresh failed")
	rt := httpx.NewRetryRoundTripper(srv.Client().Transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithMaxRetries(2),
		httpx.WithPrepareRetry(func(_ *http.Request) error {
			return wantErr
		}),
	)
	client := rt.StandardClient()

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

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport,
		httpx.WithRTBaseDelay(100*time.Millisecond),
		httpx.WithMaxRetries(10),
	)
	client := rt.StandardClient()

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

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport, httpx.WithRTBaseDelay(time.Millisecond), httpx.WithMaxRetries(2))
	client := rt.StandardClient()

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

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport, httpx.WithRTBaseDelay(time.Millisecond), httpx.WithMaxRetries(2))
	client := rt.StandardClient()

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

func TestRetryRoundTripper_MaxElapsedTime(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport,
		httpx.WithRTBaseDelay(50*time.Millisecond),
		httpx.WithMaxRetries(10),
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
	if got := calls.Load(); got > 4 {
		t.Errorf("calls = %d, want <= 4 with MaxElapsedTime", got)
	}
}

func TestRetryRoundTripper_PermanentError_stops_retry(t *testing.T) {
	var calls atomic.Int32
	permErr := httpx.Permanent(errors.New("do not retry"))
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, permErr
	})

	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithMaxRetries(5),
	)
	client := rt.StandardClient()

	_, err := client.Get("http://example.com/test") //nolint:bodyclose // error path returns no body
	if err == nil {
		t.Fatal("expected error")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1 (PermanentError should not retry)", got)
	}
}

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
		httpx.WithMaxRetries(5),
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
		httpx.WithMaxRetries(10),
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
