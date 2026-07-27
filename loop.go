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
	logger         *slog.Logger
	label          string
	baseDelay      time.Duration
	rlMaxWait      time.Duration
	attemptTimeout time.Duration
	maxAttempts    int
	exhaustedLvl   slog.Level
	exhaustedLvlOn bool
	rlMode         rlMode
	rlConflict     bool
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

// exhaustedLevelOption implements Option for WithExhaustedLevel.
type exhaustedLevelOption slog.Level

func (o exhaustedLevelOption) applyDo(c *loopConfig) {
	c.exhaustedLvl, c.exhaustedLvlOn = slog.Level(o), true
}
func (o exhaustedLevelOption) applyGet(c *getConfig) { o.applyDo(&c.loopConfig) }

// WithExhaustedLevel overrides the level of the terminal "retries exhausted"
// line (see exhaustedLevel for the default rule).
//
// It exists for the caller that reports the same terminal failure itself, with
// more context than this door can have. Such a caller wants the per-attempt
// retry diagnostics and NOT the verdict: leaving both produces two log lines
// for one failure, and silencing the whole logger to stop the second throws the
// diagnostics away with it. Pass slog.LevelDebug to keep the line for diagnosis
// while the caller's own line carries the alarm.
//
// It does not change what is returned, and it does not suppress the line: a
// level below the logger's threshold simply is not emitted, which is the
// caller's decision to make.
func WithExhaustedLevel(l slog.Level) Option { return exhaustedLevelOption(l) }

// labelOption implements DoOption for WithLabel.
type labelOption string

func (o labelOption) applyDo(c *loopConfig) { c.label = string(o) }

// WithLabel sets the operation label used in Do's log lines ("<label> failed,
// retrying", "<label> retries exhausted"). Default: "operation".
func WithLabel(s string) DoOption { return labelOption(s) }

// attemptTimeoutOption implements DoOption for WithAttemptTimeout.
type attemptTimeoutOption time.Duration

func (o attemptTimeoutOption) applyDo(c *loopConfig) { c.attemptTimeout = time.Duration(o) }

// WithAttemptTimeout bounds EACH attempt by d and makes that bound's expiry
// RETRYABLE: the per-attempt (per-try) timeout, as distinct from the caller's
// total budget. It is the piece that joins a per-attempt deadline to the retry
// loop; without it a deadline inside fn is classified terminal (see
// [AttemptTimeout] for why an error alone cannot say otherwise). A
// non-positive d means no per-attempt bound, which is the option's absence.
//
// fn receives a context derived from the caller's with d applied. Context
// keeps the EARLIER deadline, so a caller deadline nearer than d still governs
// and the total budget is never extended — d caps one attempt, never the call.
//
// The expiry is marked as the attempt's ONLY when the caller's own context is
// still live, so retrying a caller that is out of budget stays impossible: an
// expired or canceled caller context is never marked, [Do] returns ctx.Err()
// before it classifies anything, and [SleepCtx] refuses to wait on a dead
// context. The decision is made from the two contexts rather than from the
// error, because a [context.DeadlineExceeded] value is identical either way.
//
// The attempt context is canceled as soon as fn returns, so fn must not return
// a value that still depends on it — read, decode, or drain a response body
// inside fn (the same rule a caller-derived per-attempt context already
// imposes).
//
// It is a DoOption: [GetBytes] owns its own request and has no callback to
// bound. Bound that door by running it as one attempt inside Do —
// GetBytes(ctx, ...) with WithMaxAttempts(1) inside a Do callback carrying this
// option — the same composition that avoids multiplying two attempt budgets.
func WithAttemptTimeout(d time.Duration) DoOption { return attemptTimeoutOption(d) }

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

// runAttempt runs one attempt of fn under the configured per-attempt bound
// (WithAttemptTimeout). With no bound configured fn sees the caller's context
// unchanged, so the option-absent path is exactly a direct call.
func runAttempt[T any](ctx context.Context, cfg *loopConfig, fn func(ctx context.Context) (T, error)) (T, error) {
	if cfg.attemptTimeout <= 0 {
		return fn(ctx)
	}
	attemptCtx, cancel := context.WithTimeout(ctx, cfg.attemptTimeout)
	defer cancel()
	result, err := fn(attemptCtx)
	return result, markAttemptTimeout(ctx, attemptCtx, err)
}

// markAttemptTimeout marks err retryable when THIS attempt's bound is what
// expired. The three guards are what make the mark safe:
//
//   - ctx.Err() == nil: the caller still has budget. If the caller's context is
//     the one that ended, the deadline is the caller's and stays terminal —
//     which also covers the case where the caller's deadline is the earlier of
//     the two and therefore the one the attempt context inherited.
//   - attemptCtx.Err() != nil: the bound this loop installed actually expired,
//     rather than some deadline fn holds internally.
//   - the error reports a deadline: fn may translate its own failure, and the
//     mark must not claim a deadline for an error that is not one. A timeout
//     that does NOT match context.DeadlineExceeded (a net.Error i/o timeout,
//     say) is already transient without any mark.
//
// Every ambiguous case therefore resolves to "leave it alone": the mark is
// applied only when the attempt's own bound is provably the failing one.
func markAttemptTimeout(ctx, attemptCtx context.Context, err error) error {
	if err == nil || ctx.Err() != nil || attemptCtx.Err() == nil {
		return err
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return AttemptTimeout(err)
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

// exhaustedMsg is the terminal message for the configured mode.
func (c *loopConfig) exhaustedMsg() string {
	if c.rlMode == rlOnly {
		return "rate limit retries exhausted"
	}
	return c.label + " retries exhausted"
}

// exhaustedLevel picks the level for a door's terminal failure line. A
// multi-attempt budget that ran out genuinely exhausted a retry tree this door
// owns, which is an operator-visible degradation: Warn.
//
// A single-attempt budget retried nothing, so there is nothing exhausted to
// report. WithMaxAttempts(1) is how a caller runs a door as ONE attempt inside
// its own retry loop (the sanctioned no-3x3-amplification pattern - the same
// reason the Retry-After hint escapes on the returned error), and that loop owns
// both the retry policy and the terminal Warn. Warning here too would
// double-report every such failure and, on the enclosing loop's non-final
// attempts, announce a degradation that self-healed on the next attempt. Debug
// keeps the line for diagnosis without the alarm; the error is returned either
// way, and a non-retryable failure already logs nothing at all - so this also
// makes a one-attempt door's two failure classes log alike.
// A caller that publishes its own terminal verdict overrides both rules with
// WithExhaustedLevel.
func (c *loopConfig) exhaustedLevel() slog.Level {
	if c.exhaustedLvlOn {
		return c.exhaustedLvl
	}
	if c.maxAttempts > 1 {
		return slog.LevelWarn
	}
	return slog.LevelDebug
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
// attempt's error wins (the v2 RetryOnRateLimit contract). WithAttemptTimeout
// bounds each attempt and makes that bound's expiry retryable, the caller's own
// deadline staying terminal. Logging goes to WithLogger (default
// slog.Default()): per-attempt lines at Debug, the terminal exhaustion at Warn
// - or at Debug when the budget is a single attempt, since nothing was retried
// and the caller's own loop owns the warning (see exhaustedLevel).
func Do[T any](ctx context.Context, fn func(ctx context.Context) (T, error), opts ...DoOption) (T, error) {
	var zero T
	cfg := newLoopConfig(opts)
	if cfg.rlConflict {
		return zero, errors.New("httpx: WithRateLimitRetry and WithRateLimitOnly are mutually exclusive")
	}
	var lastErr error
	backoff := cfg.baseDelay
	for attempt := range cfg.maxAttempts {
		result, err := runAttempt(ctx, &cfg, fn)
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
		cfg.logger.Log(ctx, cfg.exhaustedLevel(), cfg.exhaustedMsg(),
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
//   - classification of every 5xx (not just 502/503/504) as retryable, and of
//     every non-2xx as a permanent *StatusError carrying the request URL (the
//     2xx success band is CheckHTTPStatus's, delegated to since v4; only the
//     error value differs, because this door's errors render a redacted URL).
//     A 3xx reaches here only when the client refuses redirects, and is an
//     error: the redirect stub is not the requested resource and GetBytes
//     cannot surface Location.
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
	log.Log(ctx, cfg.exhaustedLevel(), "http retries exhausted",
		"url", redactURL(reqURL), "attempts", cfg.maxAttempts, "elapsed", elapsed.Round(time.Millisecond), "error", LogSafeError(lastErr))
	exhausted := fmt.Errorf("retries exhausted after %s: %w", elapsed.Round(time.Millisecond), LogSafeError(lastErr))
	// Carry the last attempt's capped Retry-After out on the returned error.
	// Without this the hint dies in overrideWait, which only the NEXT iteration
	// of this loop reads - so a caller running GetBytes with WithMaxAttempts(1)
	// inside its own outer retry loop (the sanctioned no-3x3-amplification
	// pattern) silently lost the upstream-requested wait and fell back to
	// jittered backoff. Do already honors RetryAfterHint on a transient error,
	// so the two doors now compose: the hint is already capped by
	// ParseRetryAfter, satisfying the interface's pre-capped contract.
	if overrideWait > 0 {
		return nil, &retryAfterError{err: exhausted, hint: overrideWait}
	}
	return nil, exhausted
}

// retryAfterError carries a capped Retry-After hint on GetBytes's exhaustion
// error so an enclosing retry loop can honor it. It deliberately does NOT
// implement Transient: whether an exhausted GetBytes is worth another outer
// attempt is the caller's policy, and claiming transience here would silently
// widen every existing consumer's retryable set.
type retryAfterError struct {
	err  error
	hint time.Duration
}

func (e *retryAfterError) Error() string { return e.err.Error() }

func (e *retryAfterError) Unwrap() error { return e.err }

func (e *retryAfterError) RetryAfterHint() time.Duration { return e.hint }

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
	// 408, 429 and 5xx are all retryable and handled identically (each honors a
	// capped Retry-After); one guard avoids three byte-identical copies. A 408
	// Request Timeout is the server reporting that IT gave up waiting, which is
	// self-healing and safe to repeat on this door (GET is idempotent), so
	// excluding it forced consumers to either lose the retry or re-classify the
	// status themselves.
	if resp.StatusCode == http.StatusRequestTimeout ||
		resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		ra := ParseRetryAfter(resp.Header.Get("Retry-After"))
		DrainClose(resp.Body)
		return nil, ra, &retryableError{err: &StatusError{Code: resp.StatusCode, URL: reqURL}}
	}
	// Success is any 2xx. Everything else is a permanent failure for this
	// door: it returns body bytes, and a redirect stub or an error page is not
	// the requested resource. Since v4 CheckHTTPStatus draws the same line
	// (nil only for 2xx), so the band decision is delegated to it rather than
	// restated here — the one thing GetBytes substitutes is the error VALUE:
	// its *StatusError carries the request URL (rendered redacted), which
	// consumers type-assert on, where the classifier's errors are URL-less.
	if CheckHTTPStatus(resp) != nil {
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
