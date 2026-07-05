package httpx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// bufLogger returns a debug-level text logger writing into buf.
func bufLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

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
		{name: "PermanentError is not transient", err: Permanent(errors.New("x")), want: false},
		{name: "wrapped PermanentError is not transient", err: fmt.Errorf("wrap: %w", Permanent(errors.New("x"))), want: false},
		{name: "wrapped AuthError is not transient", err: fmt.Errorf("wrap: %w", &AuthError{Msg: "x"}), want: false},
		{name: "wrapped RateLimitError is not transient", err: fmt.Errorf("wrap: %w", &RateLimitError{Msg: "x"}), want: false},
		{name: "wrapped context.Canceled is not transient", err: fmt.Errorf("wrap: %w", context.Canceled), want: false},
		{name: "wrapped HTTPStatusError 502 is transient", err: fmt.Errorf("wrap: %w", &HTTPStatusError{Code: 502}), want: true},
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

// --- HTTPStatusError classification ---

func TestHTTPStatusError_classification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		code      int
		transient bool
		serverErr bool
		clientErr bool
	}{
		{400, false, false, true},
		{404, false, false, true},
		{499, false, false, true},
		{500, false, true, false},
		{501, false, true, false},
		{502, true, true, false},
		{503, true, true, false},
		{504, true, true, false},
	}
	for _, tt := range tests {
		e := &HTTPStatusError{Code: tt.code}
		if e.IsTransient() != tt.transient {
			t.Errorf("HTTPStatusError{%d}.IsTransient() = %v, want %v", tt.code, e.IsTransient(), tt.transient)
		}
		if e.IsServerError() != tt.serverErr {
			t.Errorf("HTTPStatusError{%d}.IsServerError() = %v, want %v", tt.code, e.IsServerError(), tt.serverErr)
		}
		if e.IsClientError() != tt.clientErr {
			t.Errorf("HTTPStatusError{%d}.IsClientError() = %v, want %v", tt.code, e.IsClientError(), tt.clientErr)
		}
	}
}

// --- Backoff primitives: concrete edge cases (property coverage in prop_test.go) ---

func TestSafeDouble_concrete(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"zero", 0, 0},
		{"negative passthrough", -3 * time.Second, -3 * time.Second},
		{"doubles", 2 * time.Second, 4 * time.Second},
		{"no overflow near top", 1 << 61, 1 << 62},
		{"overflow caps at max", time.Duration(1<<63 - 1), time.Duration(1<<63 - 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := SafeDouble(tt.in); got != tt.want {
				t.Errorf("SafeDouble(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestJitteredBackoff_zero_and_negative_passthrough(t *testing.T) {
	t.Parallel()
	if d := JitteredBackoff(0); d != 0 {
		t.Errorf("JitteredBackoff(0) = %v, want 0", d)
	}
	if d := JitteredBackoff(-time.Second); d != -time.Second {
		t.Errorf("JitteredBackoff(-1s) = %v, want -1s", d)
	}
}

// TestSleepCtx_nonpositive_returns_nil_without_consulting_context pins that a
// non-positive duration short-circuits to nil immediately, never consulting the
// context: the assertion holds even with an already-cancelled context.
func TestSleepCtx_nonpositive_returns_nil_without_consulting_context(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 200 {
		if got := SleepCtx(ctx, 0); got != nil {
			t.Fatalf("SleepCtx(cancelled ctx, 0) = %v, want nil", got)
		}
	}
	if got := SleepCtx(ctx, -time.Second); got != nil {
		t.Fatalf("SleepCtx(cancelled ctx, -1s) = %v, want nil", got)
	}
}

func TestSleepCtx_negative_returns_immediately(t *testing.T) {
	t.Parallel()
	start := time.Now()
	if err := SleepCtx(context.Background(), -time.Second); err != nil {
		t.Errorf("SleepCtx(-1s) = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("SleepCtx(-1s) took %v, want immediate return", elapsed)
	}
}

func TestSleepCtx_cancelled_context_no_goroutine_leak(t *testing.T) {
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 100 {
		_ = SleepCtx(ctx, time.Hour)
	}
	runtime.GC()
	time.Sleep(10 * time.Millisecond)
	if after := runtime.NumGoroutine(); after-before > 5 {
		t.Errorf("goroutine leak: before=%d, after=%d", before, after)
	}
}

// --- ExponentialBackoff: zero initial interval coerces to DefaultBaseDelay ---

func TestExponentialBackoff_zero_initial_interval_defaults_to_base(t *testing.T) {
	t.Parallel()
	b := NewExponentialBackoff(WithInitialInterval(0))
	got := b.NextBackOff()
	// Reset must coerce the zero interval up to DefaultBaseDelay, so the first
	// jittered delay lies in [DefaultBaseDelay/2, DefaultBaseDelay].
	if got < DefaultBaseDelay/2 || got > DefaultBaseDelay {
		t.Errorf("NextBackOff() with zero initial interval = %v, want within [%v, %v]",
			got, DefaultBaseDelay/2, DefaultBaseDelay)
	}
}

// --- RetryOnRateLimit: attempt clamp and Retry-After vs maxWait boundary ---

func TestRetryOnRateLimit_zero_attempts_clamps_to_one(t *testing.T) {
	t.Parallel()
	calls := 0
	sentinel := &RateLimitError{Msg: "rl"}
	err := RetryOnRateLimit(context.Background(), 0, time.Second, func(_ context.Context) error {
		calls++
		return sentinel
	})
	// maxAttempts<1 clamps to 1: fn runs exactly once and its error is returned.
	var rlErr *RateLimitError
	if !errors.As(err, &rlErr) {
		t.Errorf("RetryOnRateLimit(maxAttempts=0) = %v, want the single attempt's *RateLimitError", err)
	}
	if calls != 1 {
		t.Errorf("fn calls = %d, want 1 (maxAttempts<1 clamps to 1)", calls)
	}
}

func TestRetryOnRateLimit_no_sleep_after_final_attempt(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// With one attempt the loop must break before sleeping; a mutated break guard
	// would fall through to SleepCtx and return the cancelled-context error.
	err := RetryOnRateLimit(ctx, 1, time.Minute, func(_ context.Context) error {
		return &RateLimitError{Msg: "slow"}
	})
	var rlErr *RateLimitError
	if !errors.As(err, &rlErr) {
		t.Errorf("RetryOnRateLimit(maxAttempts=1) = %v, want *RateLimitError (no trailing sleep)", err)
	}
}

func TestRetryOnRateLimit_zero_retry_after_uses_max_wait(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// RetryAfter==0 must NOT override the (large) max wait, so SleepCtx waits an
	// hour against the cancelled context and returns its error.
	err := RetryOnRateLimit(ctx, 2, time.Hour, func(_ context.Context) error {
		return &RateLimitError{Msg: "slow", RetryAfter: 0}
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("RetryOnRateLimit(RetryAfter=0) = %v, want context.Canceled (max wait honored)", err)
	}
}

func TestRetryOnRateLimit_positive_retry_after_caps_below_max_wait(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	// A positive RetryAfter must shrink the wait to 10ms so the retry completes
	// well within the 150ms deadline and returns the rate-limit error.
	err := RetryOnRateLimit(ctx, 2, 10*time.Second, func(_ context.Context) error {
		return &RateLimitError{Msg: "slow", RetryAfter: 10 * time.Millisecond}
	})
	var rlErr *RateLimitError
	if !errors.As(err, &rlErr) {
		t.Errorf("RetryOnRateLimit(RetryAfter=10ms) = %v, want *RateLimitError (RetryAfter honored)", err)
	}
}

// --- RetryWithBackoff: zero base delay coercion and structured-log content ---

func TestRetryWithBackoff_zero_base_delay_defaults_to_base(t *testing.T) {
	t.Parallel()
	// A zero base delay must be coerced to DefaultBaseDelay (1s), so the pre-retry
	// sleep blows the 100ms deadline and surfaces the context error.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := RetryWithBackoff(ctx, 2, 0, "test", func(_ context.Context) (string, error) {
		return "", &HTTPStatusError{Code: 503}
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("RetryWithBackoff(baseDelay=0) = %v, want context.DeadlineExceeded (delay defaulted)", err)
	}
}

// These three swap slog.Default, so they must NOT run in parallel.

func TestRetryWithBackoff_first_try_success_omits_succeeded_log(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(bufLogger(&buf))
	defer slog.SetDefault(prev)

	_, err := RetryWithBackoff(context.Background(), 3, time.Microsecond, "lbl",
		func(_ context.Context) (string, error) { return "ok", nil })
	if err != nil {
		t.Fatalf("RetryWithBackoff = %v, want nil", err)
	}
	if strings.Contains(buf.String(), "succeeded after retry") {
		t.Errorf("first-try success logged a retry-success line:\n%s", buf.String())
	}
}

func TestRetryWithBackoff_success_after_one_retry_logs_attempt_counts(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(bufLogger(&buf))
	defer slog.SetDefault(prev)

	calls := 0
	_, err := RetryWithBackoff(context.Background(), 3, time.Microsecond, "lbl",
		func(_ context.Context) (int, error) {
			calls++
			if calls == 1 {
				return 0, &HTTPStatusError{Code: 503}
			}
			return 42, nil
		})
	if err != nil {
		t.Fatalf("RetryWithBackoff = %v, want nil", err)
	}
	logged := buf.String()
	if !strings.Contains(logged, "attempt=1") {
		t.Errorf("retry debug log = %q, want attribute attempt=1", logged)
	}
	if !strings.Contains(logged, "succeeded after retry") {
		t.Errorf("success-after-retry not logged:\n%s", logged)
	}
	if !strings.Contains(logged, "attempts=2") {
		t.Errorf("success log = %q, want attribute attempts=2", logged)
	}
}

func TestRetryWithBackoff_no_retry_log_after_final_attempt(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(bufLogger(&buf))
	defer slog.SetDefault(prev)

	calls := 0
	_, err := RetryWithBackoff(context.Background(), 2, time.Microsecond, "lbl",
		func(_ context.Context) (string, error) {
			calls++
			return "", &HTTPStatusError{Code: 503}
		})
	if err == nil {
		t.Fatal("RetryWithBackoff = nil, want error after exhaustion")
	}
	if calls != 2 {
		t.Fatalf("fn calls = %d, want 2", calls)
	}
	// Exactly one "failed, retrying" line: the break before the final attempt
	// suppresses the second.
	if got := strings.Count(buf.String(), "failed, retrying"); got != 1 {
		t.Errorf("retry-log count = %d, want 1\n%s", got, buf.String())
	}
}

// TestRetryOnRateLimit_retry_debug_log_reports_one_indexed_attempt pins the
// per-attempt "rate limited, backing off" Debug line's attempt field to the
// human (1-indexed) value: the first retry logs attempt=1, not the 0-indexed
// loop counter. Mirrors the RetryWithBackoff debug-attempt assertion. Swaps
// slog.Default, so it must not run in parallel.
func TestRetryOnRateLimit_retry_debug_log_reports_one_indexed_attempt(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(bufLogger(&buf))
	defer slog.SetDefault(prev)

	calls := 0
	err := RetryOnRateLimit(context.Background(), 3, time.Millisecond, func(_ context.Context) error {
		calls++
		if calls == 1 {
			return &RateLimitError{Msg: "429"}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RetryOnRateLimit = %v, want nil", err)
	}
	logged := buf.String()
	if !strings.Contains(logged, "rate limited, backing off") {
		t.Fatalf("expected per-attempt debug log, got:\n%s", logged)
	}
	// The first retry logs the human (1-indexed) attempt number, attempt+1 == 1.
	if !strings.Contains(logged, "attempt=1") {
		t.Errorf("retry debug log = %q, want attribute attempt=1", logged)
	}
}

func TestLogSlowUpstream_warns_and_redacts_on_slow_response(t *testing.T) {
	t.Parallel()
	// The >10s branch (Warn + redactURL) is otherwise unexercised; only the
	// fast-path negative is tested. An attemptStart 11s in the past forces it.
	var buf bytes.Buffer
	logSlowUpstream(bufLogger(&buf), "https://h.example/api?apikey=supersecret", time.Now().Add(-11*time.Second))
	logged := buf.String()
	if !strings.Contains(logged, "slow upstream response") {
		t.Errorf("logSlowUpstream(11s ago) logged %q, want the slow-upstream Warn", logged)
	}
	if strings.Contains(logged, "supersecret") {
		t.Errorf("slow-upstream log leaked the query secret:\n%s", logged)
	}
	if !strings.Contains(logged, "apikey=REDACTED") {
		t.Errorf("slow-upstream log did not redact the query value:\n%s", logged)
	}
}
