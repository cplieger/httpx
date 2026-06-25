package httpx

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"
)

func TestIsTransient(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err  error
		name string
		want bool
	}{
		{name: "nil error", err: nil, want: false},
		{name: "AuthError is not transient", err: &AuthError{Msg: "bad key"}, want: false},
		{name: "RateLimitError is not transient", err: &RateLimitError{Msg: "slow down"}, want: false},
		{name: "context.DeadlineExceeded is not transient", err: context.DeadlineExceeded, want: false},
		{name: "context.Canceled is not transient", err: context.Canceled, want: false},
		{name: "wrapped DeadlineExceeded is not transient", err: errors.Join(errors.New("op"), context.DeadlineExceeded), want: false},
		{name: "io.ErrUnexpectedEOF is transient", err: io.ErrUnexpectedEOF, want: true},
		{name: "ECONNRESET is transient", err: syscall.ECONNRESET, want: true},
		{name: "ECONNREFUSED is transient", err: syscall.ECONNREFUSED, want: true},
		{name: "DNSError is transient", err: &net.DNSError{Err: "lookup failed", Name: "example.com"}, want: true},
		{name: "net timeout error is transient", err: &fakeNetError{timeout: true}, want: true},
		{name: "net non-timeout error is not transient", err: &fakeNetError{timeout: false}, want: false},
		{name: "HTTPStatusError 502 is transient", err: &HTTPStatusError{Code: 502}, want: true},
		{name: "HTTPStatusError 503 is transient", err: &HTTPStatusError{Code: 503}, want: true},
		{name: "HTTPStatusError 504 is transient", err: &HTTPStatusError{Code: 504}, want: true},
		{name: "HTTPStatusError 400 is not transient", err: &HTTPStatusError{Code: 400}, want: false},
		{name: "HTTPStatusError 404 is not transient", err: &HTTPStatusError{Code: 404}, want: false},
		{name: "generic error is not transient", err: errors.New("something failed"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := IsTransient(tt.err)
			if got != tt.want {
				t.Errorf("IsTransient(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

type fakeNetError struct{ timeout bool }

func (e *fakeNetError) Error() string   { return "fake net error" }
func (e *fakeNetError) Timeout() bool   { return e.timeout }
func (e *fakeNetError) Temporary() bool { return false }

func TestRetryOnRateLimit(t *testing.T) {
	t.Parallel()

	t.Run("success on first call", func(t *testing.T) {
		t.Parallel()
		calls := 0
		err := RetryOnRateLimit(context.Background(), 3, 5*time.Second, func(_ context.Context) error {
			calls++
			return nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if calls != 1 {
			t.Fatalf("expected 1 call, got %d", calls)
		}
	})

	t.Run("non-rate-limit error returns immediately", func(t *testing.T) {
		t.Parallel()
		calls := 0
		wantErr := errors.New("permanent failure")
		err := RetryOnRateLimit(context.Background(), 3, 5*time.Second, func(_ context.Context) error {
			calls++
			return wantErr
		})
		if !errors.Is(err, wantErr) {
			t.Fatalf("expected %v, got %v", wantErr, err)
		}
		if calls != 1 {
			t.Fatalf("expected 1 call, got %d", calls)
		}
	})

	t.Run("rate-limit error retries", func(t *testing.T) {
		t.Parallel()
		calls := 0
		err := RetryOnRateLimit(context.Background(), 3, 5*time.Second, func(_ context.Context) error {
			calls++
			if calls < 3 {
				return &RateLimitError{Msg: "slow", RetryAfter: time.Millisecond}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if calls != 3 {
			t.Fatalf("expected 3 calls, got %d", calls)
		}
	})

	t.Run("exhausts attempts returns last error", func(t *testing.T) {
		t.Parallel()
		calls := 0
		err := RetryOnRateLimit(context.Background(), 2, 5*time.Second, func(_ context.Context) error {
			calls++
			return &RateLimitError{Msg: "slow", RetryAfter: time.Millisecond}
		})
		if err == nil {
			t.Fatal("expected error after exhausting attempts")
		}
		var rlErr *RateLimitError
		if !errors.As(err, &rlErr) {
			t.Fatalf("expected RateLimitError, got %T: %v", err, err)
		}
		if calls != 2 {
			t.Fatalf("expected 2 calls, got %d", calls)
		}
	})

	t.Run("context cancellation stops retry", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		err := RetryOnRateLimit(ctx, 5, 5*time.Second, func(_ context.Context) error {
			calls++
			if calls == 1 {
				cancel()
			}
			return &RateLimitError{Msg: "slow", RetryAfter: time.Millisecond}
		})
		if err == nil {
			t.Fatal("expected error from cancelled context")
		}
		if calls > 2 {
			t.Fatalf("expected at most 2 calls with cancellation, got %d", calls)
		}
	})

	t.Run("context is passed to fn", func(t *testing.T) {
		t.Parallel()
		ctx := context.WithValue(context.Background(), ctxKey{}, "test-value")
		err := RetryOnRateLimit(ctx, 1, time.Second, func(fnCtx context.Context) error {
			if fnCtx.Value(ctxKey{}) != "test-value" {
				t.Error("context not propagated to fn")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

type ctxKey struct{}

func TestRetryWithBackoff(t *testing.T) {
	t.Parallel()

	t.Run("success on first call", func(t *testing.T) {
		t.Parallel()
		calls := 0
		result, err := RetryWithBackoff(context.Background(), 3, time.Millisecond, "test", func(_ context.Context) (string, error) {
			calls++
			return "ok", nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result != "ok" {
			t.Fatalf("expected 'ok', got %q", result)
		}
		if calls != 1 {
			t.Fatalf("expected 1 call, got %d", calls)
		}
	})

	t.Run("retries on error then succeeds", func(t *testing.T) {
		t.Parallel()
		calls := 0
		result, err := RetryWithBackoff(context.Background(), 4, time.Millisecond, "test", func(_ context.Context) (int, error) {
			calls++
			if calls < 3 {
				return 0, &HTTPStatusError{Code: 503}
			}
			return 42, nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result != 42 {
			t.Fatalf("expected 42, got %d", result)
		}
		if calls != 3 {
			t.Fatalf("expected 3 calls, got %d", calls)
		}
	})

	t.Run("exhausts retries returns last error", func(t *testing.T) {
		t.Parallel()
		calls := 0
		_, err := RetryWithBackoff(context.Background(), 3, time.Millisecond, "test", func(_ context.Context) (string, error) {
			calls++
			return "", &HTTPStatusError{Code: 502}
		})
		if err == nil {
			t.Fatal("expected error after exhausting retries")
		}
		if calls != 3 {
			t.Fatalf("expected 3 calls, got %d", calls)
		}
	})

	t.Run("non-transient error not retried", func(t *testing.T) {
		t.Parallel()
		calls := 0
		_, err := RetryWithBackoff(context.Background(), 3, time.Millisecond, "test", func(_ context.Context) (string, error) {
			calls++
			return "", errors.New("permanent failure")
		})
		if err == nil {
			t.Fatal("expected error")
		}
		if calls != 1 {
			t.Fatalf("expected 1 call, non-transient not retried, got %d", calls)
		}
	})

	t.Run("context cancellation stops retry", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		_, err := RetryWithBackoff(ctx, 5, time.Millisecond, "test", func(_ context.Context) (string, error) {
			calls++
			if calls == 1 {
				cancel()
			}
			return "", &HTTPStatusError{Code: 503}
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	})
}

func TestHTTPStatusError_preserves_wire_format(t *testing.T) {
	t.Parallel()
	got := (&HTTPStatusError{Code: 503}).Error()
	if got != "HTTP 503" {
		t.Errorf("HTTPStatusError{503}.Error() = %q, want %q", got, "HTTP 503")
	}
}

func TestResponseTooLargeError_Error_message(t *testing.T) {
	t.Parallel()
	got := (&ResponseTooLargeError{Limit: 1024}).Error()
	want := "response body exceeds 1024 bytes"
	if got != want {
		t.Errorf("ResponseTooLargeError{Limit:1024}.Error() = %q, want %q", got, want)
	}
}

func TestPermanentError(t *testing.T) {
	t.Parallel()

	t.Run("wraps and unwraps", func(t *testing.T) {
		t.Parallel()
		inner := errors.New("bad config")
		pe := Permanent(inner)
		if pe == nil {
			t.Fatal("Permanent(non-nil) returned nil")
		}
		if pe.Error() != "bad config" {
			t.Errorf("Error() = %q, want %q", pe.Error(), "bad config")
		}
		if !errors.Is(pe, inner) {
			t.Error("errors.Is(pe, inner) = false")
		}
		var target *PermanentError
		if !errors.As(pe, &target) {
			t.Error("errors.As(*PermanentError) = false")
		}
	})

	t.Run("nil returns nil", func(t *testing.T) {
		t.Parallel()
		if Permanent(nil) != nil {
			t.Error("Permanent(nil) should return nil")
		}
	})

	t.Run("IsPermanent", func(t *testing.T) {
		t.Parallel()
		if IsPermanent(errors.New("normal")) {
			t.Error("normal error should not be permanent")
		}
		if !IsPermanent(Permanent(errors.New("x"))) {
			t.Error("Permanent error should be permanent")
		}
	})

	t.Run("IsTransient returns false for PermanentError", func(t *testing.T) {
		t.Parallel()
		pe := Permanent(&HTTPStatusError{Code: 503})
		if IsTransient(pe) {
			t.Error("PermanentError wrapping transient should not be transient")
		}
	})

	t.Run("RetryWithBackoff does not retry PermanentError", func(t *testing.T) {
		t.Parallel()
		calls := 0
		_, err := RetryWithBackoff(context.Background(), 5, time.Millisecond, "test", func(_ context.Context) (string, error) {
			calls++
			return "", Permanent(errors.New("stop"))
		})
		if err == nil {
			t.Fatal("expected error")
		}
		if calls != 1 {
			t.Errorf("calls = %d, want 1 (PermanentError not retried)", calls)
		}
	})
}

func TestExponentialBackoff(t *testing.T) {
	t.Parallel()

	t.Run("produces increasing delays", func(t *testing.T) {
		t.Parallel()
		b := NewExponentialBackoff(WithInitialInterval(10 * time.Millisecond))
		prev := time.Duration(0)
		for range 5 {
			d := b.NextBackOff()
			if d == BackoffStop {
				t.Fatal("unexpected BackoffStop")
			}
			if d < prev/4 && prev > 0 {
				t.Errorf("delay %v too small relative to prev %v", d, prev)
			}
			prev = d
		}
	})

	t.Run("MaxElapsedTime stops", func(t *testing.T) {
		t.Parallel()
		b := NewExponentialBackoff(
			WithInitialInterval(time.Millisecond),
			WithMaxElapsedTime(5*time.Millisecond),
		)
		time.Sleep(10 * time.Millisecond)
		d := b.NextBackOff()
		if d != BackoffStop {
			t.Errorf("expected BackoffStop after MaxElapsedTime, got %v", d)
		}
	})

	t.Run("Reset restarts timer", func(t *testing.T) {
		t.Parallel()
		b := NewExponentialBackoff(
			WithInitialInterval(time.Millisecond),
			WithMaxElapsedTime(50*time.Millisecond),
		)
		time.Sleep(60 * time.Millisecond)
		if d := b.NextBackOff(); d != BackoffStop {
			t.Fatalf("expected stop, got %v", d)
		}
		b.Reset()
		if d := b.NextBackOff(); d == BackoffStop {
			t.Fatal("after Reset, should not stop immediately")
		}
	})
}

func TestRetryWithBackoff_context_deadline_during_backoff_sleep(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	calls := 0
	_, err := RetryWithBackoff(ctx, 3, 10*time.Second, "test",
		func(_ context.Context) (string, error) {
			calls++
			return "", &HTTPStatusError{Code: 503}
		})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RetryWithBackoff = %v, want context.DeadlineExceeded", err)
	}
	if calls != 1 {
		t.Errorf("fn calls = %d, want 1 (deadline during first backoff sleep)", calls)
	}
}
