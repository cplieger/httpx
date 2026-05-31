package httpx_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/httpx"
)

func shortOpts() httpx.Options {
	return httpx.Options{BaseDelay: time.Millisecond, MaxBodyBytes: 10 << 20}
}

func TestRetry_success_first_try(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	body, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts())
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

	if _, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts()); err == nil {
		t.Error("expected error for 404")
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

			body, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts())
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

	body, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts())
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

	opts := httpx.Options{BaseDelay: 100 * time.Millisecond, MaxBodyBytes: 10 << 20}
	_, err := httpx.Retry(ctx, srv.Client(), srv.URL, opts)
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

	opts := httpx.Options{BaseDelay: time.Millisecond, MaxBodyBytes: 10 << 20}
	body, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, opts)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if len(body) != 10<<20 {
		t.Errorf("body size = %d, want %d", len(body), 10<<20)
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
	body, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts())
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
		{"delta seconds capped at 60", "120", 60 * time.Second},
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

func TestRedirectPolicy(t *testing.T) {
	makeReq := func(host string) *http.Request {
		u, _ := url.Parse("https://" + host + "/some/path")
		return &http.Request{URL: u}
	}
	makeVia := func(n int) []*http.Request {
		via := make([]*http.Request, n)
		for i := range n {
			via[i] = &http.Request{}
		}
		return via
	}

	tests := []struct {
		name    string
		host    string
		viaLen  int
		wantErr bool
	}{
		{"hub.docker.com allowed", "hub.docker.com", 0, false},
		{"subdomain of docker.com allowed", "auth.docker.com", 0, false},
		{"github.com allowed", "github.com", 0, false},
		{"subdomain of github.com allowed", "api.github.com", 0, false},
		{"githubusercontent.com allowed", "raw.githubusercontent.com", 0, false},
		{"evil.com refused", "evil.com", 0, true},
		{"localhost refused", "localhost", 0, true},
		{"127.0.0.1 refused", "127.0.0.1", 0, true},
		{"too many redirects", "hub.docker.com", 5, true},
		{"4 redirects still ok", "hub.docker.com", 4, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := httpx.RedirectPolicy(makeReq(tt.host), makeVia(tt.viaLen))
			if tt.wantErr && err == nil {
				t.Errorf("RedirectPolicy(%q, via=%d) = nil, want error", tt.host, tt.viaLen)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("RedirectPolicy(%q, via=%d) = %v, want nil", tt.host, tt.viaLen, err)
			}
		})
	}
}

func TestRedirectPolicyFunc(t *testing.T) {
	cfg := &httpx.RedirectConfig{
		AllowedHosts:    []string{"example.com"},
		AllowedSuffixes: []string{".example.org"},
		MaxHops:         3,
	}
	policy := httpx.RedirectPolicyFunc(cfg)

	makeReq := func(host string) *http.Request {
		u, _ := url.Parse("https://" + host + "/path")
		return &http.Request{URL: u}
	}
	makeVia := func(n int) []*http.Request {
		via := make([]*http.Request, n)
		for i := range n {
			via[i] = &http.Request{}
		}
		return via
	}

	tests := []struct {
		name    string
		host    string
		viaLen  int
		wantErr bool
	}{
		{"exact host allowed", "example.com", 0, false},
		{"suffix allowed", "sub.example.org", 0, false},
		{"unknown refused", "evil.com", 0, true},
		{"too many hops", "example.com", 3, true},
		{"2 hops ok", "example.com", 2, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := policy(makeReq(tt.host), makeVia(tt.viaLen))
			if tt.wantErr && err == nil {
				t.Errorf("want error for %s via=%d", tt.host, tt.viaLen)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error for %s via=%d: %v", tt.host, tt.viaLen, err)
			}
		})
	}

	// nil config refuses all
	nilPolicy := httpx.RedirectPolicyFunc(nil)
	if err := nilPolicy(makeReq("example.com"), nil); err == nil {
		t.Error("nil config should refuse all redirects")
	}
}

func TestNewClient_wires_timeout_and_redirect_policy(t *testing.T) {
	c := httpx.NewClient(42 * time.Second)
	if c.Timeout != 42*time.Second {
		t.Errorf("Timeout = %v, want 42s", c.Timeout)
	}
	if c.CheckRedirect == nil {
		t.Fatal("CheckRedirect is nil")
	}
	u, _ := url.Parse("https://evil.com/x")
	if err := c.CheckRedirect(&http.Request{URL: u}, nil); err == nil {
		t.Error("CheckRedirect(evil.com) = nil, want error")
	}
}

func TestDrain_small_body(t *testing.T) {
	httpx.Drain(io.NopCloser(strings.NewReader("small")))
}

func TestDrain_truncated_at_limit(t *testing.T) {
	httpx.Drain(io.NopCloser(strings.NewReader(strings.Repeat("y", 128<<10))))
}

func TestCheckHTTPStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		statusCode int
		wantNil    bool
		wantAuth   bool
		wantRate   bool
		wantStatus bool
	}{
		{"200 OK", 200, true, false, false, false},
		{"201 Created", 201, true, false, false, false},
		{"400 Bad Request", 400, false, false, false, true},
		{"401 Unauthorized", 401, false, true, false, false},
		{"403 Forbidden", 403, false, true, false, false},
		{"429 Too Many Requests", 429, false, false, true, false},
		{"500 Internal Server Error", 500, false, false, false, true},
		{"502 Bad Gateway", 502, false, false, false, true},
		{"503 Service Unavailable", 503, false, false, false, true},
		{"504 Gateway Timeout", 504, false, false, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			resp := &http.Response{StatusCode: tt.statusCode, Header: http.Header{}}
			err := httpx.CheckHTTPStatus(resp)
			if tt.wantNil {
				if err != nil {
					t.Errorf("CheckHTTPStatus(%d) = %v, want nil", tt.statusCode, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckHTTPStatus(%d) = nil, want error", tt.statusCode)
			}
			var authErr *httpx.AuthError
			var rateErr *httpx.RateLimitError
			var statusErr *httpx.HTTPStatusError
			if tt.wantAuth && !errors.As(err, &authErr) {
				t.Errorf("CheckHTTPStatus(%d) = %T, want *AuthError", tt.statusCode, err)
			}
			if tt.wantRate && !errors.As(err, &rateErr) {
				t.Errorf("CheckHTTPStatus(%d) = %T, want *RateLimitError", tt.statusCode, err)
			}
			if tt.wantStatus && !errors.As(err, &statusErr) {
				t.Errorf("CheckHTTPStatus(%d) = %T, want *HTTPStatusError", tt.statusCode, err)
			}
		})
	}
}

func TestCheckHTTPStatus_429_parses_retry_after(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("Retry-After", "30")
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: h}
	err := httpx.CheckHTTPStatus(resp)
	var rl *httpx.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("expected *RateLimitError, got %T", err)
	}
	if rl.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter = %v, want 30s", rl.RetryAfter)
	}
}

func TestCheckHTTPStatus_429_parses_http_date(t *testing.T) {
	t.Parallel()
	future := time.Now().Add(45 * time.Second).UTC().Format(http.TimeFormat)
	h := http.Header{}
	h.Set("Retry-After", future)
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: h}
	err := httpx.CheckHTTPStatus(resp)
	var rl *httpx.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("expected *RateLimitError, got %T", err)
	}
	if rl.RetryAfter < 30*time.Second || rl.RetryAfter > 60*time.Second {
		t.Errorf("RetryAfter = %v, want ~45s", rl.RetryAfter)
	}
}

// --- StatusError tests from registry-stats ---

func TestStatusError_Error(t *testing.T) {
	err := &httpx.StatusError{Code: 503, URL: "http://example.com/x"}
	want := "HTTP 503 from http://example.com/x"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestStatusError_IsRateLimited(t *testing.T) {
	tests := []struct {
		code int
		want bool
	}{
		{429, true},
		{500, false},
		{503, false},
		{400, false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d", tt.code), func(t *testing.T) {
			err := &httpx.StatusError{Code: tt.code}
			if got := errors.Is(err, httpx.ErrRateLimited); got != tt.want {
				t.Errorf("errors.Is(%d, ErrRateLimited) = %v, want %v", tt.code, got, tt.want)
			}
		})
	}
}

func TestStatusError_IsServerError(t *testing.T) {
	tests := []struct {
		code int
		want bool
	}{
		{500, true},
		{502, true},
		{503, true},
		{599, true},
		{429, false},
		{400, false},
		{600, false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d", tt.code), func(t *testing.T) {
			err := &httpx.StatusError{Code: tt.code}
			if got := errors.Is(err, httpx.ErrServerError); got != tt.want {
				t.Errorf("errors.Is(%d, ErrServerError) = %v, want %v", tt.code, got, tt.want)
			}
		})
	}
}

func TestRetry_returns_typed_StatusError_on_exhaustion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts())
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

	_, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts())
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

	_, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts())
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

			body, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts())
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
