package httpx_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/httpx/v2"
)

// This file targets specific surviving mutants found by the weekly gremlins
// run. Each test pins the exact boundary value, arithmetic result, negated
// branch, or log emission that distinguishes the original code from its
// mutant. Tests that rely on timing convert a "delay vs no-delay" difference
// into a deterministic error-vs-success outcome by pairing a large backoff
// with a short context deadline.

// bufLogger returns a debug-level text logger writing into buf.
func bufLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// --- ExponentialBackoff.Reset: L209 `initialInterval <= 0` boundary ---

func TestExponentialBackoff_zero_initial_interval_defaults_to_base(t *testing.T) {
	t.Parallel()

	// given: an initial interval of exactly zero
	b := httpx.NewExponentialBackoff(httpx.WithInitialInterval(0))

	// when
	got := b.NextBackOff()

	// then: Reset must coerce the zero interval up to DefaultBaseDelay, so the
	// first jittered delay lies in [DefaultBaseDelay/2, DefaultBaseDelay].
	// A `<= 0` -> `< 0` mutant leaves the interval at zero, yielding a 0 delay.
	if got < httpx.DefaultBaseDelay/2 || got > httpx.DefaultBaseDelay {
		t.Errorf("NextBackOff() with zero initial interval = %v, want within [%v, %v]",
			got, httpx.DefaultBaseDelay/2, httpx.DefaultBaseDelay)
	}
}

// --- ParseRetryAfterResponse: L280 ARITHMETIC in the overflow-cap branch ---

func TestParseRetryAfterResponse_caps_overflow_with_multiplication(t *testing.T) {
	t.Parallel()

	// given: a delta-seconds value above the max representable seconds
	h := http.Header{}
	h.Set("Retry-After", "9999999999")
	resp := &http.Response{Header: h}

	// when
	got := httpx.ParseRetryAfterResponse(resp)

	// then: the cap is time.Duration(maxSecs) * time.Second. A `*` -> `/`
	// mutant would return ~9ns instead of ~292 years.
	const maxSecs = 9223372036 // math.MaxInt64 / int(time.Second) on 64-bit
	want := time.Duration(maxSecs) * time.Second
	if got != want {
		t.Errorf("ParseRetryAfterResponse(%q) = %v, want %v", "9999999999", got, want)
	}
}

// --- RetryOnRateLimit: L456 `attempt == maxAttempts-1` break guard ---

func TestRetryOnRateLimit_does_not_sleep_after_final_attempt(t *testing.T) {
	t.Parallel()

	// given: a single attempt, a long max wait, and an already-cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rl := &httpx.RateLimitError{Msg: "slow", RetryAfter: 0}

	// when
	err := httpx.RetryOnRateLimit(ctx, 1, time.Minute, func(_ context.Context) error {
		return rl
	})

	// then: with one attempt the loop must break before sleeping and return the
	// rate-limit error. A mutated break guard (`maxAttempts-1` -> `maxAttempts`
	// / `maxAttempts+1`) would fall through to SleepCtx, which immediately
	// returns the cancelled-context error instead.
	var got *httpx.RateLimitError
	if !errors.As(err, &got) {
		t.Errorf("RetryOnRateLimit(maxAttempts=1) = %v, want *RateLimitError (no trailing sleep)", err)
	}
}

// --- RetryOnRateLimit: L460 `rlErr.RetryAfter > 0` (boundary + negation) ---

func TestRetryOnRateLimit_zero_retry_after_uses_max_wait(t *testing.T) {
	t.Parallel()

	// given: a cancelled context, RetryAfter==0, and a long max wait
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rl := &httpx.RateLimitError{Msg: "slow", RetryAfter: 0}

	// when
	err := httpx.RetryOnRateLimit(ctx, 2, time.Hour, func(_ context.Context) error {
		return rl
	})

	// then: RetryAfter==0 must NOT override the (large) max wait, so SleepCtx is
	// asked to wait an hour and returns the cancelled-context error. A `> 0` ->
	// `>= 0` boundary mutant sets wait=min(0,maxWait)=0; SleepCtx then returns
	// nil and the next attempt yields the rate-limit error instead.
	if !errors.Is(err, context.Canceled) {
		t.Errorf("RetryOnRateLimit(RetryAfter=0) = %v, want context.Canceled (max wait honored)", err)
	}
}

func TestRetryOnRateLimit_positive_retry_after_caps_below_max_wait(t *testing.T) {
	t.Parallel()

	// given: a short context deadline, a small RetryAfter, and a huge max wait
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	rl := &httpx.RateLimitError{Msg: "slow", RetryAfter: 10 * time.Millisecond}

	// when
	err := httpx.RetryOnRateLimit(ctx, 2, 10*time.Second, func(_ context.Context) error {
		return rl
	})

	// then: a positive RetryAfter must shrink the wait to 10ms so the retry
	// completes well within the 150ms deadline and returns the rate-limit error.
	// A `> 0` -> `<= 0` negation mutant would fall back to the 10s max wait and
	// blow the deadline, returning the context error instead.
	var got *httpx.RateLimitError
	if !errors.As(err, &got) {
		t.Errorf("RetryOnRateLimit(RetryAfter=10ms) = %v, want *RateLimitError (RetryAfter honored)", err)
	}
}

// --- Retry: L528 `cfg.baseDelay <= 0` boundary ---

func TestRetry_zero_base_delay_defaults_to_base(t *testing.T) {
	t.Parallel()

	// given: a server that always fails, a zero base delay, and a context
	// deadline far shorter than DefaultBaseDelay
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// when
	_, err := httpx.Retry(ctx, srv.Client(), srv.URL,
		httpx.WithMaxAttempts(2), httpx.WithBaseDelay(0))

	// then: a zero base delay must be coerced to DefaultBaseDelay (1s), so the
	// pre-retry sleep blows the 100ms deadline and returns the context error.
	// A `<= 0` -> `< 0` mutant leaves the delay at zero; the retry then runs
	// instantly and the call exhausts retries with a 500 instead.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Retry(baseDelay=0) = %v, want context.DeadlineExceeded (delay defaulted)", err)
	}
}

// --- Retry: L531 `cfg.maxBodyBytes <= 0` (boundary + negation) ---

func TestRetry_zero_max_body_bytes_defaults_to_full_body(t *testing.T) {
	t.Parallel()

	// given: a server returning an 11-byte body and a zero max-body-bytes
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello world"))
	}))
	defer srv.Close()

	// when
	body, err := httpx.Retry(t.Context(), srv.Client(), srv.URL,
		httpx.WithBaseDelay(time.Millisecond), httpx.WithMaxBodyBytes(0))
	if err != nil {
		t.Fatalf("Retry = %v, want nil", err)
	}

	// then: zero must be coerced to DefaultMaxBodyBytes, returning the whole
	// body. A `<= 0` -> `< 0` mutant keeps the limit at 0 and returns "".
	if string(body) != "hello world" {
		t.Errorf("Retry(maxBodyBytes=0) body = %q, want %q", body, "hello world")
	}
}

func TestRetry_positive_max_body_bytes_errors_when_exceeded(t *testing.T) {
	t.Parallel()

	// given: a server returning an 11-byte body and a 5-byte cap
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello world"))
	}))
	defer srv.Close()

	// when
	body, err := httpx.Retry(t.Context(), srv.Client(), srv.URL,
		httpx.WithBaseDelay(time.Millisecond), httpx.WithMaxBodyBytes(5))

	// then: a positive cap must be preserved; an 11-byte body over the 5-byte
	// cap fails loud with *ResponseTooLargeError{Limit:5} (v2 no longer
	// truncates). A `<= 0` -> `> 0` negation mutant would replace 5 with the
	// 10MB default, leaving the 11-byte body under the cap and returning it.
	if body != nil {
		t.Errorf("Retry(maxBodyBytes=5) body = %q, want nil", body)
	}
	var tooLarge *httpx.ResponseTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("Retry(maxBodyBytes=5) error = %v, want *ResponseTooLargeError", err)
	}
	if tooLarge.Limit != 5 {
		t.Errorf("ResponseTooLargeError.Limit = %d, want 5", tooLarge.Limit)
	}
}

// --- Retry: L544 `if attempt > 0` sleep gate boundary ---

func TestRetry_does_not_sleep_before_first_attempt(t *testing.T) {
	t.Parallel()

	// given: an immediately-successful server, a huge base delay, and a short
	// context deadline
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// when
	body, err := httpx.Retry(ctx, srv.Client(), srv.URL, httpx.WithBaseDelay(10*time.Second))
	// then: the first attempt must NOT sleep, so the fast response returns
	// before the deadline. A `> 0` -> `>= 0` mutant sleeps ~10s before the
	// first request and blows the 100ms deadline.
	if err != nil {
		t.Fatalf("Retry(first attempt) = %v, want nil (no pre-first-attempt sleep)", err)
	}
	if string(body) != "ok" {
		t.Errorf("body = %q, want %q", body, "ok")
	}
}

// --- Retry: L556 slow-upstream log (negation + arithmetic) ---

func TestRetry_fast_response_does_not_log_slow_upstream(t *testing.T) {
	t.Parallel()

	// given: a fast successful server and a captured logger
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	var buf bytes.Buffer

	// when
	body, err := httpx.Retry(t.Context(), srv.Client(), srv.URL,
		httpx.WithBaseDelay(time.Millisecond), httpx.WithLogger(bufLogger(&buf)))
	if err != nil {
		t.Fatalf("Retry = %v, want nil", err)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q, want %q", body, "ok")
	}

	// then: a sub-second response must NOT trigger the >10s "slow upstream"
	// warning. A `>` -> `<=` negation mutant logs it for every fast response;
	// a `10 * time.Second` -> `10 / time.Second` (== 0) mutant logs it whenever
	// elapsed > 0.
	if strings.Contains(buf.String(), "slow upstream response") {
		t.Errorf("fast response logged slow-upstream warning:\n%s", buf.String())
	}
}

// --- Retry: L567 ARITHMETIC in the per-attempt "will retry" debug log ---

func TestRetry_retry_debug_log_reports_one_indexed_attempt(t *testing.T) {
	t.Parallel()

	// given: a server that fails once (500) then succeeds, with a captured logger
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

	// when
	if _, err := httpx.Retry(t.Context(), srv.Client(), srv.URL,
		httpx.WithBaseDelay(time.Millisecond), httpx.WithLogger(bufLogger(&buf))); err != nil {
		t.Fatalf("Retry = %v, want nil", err)
	}

	// then: the first failure logs the human (1-indexed) attempt number, i.e.
	// attempt+1 == 1. A `+` -> `-`/`*`/`/` mutant logs attempt=-1 or attempt=0.
	logged := buf.String()
	if !strings.Contains(logged, "http request failed, will retry") {
		t.Fatalf("expected retry debug log, got:\n%s", logged)
	}
	if !strings.Contains(logged, "attempt=1") {
		t.Errorf("retry debug log = %q, want attribute attempt=1", logged)
	}
}

// --- RetryWithBackoff: L428 `baseDelay <= 0` coercion boundary ---

func TestRetryWithBackoff_zero_base_delay_defaults_to_base(t *testing.T) {
	t.Parallel()

	// given: a zero base delay and a context deadline far shorter than
	// DefaultBaseDelay (1s)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// when: fn always fails transiently
	_, err := httpx.RetryWithBackoff(ctx, 2, 0, "test", func(_ context.Context) (string, error) {
		return "", &httpx.HTTPStatusError{Code: 503}
	})

	// then: a zero base delay must be coerced to DefaultBaseDelay (1s), so the
	// pre-retry sleep blows the 100ms deadline and surfaces the context error.
	// A `<= 0` -> `< 0` boundary mutant (or a deleted coercion) leaves the delay
	// at zero; the retry then runs instantly and the call exhausts attempts with
	// the 503 instead.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("RetryWithBackoff(baseDelay=0) = %v, want context.DeadlineExceeded (delay defaulted)", err)
	}
}

// --- RetryWithBackoff: L415/L416/L432/L427 (logging via slog.Default) ---
//
// RetryWithBackoff logs through slog.Default(), so these tests swap the default
// logger and therefore must NOT run in parallel.

func TestRetryWithBackoff_first_try_success_omits_succeeded_log(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(bufLogger(&buf))
	defer slog.SetDefault(prev)

	// given/when: fn succeeds on the very first attempt
	_, err := httpx.RetryWithBackoff(context.Background(), 3, time.Microsecond, "lbl",
		func(_ context.Context) (string, error) { return "ok", nil })
	if err != nil {
		t.Fatalf("RetryWithBackoff = %v, want nil", err)
	}

	// then: no "succeeded after retry" line, since there was no retry. Both the
	// `> 0` -> `>= 0` boundary and the `> 0` -> `<= 0` negation mutant emit it
	// on attempt 0.
	if strings.Contains(buf.String(), "succeeded after retry") {
		t.Errorf("first-try success logged a retry-success line:\n%s", buf.String())
	}
}

func TestRetryWithBackoff_success_after_one_retry_logs_attempt_counts(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(bufLogger(&buf))
	defer slog.SetDefault(prev)

	// given: fn fails once (transient) then succeeds
	calls := 0
	_, err := httpx.RetryWithBackoff(context.Background(), 3, time.Microsecond, "lbl",
		func(_ context.Context) (int, error) {
			calls++
			if calls == 1 {
				return 0, &httpx.HTTPStatusError{Code: 503}
			}
			return 42, nil
		})
	if err != nil {
		t.Fatalf("RetryWithBackoff = %v, want nil", err)
	}
	logged := buf.String()

	// then: the first-failure debug line reports the 1-indexed attempt (attempt+1==1).
	// A `+` -> `-`/`*`/`/` mutant logs attempt=-1 or attempt=0 (L432).
	if !strings.Contains(logged, "attempt=1") {
		t.Errorf("retry debug log = %q, want attribute attempt=1", logged)
	}
	// the post-retry success must be logged (L415 negation), ...
	if !strings.Contains(logged, "succeeded after retry") {
		t.Errorf("success-after-retry not logged:\n%s", logged)
	}
	// ... reporting attempts+1 == 2 (L416).
	if !strings.Contains(logged, "attempts=2") {
		t.Errorf("success log = %q, want attribute attempts=2", logged)
	}
}

func TestRetryWithBackoff_no_retry_log_after_final_attempt(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(bufLogger(&buf))
	defer slog.SetDefault(prev)

	// given: fn always fails transiently, with exactly 2 attempts allowed
	calls := 0
	_, err := httpx.RetryWithBackoff(context.Background(), 2, time.Microsecond, "lbl",
		func(_ context.Context) (string, error) {
			calls++
			return "", &httpx.HTTPStatusError{Code: 503}
		})
	if err == nil {
		t.Fatal("RetryWithBackoff = nil, want error after exhaustion")
	}
	if calls != 2 {
		t.Fatalf("fn calls = %d, want 2", calls)
	}

	// then: exactly one "failed, retrying" debug line fires — the break before
	// the final attempt suppresses the second. A mutated break guard
	// (`maxAttempts-1` -> `maxAttempts`/`maxAttempts+1`) logs (and sleeps) twice.
	if got := strings.Count(buf.String(), "failed, retrying"); got != 1 {
		t.Errorf("retry-log count = %d, want 1\n%s", got, buf.String())
	}
}

// --- Drain: L634 `err != nil && !errors.Is(err, io.EOF)` negation ---

func TestDrain_clean_drain_does_not_log_failure(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(bufLogger(&buf))
	defer slog.SetDefault(prev)

	// given/when: a body larger than the 64KB drain limit (CopyN returns nil, no EOF)
	httpx.Drain(io.NopCloser(strings.NewReader(strings.Repeat("y", 128<<10))))

	// then: a clean drain must not log a failure. An `err != nil` -> `err == nil`
	// negation logs "failed to drain response body" on every successful drain.
	if strings.Contains(buf.String(), "failed to drain") {
		t.Errorf("clean drain logged a failure:\n%s", buf.String())
	}
}

// --- RedirectPolicyFunc: L702 refuse-all guard negation ---

func TestRedirectPolicyFunc_hosts_only_allows_configured_host(t *testing.T) {
	t.Parallel()

	// given: an allowlist with hosts but no suffixes
	policy := httpx.RedirectPolicyFunc(httpx.WithAllowedHosts("example.com"))
	u, _ := url.Parse("https://example.com/path")

	// when/then: the configured host must be allowed. A
	// `len(hosts)==0` -> `len(hosts)!=0` negation flips the empty-allowlist guard
	// so this config wrongly selects the refuse-all branch.
	if err := policy(&http.Request{URL: u}, nil); err != nil {
		t.Errorf("RedirectPolicyFunc(hosts only) to example.com = %v, want nil", err)
	}
}

func TestRedirectPolicyFunc_suffixes_only_allows_configured_suffix(t *testing.T) {
	t.Parallel()

	// given: an allowlist with suffixes but no hosts
	policy := httpx.RedirectPolicyFunc(httpx.WithAllowedSuffixes(".example.org"))
	u, _ := url.Parse("https://sub.example.org/path")

	// when/then: the configured suffix must be allowed. A
	// `len(suffixes)==0` -> `len(suffixes)!=0` negation flips the empty-allowlist
	// guard so this config wrongly selects the refuse-all branch.
	if err := policy(&http.Request{URL: u}, nil); err != nil {
		t.Errorf("RedirectPolicyFunc(suffixes only) to sub.example.org = %v, want nil", err)
	}
}

// --- RetryRoundTripper.getBaseDelay: L190 `baseDelay > 0` boundary ---

func TestRetryRoundTripper_zero_base_delay_defaults_to_base(t *testing.T) {
	t.Parallel()

	// given: an always-503 server, a zero base delay, and a 100ms deadline
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport,
		httpx.WithRTBaseDelay(0), httpx.WithRTMaxAttempts(2))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)

	// when
	resp, err := rt.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}

	// then: a zero base delay must default to DefaultBaseDelay (1s); the single
	// retry sleep (~[0.5s,1s]) blows the 100ms deadline. A `> 0` -> `>= 0` mutant
	// keeps the delay at 0, so the retry runs instantly and returns 503/nil.
	if err == nil {
		t.Errorf("RoundTrip(baseDelay=0) err = nil, want context deadline (delay defaulted)")
	}
}

// --- RetryRoundTripper: L240 `bo == nil` default-backoff doubling ---

func TestRetryRoundTripper_default_backoff_doubles_between_retries(t *testing.T) {
	t.Parallel()

	// given: an always-503 server, a 120ms base delay, 3 retries, and a 400ms
	// deadline. With doubling the three jittered sleeps total >= 60+120+240 =
	// 420ms (> 400ms); without doubling they total <= 120*3 = 360ms (< 400ms).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport,
		httpx.WithRTBaseDelay(120*time.Millisecond), httpx.WithRTMaxAttempts(4))
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)

	// when
	resp, err := rt.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}

	// then: doubling pushes the cumulative backoff past the deadline -> error.
	// A `bo == nil` -> `bo != nil` mutant skips SafeDouble, so every retry keeps
	// the base delay, all retries finish within 400ms, and 503/nil is returned.
	if err == nil {
		t.Errorf("RoundTrip(default backoff) err = nil, want context deadline (backoff must double)")
	}
}

// --- RetryRoundTripper: L284 max-elapsed-time guard (boundary + negation) ---

func TestRetryRoundTripper_zero_max_elapsed_does_not_cap(t *testing.T) {
	t.Parallel()

	// given: a server that fails once then succeeds, with max-elapsed-time unset (0)
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
		httpx.WithRTBaseDelay(time.Millisecond), httpx.WithRTMaxAttempts(3))
	client := rt.StandardClient()

	// when
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get = %v, want nil", err)
	}
	defer resp.Body.Close()

	// then: with maxElapsedTime==0 there is no cap, so the retry reaches 200.
	// A `> 0` -> `>= 0` boundary or `> 0` -> `<= 0` negation mutant treats 0 as
	// "cap enabled" and aborts the first retry with "max elapsed time 0s exceeded".
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (no elapsed-time cap)", resp.StatusCode)
	}
}

func TestRetryRoundTripper_large_max_elapsed_allows_fast_retry(t *testing.T) {
	t.Parallel()

	// given: a server that fails once then succeeds, with a 10s elapsed cap
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
		httpx.WithRTMaxAttempts(3),
		httpx.WithRTMaxElapsedTime(10*time.Second))
	client := rt.StandardClient()

	// when
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get = %v, want nil", err)
	}
	defer resp.Body.Close()

	// then: elapsed (~ms) is far below the 10s cap, so the retry reaches 200.
	// A `since >= maxElapsed` -> `since < maxElapsed` negation aborts immediately.
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (within elapsed cap)", resp.StatusCode)
	}
}

// --- RetryRoundTripper.sleepBeforeRetry: L307 429 Retry-After override boundary ---

func TestRetryRoundTripper_429_without_retry_after_uses_backoff(t *testing.T) {
	t.Parallel()

	// given: a 429 without a Retry-After header, a huge base delay, 100ms deadline
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport,
		httpx.WithRTBaseDelay(10*time.Second), httpx.WithRTMaxAttempts(2))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)

	// when
	resp, err := rt.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}

	// then: with no Retry-After, ParseRetryAfter==0 must NOT override the huge
	// jittered backoff, so the retry sleep blows the 100ms deadline -> error.
	// A `ra > 0` -> `ra >= 0` mutant sets wait=0, runs the retry instantly, 200/nil.
	if err == nil {
		t.Errorf("RoundTrip(429 no Retry-After) err = nil, want context deadline (backoff used)")
	}
}

// --- RetryRoundTripper.sleepBeforeRetry: L314 sleep-error handling negation ---

func TestRetryRoundTripper_sleep_error_aborts_with_bare_context_error(t *testing.T) {
	t.Parallel()

	// given: an always-503 server, a huge base delay, and an 80ms deadline
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	rt := httpx.NewRetryRoundTripper(srv.Client().Transport,
		httpx.WithRTBaseDelay(10*time.Second), httpx.WithRTMaxAttempts(2))
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)

	// when
	resp, err := rt.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("RoundTrip err = nil, want context error from interrupted sleep")
	}

	// then: the deadline-interrupted sleep must abort RoundTrip and surface the
	// bare context error. A `sleepErr != nil` -> `sleepErr == nil` negation
	// swallows it, runs another attempt against the dead context, and returns a
	// transport *url.Error instead.
	var ue *url.Error
	if errors.As(err, &ue) {
		t.Errorf("RoundTrip err = %T (%v), want bare context error, not *url.Error", err, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("RoundTrip err = %v, want context.DeadlineExceeded", err)
	}
}

type errDrainBody struct{}

func (errDrainBody) Read([]byte) (int, error) { return 0, errors.New("read boom") }

func (errDrainBody) Close() error { return nil }

// TestDrain_logs_on_non_eof_read_error must NOT run in parallel: it swaps
// slog.Default() to capture the drain-failure debug line.
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
