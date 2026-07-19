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
//     [WithRateLimitRetry], [WithRateLimitOnly]).
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
// the [Transient] and [RetryAfterHint] interfaces. [Permanent] (and
// [PermanentError], [IsPermanent]) marks an error non-retryable.
// [CheckHTTPStatus] maps response codes to typed errors: [AuthError],
// [RateLimitError], [StatusError], [HTTPStatusError],
// [ResponseTooLargeError], and the [ErrRateLimited] and [ErrServerError]
// sentinels.
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
// [WithAllowSchemeDowngrade]).
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
// never logs or returns a raw URL.
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
