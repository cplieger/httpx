package httpx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// bufLogger returns a slog.Logger writing text records (Debug and up) to buf.
func bufLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// doRateLimited adapts the v2 RetryOnRateLimit shape onto Do +
// WithRateLimitOnly: an error-only fn under the rate-limit-only mode. It is
// the same adapter shape consumers of the deleted helper write (see the v3
// design doc's subflux migration).
func doRateLimited(ctx context.Context, maxAttempts int, maxWait time.Duration, fn func(ctx context.Context) error) error {
	_, err := Do(ctx, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, fn(ctx)
	}, WithRateLimitOnly(maxWait), WithMaxAttempts(maxAttempts))
	return err
}

func TestIsTransient(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"auth error", &AuthError{Msg: "denied"}, false},
		{"rate limit error", &RateLimitError{Msg: "slow down"}, false},
		{"deadline exceeded", context.DeadlineExceeded, false},
		{"canceled", context.Canceled, false},
		{"permanent", Permanent(errors.New("nope")), false},
		{"permanent transient", Permanent(&HTTPStatusError{Code: 503}), false},
		{"http 502", &HTTPStatusError{Code: 502}, true},
		{"http 503", &HTTPStatusError{Code: 503}, true},
		{"http 504", &HTTPStatusError{Code: 504}, true},
		{"http 500", &HTTPStatusError{Code: 500}, false},
		{"http 400", &HTTPStatusError{Code: 400}, false},
		{"net timeout", &fakeNetError{timeout: true}, true},
		{"net non-timeout", &fakeNetError{timeout: false}, false},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"conn reset", syscall.ECONNRESET, true},
		{"conn refused", syscall.ECONNREFUSED, true},
		{"dns error", &net.DNSError{Err: "no such host"}, true},
		{"plain error", errors.New("misc"), false},
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

func TestDoRateLimitOnly(t *testing.T) {
	t.Parallel()

	t.Run("success on first call", func(t *testing.T) {
		t.Parallel()
		calls := 0
		err := doRateLimited(context.Background(), 3, 5*time.Second, func(_ context.Context) error {
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
		err := doRateLimited(context.Background(), 3, 5*time.Second, func(_ context.Context) error {
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

	t.Run("transient error returns immediately under rate-limit-only mode", func(t *testing.T) {
		t.Parallel()
		calls := 0
		err := doRateLimited(context.Background(), 3, 5*time.Second, func(_ context.Context) error {
			calls++
			return &HTTPStatusError{Code: 503}
		})
		var statusErr *HTTPStatusError
		if !errors.As(err, &statusErr) {
			t.Fatalf("expected *HTTPStatusError, got %v", err)
		}
		if calls != 1 {
			t.Fatalf("expected 1 call (transients are NOT retried in rate-limit-only mode), got %d", calls)
		}
	})

	t.Run("rate-limit error retries", func(t *testing.T) {
		t.Parallel()
		calls := 0
		err := doRateLimited(context.Background(), 3, 5*time.Second, func(_ context.Context) error {
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
		err := doRateLimited(context.Background(), 2, 5*time.Second, func(_ context.Context) error {
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
		err := doRateLimited(ctx, 5, 5*time.Second, func(_ context.Context) error {
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
		err := doRateLimited(ctx, 1, time.Second, func(fnCtx context.Context) error {
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

func TestDo(t *testing.T) {
	t.Parallel()

	t.Run("success on first call", func(t *testing.T) {
		t.Parallel()
		calls := 0
		result, err := Do(context.Background(), func(_ context.Context) (string, error) {
			calls++
			return "ok", nil
		}, WithMaxAttempts(3), WithBaseDelay(time.Millisecond), WithLabel("test"))
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
		result, err := Do(context.Background(), func(_ context.Context) (int, error) {
			calls++
			if calls < 3 {
				return 0, &HTTPStatusError{Code: 503}
			}
			return 42, nil
		}, WithMaxAttempts(4), WithBaseDelay(time.Millisecond), WithLabel("test"))
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
		_, err := Do(context.Background(), func(_ context.Context) (string, error) {
			calls++
			return "", &HTTPStatusError{Code: 502}
		}, WithMaxAttempts(3), WithBaseDelay(time.Millisecond), WithLabel("test"))
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
		_, err := Do(context.Background(), func(_ context.Context) (string, error) {
			calls++
			return "", errors.New("permanent failure")
		}, WithMaxAttempts(3), WithBaseDelay(time.Millisecond), WithLabel("test"))
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
		_, err := Do(ctx, func(_ context.Context) (string, error) {
			calls++
			if calls == 1 {
				cancel()
			}
			return "", &HTTPStatusError{Code: 503}
		}, WithMaxAttempts(5), WithBaseDelay(time.Millisecond), WithLabel("test"))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	})

	t.Run("rate limit not retried by default", func(t *testing.T) {
		t.Parallel()
		calls := 0
		_, err := Do(context.Background(), func(_ context.Context) (string, error) {
			calls++
			return "", &RateLimitError{Msg: "429", RetryAfter: time.Millisecond}
		}, WithMaxAttempts(3), WithBaseDelay(time.Millisecond))
		var rlErr *RateLimitError
		if !errors.As(err, &rlErr) {
			t.Fatalf("expected *RateLimitError, got %v", err)
		}
		if calls != 1 {
			t.Fatalf("expected 1 call (rate limits non-retryable by default), got %d", calls)
		}
	})
}

// --- Do: WithRateLimitRetry (additive mode) ---

func TestDo_WithRateLimitRetry(t *testing.T) {
	t.Parallel()

	t.Run("retries rate limits alongside transients", func(t *testing.T) {
		t.Parallel()
		calls := 0
		result, err := Do(context.Background(), func(_ context.Context) (string, error) {
			calls++
			switch calls {
			case 1:
				return "", &RateLimitError{Msg: "429", RetryAfter: time.Millisecond}
			case 2:
				return "", &HTTPStatusError{Code: 503} // transient, jittered backoff
			}
			return "ok", nil
		}, WithMaxAttempts(4), WithBaseDelay(time.Millisecond), WithRateLimitRetry(time.Second))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result != "ok" {
			t.Fatalf("result = %q, want ok", result)
		}
		if calls != 3 {
			t.Fatalf("expected 3 calls (RL retry + transient retry + success), got %d", calls)
		}
	})

	t.Run("non-transient still returns immediately", func(t *testing.T) {
		t.Parallel()
		calls := 0
		_, err := Do(context.Background(), func(_ context.Context) (string, error) {
			calls++
			return "", errors.New("permanent")
		}, WithMaxAttempts(3), WithRateLimitRetry(time.Second))
		if err == nil {
			t.Fatal("expected error")
		}
		if calls != 1 {
			t.Fatalf("expected 1 call, got %d", calls)
		}
	})

	t.Run("hint capped at maxWait", func(t *testing.T) {
		t.Parallel()
		// A 10s hint under a 10ms maxWait must wait only ~10ms; completing
		// well inside the 5s deadline proves the cap applied.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		calls := 0
		_, err := Do(ctx, func(_ context.Context) (string, error) {
			calls++
			if calls == 1 {
				return "", &RateLimitError{Msg: "429", RetryAfter: 10 * time.Second}
			}
			return "ok", nil
		}, WithMaxAttempts(2), WithRateLimitRetry(10*time.Millisecond))
		if err != nil {
			t.Fatalf("unexpected error: %v (hint must be capped at maxWait)", err)
		}
		if calls != 2 {
			t.Fatalf("expected 2 calls, got %d", calls)
		}
	})

	t.Run("ctx cancellation wins over final rate-limit error", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := Do(ctx, func(_ context.Context) (string, error) {
			return "", &RateLimitError{Msg: "429"}
		}, WithMaxAttempts(1), WithRateLimitRetry(time.Second))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Do(WithRateLimitRetry, dead ctx) = %v, want context.Canceled (default-mode ctx contract)", err)
		}
	})
}

// TestDo_rate_limit_modes_mutually_exclusive pins the config error: supplying
// both rate-limit modes fails loud before fn runs.
func TestDo_rate_limit_modes_mutually_exclusive(t *testing.T) {
	t.Parallel()
	calls := 0
	_, err := Do(context.Background(), func(_ context.Context) (string, error) {
		calls++
		return "ok", nil
	}, WithRateLimitRetry(time.Second), WithRateLimitOnly(time.Second))
	if err == nil {
		t.Fatal("expected config error for mutually exclusive modes")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %v, want mention of mutual exclusion", err)
	}
	if calls != 0 {
		t.Errorf("fn calls = %d, want 0 (config error precedes execution)", calls)
	}
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

	t.Run("Do does not retry PermanentError", func(t *testing.T) {
		t.Parallel()
		calls := 0
		_, err := Do(context.Background(), func(_ context.Context) (string, error) {
			calls++
			return "", Permanent(errors.New("stop"))
		}, WithMaxAttempts(5), WithBaseDelay(time.Millisecond), WithLabel("test"))
		if err == nil {
			t.Fatal("expected error")
		}
		if calls != 1 {
			t.Errorf("calls = %d, want 1 (PermanentError not retried)", calls)
		}
	})
}

func TestDo_context_deadline_during_backoff_sleep(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	calls := 0
	_, err := Do(ctx, func(_ context.Context) (string, error) {
		calls++
		return "", &HTTPStatusError{Code: 503}
	}, WithMaxAttempts(3), WithBaseDelay(10*time.Second), WithLabel("test"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Do = %v, want context.DeadlineExceeded", err)
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

// --- Do rate-limit-only mode: attempt clamp and Retry-After vs maxWait boundary ---

func TestDoRateLimitOnly_zero_attempts_clamps_to_one(t *testing.T) {
	t.Parallel()
	calls := 0
	sentinel := &RateLimitError{Msg: "rl"}
	err := doRateLimited(context.Background(), 0, time.Second, func(_ context.Context) error {
		calls++
		return sentinel
	})
	// maxAttempts<1 clamps to 1: fn runs exactly once and its error is returned.
	var rlErr *RateLimitError
	if !errors.As(err, &rlErr) {
		t.Errorf("Do(rate-limit-only, maxAttempts=0) = %v, want the single attempt's *RateLimitError", err)
	}
	if calls != 1 {
		t.Errorf("fn calls = %d, want 1 (maxAttempts<1 clamps to 1)", calls)
	}
}

func TestDoRateLimitOnly_no_sleep_after_final_attempt(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// With one attempt the loop must break before sleeping; a mutated break guard
	// would fall through to SleepCtx and return the cancelled-context error.
	// (Under WithRateLimitOnly the final attempt's error wins even on a dead
	// context — the v2 RetryOnRateLimit contract.)
	err := doRateLimited(ctx, 1, time.Minute, func(_ context.Context) error {
		return &RateLimitError{Msg: "slow"}
	})
	var rlErr *RateLimitError
	if !errors.As(err, &rlErr) {
		t.Errorf("Do(rate-limit-only, maxAttempts=1) = %v, want *RateLimitError (no trailing sleep)", err)
	}
}

func TestDoRateLimitOnly_zero_retry_after_uses_max_wait(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// RetryAfter==0 must NOT override the (large) max wait, so SleepCtx waits an
	// hour against the cancelled context and returns its error.
	err := doRateLimited(ctx, 2, time.Hour, func(_ context.Context) error {
		return &RateLimitError{Msg: "slow", RetryAfter: 0}
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Do(rate-limit-only, RetryAfter=0) = %v, want context.Canceled (max wait honored)", err)
	}
}

func TestDoRateLimitOnly_positive_retry_after_caps_below_max_wait(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	// A positive RetryAfter must shrink the wait to 10ms so the retry completes
	// well within the 150ms deadline and returns the rate-limit error.
	err := doRateLimited(ctx, 2, 10*time.Second, func(_ context.Context) error {
		return &RateLimitError{Msg: "slow", RetryAfter: 10 * time.Millisecond}
	})
	var rlErr *RateLimitError
	if !errors.As(err, &rlErr) {
		t.Errorf("Do(rate-limit-only, RetryAfter=10ms) = %v, want *RateLimitError (RetryAfter honored)", err)
	}
}

// TestDoRateLimitOnly_non_positive_max_wait_clamps_to_cap pins the maxWait<=0
// fallback to RetryAfterCap: the inter-attempt wait must stay positive (here
// the clamped 60s against an expiring context), so the loop parks in SleepCtx
// and surfaces the context error. Before the clamp existed, maxWait=0 zeroed
// the wait, SleepCtx returned nil without checking ctx, and the loop hot-spun
// through every attempt in microseconds returning the rate-limit error instead.
func TestDoRateLimitOnly_non_positive_max_wait_clamps_to_cap(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	calls := 0
	err := doRateLimited(ctx, 3, 0, func(_ context.Context) error {
		calls++
		return &RateLimitError{Msg: "rl"}
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Do(rate-limit-only, maxWait=0) = %v, want context.DeadlineExceeded (clamped wait parked in SleepCtx)", err)
	}
	if calls != 1 {
		t.Errorf("fn calls = %d, want 1 (no hot-spin through attempts)", calls)
	}
}

// --- Do: zero base delay coercion and structured-log content ---

func TestDo_zero_base_delay_defaults_to_base(t *testing.T) {
	t.Parallel()
	// A zero base delay must be coerced to DefaultBaseDelay (1s), so the pre-retry
	// sleep blows the 100ms deadline and surfaces the context error.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := Do(ctx, func(_ context.Context) (string, error) {
		return "", &HTTPStatusError{Code: 503}
	}, WithMaxAttempts(2), WithBaseDelay(0), WithLabel("test"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Do(baseDelay=0) = %v, want context.DeadlineExceeded (delay defaulted)", err)
	}
}

// These swap slog.Default, so they must NOT run in parallel.

func TestDo_first_try_success_omits_succeeded_log(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(bufLogger(&buf))
	defer slog.SetDefault(prev)

	_, err := Do(context.Background(),
		func(_ context.Context) (string, error) { return "ok", nil },
		WithMaxAttempts(3), WithBaseDelay(time.Microsecond), WithLabel("lbl"))
	if err != nil {
		t.Fatalf("Do = %v, want nil", err)
	}
	if strings.Contains(buf.String(), "succeeded after retry") {
		t.Errorf("first-try success logged a retry-success line:\n%s", buf.String())
	}
}

func TestDo_success_after_one_retry_logs_attempt_counts(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(bufLogger(&buf))
	defer slog.SetDefault(prev)

	calls := 0
	_, err := Do(context.Background(),
		func(_ context.Context) (int, error) {
			calls++
			if calls == 1 {
				return 0, &HTTPStatusError{Code: 503}
			}
			return 42, nil
		},
		WithMaxAttempts(3), WithBaseDelay(time.Microsecond), WithLabel("lbl"))
	if err != nil {
		t.Fatalf("Do = %v, want nil", err)
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

func TestDo_no_retry_log_after_final_attempt(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(bufLogger(&buf))
	defer slog.SetDefault(prev)

	calls := 0
	_, err := Do(context.Background(),
		func(_ context.Context) (string, error) {
			calls++
			return "", &HTTPStatusError{Code: 503}
		},
		WithMaxAttempts(2), WithBaseDelay(time.Microsecond), WithLabel("lbl"))
	if err == nil {
		t.Fatal("Do = nil, want error after exhaustion")
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

// TestDo_WithLogger_routes_all_lines pins the NEW v3 capability: a per-call
// logger receives the loop's Debug and Warn lines, and slog.Default() stays
// silent. Swaps slog.Default to prove the negative, so not parallel.
func TestDo_WithLogger_routes_all_lines(t *testing.T) {
	var defBuf, callBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(bufLogger(&defBuf))
	defer slog.SetDefault(prev)

	_, err := Do(context.Background(),
		func(_ context.Context) (string, error) {
			return "", &HTTPStatusError{Code: 503}
		},
		WithMaxAttempts(2), WithBaseDelay(time.Microsecond), WithLabel("lbl"), WithLogger(bufLogger(&callBuf)))
	if err == nil {
		t.Fatal("Do = nil, want exhaustion error")
	}
	if !strings.Contains(callBuf.String(), "failed, retrying") || !strings.Contains(callBuf.String(), "retries exhausted") {
		t.Errorf("per-call logger missing loop lines:\n%s", callBuf.String())
	}
	if defBuf.Len() != 0 {
		t.Errorf("slog.Default() received lines despite WithLogger:\n%s", defBuf.String())
	}
}

// TestDoRateLimitOnly_retry_debug_log_reports_one_indexed_attempt pins the
// per-attempt Debug line's attempt field to the human (1-indexed) value: the
// first retry logs attempt=1, not the 0-indexed loop counter. Swaps
// slog.Default, so it must not run in parallel.
func TestDoRateLimitOnly_retry_debug_log_reports_one_indexed_attempt(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(bufLogger(&buf))
	defer slog.SetDefault(prev)

	calls := 0
	err := doRateLimited(context.Background(), 3, time.Millisecond, func(_ context.Context) error {
		calls++
		if calls == 1 {
			return &RateLimitError{Msg: "429"}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do(rate-limit-only) = %v, want nil", err)
	}
	logged := buf.String()
	if !strings.Contains(logged, "failed, retrying") {
		t.Fatalf("expected per-attempt debug log, got:\n%s", logged)
	}
	// The first retry logs the human (1-indexed) attempt number, attempt+1 == 1.
	if !strings.Contains(logged, "attempt=1") {
		t.Errorf("retry debug log = %q, want attribute attempt=1", logged)
	}
}

// TestDoRateLimitOnly_zero_max_wait_still_honors_hint pins that the maxWait
// clamp keeps hint capping intact: with maxWait=0 (clamped to RetryAfterCap) a
// 5ms RetryAfter hint is honored as min(hint, cap) = 5ms. Without the clamp
// the same call computed min(5ms, 0) = 0 and logged delay=0s. Swaps
// slog.Default, so it must not run in parallel.
func TestDoRateLimitOnly_zero_max_wait_still_honors_hint(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(bufLogger(&buf))
	defer slog.SetDefault(prev)

	calls := 0
	err := doRateLimited(context.Background(), 2, 0, func(_ context.Context) error {
		calls++
		if calls == 1 {
			return &RateLimitError{Msg: "rl", RetryAfter: 5 * time.Millisecond}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do(rate-limit-only) = %v, want nil", err)
	}
	if calls != 2 {
		t.Fatalf("fn calls = %d, want 2", calls)
	}
	if !strings.Contains(buf.String(), "delay=5ms") {
		t.Errorf("retry debug log = %q, want delay=5ms (hint honored under the clamped cap)", buf.String())
	}
}

// TestDoRateLimitOnly_exhaustion_warn_message pins the terminal Warn text the
// v2 helper emitted ("rate limit retries exhausted"), preserved by the mode.
func TestDoRateLimitOnly_exhaustion_warn_message(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(bufLogger(&buf))
	defer slog.SetDefault(prev)

	err := doRateLimited(context.Background(), 2, time.Millisecond, func(_ context.Context) error {
		return &RateLimitError{Msg: "rl", RetryAfter: time.Millisecond}
	})
	if err == nil {
		t.Fatal("expected exhaustion error")
	}
	if !strings.Contains(buf.String(), "rate limit retries exhausted") {
		t.Errorf("terminal warn = %q, want 'rate limit retries exhausted'", buf.String())
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
