// Package httpx provides a resilient outbound-HTTP toolkit: transient-error
// classification, generic retry with jittered exponential backoff, Retry-After
// parsing, HTTP status mapping, secret redaction, body draining, and a
// configurable redirect allowlist.
package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// --- Error types (inlined from app-internal packages) ---

// AuthError indicates invalid or expired credentials.
type AuthError struct{ Msg string }

func (e *AuthError) Error() string { return e.Msg }

// RateLimitError indicates a rate limit was exceeded.
// RetryAfter, when non-zero, is the hint from the upstream's Retry-After header.
type RateLimitError struct {
	Msg        string
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string { return e.Msg }

// Transient is the interface for errors that can report whether they
// represent a transient (retryable) failure.
type Transient interface {
	IsTransient() bool
}

// ErrRateLimited is a sentinel callers use with errors.Is to detect 429 responses.
var ErrRateLimited = errors.New("rate limited")

// ErrServerError is a sentinel for upstream 5xx responses.
var ErrServerError = errors.New("server error")

// --- HTTP status errors ---

// HTTPStatusError represents a non-2xx HTTP response not covered by AuthError
// or RateLimitError. Implements the Transient interface for 502/503/504.
type HTTPStatusError struct {
	Code int
}

var _ Transient = (*HTTPStatusError)(nil)

func (e *HTTPStatusError) Error() string { return fmt.Sprintf("HTTP %d", e.Code) }

// IsTransient reports whether the status code is a retryable server failure (502/503/504).
func (e *HTTPStatusError) IsTransient() bool {
	return e.Code == 502 || e.Code == 503 || e.Code == 504
}

// IsServerError reports whether the status code is 5xx.
func (e *HTTPStatusError) IsServerError() bool { return e.Code >= 500 }

// IsClientError reports whether the status code is 4xx.
func (e *HTTPStatusError) IsClientError() bool { return e.Code >= 400 && e.Code < 500 }

// StatusError represents a non-2xx response with URL context. Used by Retry.
// Supports errors.Is matching against ErrRateLimited and ErrServerError.
type StatusError struct {
	URL  string
	Code int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("HTTP %d from %s", e.Code, e.URL)
}

// Is reports whether this StatusError matches ErrRateLimited or ErrServerError.
func (e *StatusError) Is(target error) bool {
	switch target {
	case ErrRateLimited:
		return e.Code == http.StatusTooManyRequests
	case ErrServerError:
		return e.Code >= 500 && e.Code < 600
	}
	return false
}

// --- PermanentError ---

// PermanentError wraps an error to signal that it should NOT be retried,
// regardless of other retry policies. Mirrors cenkalti/backoff.PermanentError.
// Use Permanent(err) to wrap.
type PermanentError struct {
	Err error
}

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Is allows errors.Is matching against other PermanentErrors.
func (e *PermanentError) Is(target error) bool {
	_, ok := target.(*PermanentError)
	return ok
}

// Permanent wraps err to indicate it should never be retried.
// Mirrors cenkalti/backoff.Permanent().
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent reports whether err (or any wrapped error) is a *PermanentError.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// --- Backoff strategy interface ---

// Backoff is a pluggable backoff strategy. NextBackOff returns the duration to
// wait before the next retry. Return BackoffStop to signal no more retries.
// Mirrors cenkalti/backoff.BackOff.
type Backoff interface {
	// NextBackOff returns the next wait duration, or BackoffStop to stop.
	NextBackOff() time.Duration
	// Reset restores the strategy to its initial state.
	Reset()
}

// BackoffStop signals that no more retries should be made.
const BackoffStop time.Duration = -1

// --- ExponentialBackoff with functional options ---

// expBackoffCfg holds configuration for ExponentialBackoff.
type expBackoffCfg struct {
	initialInterval time.Duration
	maxElapsedTime  time.Duration
}

// ExpBackoffOption configures an ExponentialBackoff.
type ExpBackoffOption func(*expBackoffCfg)

// WithInitialInterval sets the first backoff duration. Default: DefaultBaseDelay.
func WithInitialInterval(d time.Duration) ExpBackoffOption {
	return func(c *expBackoffCfg) { c.initialInterval = d }
}

// WithMaxElapsedTime caps total retry time for the backoff. Zero means no cap.
func WithMaxElapsedTime(d time.Duration) ExpBackoffOption {
	return func(c *expBackoffCfg) { c.maxElapsedTime = d }
}

// ExponentialBackoff implements Backoff with jittered exponential backoff.
// This is the default strategy used throughout httpx.
type ExponentialBackoff struct {
	startTime       time.Time
	initialInterval time.Duration
	maxElapsedTime  time.Duration
	current         time.Duration
}

// NewExponentialBackoff creates an ExponentialBackoff with functional options.
func NewExponentialBackoff(opts ...ExpBackoffOption) *ExponentialBackoff {
	cfg := expBackoffCfg{
		initialInterval: DefaultBaseDelay,
	}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	b := &ExponentialBackoff{
		initialInterval: cfg.initialInterval,
		maxElapsedTime:  cfg.maxElapsedTime,
	}
	b.Reset()
	return b
}

// NextBackOff returns the next jittered backoff duration, or BackoffStop if
// MaxElapsedTime has been exceeded.
func (b *ExponentialBackoff) NextBackOff() time.Duration {
	if b.current == 0 {
		b.Reset()
	}
	if b.maxElapsedTime > 0 && time.Since(b.startTime) >= b.maxElapsedTime {
		return BackoffStop
	}
	wait := JitteredBackoff(b.current)
	b.current = SafeDouble(b.current)
	return wait
}

// Reset restores the backoff to its initial state.
func (b *ExponentialBackoff) Reset() {
	if b.initialInterval <= 0 {
		b.initialInterval = DefaultBaseDelay
	}
	b.current = b.initialInterval
	b.startTime = time.Now()
}

// --- Constants ---

const (
	// DefaultBaseDelay is the production base for exponential-backoff retry.
	DefaultBaseDelay = time.Second
	// DefaultMaxAttempts caps Retry at three tries.
	DefaultMaxAttempts = 3
	// DefaultMaxBodyBytes caps response bodies at 10 MB.
	DefaultMaxBodyBytes int64 = 10 << 20
	// RetryAfterCap is the maximum Retry-After honor duration.
	RetryAfterCap = 60 * time.Second
)

// drainLimit caps body drain reads for connection reuse.
const drainLimit = 64 << 10

// redirectCap is the maximum redirect hops.
const redirectCap = 5

// --- Retry-After parsing ---

// ParseRetryAfter parses a Retry-After header value (delta-seconds or HTTP-date).
// Returns zero for missing/malformed values. Caps at RetryAfterCap for safety
// (prevents unbounded waits in retry loops). For raw uncapped values, use
// ParseRetryAfterResponse.
func ParseRetryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if n, err := strconv.Atoi(strings.TrimSpace(h)); err == nil {
		if n <= 0 {
			return 0
		}
		// Cap before multiplication to prevent int64 overflow.
		capSecs := int(RetryAfterCap / time.Second)
		if n > capSecs {
			return RetryAfterCap
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return min(d, RetryAfterCap)
		}
	}
	return 0
}

// ParseRetryAfterResponse parses the Retry-After header from an *http.Response.
// Returns zero if absent or unparseable. Does NOT cap — preserves the raw
// duration so callers (e.g., CheckHTTPStatus) can make their own decisions.
// For capped values suitable for retry loops, use ParseRetryAfter.
func ParseRetryAfterResponse(resp *http.Response) time.Duration {
	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		return 0
	}
	if secs, err := strconv.Atoi(ra); err == nil {
		if secs <= 0 {
			return 0
		}
		// Guard against int64 overflow: max representable seconds in time.Duration.
		const maxSecs = int(^uint(0)>>1) / int(time.Second)
		if secs > maxSecs {
			return time.Duration(maxSecs) * time.Second
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(ra); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0
		}
		return d
	}
	return 0
}

// --- Status checking ---

// CheckHTTPStatus maps HTTP error status codes to typed errors.
// Returns nil for 2xx/3xx. 401/403 → *AuthError, 429 → *RateLimitError,
// others ≥400 → *HTTPStatusError.
func CheckHTTPStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return nil
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return &AuthError{Msg: "invalid API key (401)"}
	case http.StatusForbidden:
		return &AuthError{Msg: "access denied (403)"}
	case http.StatusTooManyRequests:
		return &RateLimitError{Msg: "rate limited (429)", RetryAfter: ParseRetryAfterResponse(resp)}
	}
	if resp.StatusCode >= 400 {
		return &HTTPStatusError{Code: resp.StatusCode}
	}
	return nil
}

// --- Backoff helpers ---

// JitteredBackoff returns a duration in [backoff/2, backoff] using the "equal
// jitter" strategy (per AWS Builders' Library). Full jitter and decorrelated
// jitter are intentionally not provided — equal jitter is the recommended
// default for HTTP retry as it avoids thundering herd while maintaining a
// minimum backoff floor.
func JitteredBackoff(backoff time.Duration) time.Duration {
	if backoff <= 0 {
		return backoff
	}
	half := int64(backoff) / 2
	jitter := rand.Int64N(half + 1) //nolint:gosec // G404: jitter, not crypto
	return time.Duration(half + jitter)
}

// SafeDouble doubles a duration, guarding against int64 overflow.
func SafeDouble(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	doubled := d * 2
	if doubled < d {
		return time.Duration(1<<63 - 1)
	}
	return doubled
}

// SleepCtx sleeps for d or returns early on context cancellation.
func SleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	select {
	case <-ctx.Done():
		t.Stop()
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// --- Transient classification ---

// IsTransient returns true for errors likely caused by temporary server or
// network issues worth retrying. Auth, rate-limit, permanent, and context
// errors are never transient.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	if IsPermanent(err) {
		return false
	}
	var authErr *AuthError
	if errors.As(err, &authErr) {
		return false
	}
	var rlErr *RateLimitError
	if errors.As(err, &rlErr) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	var t Transient
	if errors.As(err, &t) {
		return t.IsTransient()
	}
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr)
}

// --- Generic retry ---

// RetryWithBackoff retries fn up to maxRetries times with jittered exponential
// backoff. Non-transient errors are returned immediately.
// Logging uses slog.Default() and cannot be overridden per-call; control output
// via slog.SetDefault().
func RetryWithBackoff[T any](ctx context.Context, maxRetries int, baseDelay time.Duration,
	label string, fn func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	var lastErr error
	backoff := baseDelay
	for attempt := range maxRetries {
		result, err := fn(ctx)
		if err == nil {
			if attempt > 0 {
				slog.Debug(label+" succeeded after retry", "attempts", attempt+1)
			}
			return result, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		if !IsTransient(err) {
			return zero, err
		}
		if attempt == maxRetries-1 {
			break
		}
		wait := JitteredBackoff(backoff)
		slog.Warn(label+" failed, retrying",
			"attempt", attempt+1, "max", maxRetries,
			"delay", wait.String(), "error", err)
		if err := SleepCtx(ctx, wait); err != nil {
			return zero, err
		}
		backoff = SafeDouble(backoff)
	}
	return zero, lastErr
}

// RetryOnRateLimit retries fn up to maxAttempts times when it returns a
// *RateLimitError. Non-rate-limit errors are returned immediately.
// The context is passed to fn on each attempt.
func RetryOnRateLimit(ctx context.Context, maxAttempts int, maxWait time.Duration, fn func(ctx context.Context) error) error {
	var lastErr error
	for attempt := range maxAttempts {
		lastErr = fn(ctx)
		if lastErr == nil {
			return nil
		}
		rlErr, ok := errors.AsType[*RateLimitError](lastErr)
		if !ok {
			return lastErr
		}
		if attempt == maxAttempts-1 {
			break
		}
		wait := maxWait
		if rlErr.RetryAfter > 0 {
			wait = min(rlErr.RetryAfter, maxWait)
		}
		if err := SleepCtx(ctx, wait); err != nil {
			return err
		}
	}
	return lastErr
}

// --- HTTP GET with retry (functional options) ---

// retryCfg holds internal configuration for a single Retry call.
type retryCfg struct {
	setHeaders   func(*http.Request)
	logger       *slog.Logger
	baseDelay    time.Duration
	maxBodyBytes int64
	maxAttempts  int
}

// Option configures a Retry call.
type Option func(*retryCfg)

// WithMaxAttempts sets the maximum number of attempts (including the first).
// Default: DefaultMaxAttempts (3).
func WithMaxAttempts(n int) Option {
	return func(c *retryCfg) { c.maxAttempts = n }
}

// WithBaseDelay sets the initial backoff delay. Default: DefaultBaseDelay (1s).
func WithBaseDelay(d time.Duration) Option {
	return func(c *retryCfg) { c.baseDelay = d }
}

// WithMaxBodyBytes sets the maximum response body size to read.
// Default: DefaultMaxBodyBytes (10 MB).
func WithMaxBodyBytes(n int64) Option {
	return func(c *retryCfg) { c.maxBodyBytes = n }
}

// WithHeaders sets a function that is called to set headers on each request.
func WithHeaders(fn func(*http.Request)) Option {
	return func(c *retryCfg) { c.setHeaders = fn }
}

// WithLogger sets the logger for retry diagnostics. Default: slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(c *retryCfg) { c.logger = l }
}

// Retry performs an HTTP GET with bounded exponential-backoff retry on
// 429 and 5xx responses. 4xx (non-429) and transport errors are returned
// immediately. Honors Retry-After (capped at RetryAfterCap).
func Retry(ctx context.Context, client *http.Client, reqURL string, opts ...Option) ([]byte, error) {
	cfg := retryCfg{
		maxAttempts:  DefaultMaxAttempts,
		baseDelay:    DefaultBaseDelay,
		maxBodyBytes: DefaultMaxBodyBytes,
	}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	if cfg.maxAttempts <= 0 {
		cfg.maxAttempts = DefaultMaxAttempts
	}
	if cfg.baseDelay <= 0 {
		cfg.baseDelay = DefaultBaseDelay
	}
	if cfg.maxBodyBytes <= 0 {
		cfg.maxBodyBytes = DefaultMaxBodyBytes
	}
	log := cfg.logger
	if log == nil {
		log = slog.Default()
	}

	start := time.Now()
	var lastErr error
	var overrideWait time.Duration
	backoff := cfg.baseDelay
	for attempt := range cfg.maxAttempts {
		if attempt > 0 {
			delay := overrideWait
			if delay <= 0 {
				delay = JitteredBackoff(backoff)
			}
			if err := SleepCtx(ctx, delay); err != nil {
				return nil, err
			}
			backoff = SafeDouble(backoff)
		}
		body, retryAfter, err := retryAttempt(ctx, client, reqURL, &cfg)
		if body != nil {
			if elapsed := time.Since(start); elapsed > 10*time.Second {
				log.Warn("slow upstream response", "url", reqURL, "duration", elapsed.Round(time.Millisecond))
			}
			return body, nil
		}
		if err != nil && !isRetryStatus(err) {
			return nil, err
		}
		lastErr = err
		overrideWait = retryAfter
		log.Debug("http request failed, will retry",
			"url", reqURL, "attempt", attempt+1, "max_attempts", cfg.maxAttempts, "error", err)
	}
	elapsed := time.Since(start)
	log.Warn("http retries exhausted",
		"url", reqURL, "attempts", cfg.maxAttempts, "elapsed", elapsed.Round(time.Millisecond), "error", lastErr)
	return nil, fmt.Errorf("retries exhausted after %s: %w", elapsed.Round(time.Millisecond), lastErr)
}

// retryAttempt performs a single HTTP GET attempt. Returns (body, 0, nil) on
// success, (nil, retryAfter, err) on retryable failure, or (nil, 0, err) on
// permanent failure.
func retryAttempt(ctx context.Context, client *http.Client, reqURL string, cfg *retryCfg) ([]byte, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, http.NoBody)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}
	if cfg.setHeaders != nil {
		cfg.setHeaders(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		if !IsTransient(err) {
			return nil, 0, err
		}
		return nil, 0, &retryableError{err: err}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		ra := ParseRetryAfter(resp.Header.Get("Retry-After"))
		Drain(resp.Body)
		resp.Body.Close()
		return nil, ra, &retryableError{err: &StatusError{Code: resp.StatusCode, URL: reqURL}}
	}
	if resp.StatusCode >= 500 {
		Drain(resp.Body)
		resp.Body.Close()
		return nil, 0, &retryableError{err: &StatusError{Code: resp.StatusCode, URL: reqURL}}
	}
	if resp.StatusCode != http.StatusOK {
		Drain(resp.Body)
		resp.Body.Close()
		return nil, 0, &StatusError{Code: resp.StatusCode, URL: reqURL}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, cfg.maxBodyBytes))
	resp.Body.Close()
	if err != nil {
		return nil, 0, fmt.Errorf("read response: %w", err)
	}
	return body, 0, nil
}

// retryableError is an internal marker for errors that should be retried.
type retryableError struct{ err error }

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

// isRetryStatus reports whether an error from retryAttempt is retryable.
func isRetryStatus(err error) bool {
	var re *retryableError
	return errors.As(err, &re)
}

// --- Body helpers ---

// Drain reads and discards up to 64 KB of a response body to enable
// HTTP connection reuse.
func Drain(body io.ReadCloser) {
	if _, err := io.CopyN(io.Discard, body, drainLimit); err != nil && !errors.Is(err, io.EOF) {
		slog.Debug("failed to drain response body", "error", err)
	}
}

// DrainClose reads remaining bytes (up to drainLimit) from rc before closing it.
func DrainClose(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, drainLimit))
	rc.Close()
}

// LimitedBody wraps resp.Body with an io.LimitReader capped at limit bytes,
// preserving the original Close method.
func LimitedBody(resp *http.Response, limit int64) io.ReadCloser {
	return &limitedReadCloser{
		Reader: io.LimitReader(resp.Body, limit),
		Closer: resp.Body,
	}
}

type limitedReadCloser struct {
	io.Reader
	io.Closer
}

// --- Redirect allowlist (functional options) ---

// redirectCfg holds internal configuration for the redirect policy.
type redirectCfg struct {
	allowedHosts    []string
	allowedSuffixes []string
	maxHops         int
}

// RedirectOption configures a redirect policy created by RedirectPolicyFunc.
type RedirectOption func(*redirectCfg)

// WithAllowedHosts sets the exact hostnames allowed as redirect targets.
func WithAllowedHosts(hosts ...string) RedirectOption {
	return func(c *redirectCfg) { c.allowedHosts = hosts }
}

// WithAllowedSuffixes sets the domain suffixes allowed (e.g. ".docker.com").
func WithAllowedSuffixes(suffixes ...string) RedirectOption {
	return func(c *redirectCfg) { c.allowedSuffixes = suffixes }
}

// WithMaxHops sets the maximum number of redirect hops. Default: 5.
func WithMaxHops(n int) RedirectOption {
	return func(c *redirectCfg) { c.maxHops = n }
}

// RedirectPolicyFunc returns a CheckRedirect function configured with the
// given options. With no options, all redirects are refused.
func RedirectPolicyFunc(opts ...RedirectOption) func(*http.Request, []*http.Request) error {
	cfg := redirectCfg{}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	if len(cfg.allowedHosts) == 0 && len(cfg.allowedSuffixes) == 0 {
		return func(_ *http.Request, _ []*http.Request) error {
			return errors.New("redirects not allowed")
		}
	}
	maxHops := cfg.maxHops
	if maxHops <= 0 {
		maxHops = redirectCap
	}
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxHops {
			return errors.New("too many redirects")
		}
		host := req.URL.Hostname()
		if slices.Contains(cfg.allowedHosts, host) {
			return nil
		}
		for _, s := range cfg.allowedSuffixes {
			if strings.HasSuffix(host, s) {
				return nil
			}
		}
		return fmt.Errorf("refusing redirect to %s", host)
	}
}

// DefaultRedirectPolicy is the default redirect policy: it denies cross-host
// redirects, allowing only redirects to the same host as the original request.
// For custom allowlists, use RedirectPolicyFunc.
func DefaultRedirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) >= redirectCap {
		return errors.New("too many redirects")
	}
	if len(via) == 0 {
		return nil
	}
	origHost := via[0].URL.Hostname()
	if req.URL.Hostname() == origHost {
		return nil
	}
	return fmt.Errorf("refusing redirect to %s", req.URL.Hostname())
}

// DockerGitHubRedirectPolicy is an OPTIONAL example redirect policy allowing
// docker.com and github.com hosts. Use it by assigning to Client.CheckRedirect
// or pass RedirectOption values to RedirectPolicyFunc for other allowlists.
func DockerGitHubRedirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) >= redirectCap {
		return errors.New("too many redirects")
	}
	host := req.URL.Hostname()
	switch {
	case host == "hub.docker.com",
		strings.HasSuffix(host, ".docker.com"),
		host == "github.com",
		strings.HasSuffix(host, ".github.com"),
		strings.HasSuffix(host, ".githubusercontent.com"):
		return nil
	default:
		return fmt.Errorf("refusing redirect to %s", host)
	}
}

// RedirectPolicy is a legacy alias for DockerGitHubRedirectPolicy, kept for
// backward compatibility. New code should use DefaultRedirectPolicy (same-host
// only, used by NewClient) or DockerGitHubRedirectPolicy explicitly.
//
// Deprecated: Use DefaultRedirectPolicy or DockerGitHubRedirectPolicy directly.
var RedirectPolicy = DockerGitHubRedirectPolicy

// --- Client helpers ---

// NewClient returns an *http.Client with the given timeout and the
// DefaultRedirectPolicy (same-host only). For custom redirect allowlists,
// configure CheckRedirect with RedirectPolicyFunc or assign
// DockerGitHubRedirectPolicy.
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: DefaultRedirectPolicy,
	}
}

// Close drains idle connections on the client's transport.
func Close(c *http.Client) {
	c.CloseIdleConnections()
}

// --- Secret redaction ---

// RedactTransportError unwraps *url.Error and redacts the secret from the
// error message. Returns nil for nil input.
func RedactTransportError(err error, prefix, secret string) error {
	if err == nil {
		return nil
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	var wrapped error
	if prefix == "" {
		wrapped = err
	} else {
		wrapped = fmt.Errorf("%s: %w", prefix, err)
	}
	if secret == "" {
		return wrapped
	}
	msg := wrapped.Error()
	if !strings.Contains(msg, secret) {
		return wrapped
	}
	return errors.New(strings.ReplaceAll(msg, secret, "REDACTED"))
}

// RedactSecret replaces occurrences of secret in err's message with "REDACTED".
func RedactSecret(err error, secret string) error {
	return RedactTransportError(err, "", secret)
}
