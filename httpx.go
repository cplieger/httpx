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

// Transient is the interface an error implements to report whether it
// represents a transient (retryable) failure. It embeds error because every
// value this interface can ever name IS one: it is read through
// [errors.As]/[errors.AsType] over an error tree, whose nodes are errors by
// construction, so a non-error implementation is unreachable. Declaring the
// error half makes the type say that, keeps Error() available on a recovered
// value for diagnostics, and is what lets a caller write
// errors.AsType[Transient](err) — [errors.AsType] constrains its type
// parameter to error, where the older [errors.As] accepted any target. Same
// shape as [net.Error].
type Transient interface {
	error
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
//
// It embeds error for the same reason [Transient] does: the interface is read
// through [errors.As]/[errors.AsType] over an error tree, so it can only ever
// name an error.
type RetryAfterHint interface {
	error
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
	_, ok := errors.AsType[*PermanentError](err)
	return ok
}

// --- Transient marking ---

// transientError wraps an error to declare it retryable. It is unexported
// because the mark is read through the [Transient] interface (and so through
// [IsTransient]) rather than by type: a consumer asking "is this retryable"
// must get the same answer for a marked error and for its own Transient
// implementation.
type transientError struct{ Err error }

var _ Transient = (*transientError)(nil)

func (e *transientError) Error() string { return e.Err.Error() }
func (e *transientError) Unwrap() error { return e.Err }

// IsTransient implements Transient: the caller marked this error retryable.
func (e *transientError) IsTransient() bool { return true }

// MarkTransient wraps err in a value satisfying the [Transient] interface with
// a true verdict, so the retry doors treat it as a transient failure. Returns
// nil for nil input. It is the mirror of [Permanent], which forces the opposite
// verdict; the name is not the bare adjective only because [Transient] is the
// interface it satisfies.
//
// It exists so a caller widening the retryable set for its own operation does
// not have to declare a one-method wrapper type to do it. That wrapper is
// pure boilerplate — an Error, an Unwrap and an IsTransient returning true —
// and hand-rolling it invites the two mistakes this function cannot make:
// forgetting Unwrap (which hides the cause from every errors.Is and errors.As
// the caller's own callers run) and marking a nil error.
//
// The mark is the OUTERMOST verdict, so it overrides a non-transient verdict
// already on the error: MarkTransient(&HTTPStatusError{Code: 500}) is
// retryable even though a plain 500 is not. Use it for a failure your
// operation knows is self-healing where the shared policy cannot know — a
// server-side fault delivered inside a 200 envelope, for example, which no
// status-based classification can see:
//
//	if env := serverFault(body); env != nil {
//		return httpx.MarkTransient(env) // this clears; spend another attempt
//	}
//
// [IsTransient]'s standing rejections still outrank the mark, because they
// answer questions the caller is not the authority on:
//
//   - [Permanent] wins. Permanent(MarkTransient(err)) is not retried.
//   - An *[AuthError] and a *[RateLimitError] stay terminal. Credentials do not
//     fix themselves, and a rate limit is retried by naming a wait budget
//     ([WithRateLimitRetry], [WithRateLimitOnly]), not by claiming transience.
//   - A caller-context error stays terminal: the budget that authorized the
//     work is gone, so another attempt has nothing to spend. For a timeout that
//     bounded ONE attempt and therefore carries a deadline that is not the
//     caller's, use [AttemptTimeout] — [IsTransient] consults that mark ahead of
//     the context rejection, which is the whole reason it is a separate mark.
func MarkTransient(err error) error {
	if err == nil {
		return nil
	}
	return &transientError{Err: err}
}

// --- AttemptTimeout ---

// attemptTimeoutError marks a timeout as the expiry of a bound that governed
// ONE attempt. It WRAPS the error rather than replacing it, so both things a
// caller needs survive the mark: the cause that says which phase stalled (a
// dial, the TLS handshake, the response headers) and the
// errors.Is(err, context.DeadlineExceeded) match the caller's own callers test
// for. That match is exactly why IsTransient has to consult this mark before
// its caller-context rejection — the marked cause typically CARRIES the
// sentinel, which the rejection reads as terminal.
type attemptTimeoutError struct{ Err error }

func (e *attemptTimeoutError) Error() string { return "attempt timeout: " + e.Err.Error() }
func (e *attemptTimeoutError) Unwrap() error { return e.Err }

// IsTransient implements Transient: a bound that governed a single attempt
// expired, which is a retryable failure by construction.
func (e *attemptTimeoutError) IsTransient() bool { return true }

// AttemptTimeout wraps err to declare that the timeout it reports bounded ONE
// ATTEMPT rather than the caller's total budget, making it retryable:
// [IsTransient] reports true for it (checked ahead of the context-error
// rejection), and the retry doors retry it. Returns nil for nil input. It is
// the mirror of [Permanent], which forces the opposite verdict.
//
// It exists because a context deadline alone cannot express the difference. A
// caller's expired deadline means "the budget is gone, stop"; the expiry of a
// per-attempt bound means "this attempt failed, try another" — opposite
// instructions from the same [context.DeadlineExceeded]. Only the code that
// installed the bound knows which one it is, so httpx cannot infer it and
// defaults to the safe reading (terminal). This mark is how the installer says
// otherwise, and the wrapper deliberately keeps the deadline visible to
// errors.Is instead of hiding it, which was the only way to opt in before.
//
// Reach for it when the per-attempt bound is NOT the retry door's own and
// httpx cannot see whose bound it was:
//
//   - a [context.WithTimeout] a caller derives itself inside a [Do] callback.
//     Its expiry carries the deadline sentinel, indistinguishable from the
//     caller's own. [WithAttemptTimeout] does this for you, mark included;
//     hand-roll it only when the bound must cover something other than one
//     whole attempt.
//   - a net-level bound such as a [net.Dialer] Timeout. The net package maps
//     every expired context onto one shared "i/o timeout" value, so a
//     *net.OpError or *net.DNSError reporting a deadline cannot say whose it
//     was and stays terminal (see IsTransient).
//
// An [http.Client.Timeout] and a [net/http.Transport] ResponseHeaderTimeout do
// NOT need the mark: net/http reports those through its own timeout error,
// which never carries the sentinel, so [IsTransient] already reads them as
// per-attempt and retries them — including under the [RetryRoundTripper]'s
// default policy. Use [Permanent] (or a [TransportConfig].CheckRetry) if you
// want one of them to stop the loop instead.
//
// The caller owns the judgment: mark ONLY a bound you installed over a single
// attempt. Marking a deadline that came from your own caller makes an
// exhausted budget look retryable, which is the failure mode the default
// classification prevents. [WithAttemptTimeout] avoids the judgment entirely by
// deciding from the two contexts it owns.
func AttemptTimeout(err error) error {
	if err == nil {
		return nil
	}
	return &attemptTimeoutError{Err: err}
}

// IsAttemptTimeout reports whether err (or any wrapped error) was marked by
// [AttemptTimeout] as a per-attempt timeout.
func IsAttemptTimeout(err error) bool {
	_, ok := errors.AsType[*attemptTimeoutError](err)
	return ok
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

// CheckHTTPStatus classifies an HTTP response, mapping anything that is not a
// success to a typed error. Success is EXACTLY 2xx: a status in [200, 300)
// returns nil and EVERY other status returns an error — 401/403 →
// *AuthError, 429 → *RateLimitError, and everything else (a 3xx, any other
// 4xx, any 5xx, and an informational 1xx) → *HTTPStatusError carrying the
// code.
//
// The 2xx-only window is a v4 change; v3 and earlier returned nil for the
// whole 200-399 band. A 3xx reaches a caller only when the client is
// configured NOT to follow redirects — [RefuseAllRedirects], or any
// CheckRedirect returning [http.ErrUseLastResponse], which net/http hands back
// as the 3xx response itself with a nil error. Under the old window that
// redirect stub classified as SUCCESS, so a token-bearing client that
// deliberately refuses the hop then treated the unfollowed redirect as a
// completed request; this is the "caller's own status handling"
// [RefuseAllRedirects] delegates to, and it now reports the 3xx as the failure
// it is. A caller that pairs a non-following policy with its own hand-rolled
// 2xx band check no longer needs it.
//
// A 3xx is deliberately a *HTTPStatusError rather than a new error type or a
// second classifier, so it flows through the existing plumbing unchanged:
// [IsTransient] reports false (only 502/503/504 are transient),
// [HTTPStatusError.IsServerError] and [HTTPStatusError.IsClientError] both
// report false (a 3xx is neither ≥ 500 nor in [400, 500)), and [LogSafeError]
// and the redaction helpers pass it through untouched (it embeds no URL).
func CheckHTTPStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
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
	return &HTTPStatusError{Code: resp.StatusCode}
}

// IsRetryableStatus reports whether a response status is one the retry loop
// treats as a transient failure worth another attempt: 408 Request Timeout,
// 429 Too Many Requests, and any 5xx (a code >= 500). Everything else — every
// 2xx, every 3xx, an informational 1xx, and every other 4xx — is a settled
// answer this package does not repeat.
//
// It is the SAME rule the built-in retry uses, not a restatement of it:
// [GetBytes]'s own attempt function calls this function to classify a
// response, so a caller's verdict and the door's verdict cannot disagree.
//
// It exists for the caller that runs [GetBytes] under WithMaxAttempts(1)
// inside its own outer retry budget (the sanctioned way to avoid multiplying
// the two attempt counts). That caller receives the door's *[StatusError] and
// owns the retry decision — GetBytes deliberately does not mark its exhaustion
// error [Transient], because whether an exhausted GET is worth another outer
// attempt is the caller's policy — so it needs to ask the question the door
// would have asked:
//
//	if se, ok := errors.AsType[*httpx.StatusError](err); ok && httpx.IsRetryableStatus(se.Code) {
//		return httpx.MarkTransient(err) // spend another of MY attempts
//	}
//
// 408 is included because it is the server reporting that IT gave up waiting,
// which self-heals and is safe to repeat on an idempotent GET. 429 is here
// because this door retries a rate limit by default; [Do] does not (its
// *[RateLimitError] is retryable only under [WithRateLimitRetry] or
// [WithRateLimitOnly]), so pair this predicate with the door whose policy it
// describes.
//
// The [RetryRoundTripper] is the one deliberate divergence: its default policy
// retries 429/502/503/504 only, a narrower set than this, so a plain 500 that
// this predicate calls retryable is not retried by the transport. Widen the
// transport with [TransportConfig].CheckRetry (this predicate is a ready-made
// one) rather than assuming the two agree.
func IsRetryableStatus(code int) bool {
	return code == http.StatusRequestTimeout ||
		code == http.StatusTooManyRequests ||
		code >= 500
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
	// rand.N is generic over any integer kind, so the draw happens in
	// time.Duration itself rather than round-tripping through int64. half is at
	// most MaxInt64/2, so half+1 cannot overflow.
	half := backoff / 2
	return half + rand.N(half+1) //nolint:gosec // G404: jitter, not crypto
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

// ContextWithDefaultTimeout bounds ctx by def ONLY when ctx carries no
// deadline; a deadline the caller already set is the authoritative budget
// and is never undercut (or extended). A def <= 0 means "no default" and
// leaves ctx unbounded. The returned cancel is always non-nil and must be
// called (a no-op on the passthrough paths), matching context.WithTimeout's
// contract.
//
// It is the per-request timeout rule for an API client: per-attempt bounds
// live on the transport (ResponseHeaderTimeout), the total budget is the
// caller's ctx, and this default applies only when the caller brought no
// budget of its own. Extracted from the identical requestContext helpers in
// the plexapi and arrapi client libraries.
func ContextWithDefaultTimeout(ctx context.Context, def time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok || def <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, def)
}

// --- Transient classification ---

// IsTransient returns true for errors likely caused by temporary server or
// network issues worth retrying. Auth, rate-limit, permanent, and
// caller-context errors are never transient.
//
// "Caller-context error" is decided by the sentinel actually being in the
// error's unwrap chain, not by [errors.Is] — net/http's timeout errors match
// [context.DeadlineExceeded] without carrying it, so an [http.Client.Timeout]
// and a transport ResponseHeaderTimeout are per-ATTEMPT bounds and ARE
// transient (see isCallerContextError). A caller's own expired deadline stays
// terminal; [AttemptTimeout] marks a bound whose expiry this package cannot
// otherwise tell apart from one.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	if IsPermanent(err) {
		return false
	}
	if _, ok := errors.AsType[*AuthError](err); ok {
		return false
	}
	if _, ok := errors.AsType[*RateLimitError](err); ok {
		return false
	}
	// Ordered ahead of the context-error rejection on purpose: the mark exists
	// on errors that DO carry context.DeadlineExceeded (the wrapper keeps the
	// deadline visible to a caller's errors.Is), so the rejection below would
	// otherwise decide first and no mark could ever be seen. Permanent still
	// outranks it above — Permanent(AttemptTimeout(err)) is not retried.
	if IsAttemptTimeout(err) {
		return true
	}
	// A context error means the CALLER's budget is gone: terminal. Only code
	// holding both contexts can tell a caller's deadline from a per-attempt
	// one, which is why nothing here tries to infer it — AttemptTimeout above
	// is how the code that installed the bound says so.
	if isCallerContextError(err) {
		return false
	}
	if t, ok := errors.AsType[Transient](err); ok {
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
	_, isDNS := errors.AsType[*net.DNSError](err)
	return isDNS
}

// isCallerContextError reports whether err says the CALLER's context expired
// or was canceled, which is terminal: the budget that authorized the work is
// gone, so another attempt has nothing to spend.
//
// The deadline test is Unwrap-chain IDENTITY, not [errors.Is], because
// errors.Is is unsound for this question. An error can declare
// Is(context.DeadlineExceeded) == true without the sentinel ever appearing in
// its chain, and net/http's own timeout error does exactly that
// (net/http.timeoutError.Is compares the target to context.DeadlineExceeded
// and returns true; the value it reports is built from a message string and
// unwraps to nothing). errors.Is therefore folded the two bounds net/http
// installs ITSELF — an [http.Client.Timeout] and a [net/http.Transport]
// ResponseHeaderTimeout, each of which governs ONE attempt — in with caller
// budget expiry and classified them terminal, so neither was ever retried
// despite being the per-attempt bound this package tells callers to use.
// Identity separates them: a real context expiry carries the sentinel VALUE,
// net/http's timeout only claims to match it.
//
// Two carriers keep the old errors.Is verdict deliberately:
//
//   - Cancellation. [context.Canceled] is tested with errors.Is because the one
//     stdlib type that matches it without being it (net.canceledError, "operation
//     was canceled") is produced ONLY by mapping a genuinely canceled context, so
//     the match is faithful and identity would lose it.
//   - A net-package error reporting a deadline. The net package maps EVERY
//     expired context onto one shared value ("i/o timeout") that likewise
//     matches the deadline without carrying it, so a *net.OpError or
//     *net.DNSError reporting a deadline cannot say whose deadline it was: the
//     caller's, or a net.Dialer.Timeout the transport installed. Unknowable
//     stays terminal — mark it with [AttemptTimeout] to opt a bound you own
//     back in. (Through an *http.Client this case does not arise for a caller
//     deadline: net/http prefers the request context's own error over the dial
//     error whenever that context is done, so the sentinel is present.)
//
// The final line stays on [errors.As] rather than [errors.AsType]: it asks one
// question about two types, and AsType cannot be an expression, so the rewrite
// would split that single boolean fact into two statements and an early return.
// Neither binding is read, so nothing is gained by paying for it.
func isCallerContextError(err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if unwrapsTo(err, context.DeadlineExceeded) {
		return true
	}
	var opErr *net.OpError
	var dnsErr *net.DNSError
	return errors.As(err, &opErr) || errors.As(err, &dnsErr)
}

// unwrapsTo reports whether target appears in err's unwrap tree BY IDENTITY,
// consulting no Is method. It is the "is this actually that value" test
// [errors.Is] cannot express once any error in the chain answers Is for a
// value it does not hold (see isCallerContextError).
func unwrapsTo(err, target error) bool {
	for err != nil {
		if err == target { //nolint:errorlint // identity is the whole point: errors.Is would consult Is methods.
			return true
		}
		switch x := err.(type) {
		case interface{ Unwrap() error }:
			err = x.Unwrap()
		case interface{ Unwrap() []error }:
			return slices.ContainsFunc(x.Unwrap(), func(e error) bool { return unwrapsTo(e, target) })
		default:
			return false
		}
	}
	return false
}

// --- Body helpers ---

// Drain reads and discards up to 64 KB of a response body to enable
// HTTP connection reuse. A failed drain forfeits only that reuse, and is
// reported as a bare Debug line on slog.Default(). The read error itself is
// deliberately NOT logged.
//
// That omission is a CWE-532 fix, not an oversight. A body-read error's text is
// written by the FAR END: net/http renders a malformed chunked trailer as
//
//	malformed MIME header: missing colon: "<remote bytes>"
//
// and for a consumer whose request URL carries its credential in the PATH (a
// webhook token is the canonical shape) an edge that echoes the request URI
// puts that credential in those bytes. Logging it needs only the consumer to be
// running at Debug — the level an operator raises precisely while diagnosing
// failing deliveries. Three properties make the site uncloseable from anywhere
// else, which is why the value is dropped HERE rather than by the caller:
//
//   - Nobody can route it. Drain takes no logger (it is called from defer at
//     consumer call sites and from four sites inside this package, one of them
//     a RoundTripper that has no logger at all), and the retry doors'
//     [WithLogger] governs their own loop lines, not slog.Default().
//   - [LogSafeError] does not reduce it. That boundary is TYPE-based: it strips
//     the *url.Error envelope this package's own machinery adds. A body-read
//     error is not a *url.Error — a malformed trailer surfaces as
//     textproto.ProtocolError, whose entire value IS the message — so the
//     reduction returns it byte-identically.
//   - Redaction needs a secret to redact, and Drain is handed none.
//
// No diagnosis a caller can act on is lost. Drain runs only where the body is
// being thrown away, so the response's own outcome is already reported by the
// path that discarded it (CheckHTTPStatus's typed error, the retry lines,
// ReadLimitedBody's error); a drain error is never the only signal of a
// failure, and no caller can repair a drain. This is the same call the package
// makes in urlErrorCause, which substitutes a contentless stand-in rather than
// log the one field it holds.
func Drain(body io.ReadCloser) {
	if _, err := io.CopyN(io.Discard, body, drainLimit); err != nil && !errors.Is(err, io.EOF) {
		slog.Debug("failed to drain response body")
	}
}

// DrainClose reads remaining bytes (up to drainLimit) from rc before closing it.
// It inherits [Drain]'s logging contract: a failed drain logs a bare Debug line
// carrying no remote-authored text.
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
	preserveMethod       bool
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

// WithSameHost permits a redirect whose target host equals the original
// request's host (ASCII case-insensitive, RFC 3986 §6.2.2.1), in addition to
// any WithAllowedHosts / WithAllowedSuffixes entries. The default (false)
// matches leaving the option out: only allowlisted targets are followed.
// WithSameHost(true) is the building block for a same-origin policy: combined
// with the default scheme-downgrade refusal (see WithAllowSchemeDowngrade),
// it follows a service's own same-host redirects (including an http->https
// upgrade) while refusing a cross-host hop that would forward a custom auth
// header to another origin. A policy built with only WithSameHost(true) (no
// allowlisted hosts) permits exactly the same-host set.
//
// Like every RedirectOption, later values overwrite earlier ones: appending
// WithSameHost(false) to an option slice that already carries
// WithSameHost(true) turns the permission back off.
func WithSameHost(same bool) RedirectOption {
	return func(c *redirectCfg) { c.sameHost = same }
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

// WithPreserveMethod REFUSES a redirect hop that would change the request
// method, rather than rewriting the method back. The default (false) matches
// leaving the option out: a method-changing hop is followed as net/http
// rewrites it. net/http downgrades a POST (or PUT/PATCH/DELETE) to a GET
// across a 301, 302, or 303 and drops the body, per RFC 9110 §15.4 and Go's
// issue 18570 compatibility rule; only a 307 or 308 carries the method and
// body forward. For an API call whose meaning IS its method, that silent
// downgrade turns a write into a read against a URL the caller never named,
// so WithPreserveMethod(true) makes the client stop instead.
//
// A refused hop returns [http.ErrUseLastResponse], not an error: net/http then
// hands the caller the 3xx response itself (status, Location header, body open,
// nil error), exactly as [RefuseAllRedirects] does, and the surfaced 3xx is an
// error under [CheckHTTPStatus]. The method is never rewritten back to the
// original — the request is simply not re-sent.
//
// The comparison is against the ORIGINAL request (via[0]), so a chain that
// preserves the method once and changes it later is still refused: a POST
// through a 307 (method kept) followed by a 302 (method downgraded to GET) is
// refused at the second hop. A same-method hop (a GET chain, or a POST through
// a 307/308) is unaffected and still subject to the hop cap, the target
// allowlist, and the scheme-downgrade rule, which all take precedence — a
// cross-host or downgrading hop is refused as a hard error even when the
// method also changes. When via is empty (net/http never does this; a
// hand-built chain can) the original method is unknowable, so the hop is
// refused too: the option fails closed.
//
// It only ever narrows what a policy follows, so it grants nothing on its own:
// [RedirectPolicyFunc] with WithPreserveMethod(true) and no allowlist and no
// [WithSameHost] entry still refuses every redirect.
func WithPreserveMethod(preserve bool) RedirectOption {
	return func(c *redirectCfg) { c.preserveMethod = preserve }
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
// WithSameHost(true)) the original request's own host — and, unless
// WithAllowSchemeDowngrade is set, the redirect does not downgrade https->http.
// With no allowlist and no WithSameHost(true), all redirects are refused. The
// hop cap is WithMaxHops (default 5). WithPreserveMethod(true) additionally
// refuses (via http.ErrUseLastResponse, so the 3xx surfaces to the caller) a
// hop that would change the request method.
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
		preserveMethod: cfg.preserveMethod,
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
	preserveMethod bool
}

// check implements the CheckRedirect contract for a resolved policy: it caps
// hops, refuses a target that is neither allowlisted nor (with sameHost) the
// origin's own host, refuses a scheme downgrade unless allowed, and (with
// preserveMethod) refuses a method-changing hop.
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
	// Ordered last on purpose: the refusals above are the stronger security
	// decisions and must keep precedence, because they fail the request with a
	// hard error while this one returns ErrUseLastResponse (a nil-error 3xx the
	// caller must classify). A hop that both leaves the allowlist and changes
	// the method is refused as the allowlist violation it is.
	if rp.preserveMethod && methodChanged(req, via) {
		return http.ErrUseLastResponse
	}
	return nil
}

// methodChanged reports whether req's method differs from the ORIGINAL
// request's (via[0]) — the comparison that also catches a chain whose method
// survives an early 307 and is downgraded by a later 302. An empty Method is
// normalized to GET, which is how net/http itself reads it (redirectBehavior
// rewrites a "" method to "GET" on a 301/302/303 hop), so a hand-built
// zero-value origin request is not a spurious mismatch. An empty via chain has
// no original method to compare against and reports true: the caller of this
// helper fails closed rather than vouching for a hop it cannot verify.
func methodChanged(req *http.Request, via []*http.Request) bool {
	if len(via) == 0 {
		return true
	}
	return effectiveMethod(req.Method) != effectiveMethod(via[0].Method)
}

// effectiveMethod returns the method net/http would use for a request carrying
// method: an empty Request.Method means GET.
func effectiveMethod(method string) string {
	if method == "" {
		return http.MethodGet
	}
	return method
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
// RedirectPolicyFunc(WithSameHost(true)).
var defaultRedirectPolicy = RedirectPolicyFunc(WithSameHost(true))

// DefaultRedirectPolicy is the default redirect policy: it allows a redirect
// only to the same host as the original request, and refuses a same-host
// https->http scheme downgrade (which would forward a custom auth header onto a
// cleartext hop). A cross-host redirect is refused (Go forwards a custom header
// across a redirect, so it would leak) and an http->https upgrade is allowed.
// It delegates to RedirectPolicyFunc(WithSameHost(true)), with one addition: a
// call with an empty via chain (which net/http never produces — via always
// carries at least the original request) is allowed rather than refused. Use
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
// status handling — which is [CheckHTTPStatus]: since v4 it classifies a 3xx
// as an error (*HTTPStatusError), so a surfaced redirect stub is reported as
// the failure it is instead of passing as success.
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

// --- Secret redaction ---

// Secret is a credential value being redacted: the needle the redaction
// helpers scan FOR, never the text they scan. It is a distinct type so the
// value-to-hide and the text-to-scan cannot be transposed at a call site — a
// reversed call turns the redactor into a leak, scanning the credential for
// occurrences of the log text and returning the credential untouched. With
// the positions typed apart, a plain string variable in the secret position
// is a compile error; the caller converts exactly the value that is the
// credential (httpx.Secret(token)) and nothing else fits there.
//
// Convert at the call boundary, never store one: Secret carries no logging
// protection of its own — formatting a Secret with %v/%s or logging it via
// slog prints the credential verbatim. The zero value ("") disables
// redaction: every helper returns its input unredacted when the secret is
// empty, because there is nothing to scan for. Callers redacting a
// possibly-absent credential must treat empty as "no redaction happened",
// not as "redaction succeeded".
type Secret string

// RedactTransportError unwraps *url.Error and redacts the secret from the
// error message. Returns nil for nil input, and never nil for a non-nil one
// (see urlErrorCause for the *url.Error that carries no cause of its own).
// The replacement runs through RedactSecretString, whose doc comment carries the
// ordering and representation rules for composing redaction with a normalizing
// transform or a byte cap. An empty secret disables redaction: the (possibly
// prefixed) error is returned with its message untouched, since there is
// nothing to scan for.
func RedactTransportError(err error, prefix string, secret Secret) error {
	if err == nil {
		return nil
	}
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		err = urlErrorCause(urlErr)
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
	if !strings.Contains(msg, string(secret)) {
		return wrapped
	}
	return errors.New(RedactSecretString(msg, secret))
}

// RedactSecret replaces occurrences of secret in err's message with "REDACTED".
// It funnels into RedactSecretString, whose doc comment carries the ordering and
// representation rules the caller owns.
func RedactSecret(err error, secret string) error {
	return RedactTransportError(err, "", Secret(secret))
}

// RedactSecretString replaces every occurrence of secret in s with "REDACTED"
// and returns the result. It is the string-level building block behind
// RedactSecret and RedactTransportError, exposed for callers that must redact a
// secret from a plain string — a captured HTTP response body destined for an
// error field or a log line — rather than from an error value. An empty secret
// is a no-op (s is returned unchanged), matching the error-shaped variants.
//
// The replacement is literal and byte-exact, which puts three obligations on the
// caller:
//
//  1. Hold the needle in the SAME representation the haystack carries. A decoded
//     token does not match a JSON-escaped or percent-encoded haystack, and the
//     mismatch is silent: nothing is replaced and no error is reported.
//  2. Call this on BOTH sides of any normalizing transform (runesafe
//     Sanitize/SanitizeSingleLine, unicode/norm NFC/NFKC, strconv.Unquote,
//     JSON/URL/HTML unescape, case folding, whitespace collapse), passing the
//     needle in the representation each side carries. A call placed only before
//     the transform misses a secret the transform CONSTRUCTS out of text the
//     needle never matched; a call placed only after it misses a secret the
//     transform REWRITES, leaving a near-complete fragment of the value.
//  3. Put any byte cap (ReadLimitedBody, a bounded sanitizer preset, a plain
//     truncation) LAST. A cut that splits the value leaves a surviving prefix
//     this function can no longer match.
//
// Composed, that order is: redact, normalize, redact, cap.
//
// # This package performs one of those transforms
//
// Every URL httpx renders — *[StatusError].Error(), and every "url" attribute
// in a [GetBytes] log line — goes through a parse-and-re-serialize step that
// blanks query values and drops userinfo. That step is a URL-encoding transform
// in rule 2's sense, so redacting a known secret out of one of those strings is
// the "only after the transform" mistake. Measured on go1.27.0 (identical on
// go1.26.7): for a secret in the request PATH, which is the one
// credential-bearing position the rendering keeps verbatim, a secret containing
// a space renders as tok%20en, a non-ASCII byte as tok%C3%A9n, and a '#'
// truncates the value at the fragment boundary — so a byte-exact needle held in
// the caller's own representation matches none of them and the value reaches the
// log percent-encoded. A stray '%' is the safe case: the URL fails to parse and
// the whole rendering collapses to a fixed placeholder.
//
// So a caller holding a credential that rides in a URL PATH must redact it
// before the URL reaches this package (or pass the needle in its
// percent-encoded form as well). A credential in the query string or the
// userinfo needs nothing: the rendering removes those wholesale, without
// matching anything.
func RedactSecretString(s string, secret Secret) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, string(secret), "REDACTED")
}

// redactURL returns a log-safe rendering of rawURL. It masks the userinfo
// password (like url.URL.Redacted, mirroring the go-retryablehttp CVE-2024-6104
// fix) and replaces every query value with "REDACTED" (query values commonly
// carry api keys, tokens, and signatures — the same default .NET 9's
// IHttpClientFactory adopted). Query keys, scheme, host, and path are kept for
// debugging; the fragment is dropped. Unparseable input yields a fixed
// placeholder rather than risk logging a raw secret-bearing string.
//
// It re-serializes the URL, so it is a normalizing transform in the sense
// [RedactSecretString]'s ordering contract means: a secret in the PATH — the one
// credential-bearing position this function keeps verbatim — can come out
// percent-encoded, and a caller redacting it out of the result afterwards then
// matches nothing. The contract's "this package performs one of those
// transforms" section carries the measurements and the caller's obligation.
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
	// Drop userinfo entirely rather than leaning on url.Redacted(), which masks
	// only the PASSWORD and preserves the username verbatim. A username-only
	// token is a real credential shape for API endpoints
	// (https://token@prowlarr/1/api), so Redacted() alone leaks it into every
	// log line this helper is supposed to make safe (CWE-532) - and callers
	// cannot tell, because the function's whole promise is log-safety. Consumers
	// that pre-scrubbed userinfo before constructing a StatusError can delete
	// that workaround.
	if u.User != nil {
		u.User = url.User("REDACTED")
	}
	return u.Redacted()
}

// LogSafeError returns an error whose message is safe to log. A transport
// *url.Error embeds the full request URL (with any userinfo/query secrets), so
// it is reduced to its underlying cause. Nil returns nil, and a non-nil error
// NEVER reduces to nil (see urlErrorCause); *StatusError already renders a
// redacted URL via Error(), so it (and everything else) passes through
// unchanged — preserving errors.Is/As chains for callers.
//
// httpx applies this reduction to every transport error it logs or wraps; it
// is exported so a caller wrapping transport errors into its own messages can
// apply the same one (equivalent to RedactTransportError(err, "", "") — reach
// for that variant when a known secret must also be scrubbed from the text).
func LogSafeError(err error) error {
	if err == nil {
		return nil
	}
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		return urlErrorCause(urlErr)
	}
	return err
}

// errRequestFailed stands in for the cause of a *url.Error that has none. It
// is deliberately contentless: an error with no cause has nothing else to
// report, and the one field such a *url.Error does carry is the URL this
// package exists to keep out of logs.
var errRequestFailed = errors.New("request failed")

// urlErrorCause returns the cause the redaction helpers reduce a *url.Error
// to. That is urlErr.Err for every error net/http builds — it always populates
// the field. A *url.Error whose Err is nil, or a typed-nil *url.Error (which
// errors.As binds just as happily, and whose field access would panic), has no
// cause to hand back, and both answers a reader might expect are wrong there:
//
//   - nil would turn a non-nil failure into "no error". Do logs every attempt
//     error as "error", LogSafeError(err), so the diagnostic would vanish from
//     the retry and exhaustion lines at exactly the moment it is needed, and a
//     consumer that returns the reduced value as its own error (the reduction
//     is exported for that) would report a failure as a success.
//   - the original *url.Error would render its raw URL — the CWE-532 leak the
//     reduction exists to prevent, and no less a leak for the cause being nil.
//
// So this path substitutes a fixed, URL-free stand-in: non-nil, log-safe, and
// as informative as an error carrying no cause can be.
func urlErrorCause(urlErr *url.Error) error {
	if urlErr == nil || urlErr.Err == nil {
		return errRequestFailed
	}
	return urlErr.Err
}
