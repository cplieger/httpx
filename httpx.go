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
// Returns zero for missing/malformed values. Caps at RetryAfterCap.
func ParseRetryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if n, err := strconv.Atoi(strings.TrimSpace(h)); err == nil {
		if n <= 0 {
			return 0
		}
		d := time.Duration(n) * time.Second
		return min(d, RetryAfterCap)
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return min(d, RetryAfterCap)
		}
	}
	return 0
}

// ParseRetryAfterResponse parses the Retry-After header from an *http.Response.
// Returns zero if absent or unparseable. Does NOT cap (preserves raw duration).
func ParseRetryAfterResponse(resp *http.Response) time.Duration {
	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		return 0
	}
	if secs, err := strconv.Atoi(ra); err == nil {
		if secs < 0 {
			return 0
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

// JitteredBackoff returns a duration in [backoff/2, backoff].
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
// network issues worth retrying. Auth, rate-limit, and context errors are
// never transient.
func IsTransient(err error) bool {
	if err == nil {
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
func RetryOnRateLimit(ctx context.Context, maxAttempts int, maxWait time.Duration, fn func() error) error {
	var lastErr error
	for attempt := range maxAttempts {
		lastErr = fn()
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

// --- HTTP GET with retry (from registry-stats) ---

// Options configures a single Retry call.
type Options struct {
	SetHeaders   func(*http.Request)
	BaseDelay    time.Duration
	MaxAttempts  int
	MaxBodyBytes int64
}

// Retry performs an HTTP GET with bounded exponential-backoff retry on
// 429 and 5xx responses. 4xx (non-429) and transport errors are returned
// immediately. Honors Retry-After (capped at RetryAfterCap).
func Retry(ctx context.Context, client *http.Client, reqURL string, opts Options) ([]byte, error) {
	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	baseDelay := opts.BaseDelay
	if baseDelay <= 0 {
		baseDelay = DefaultBaseDelay
	}
	maxBody := opts.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = DefaultMaxBodyBytes
	}

	start := time.Now()
	var lastErr error
	var overrideWait time.Duration
	for attempt := range maxAttempts {
		if attempt > 0 {
			delay := overrideWait
			if delay <= 0 {
				delay = time.Duration(1<<attempt)*baseDelay +
					time.Duration(rand.IntN(500))*time.Millisecond //nolint:gosec // G404: backoff jitter
			}
			overrideWait = 0
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, http.NoBody)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		if opts.SetHeaders != nil {
			opts.SetHeaders(req)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			slog.Debug("http request failed, will retry",
				"url", reqURL, "attempt", attempt+1, "max_attempts", maxAttempts, "error", err)
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			overrideWait = ParseRetryAfter(resp.Header.Get("Retry-After"))
			slog.Debug("rate limited by upstream",
				"url", reqURL, "attempt", attempt+1, "retry_after", overrideWait)
			Drain(resp.Body)
			resp.Body.Close()
			lastErr = &StatusError{Code: resp.StatusCode, URL: reqURL}
			continue
		}
		if resp.StatusCode >= 500 {
			Drain(resp.Body)
			resp.Body.Close()
			lastErr = &StatusError{Code: resp.StatusCode, URL: reqURL}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			Drain(resp.Body)
			resp.Body.Close()
			return nil, &StatusError{Code: resp.StatusCode, URL: reqURL}
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			slog.Warn("slow upstream response", "url", reqURL, "duration", elapsed.Round(time.Millisecond))
		}
		return body, nil
	}
	elapsed := time.Since(start)
	slog.Warn("http retries exhausted",
		"url", reqURL, "attempts", maxAttempts, "elapsed", elapsed.Round(time.Millisecond), "error", lastErr)
	return nil, fmt.Errorf("retries exhausted after %s: %w", elapsed.Round(time.Millisecond), lastErr)
}

// --- Body drain ---

// Drain reads and discards up to 64 KB of a response body to enable
// HTTP connection reuse.
func Drain(body io.ReadCloser) {
	if _, err := io.CopyN(io.Discard, body, drainLimit); err != nil && !errors.Is(err, io.EOF) {
		slog.Debug("failed to drain response body", "error", err)
	}
}

// DrainClose reads remaining bytes (up to 4 KB) from rc before closing it.
func DrainClose(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 4096))
	rc.Close()
}

// --- Redirect allowlist ---

// RedirectConfig holds the configurable redirect policy settings.
type RedirectConfig struct {
	// AllowedHosts is the set of exact hostnames allowed as redirect targets.
	AllowedHosts []string
	// AllowedSuffixes is the set of domain suffixes allowed (e.g. ".docker.com").
	AllowedSuffixes []string
	// MaxHops is the maximum number of redirect hops (default: 5).
	MaxHops int
}

// RedirectPolicyFunc returns a CheckRedirect function configured with the
// given allowlist. If cfg is nil, all redirects are refused.
func RedirectPolicyFunc(cfg *RedirectConfig) func(*http.Request, []*http.Request) error {
	if cfg == nil {
		return func(_ *http.Request, _ []*http.Request) error {
			return errors.New("redirects not allowed")
		}
	}
	maxHops := cfg.MaxHops
	if maxHops <= 0 {
		maxHops = redirectCap
	}
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxHops {
			return errors.New("too many redirects")
		}
		host := req.URL.Hostname()
		for _, h := range cfg.AllowedHosts {
			if host == h {
				return nil
			}
		}
		for _, s := range cfg.AllowedSuffixes {
			if strings.HasSuffix(host, s) {
				return nil
			}
		}
		return fmt.Errorf("refusing redirect to %s", host)
	}
}

// RedirectPolicy is a default redirect policy allowing docker.com and github.com.
// For custom allowlists, use RedirectPolicyFunc.
func RedirectPolicy(req *http.Request, via []*http.Request) error {
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

// --- Client helpers ---

// NewClient returns an *http.Client with the given timeout and the default
// RedirectPolicy. For custom redirect allowlists, configure CheckRedirect
// with RedirectPolicyFunc.
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: RedirectPolicy,
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
