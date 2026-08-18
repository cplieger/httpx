// Package httpx provides a resilient outbound-HTTP toolkit: transient-error
// classification, generic typed retry (Do) and a bounded-bytes GET (GetBytes)
// over one jittered-exponential-backoff loop, a transparent retrying
// RoundTripper, Retry-After parsing, HTTP status mapping, secret redaction,
// body draining, custom-CA TLS transports, and a configurable redirect
// allowlist.
//
// The package deliberately keeps these concerns together: they compose into
// a single [net/http.Client], whose configuration surface (Transport,
// CheckRedirect, per-request contexts) spans exactly this set. This overview
// maps the surface; the README carries usage examples, the v2 migration
// table, and the timeout model.
//
// # Retry doors
//
// Three entry points with different ownership contracts (who builds the
// request, who owns the body, who sees the response). They share one option
// vocabulary and the same equal-jitter backoff progression; passing an
// option to the wrong door does not compile.
//
//   - [Do]: retry a typed operation fn; you build requests, you keep the
//     typed result. Options: [Option] and [DoOption] ([WithLabel],
//     [WithAttemptTimeout], [WithRateLimitRetry], [WithRateLimitOnly]).
//   - [GetBytes]: bounded-bytes GET with redacted diagnostics; the door owns
//     the request, the body cap, and the close. Options: [Option] and
//     [GetOption] ([WithHeaders], [WithMaxBodyBytes]).
//   - [NewRetryRoundTripper]: a transparent retrying [RetryRoundTripper]
//     beneath any client; configured by the [TransportConfig] struct
//     (zero value ready), with [CheckRetry], [OnRetry], and [PrepareRetry]
//     hooks and opt-in body replay.
//
// Shared loop options ([Option]): [WithMaxAttempts], [WithBaseDelay],
// [WithLogger].
//
// # Clients
//
// [NewClient] (timeout + same-host redirect policy preinstalled),
// [NewRetryClient] (retry transport + REQUIRED explicit redirect policy),
// [ContextWithDefaultTimeout] (request-deadline helper).
//
// # Classification and error control
//
// [IsTransient] decides retryability; extend it for your own error types via
// the [Transient] and [RetryAfterHint] interfaces, or mark one error at a time
// with [MarkTransient]. [Permanent] (and [PermanentError], [IsPermanent])
// marks an error non-retryable;
// [AttemptTimeout] (and [IsAttemptTimeout]) marks a timeout as the expiry of a
// bound over ONE attempt, which IS retryable — the only way an error CARRYING
// a context deadline becomes so, applied for you by [WithAttemptTimeout].
// [CheckHTTPStatus] maps response codes to typed errors: [AuthError],
// [RateLimitError], [StatusError], [HTTPStatusError],
// [ResponseTooLargeError], and the [ErrRateLimited] and [ErrServerError]
// sentinels. [IsRetryableStatus] answers the status-code half — the same rule
// the [GetBytes] loop applies — for a caller running one attempt inside its own
// retry budget.
//
// Success is EXACTLY 2xx. [CheckHTTPStatus] returns nil only for [200, 300)
// and an error for every other status, a 3xx included — the v4 breaking change
// (v3 accepted the whole 200-399 band as success). A 3xx reaches a caller only
// under a non-following redirect policy ([RefuseAllRedirects], or any
// CheckRedirect returning [http.ErrUseLastResponse], which net/http surfaces
// as the 3xx response itself with a nil error), and the old window reported
// that redirect stub as success. It is now an *[HTTPStatusError]: still
// non-transient, still unchanged by the redaction helpers. Migrating from v3:
// a surfaced 3xx now errors, which affects only callers pairing a
// non-following policy with [CheckHTTPStatus]; a hand-rolled 2xx band check
// beside such a call is now redundant and can be deleted.
//
// # Retry-After
//
// [ParseRetryAfter] and [ParseRetryAfterResponse] parse the header with a
// 60-second cap ([RetryAfterCap]). [RateLimitError].RetryAfter carries the
// RAW uncapped hint; callers sleeping on it must bound it (the rate-limit
// retry modes do).
//
// # Redirect policies
//
// A [CheckRedirect] policy is required knowledge the caller supplies:
// [DefaultRedirectPolicy] (same host), [RefuseAllRedirects],
// [DockerGitHubRedirectPolicy], or a custom allowlist built with
// [RedirectPolicyFunc] and [RedirectOption] ([WithAllowedHosts],
// [WithAllowedSuffixes], [WithSameHost], [WithMaxHops],
// [WithAllowSchemeDowngrade], [WithPreserveMethod]).
//
// # TLS transports
//
// [CATransport] pins PEM CA certificate(s) as the sole trust anchors on a
// cloned default transport ([ErrNoCertsInPEM] on empty input);
// [CloneDefaultTransport] yields a private mutable transport clone. The
// certtest subpackage generates throwaway CA material for tests.
//
// # Secret redaction
//
// [RedactSecret], [RedactSecretString], [RedactTransportError], and
// [LogSafeError] keep credentials out of logs and returned errors. GetBytes
// never logs or returns a raw URL. [Secret] types the value-to-hide apart
// from the text-to-scan, so a transposed call does not compile. The redactors
// match a literal value in the
// exact representation the text carries, so a needle held in one encoding never
// matches a haystack carrying another. A caller composing them with a
// normalizing transform or a byte cap follows the order documented on
// [RedactSecretString]: redact, normalize, redact, cap.
//
// # Body helpers
//
// [Drain], [DrainClose], [LimitedBody], [ReadLimitedBody].
//
// # Conditional GET
//
// [DoConditional] with [Validators] and [ConditionalResult] implements
// ETag/Last-Modified revalidation over one bounded read.
//
// # Backoff primitives
//
// [JitteredBackoff], [SafeDouble], and [SleepCtx] are the exported building
// blocks the doors are made of, for callers composing their own loops.
package httpx
