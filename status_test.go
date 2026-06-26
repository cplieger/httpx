package httpx_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/cplieger/httpx/v2"
)

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
		{"301 Moved Permanently", 301, true, false, false, false},
		{"399 boundary below 400", 399, true, false, false, false},
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

func TestCheckHTTPStatus_informational_1xx_returns_nil(t *testing.T) {
	t.Parallel()
	if err := httpx.CheckHTTPStatus(&http.Response{StatusCode: http.StatusContinue, Header: http.Header{}}); err != nil {
		t.Errorf("CheckHTTPStatus(100) = %v, want nil (sub-200 is not an error)", err)
	}
	if err := httpx.CheckHTTPStatus(&http.Response{StatusCode: 199, Header: http.Header{}}); err != nil {
		t.Errorf("CheckHTTPStatus(199) = %v, want nil (sub-200 is not an error)", err)
	}
}

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

func TestStatusError_Is_unrelated_target_returns_false(t *testing.T) {
	se := &httpx.StatusError{Code: http.StatusTooManyRequests, URL: "http://example.com"}
	if errors.Is(se, io.EOF) {
		t.Error("errors.Is(StatusError{429}, io.EOF) = true, want false")
	}
	if errors.Is(se, context.Canceled) {
		t.Error("errors.Is(StatusError{429}, context.Canceled) = true, want false")
	}
}
