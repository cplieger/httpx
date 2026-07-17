// Package httpx provides a resilient outbound-HTTP toolkit: transient-error
// classification, generic typed retry (Do) and a bounded-bytes GET (GetBytes)
// over one jittered-exponential-backoff loop, a transparent retrying
// RoundTripper, Retry-After parsing, HTTP status mapping, secret redaction,
// body draining, custom-CA TLS transports, and a configurable redirect
// allowlist.
package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
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
// RetryAfter, when non-zero, is the RAW, UNCAPPED hint from the upstream's
// Retry-After header (populated via ParseRetryAfterResponse). The upstream
// controls this value; a hostile or misconfigured server can supply a very
// large duration (CWE-400 uncontrolled resource consumption). Callers that
// sleep on it directly MUST bound it first, e.g. min(err.RetryAfter, cap).
// Do's rate-limit modes (WithRateLimitRetry, WithRateLimitOnly) already do
// this (they cap at their maxWait argument). For a pre-capped value use
// ParseRetryAfter (bounded at RetryAfterCap = 60s).
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

// RetryAfterHint is implemented by errors that carry an explicit wait duration
// for the next retry, typically a parsed and capped Retry-After. When fn's
// returned error is transient AND implements this interface with a positive
// duration, Do waits that duration before the next attempt instead of its
// jittered exponential backoff. The exponential base still advances, so a
// later transient error without a hint resumes the normal progression. The
// hint MUST already be capped by the implementer (e.g. via ParseRetryAfter);
// Do sleeps on it verbatim and applies no ceiling of its own, so an uncapped
// value is an unbounded-wait hazard.
type RetryAfterHint interface {
	RetryAfterHint() time.Duration
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

// StatusError represents a non-2xx response with URL context. Used by GetBytes.
// Supports errors.Is matching against ErrRateLimited and ErrServerError.
type StatusError struct {
	URL  string
	Code int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("HTTP %d from %s", e.Code, redactURL(e.URL))
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

// ResponseTooLargeError is returned by GetBytes when the response body exceeds
// the configured maximum (WithMaxBodyBytes, default DefaultMaxBodyBytes). The
// body is not returned: a truncated payload indistinguishable from a complete
// one is a silent-corruption hazard, so GetBytes fails loud instead. Limit is
// the cap that was exceeded, mirroring the stdlib *http.MaxBytesError shape.
type ResponseTooLargeError struct {
	Limit int64
}

func (e *ResponseTooLargeError) Error() string {
	return fmt.Sprintf("response body exceeds %d bytes", e.Limit)
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

// --- Constants ---

const (
	// DefaultBaseDelay is the production base for exponential-backoff retry.
	DefaultBaseDelay = time.Second
	// DefaultMaxAttempts caps the retry doors at three total attempts.
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

// parseRetryAfterValue parses a Retry-After header value (delta-seconds or
// HTTP-date) into an uncapped, non-negative duration. Returns zero for
// missing, malformed, or past values. It is the shared core for both
// ParseRetryAfter (capped) and ParseRetryAfterResponse (uncapped).
func parseRetryAfterValue(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if n, err := strconv.ParseInt(h, 10, 64); err == nil {
		if n <= 0 {
			return 0
		}
		// int64 guard is correct on 32-bit platforms: ParseInt(...,10,64)
		// keeps parsing and the guard in int64 space, so a large delta-seconds
		// value is capped rather than (as strconv.Atoi did) failing with a
		// range error above the platform int max on GOARCH=386 and falling
		// through to HTTP-date parsing.
		const maxSecs = (1<<63 - 1) / int64(time.Second)
		if n > maxSecs {
			return time.Duration(maxSecs) * time.Second
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// ParseRetryAfter parses a Retry-After header value (delta-seconds or HTTP-date).
// Returns zero for missing/malformed values. Caps at RetryAfterCap for safety
// (prevents unbounded waits in retry loops). For raw uncapped values, use
// ParseRetryAfterResponse.
func ParseRetryAfter(h string) time.Duration {
	return min(parseRetryAfterValue(h), RetryAfterCap)
}

// ParseRetryAfterResponse parses the Retry-After header from an *http.Response.
// Returns zero if absent or unparseable. Does NOT cap — preserves the raw
// duration so callers (e.g., CheckHTTPStatus) can make their own decisions.
// For capped values suitable for retry loops, use ParseRetryAfter.
func ParseRetryAfterResponse(resp *http.Response) time.Duration {
	return parseRetryAfterValue(resp.Header.Get("Retry-After"))
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
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
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
	Drain(rc)
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

// ReadLimitedBody reads body up to limit bytes, always closes body, and returns
// the bytes read. It reads one byte past limit to detect an over-limit body and
// returns *ResponseTooLargeError (with nil bytes) rather than a silently
// truncated payload — a truncated body indistinguishable from a complete one is
// a corruption hazard. A limit of math.MaxInt64 means "effectively unlimited"
// and is guarded against probe-size overflow.
//
// It is the read-all-with-overflow-detection companion to LimitedBody (which
// only caps the stream and leaves reading and overflow handling to the caller),
// and is the same cap+1 read GetBytes applies internally — exposed for callers
// that issue their own request and decode outside GetBytes but still want the
// fail-loud size bound. On any error the body is already closed.
func ReadLimitedBody(body io.ReadCloser, limit int64) ([]byte, error) {
	defer body.Close()
	probe := limit
	if probe < math.MaxInt64 {
		probe++
	}
	data, err := io.ReadAll(io.LimitReader(body, probe))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, &ResponseTooLargeError{Limit: limit}
	}
	return data, nil
}

// --- Redirect allowlist (functional options) ---

// redirectCfg holds internal configuration for the redirect policy.
type redirectCfg struct {
	allowedHosts         []string
	allowedSuffixes      []string
	maxHops              int
	sameHost             bool
	allowSchemeDowngrade bool
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

// WithSameHost additionally allows a redirect whose target host equals the
// original request's host (ASCII case-insensitive, RFC 3986 §6.2.2.1), in
// addition to any WithAllowedHosts / WithAllowedSuffixes entries. It is the
// building block for a same-origin policy: combined with the default
// scheme-downgrade refusal (see WithAllowSchemeDowngrade), it follows a
// service's own same-host redirects (including an http->https upgrade) while
// refusing a cross-host hop that would forward a custom auth header to another
// origin. A policy built with only WithSameHost (no allowlisted hosts) permits
// exactly the same-host set.
func WithSameHost() RedirectOption {
	return func(c *redirectCfg) { c.sameHost = true }
}

// WithAllowSchemeDowngrade permits a redirect that downgrades the scheme
// (https on the original request -> http on the target). The default (false)
// refuses such a downgrade so a credential carried in a custom request header
// (which Go forwards across a redirect, stripping only Authorization/Cookie) is
// never sent over a cleartext hop. A scheme upgrade (http->https) is always
// allowed regardless of this setting. The downgrade is judged against the
// ORIGINAL request's scheme (via[0]).
func WithAllowSchemeDowngrade(allow bool) RedirectOption {
	return func(c *redirectCfg) { c.allowSchemeDowngrade = allow }
}

// asciiLower lowercases only ASCII letters A-Z, leaving every other byte
// unchanged. Host comparison in RFC 3986 §6.2.2.1 is ASCII case-insensitive;
// strings.ToLower must NOT be used here because it folds each invalid UTF-8
// byte to U+FFFD, collapsing distinct hosts (e.g. "\xfe" and "\xae") into one
// allowlist-matching equivalence class — a redirect-allowlist bypass. It also
// allocates only when an uppercase letter is present (hostnames are normally
// already lowercase), so the common path is zero-allocation.
func asciiLower(s string) string {
	var b []byte
	for i := range len(s) {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			if b == nil {
				b = []byte(s)
			}
			b[i] = c + ('a' - 'A')
		}
	}
	if b == nil {
		return s
	}
	return string(b)
}

// hostMatchesSuffix reports whether host matches the given dot-prefixed suffix.
// The suffix must start with ".". It matches if host equals the suffix without
// the leading dot, or if host ends with the suffix.
func hostMatchesSuffix(host, suffix string) bool {
	return host == suffix[1:] || strings.HasSuffix(host, suffix)
}

// RedirectPolicyFunc returns a CheckRedirect function configured with the given
// options. A redirect is followed only when its target host is allowed — an
// exact WithAllowedHosts entry, a WithAllowedSuffixes match, or (with
// WithSameHost) the original request's own host — and, unless
// WithAllowSchemeDowngrade is set, the redirect does not downgrade https->http.
// With no allowlist and no WithSameHost, all redirects are refused. The hop cap
// is WithMaxHops (default 5).
func RedirectPolicyFunc(opts ...RedirectOption) func(*http.Request, []*http.Request) error {
	cfg := redirectCfg{}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	if len(cfg.allowedHosts) == 0 && len(cfg.allowedSuffixes) == 0 && !cfg.sameHost {
		return func(_ *http.Request, _ []*http.Request) error {
			return errors.New("redirects not allowed")
		}
	}
	maxHops := cfg.maxHops
	if maxHops <= 0 {
		maxHops = redirectCap
	}
	// Hostnames are case-insensitive (RFC 3986 §6.2.2.1) and suffixes are
	// dot-anchored to prevent substring bypass; normalize once up front.
	rp := &resolvedRedirect{
		allowedHosts:   lowercaseAll(cfg.allowedHosts),
		suffixes:       normalizeSuffixes(cfg.allowedSuffixes),
		maxHops:        maxHops,
		sameHost:       cfg.sameHost,
		allowDowngrade: cfg.allowSchemeDowngrade,
	}
	return rp.check
}

// resolvedRedirect is a compiled redirect policy: RedirectPolicyFunc resolves
// its options into one of these once and returns its check method as the
// http.Client CheckRedirect.
type resolvedRedirect struct {
	allowedHosts   []string
	suffixes       []string
	maxHops        int
	sameHost       bool
	allowDowngrade bool
}

// check implements the CheckRedirect contract for a resolved policy: it caps
// hops, refuses a target that is neither allowlisted nor (with sameHost) the
// origin's own host, and refuses a scheme downgrade unless allowed.
func (rp *resolvedRedirect) check(req *http.Request, via []*http.Request) error {
	if len(via) >= rp.maxHops {
		return errors.New("too many redirects")
	}
	// via[0] is the original request; net/http always populates its URL, but
	// guard against a nil URL so the policy degrades gracefully rather than
	// panicking if invoked with a hand-built via chain.
	var origURL *url.URL
	if len(via) > 0 {
		origURL = via[0].URL
	}
	host := asciiLower(req.URL.Hostname())
	if !rp.targetAllowed(host, origURL) {
		return fmt.Errorf("refusing redirect to %s", host)
	}
	if !rp.allowDowngrade && origURL != nil && isSchemeDowngrade(origURL.Scheme, req.URL.Scheme) {
		return fmt.Errorf("refusing scheme downgrade to %s", host)
	}
	return nil
}

// targetAllowed reports whether host is an allowed redirect target: an exact or
// suffix allowlist match, or (with sameHost) the origin request's own host.
func (rp *resolvedRedirect) targetAllowed(host string, origURL *url.URL) bool {
	if redirectAllowed(host, rp.allowedHosts, rp.suffixes) {
		return true
	}
	return rp.sameHost && origURL != nil && host == asciiLower(origURL.Hostname())
}

// isSchemeDowngrade reports whether redirecting from scheme `from` to scheme
// `to` drops transport security (https -> http). Comparison is ASCII
// case-insensitive. A same-scheme redirect and an http->https upgrade are not
// downgrades.
func isSchemeDowngrade(from, to string) bool {
	return strings.EqualFold(from, "https") && strings.EqualFold(to, "http")
}

// normalizeSuffixes dot-anchors and lowercases each allowed redirect suffix so a
// bare "docker.com" cannot be bypassed by a substring match like "evildocker.com".
func normalizeSuffixes(suffixes []string) []string {
	out := make([]string, 0, len(suffixes))
	for _, s := range suffixes {
		s = strings.TrimSpace(s)
		if !strings.HasPrefix(s, ".") {
			s = "." + s
		}
		// Drop an empty or label-less suffix (""/"."/whitespace): it would
		// dot-anchor to a bare ".", which hostMatchesSuffix then matches
		// against any trailing-dot FQDN ("evil.com.") and the empty host --
		// a redirect-allowlist bypass. Dropping it fails closed: a policy
		// left with no hosts and no suffixes refuses every redirect.
		if len(s) <= 1 {
			continue
		}
		out = append(out, asciiLower(s))
	}
	return out
}

// lowercaseAll returns an ASCII-lowercased copy of in (RFC 3986 host comparison).
func lowercaseAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = asciiLower(s)
	}
	return out
}

// redirectAllowed reports whether host matches an exact allowed host or an
// allowed (dot-anchored, lowercased) suffix.
func redirectAllowed(host string, allowedHosts, normalizedSuffixes []string) bool {
	if slices.Contains(allowedHosts, host) {
		return true
	}
	for _, s := range normalizedSuffixes {
		if hostMatchesSuffix(host, s) {
			return true
		}
	}
	return false
}

// defaultRedirectPolicy is the compiled same-host policy DefaultRedirectPolicy
// delegates to, so the same-host + downgrade logic lives in exactly one place
// (resolvedRedirect.check) and cannot drift from
// RedirectPolicyFunc(WithSameHost()).
var defaultRedirectPolicy = RedirectPolicyFunc(WithSameHost())

// DefaultRedirectPolicy is the default redirect policy: it allows a redirect
// only to the same host as the original request, and refuses a same-host
// https->http scheme downgrade (which would forward a custom auth header onto a
// cleartext hop). A cross-host redirect is refused (Go forwards a custom header
// across a redirect, so it would leak) and an http->https upgrade is allowed.
// It delegates to RedirectPolicyFunc(WithSameHost()), with one addition: a call
// with an empty via chain (which net/http never produces — via always carries
// at least the original request) is allowed rather than refused. Use
// RedirectPolicyFunc for a custom allowlist, a higher hop cap (WithMaxHops), or
// to permit downgrades (WithAllowSchemeDowngrade).
func DefaultRedirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	return defaultRedirectPolicy(req, via)
}

// DockerGitHubRedirectPolicy is an OPTIONAL example redirect policy allowing
// docker.com and github.com hosts. Like every shipped policy it refuses an
// https->http scheme downgrade (judged against the original request's scheme,
// see WithAllowSchemeDowngrade), so a custom auth header never rides a
// cleartext hop even to an allowlisted host. Use it by assigning to
// Client.CheckRedirect or pass RedirectOption values to RedirectPolicyFunc for
// other allowlists.
func DockerGitHubRedirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) >= redirectCap {
		return errors.New("too many redirects")
	}
	host := asciiLower(req.URL.Hostname())
	switch {
	case host == "hub.docker.com",
		strings.HasSuffix(host, ".docker.com"),
		host == "github.com",
		strings.HasSuffix(host, ".github.com"),
		strings.HasSuffix(host, ".githubusercontent.com"):
		// Allowed host; still subject to the scheme-downgrade guard below.
	default:
		return fmt.Errorf("refusing redirect to %s", host)
	}
	if len(via) > 0 && via[0].URL != nil && isSchemeDowngrade(via[0].URL.Scheme, req.URL.Scheme) {
		return fmt.Errorf("refusing scheme downgrade to %s", host)
	}
	return nil
}

// RefuseAllRedirects is a CheckRedirect policy that follows NO redirect: it
// returns http.ErrUseLastResponse, so the client hands the caller the redirect
// response itself (status 3xx, body open, nil error) instead of the followed
// hop. It is the policy for a token-bearing client of an API that issues no
// redirects: Go's client forwards custom request headers (an X-Plex-Token, an
// X-Api-Key) across redirects — only Authorization, Cookie, and
// WWW-Authenticate are stripped, and only on a cross-domain hop — so a hostile
// 302 (MITM, DNS poisoning) would exfiltrate the credential to an
// attacker-chosen origin. With the hop refused, the credential never leaves
// the configured host and the unexpected 3xx surfaces to the caller's own
// status handling.
func RefuseAllRedirects(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// --- Client helpers ---

// CheckRedirect is the http.Client.CheckRedirect function shape. It is a type
// alias, so values are assignable in both directions; every shipped policy
// (DefaultRedirectPolicy, RefuseAllRedirects, DockerGitHubRedirectPolicy, and
// anything built with RedirectPolicyFunc) is a CheckRedirect.
type CheckRedirect = func(req *http.Request, via []*http.Request) error

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
	return errors.New(RedactSecretString(msg, secret))
}

// RedactSecret replaces occurrences of secret in err's message with "REDACTED".
func RedactSecret(err error, secret string) error {
	return RedactTransportError(err, "", secret)
}

// RedactSecretString replaces every occurrence of secret in s with "REDACTED"
// and returns the result. It is the string-level building block behind
// RedactSecret and RedactTransportError, exposed for callers that must redact a
// secret from a plain string — a captured HTTP response body destined for an
// error field or a log line — rather than from an error value. An empty secret
// is a no-op (s is returned unchanged), matching the error-shaped variants.
func RedactSecretString(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "REDACTED")
}

// redactURL returns a log-safe rendering of rawURL. It masks the userinfo
// password (like url.URL.Redacted, mirroring the go-retryablehttp CVE-2024-6104
// fix) and replaces every query value with "REDACTED" (query values commonly
// carry api keys, tokens, and signatures — the same default .NET 9's
// IHttpClientFactory adopted). Query keys, scheme, host, and path are kept for
// debugging; the fragment is dropped. Unparseable input yields a fixed
// placeholder rather than risk logging a raw secret-bearing string.
func redactURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "[unparseable url]"
	}
	if u.RawQuery != "" {
		q := u.Query()
		for k := range q {
			q[k] = []string{"REDACTED"}
		}
		u.RawQuery = q.Encode()
	}
	u.Fragment = ""
	u.RawFragment = ""
	return u.Redacted()
}

// LogSafeError returns an error whose message is safe to log. A transport
// *url.Error embeds the full request URL (with any userinfo/query secrets), so
// it is reduced to its underlying cause. Nil returns nil; *StatusError already
// renders a redacted URL via Error(), so it (and everything else) passes
// through unchanged — preserving errors.Is/As chains for callers.
//
// httpx applies this reduction to every transport error it logs or wraps; it
// is exported so a caller wrapping transport errors into its own messages can
// apply the same one (equivalent to RedactTransportError(err, "", "") — reach
// for that variant when a known secret must also be scrubbed from the text).
func LogSafeError(err error) error {
	if err == nil {
		return nil
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}
