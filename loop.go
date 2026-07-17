package httpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// This file holds the two retry LOOP doors (Do and GetBytes) and their shared
// option vocabulary. The transparent-transport door lives in roundtripper.go
// with its own struct-based config (TransportConfig); the loop doors use
// functional options because their dominant call pattern is zero to two
// options inline at a call expression.

// --- Option vocabulary ---

// DoOption configures Do. Options constructed as Option values apply to both
// loop doors; Do-only options (WithLabel, WithRateLimitRetry,
// WithRateLimitOnly) implement only this interface, so passing one to
// GetBytes is a compile error.
type DoOption interface {
	applyDo(*loopConfig)
}

// GetOption configures GetBytes. GetBytes-only options (WithHeaders,
// WithMaxBodyBytes) implement only this interface, so passing one to Do is a
// compile error.
type GetOption interface {
	applyGet(*getConfig)
}

// Option is a retry option accepted by BOTH loop doors, Do and GetBytes:
// WithMaxAttempts, WithBaseDelay, and WithLogger construct Option values.
type Option interface {
	DoOption
	GetOption
}

// rlMode selects how Do treats *RateLimitError.
type rlMode uint8

const (
	rlNone  rlMode = iota // default: rate limits are not retried (IsTransient excludes them)
	rlRetry               // WithRateLimitRetry: transients AND rate limits are retried
	rlOnly                // WithRateLimitOnly: ONLY rate limits are retried
)

// loopConfig holds the retry-loop settings shared by Do and GetBytes, plus
// the Do-only fields.
type loopConfig struct {
	logger      *slog.Logger
	label       string
	baseDelay   time.Duration
	rlMaxWait   time.Duration
	maxAttempts int
	rlMode      rlMode
	rlConflict  bool
}

// getConfig holds GetBytes settings: the shared loop settings plus the
// GET-door specifics.
type getConfig struct {
	setHeaders func(*http.Request)
	loopConfig
	maxBodyBytes int64
}

// maxAttemptsOption implements Option for WithMaxAttempts.
type maxAttemptsOption int

func (o maxAttemptsOption) applyDo(c *loopConfig) { c.maxAttempts = int(o) }
func (o maxAttemptsOption) applyGet(c *getConfig) { o.applyDo(&c.loopConfig) }

// WithMaxAttempts sets the maximum number of attempts (TOTAL, including the
// first call). Default: DefaultMaxAttempts (3). A value below 1 is treated as
// 1, so the operation always runs at least once (never a silent no-op).
func WithMaxAttempts(n int) Option { return maxAttemptsOption(n) }

// baseDelayOption implements Option for WithBaseDelay.
type baseDelayOption time.Duration

func (o baseDelayOption) applyDo(c *loopConfig) { c.baseDelay = time.Duration(o) }
func (o baseDelayOption) applyGet(c *getConfig) { o.applyDo(&c.loopConfig) }

// WithBaseDelay sets the initial backoff delay. Default: DefaultBaseDelay
// (1s). A non-positive value falls back to the default.
func WithBaseDelay(d time.Duration) Option { return baseDelayOption(d) }

// loggerOption implements Option for WithLogger.
type loggerOption struct{ l *slog.Logger }

func (o loggerOption) applyDo(c *loopConfig) { c.logger = o.l }
func (o loggerOption) applyGet(c *getConfig) { o.applyDo(&c.loopConfig) }

// WithLogger sets the logger for retry diagnostics. Default: slog.Default().
// A nil logger falls back to the default.
func WithLogger(l *slog.Logger) Option { return loggerOption{l: l} }

// labelOption implements DoOption for WithLabel.
type labelOption string

func (o labelOption) applyDo(c *loopConfig) { c.label = string(o) }

// WithLabel sets the operation label used in Do's log lines ("<label> failed,
// retrying", "<label> retries exhausted"). Default: "operation".
func WithLabel(s string) DoOption { return labelOption(s) }

// rateLimitOption implements DoOption for WithRateLimitRetry and
// WithRateLimitOnly.
type rateLimitOption struct {
	maxWait time.Duration
	mode    rlMode
}

func (o rateLimitOption) applyDo(c *loopConfig) {
	if c.rlMode != rlNone && c.rlMode != o.mode {
		c.rlConflict = true
	}
	c.rlMode = o.mode
	c.rlMaxWait = o.maxWait
}

// WithRateLimitRetry makes Do additionally treat *RateLimitError as retryable,
// alongside the transient set. The wait before a rate-limit retry is
// min(err.RetryAfter, maxWait) when the error carries a positive hint, else
// maxWait; a non-positive maxWait falls back to RetryAfterCap, so the wait is
// always positive and a canceled context is observed before every retry.
// Transient errors keep their jittered-backoff waits (with the RetryAfterHint
// override), and the exponential base advances on every retry. Mutually
// exclusive with WithRateLimitOnly (Do returns a configuration error when
// both are supplied).
func WithRateLimitRetry(maxWait time.Duration) DoOption {
	return rateLimitOption{maxWait: maxWait, mode: rlRetry}
}

// WithRateLimitOnly makes Do retry ONLY *RateLimitError (matched through
// wrapped errors); every other error, including transient transport errors,
// is returned immediately. It absorbs v2's RetryOnRateLimit: the wait
// semantics match WithRateLimitRetry, the terminal Warn is "rate limit
// retries exhausted", and, matching the v2 contract, the FINAL attempt's
// error wins even under an already-canceled context (cancellation is observed
// in the always-positive inter-attempt sleep instead). Mutually exclusive
// with WithRateLimitRetry.
func WithRateLimitOnly(maxWait time.Duration) DoOption {
	return rateLimitOption{maxWait: maxWait, mode: rlOnly}
}

// headersOption implements GetOption for WithHeaders.
type headersOption struct{ fn func(*http.Request) }

func (o headersOption) applyGet(c *getConfig) { c.setHeaders = o.fn }

// WithHeaders sets a function that is called to set headers on each request.
func WithHeaders(fn func(*http.Request)) GetOption { return headersOption{fn: fn} }

// maxBodyBytesOption implements GetOption for WithMaxBodyBytes.
type maxBodyBytesOption int64

func (o maxBodyBytesOption) applyGet(c *getConfig) { c.maxBodyBytes = int64(o) }

// WithMaxBodyBytes sets the maximum response body size to read.
// Default: DefaultMaxBodyBytes (10 MB). A non-positive value falls back to
// the default.
func WithMaxBodyBytes(n int64) GetOption { return maxBodyBytesOption(n) }

// --- Config assembly ---

// normalize applies the shared defaults and clamps: maxAttempts below 1
// clamps to 1 (the option-absent default is DefaultMaxAttempts, set by the
// config constructors — unlike TransportConfig's struct fields, option
// absence is expressible here, so WithMaxAttempts(0) keeps its v2 meaning of
// "exactly one attempt"), a non-positive baseDelay takes DefaultBaseDelay, a
// nil logger takes slog.Default(), an empty label reads "operation", and a
// non-positive rate-limit maxWait clamps to RetryAfterCap (a zero ceiling
// would zero every wait; SleepCtx returns immediately for non-positive
// durations, and the loop would hot-spin with no cancellation check).
func (c *loopConfig) normalize() {
	if c.maxAttempts < 1 {
		c.maxAttempts = 1
	}
	if c.baseDelay <= 0 {
		c.baseDelay = DefaultBaseDelay
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	if c.label == "" {
		c.label = "operation"
	}
	if c.rlMode != rlNone && c.rlMaxWait <= 0 {
		c.rlMaxWait = RetryAfterCap
	}
}

// newLoopConfig builds a Do configuration from opts (nil options are skipped)
// and applies defaults.
func newLoopConfig(opts []DoOption) loopConfig {
	cfg := loopConfig{maxAttempts: DefaultMaxAttempts}
	for _, o := range opts {
		if o != nil {
			o.applyDo(&cfg)
		}
	}
	cfg.normalize()
	return cfg
}

// newGetConfig builds a GetBytes configuration from opts (nil options are
// skipped) and applies defaults.
func newGetConfig(opts []GetOption) getConfig {
	cfg := getConfig{loopConfig: loopConfig{maxAttempts: DefaultMaxAttempts}}
	for _, o := range opts {
		if o != nil {
			o.applyGet(&cfg)
		}
	}
	cfg.normalize()
	if cfg.maxBodyBytes <= 0 {
		cfg.maxBodyBytes = DefaultMaxBodyBytes
	}
	return cfg
}

// --- Shared loop helpers ---

// logRetrySuccess emits the debug line when fn recovered after at least one
// retry (attempt is 0-indexed, so attempt > 0 means a prior failure recovered).
func (c *loopConfig) logRetrySuccess(attempt int) {
	if attempt > 0 {
		c.logger.Debug(c.label+" succeeded after retry", "attempts", attempt+1)
	}
}

// retryAfterHintWait extracts a positive Retry-After hint carried by err via
// the RetryAfterHint interface (already capped by the implementer, see the
// interface doc); zero means no hint.
func retryAfterHintWait(err error) time.Duration {
	var h RetryAfterHint
	if errors.As(err, &h) {
		if d := h.RetryAfterHint(); d > 0 {
			return d
		}
	}
	return 0
}

// resolveWait returns the wait before the next retry: a positive explicit
// wait (a capped Retry-After hint or a rate-limit wait) takes precedence over
// the jittered exponential backoff. It is the single wait-resolution point
// for both loop doors.
func resolveWait(explicit, backoff time.Duration) time.Duration {
	if explicit > 0 {
		return explicit
	}
	return JitteredBackoff(backoff)
}

// classify reports whether err is retryable under the configured mode and the
// explicit wait to honor (zero means jittered backoff).
func (c *loopConfig) classify(err error) (retryable bool, explicitWait time.Duration) {
	if c.rlMode != rlNone {
		var rl *RateLimitError
		if errors.As(err, &rl) {
			wait := c.rlMaxWait
			if rl.RetryAfter > 0 {
				wait = min(rl.RetryAfter, c.rlMaxWait)
			}
			return true, wait
		}
		if c.rlMode == rlOnly {
			return false, 0
		}
	}
	if !IsTransient(err) {
		return false, 0
	}
	return true, retryAfterHintWait(err)
}

// exhaustedMsg is the terminal Warn message for the configured mode.
func (c *loopConfig) exhaustedMsg() string {
	if c.rlMode == rlOnly {
		return "rate limit retries exhausted"
	}
	return c.label + " retries exhausted"
}

// --- Door 1: Do ---

// Do calls fn up to WithMaxAttempts times (total, including the first call)
// with jittered exponential backoff, returning the first success.
// Non-retryable errors are returned immediately. By default the retryable set
// is IsTransient (a *RateLimitError is deliberately NOT transient; a generic
// operation must not blindly re-fire a rate-limited call) and a transient
// error carrying a positive RetryAfterHint waits that hint instead of the
// backoff, the exponential base still advancing. WithRateLimitRetry and
// WithRateLimitOnly opt into rate-limit retry per their docs.
//
// Under the default and WithRateLimitRetry modes a context canceled after a
// failed attempt returns ctx.Err(); under WithRateLimitOnly the final
// attempt's error wins (the v2 RetryOnRateLimit contract). Logging goes to
// WithLogger (default slog.Default()): per-attempt lines at Debug, the
// terminal exhaustion at Warn.
func Do[T any](ctx context.Context, fn func(ctx context.Context) (T, error), opts ...DoOption) (T, error) {
	var zero T
	cfg := newLoopConfig(opts)
	if cfg.rlConflict {
		return zero, errors.New("httpx: WithRateLimitRetry and WithRateLimitOnly are mutually exclusive")
	}
	var lastErr error
	backoff := cfg.baseDelay
	for attempt := range cfg.maxAttempts {
		result, err := fn(ctx)
		if err == nil {
			cfg.logRetrySuccess(attempt)
			return result, nil
		}
		lastErr = err
		if cfg.rlMode != rlOnly && ctx.Err() != nil {
			return zero, ctx.Err()
		}
		retryable, explicitWait := cfg.classify(err)
		if !retryable {
			return zero, err
		}
		if attempt == cfg.maxAttempts-1 {
			break
		}
		wait := resolveWait(explicitWait, backoff)
		cfg.logger.Debug(cfg.label+" failed, retrying",
			"attempt", attempt+1, "max", cfg.maxAttempts,
			"delay", wait.String(), "error", LogSafeError(err))
		if err := SleepCtx(ctx, wait); err != nil {
			return zero, err
		}
		backoff = SafeDouble(backoff)
	}
	if lastErr != nil {
		cfg.logger.Warn(cfg.exhaustedMsg(),
			"attempts", cfg.maxAttempts, "error", LogSafeError(lastErr))
	}
	return zero, lastErr
}

// --- Door 2: GetBytes ---

// GetBytes performs an HTTP GET with bounded exponential-backoff retry on
// 429 and 5xx responses and on transient transport errors (timeouts,
// connection resets, DNS failures - see IsTransient). 4xx (non-429) and
// non-transient transport errors are returned immediately. Honors
// Retry-After (capped at RetryAfterCap). The response body is read to
// WithMaxBodyBytes and returned; an over-limit body fails loud with
// *ResponseTooLargeError (no body). Every logged url attribute and every
// returned error is redacted (see the package's URL redaction docs).
//
// GetBytes deliberately keeps its own retry loop rather than delegating to
// RetryRoundTripper.RoundTrip. It is a decorator over the same shared
// primitives (resolveWait, JitteredBackoff, SafeDouble, SleepCtx,
// ParseRetryAfter, IsTransient, Drain), not a thin wrapper over the
// RoundTripper cycle, because GetBytes carries behavior the transparent
// RoundTripper has no equivalent for and which must stay byte-for-byte stable
// for existing consumers:
//   - []byte return with the body capped at WithMaxBodyBytes (the RoundTripper
//     hands back an *http.Response and never reads the body);
//   - URL/secret redaction on every log "url" attr (redactURL) and every
//     returned/wrapped error (LogSafeError, StatusError.Error()), the
//     CWE-532 hardening the RoundTripper path does not perform;
//   - rich per-attempt slog logging plus the "retries exhausted after %s: %w"
//     wrapper, which the RoundTripper exposes only as an OnRetry hook;
//   - classification of every 5xx (not just 502/503/504) as retryable and of
//     any non-2xx (3xx included) as a permanent *StatusError. A 2xx response
//     returns the body; GetBytes cannot surface a redirect, so 3xx is an error.
//
// Routing GetBytes through RoundTrip would silently change one or more of
// these, so the loop is intentionally not merged.
func GetBytes(ctx context.Context, client *http.Client, reqURL string, opts ...GetOption) ([]byte, error) {
	cfg := newGetConfig(opts)
	log := cfg.logger

	start := time.Now()
	var lastErr error
	var overrideWait time.Duration
	backoff := cfg.baseDelay
	for attempt := range cfg.maxAttempts {
		if attempt > 0 {
			if err := SleepCtx(ctx, resolveWait(overrideWait, backoff)); err != nil {
				return nil, err
			}
			backoff = SafeDouble(backoff)
		}
		attemptStart := time.Now()
		body, retryAfter, err := getAttempt(ctx, client, reqURL, &cfg)
		if body != nil {
			logSlowUpstream(log, reqURL, attemptStart)
			return body, nil
		}
		if err != nil && !isRetryStatus(err) {
			return nil, LogSafeError(err)
		}
		lastErr = err
		overrideWait = retryAfter
		if attempt == cfg.maxAttempts-1 {
			break
		}
		log.Debug("http request failed, will retry",
			"url", redactURL(reqURL), "attempt", attempt+1, "max", cfg.maxAttempts, "error", LogSafeError(err))
	}
	elapsed := time.Since(start)
	log.Warn("http retries exhausted",
		"url", redactURL(reqURL), "attempts", cfg.maxAttempts, "elapsed", elapsed.Round(time.Millisecond), "error", LogSafeError(lastErr))
	return nil, fmt.Errorf("retries exhausted after %s: %w", elapsed.Round(time.Millisecond), LogSafeError(lastErr))
}

// logSlowUpstream warns when a successful attempt took longer than 10s. Timed
// per-attempt so the library's own backoff sleeps are not mislabeled as
// upstream latency.
func logSlowUpstream(log *slog.Logger, reqURL string, attemptStart time.Time) {
	if elapsed := time.Since(attemptStart); elapsed > 10*time.Second {
		log.Warn("slow upstream response", "url", redactURL(reqURL), "duration", elapsed.Round(time.Millisecond))
	}
}

// getAttempt performs a single HTTP GET attempt. Returns (body, 0, nil) on
// success, (nil, retryAfter, err) on retryable failure, or (nil, 0, err) on
// permanent failure.
func getAttempt(ctx context.Context, client *http.Client, reqURL string, cfg *getConfig) ([]byte, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, http.NoBody)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}
	if cfg.setHeaders != nil {
		cfg.setHeaders(req)
	}
	resp, err := client.Do(req) //nolint:bodyclose // resp.Body is closed on every path: DrainClose (429/5xx, non-2xx) or ReadLimitedBody's deferred close (2xx); bodyclose can't trace the close through the helper.
	if err != nil {
		if !IsTransient(err) {
			return nil, 0, err
		}
		return nil, 0, &retryableError{err: err}
	}
	// 429 and 5xx are both retryable and handled identically (both honor a
	// capped Retry-After); one guard avoids two byte-identical copies.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		ra := ParseRetryAfter(resp.Header.Get("Retry-After"))
		DrainClose(resp.Body)
		return nil, ra, &retryableError{err: &StatusError{Code: resp.StatusCode, URL: reqURL}}
	}
	// Success is any 2xx. GetBytes returns the body bytes, so a 3xx (which
	// reaches here only when the client is configured not to follow redirects)
	// is a permanent *StatusError: the redirect stub is not the requested
	// resource and GetBytes cannot surface Location. Intentional divergence
	// from CheckHTTPStatus, a general error-classifier that treats 3xx as
	// non-error.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		DrainClose(resp.Body)
		return nil, 0, &StatusError{Code: resp.StatusCode, URL: reqURL}
	}
	// Read the body with overflow detection: an over-limit body fails loud with
	// *ResponseTooLargeError rather than being silently truncated (a truncated
	// payload that looks complete is a corruption hazard). ReadLimitedBody owns
	// the cap+1 probe, its int64-overflow guard, and closing the body.
	body, err := ReadLimitedBody(resp.Body, cfg.maxBodyBytes)
	if err != nil {
		var tooLarge *ResponseTooLargeError
		if errors.As(err, &tooLarge) {
			return nil, 0, err
		}
		return nil, 0, fmt.Errorf("read response: %w", err)
	}
	return body, 0, nil
}

// retryableError is an internal marker for errors that should be retried.
type retryableError struct{ err error }

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

// isRetryStatus reports whether an error from getAttempt is retryable.
func isRetryStatus(err error) bool {
	var re *retryableError
	return errors.As(err, &re)
}
