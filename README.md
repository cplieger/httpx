# httpx

[![Go Reference](https://pkg.go.dev/badge/github.com/cplieger/httpx/v5.svg)](https://pkg.go.dev/github.com/cplieger/httpx/v5)
[![Go version](https://img.shields.io/github/go-mod/go-version/cplieger/httpx)](https://github.com/cplieger/httpx/blob/main/go.mod)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/httpx/badges/coverage.json)](https://github.com/cplieger/httpx/actions/workflows/coverage.yml)
[![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/httpx/badges/mutation.json)](https://github.com/cplieger/httpx/issues?q=label%3Agremlins-tracker)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13213/badge)](https://www.bestpractices.dev/projects/13213)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/httpx/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/httpx)

> Resilient outbound-HTTP toolkit for Go: retry, backoff, transient-error classification, and more.

A resilient outbound-HTTP toolkit for Go providing jittered exponential backoff, transient-error classification, Retry-After parsing, HTTP status mapping, secret redaction, body draining, a transparent retrying `http.RoundTripper` with body replay, and a configurable redirect allowlist. Zero dependencies beyond the Go standard library and pgregory.net/rapid (test only).

The toolkit presents three retry doors sharing one option vocabulary:

- **`Do[T]`** retries a typed operation you own (any closure returning `(T, error)`).
- **`GetBytes`** retries an HTTP GET and returns bounded, redaction-safe body bytes.
- **`NewRetryRoundTripper`** retries transparently inside a stdlib `http.RoundTripper`.

`NewRetryClient` assembles the retrying client (transport + an explicit, required redirect policy) in one call.

v4 tightens what counts as success: `CheckHTTPStatus` returns nil for **2xx only**, so a 3xx surfaced by a non-following redirect policy is now an error (see [Status checking](#status-checking) and [Migrating from v3](#migrating-from-v3)). v5 hardens signatures with no behavior change: the redaction helpers take the secret as the named `Secret` type, and `WithSameHost` / `WithPreserveMethod` take a `bool` (see [Migrating from v4](#migrating-from-v4)).

## Install

`go get github.com/cplieger/httpx/v5@latest`

## Usage

```go
// Bounded-bytes GET with retry
body, err := httpx.GetBytes(ctx, client, url,
    httpx.WithMaxAttempts(3),
    httpx.WithBaseDelay(time.Second),
)

// Generic typed retry
result, err := httpx.Do(ctx, func(ctx context.Context) (T, error) {
    return doWork(ctx)
}, httpx.WithMaxAttempts(3), httpx.WithLabel("fetch"))

// Retry rate limits too (opt-in): a *RateLimitError is retried after
// min(its Retry-After hint, maxWait)
result, err := httpx.Do(ctx, fn,
    httpx.WithRateLimitRetry(30*time.Second),
)

// Retry ONLY rate limits (transients fail fast; e.g. when transient retry is
// handled by an outer layer)
_, err := httpx.Do(ctx, func(ctx context.Context) (struct{}, error) {
    return struct{}{}, download(ctx)
}, httpx.WithRateLimitOnly(5*time.Minute), httpx.WithMaxAttempts(3))

// Retrying client: transport retry + an explicit redirect policy in one call.
// The policy parameter is required (there is no safe universal default; nil
// panics). No Client.Timeout is set; bound totals with a context deadline or
// TransportConfig.MaxElapsedTime, and single attempts on the base transport.
client := httpx.NewRetryClient(nil, httpx.DefaultRedirectPolicy, httpx.TransportConfig{
    MaxAttempts: 4,
    BaseDelay:   time.Second,
})

// Transparent retrying RoundTripper (inspired by hashicorp/go-retryablehttp)
rt := httpx.NewRetryRoundTripper(http.DefaultTransport, httpx.TransportConfig{
    MaxAttempts: 4,
    OnRetry: func(attempt int, req *http.Request, resp *http.Response, err error) {
        log.Printf("retry #%d for %s", attempt, req.URL)
    },
    PrepareRetry: func(req *http.Request) error {
        req.Header.Set("Authorization", "Bearer "+freshToken())
        return nil
    },
})
client := &http.Client{Transport: rt, CheckRedirect: httpx.DefaultRedirectPolicy}

// Retry POST/PUT with body replay (opt-in, requires GetBody)
client := httpx.NewRetryClient(nil, httpx.DefaultRedirectPolicy, httpx.TransportConfig{
    MaxAttempts:        4,
    RetryNonIdempotent: true,
})
payload := []byte(`{"key":"value"}`)
req, _ := http.NewRequest("POST", url, bytes.NewReader(payload))
req.GetBody = func() (io.ReadCloser, error) {
    return io.NopCloser(bytes.NewReader(payload)), nil
}
resp, err := client.Do(req)

// PermanentError: signal "do not retry" (mirrors cenkalti/backoff)
if configErr != nil {
    return httpx.Permanent(configErr) // will not be retried
}

// Custom redirect policy
policy := httpx.RedirectPolicyFunc(
    httpx.WithAllowedHosts("api.example.com"),
    httpx.WithAllowedSuffixes(".cdn.example.com"),
    httpx.WithMaxHops(3),
)

// Refuse a redirect that would change the method (POST -> GET across a
// 301/302/303) instead of silently re-issuing it as a GET. The refused hop
// surfaces the 3xx response itself, which CheckHTTPStatus reports as an error.
policy = httpx.RedirectPolicyFunc(httpx.WithSameHost(true), httpx.WithPreserveMethod(true))

// Status checking: nil for 2xx only. A 3xx (surfaced by a non-following
// policy) is an error.
if err := httpx.CheckHTTPStatus(resp); err != nil { /* typed: Auth/RateLimit/HTTPStatus */ }

// Pin a private / self-signed CA as the SOLE trust anchor (verification stays
// ON, TLS 1.2 minimum). The caller reads the PEM bytes (file, secret, env),
// keeping the helper I/O-free.
tr, err := httpx.CATransport(pemBytes)
client := &http.Client{Transport: tr, CheckRedirect: httpx.DefaultRedirectPolicy}
// ...or compose the pinned transport with retry:
client = httpx.NewRetryClient(tr, httpx.DefaultRedirectPolicy, httpx.TransportConfig{MaxAttempts: 3})

// Transient error classification
if httpx.IsTransient(err) { /* safe to retry */ }

// Limit response body size
rc := httpx.LimitedBody(resp, 1<<20) // 1 MB cap
defer rc.Close()
```

## Migrating from v4

v5 is signature hardening only — no behavior changes. The redaction helpers take the secret as a named type, `Secret`, so the value-to-hide and the text-to-scan cannot be transposed at a call site (a reversed call turns the redactor into a leak); the two presence-style redirect options take the `bool` their neighbour `WithAllowSchemeDowngrade` always did; and the two capability interfaces embed `error`.

| v4 | v5 |
| --- | --- |
| `RedactSecretString(s, secret)` — both plain `string` | `secret` is the named type `Secret`: `RedactSecretString(s, httpx.Secret(token))` |
| `RedactTransportError(err, prefix, secret)` — `prefix` and `secret` both plain `string` | `RedactTransportError(err, prefix, httpx.Secret(token))` |
| `WithSameHost()` | `WithSameHost(true)`; `false` is the no-option default (only allowlisted targets are followed) |
| `WithPreserveMethod()` | `WithPreserveMethod(true)`; `false` is the no-option default (a method-changing hop is followed as net/http rewrites it) |
| `Transient` = `IsTransient() bool` | `Transient` = `error` + `IsTransient() bool` |
| `RetryAfterHint` = `RetryAfterHint() time.Duration` | `RetryAfterHint` = `error` + `RetryAfterHint() time.Duration` |
| module path suffix `/v4` | `github.com/cplieger/httpx/v5` in `go.mod` and every import |

A call passing an untyped string constant (`RedactSecretString(s, "token")`) compiles unchanged; only a `string` variable in the secret position needs the `httpx.Secret(...)` conversion. A transposed call — the secret where the text belongs — is now a compile error, which is the point. `RedactSecret(err, secret)` is unchanged: its parameters already differ in type, so a swap never compiled.

The two interface changes need no work in a normal implementor: an implementor is an error already, or it could not appear in the chain `errors.As` walks. Only a type that declared `IsTransient()`/`RetryAfterHint()` without an `Error()` method stops satisfying — such a type was never reachable through either interface's only access path. What the change buys the caller is `errors.AsType`: `errors.AsType[httpx.Transient](err)` and `errors.AsType[httpx.RetryAfterHint](err)` now compile, where before `errors.AsType`'s `[E error]` constraint refused both and `go fix -errorsastype` produced code that did not build.

## Migrating from v3

Two changes, one of them breaking.

| v3 | v4 |
| --- | --- |
| `CheckHTTPStatus` returns nil for 200-399 | **BREAKING**: nil for **2xx only**; every other status errors, a 3xx included (`*HTTPStatusError`) |
| a method-changing redirect hop is followed as a GET | opt in to refusing it with `RedirectPolicyFunc(..., WithPreserveMethod(true))` (additive, off by default) |
| `github.com/cplieger/httpx/v3` | `github.com/cplieger/httpx/v5` in `go.mod` and every import — coming from v3 you cross the v5 signature changes in the same bump, so apply [Migrating from v4](#migrating-from-v4) too |

**What changes:** a 3xx that reaches your code now returns `*HTTPStatusError{Code}` instead of nil.

**Who is affected:** only callers that pair a non-following redirect policy (`RefuseAllRedirects`, or any `CheckRedirect` returning `http.ErrUseLastResponse`) with `CheckHTTPStatus` — a followed redirect never surfaces a 3xx, so a client on `DefaultRedirectPolicy` or an allowlist policy sees no difference. `GetBytes` and the `RetryRoundTripper` are untouched: `GetBytes` already returned a `*StatusError` for a 3xx (it now derives that from the classifier instead of restating the band), and the transport never classified statuses through it. `DoConditional` still errors on a 3xx, but the error is now `*HTTPStatusError{Code}` instead of the plain `unexpected status %d` fallback — a caller matching on that message must switch to `errors.As`.

**What to do:** delete the hand-rolled band check next to the call — a guard like `if resp.StatusCode < 200 || resp.StatusCode >= 300 { ... }` sitting after `CheckHTTPStatus` (written precisely because the classifier accepted 3xx) is now redundant. If a 3xx must stay non-fatal for one call site, check `resp.StatusCode` there instead of widening the classifier back.

## Migrating from v2

One option vocabulary replaces v2's three config dialects; two loop doors replace three retry functions; the retrying client gains a required redirect policy. Mechanical mapping:

| v2 | v3 |
| --- | --- |
| `Retry(ctx, client, url, opts...)` | `GetBytes(ctx, client, url, opts...)` (identical contract and option names) |
| `RetryWithBackoff[T](ctx, n, d, label, fn)` | `Do[T](ctx, fn, WithMaxAttempts(n), WithBaseDelay(d), WithLabel(label))` |
| `RetryOnRateLimit(ctx, n, maxWait, fn)` | `Do[struct{}](ctx, wrap(fn), WithRateLimitOnly(maxWait), WithMaxAttempts(n))`; `wrap` adapts `func(ctx) error` to `(struct{}, error)` |
| `NewRetryRoundTripper(base, WithRTMaxAttempts(4), ...)` | `NewRetryRoundTripper(base, TransportConfig{MaxAttempts: 4, ...})` |
| `WithRTMaxAttempts(0)` (try once) | `TransportConfig{MaxAttempts: -1}`; zero now means unset (default 3), NEGATIVE means exactly one attempt |
| `rt.StandardClient()` + manual `CheckRedirect` | `NewRetryClient(base, policy, cfg)`; policy is a required argument |
| `WithBackoffFunc` / `Backoff` / `BackoffStop` / `NewExponentialBackoff` | removed; the equal-jitter progression configured by `BaseDelay` is the strategy, `MaxElapsedTime` is the hard ceiling |

`Do` keeps v2's generic-loop semantics exactly: total attempts (minimum 1, `WithMaxAttempts(0)` still means one attempt), transient-only default classification, `RetryAfterHint` honored, context checked after each failure.

## API

### Retry doors

- `Do[T]`: generic retry with jittered exponential backoff. When a transient error implements `RetryAfterHint`, its pre-capped duration replaces the backoff for the next wait (the exponential base keeps advancing). Rate-limit handling is opt-in per call: `WithRateLimitRetry(maxWait)` adds `*RateLimitError` to the retryable set; `WithRateLimitOnly(maxWait)` retries nothing else. `WithAttemptTimeout(d)` adds a retryable per-attempt bound. Counts **total** attempts (a non-positive count clamps to 1).
- `GetBytes`: HTTP GET with exponential backoff on 408/429/5xx **and transient transport errors** (timeouts, connection resets, DNS failures; see `IsTransient`); other 4xx and non-transient transport errors return immediately. Honors Retry-After (capped at `RetryAfterCap`). Counts **total** attempts.
- `NewRetryRoundTripper(base, TransportConfig{...})`: create a retrying `http.RoundTripper`. `TransportConfig{}` is ready to use (3 attempts, 1s base delay, default policy); `MaxAttempts: -1` means exactly one attempt.

### Loop options

Shared by both loop doors (`Option`): `WithMaxAttempts`, `WithBaseDelay`, `WithLogger`, `WithExhaustedLevel`. `Do`-only (`DoOption`): `WithLabel`, `WithAttemptTimeout`, `WithRateLimitRetry`, `WithRateLimitOnly`. `GetBytes`-only (`GetOption`): `WithHeaders`, `WithMaxBodyBytes`. Passing an option to the wrong door is a compile error. A non-positive rate-limit `maxWait` falls back to `RetryAfterCap` (60s), so the inter-attempt wait is always positive (never a hot spin); supplying both rate-limit modes is a configuration error.

### Clients

- `NewClient(timeout)`: simple client with the same-host `DefaultRedirectPolicy` preinstalled.
- `NewRetryClient(base, policy, cfg)`: retrying client; `policy` is **required** (nil panics; a nil `CheckRedirect` would silently mean net/http's follow-anywhere default). Sets no `Client.Timeout` (it would cap the whole retry sequence); see [Timeouts and deadlines](#timeouts-and-deadlines) for bounding attempts and totals.
- `ContextWithDefaultTimeout(ctx, def)`: bound `ctx` by `def` only when the caller brought no deadline. A caller deadline is never undercut or extended; `def <= 0` means no default; the returned cancel is always non-nil. The per-request timeout rule for an API client.

### TLS transports

- `CATransport(pem)`: build an `*http.Transport` (cloned from `http.DefaultTransport`, so pooling, timeouts, and proxy settings are preserved) that pins the CA certificate(s) in `pem` as the **sole** trust anchors. Verification stays **on** (`InsecureSkipVerify` is never set) with a TLS 1.2 minimum. Returns the concrete, mutable transport so it composes with `NewRetryRoundTripper`. The caller reads the PEM bytes (file, secret, env), keeping the helper I/O-free.
- `ErrNoCertsInPEM`: returned by `CATransport` when `pem` yields no certificates (a loud error instead of a silently-empty pool).
- `CloneDefaultTransport()`: a private clone of `http.DefaultTransport` that is yours to mutate (a per-attempt `ResponseHeaderTimeout`, `MaxIdleConnsPerHost`, the base of `NewRetryRoundTripper`) without reconfiguring every other client in the process. Errors when `http.DefaultTransport` has been replaced by a non-`*http.Transport`.

### Test helpers (`certtest` subpackage)

The `github.com/cplieger/httpx/v5/certtest` subpackage supplies throwaway self-signed CA material for tests, the companion to `CATransport`. Only `_test.go` files import it, so its certificate-generation code never links into a production binary.

- `certtest.SelfSignedCA(tb)`: a fresh self-signed CA certificate, PEM-encoded. Each call generates a new key, so two certs are mutually untrusted (handy for asserting a pin is enforced).
- `certtest.WriteSelfSignedCA(tb)`: the same certificate written to a `ca.pem` file under `tb.TempDir()`, returning the path.

### Transport hooks & policies (`TransportConfig` fields)

- `CheckRetry`: pluggable retry policy, `func(ctx, resp, err) (bool, error)`. The default retries transient transport errors and 429/502/503/504 (deliberately narrower than `GetBytes`, which retries 408 and every 5xx).
- `OnRetry`: per-attempt callback for observability/metrics (the transport's only seam; it logs nothing itself)
- `PrepareRetry`: mutate the request before a retry (e.g., re-sign tokens)
- `MaxElapsedTime`: hard total-time ceiling across retries, including honored Retry-After (checked between attempts)
- `RetryNonIdempotent`: opt-in POST/PUT/PATCH/DELETE retry with `GetBody` replay

### Classification & Parsing

- `IsTransient`: classify errors as transient (retryable); respects `PermanentError`. A caller's expired or canceled context is terminal; an `http.Client.Timeout` or transport `ResponseHeaderTimeout` is a per-attempt bound and IS retried (see [Timeouts and deadlines](#timeouts-and-deadlines))
- `AttemptTimeout(err)` / `IsAttemptTimeout(err)`: mark a timeout as the expiry of a bound over ONE attempt, making it retryable, and test for that mark. The mirror of `Permanent`, and the only way an error that CARRIES `context.DeadlineExceeded` becomes retryable — the mark keeps the deadline visible to `errors.Is(err, context.DeadlineExceeded)` for the caller's own callers. `WithAttemptTimeout` applies it for you (see [Timeouts and deadlines](#timeouts-and-deadlines)).
- `RetryAfterHint`: an interface (`error` + `RetryAfterHint() time.Duration`) an error implements to supply the next retry wait. `Do` honors it when the error is transient and the duration is positive; the implementer must cap the value, since httpx applies no ceiling of its own here.
- `Transient`: an interface (`error` + `IsTransient() bool`) an error implements to declare its own retryability, consulted by `IsTransient`. Both capability interfaces embed `error` — like `net.Error` — because they are read through `errors.As`/`errors.AsType` over an error tree, whose nodes are errors by construction, so a caller can write `errors.AsType[httpx.Transient](err)` (`errors.AsType` constrains its type parameter to `error`, where `errors.As` accepted any target).
- `CheckHTTPStatus`: map an HTTP status to a typed error; success is 2xx only (see [Status checking](#status-checking))
- `IsRetryableStatus(code)`: does the retry loop treat this status as transient? True for 408, 429, and any 5xx. It is the same rule the built-in retry uses, not a copy of it — `GetBytes`'s attempt function calls this function, so the two cannot drift. For the caller that runs `GetBytes` with `WithMaxAttempts(1)` inside its own retry budget and therefore classifies the returned `*StatusError` itself (see [Nesting a door in your own retry loop](#nesting-a-door-in-your-own-retry-loop)). The `RetryRoundTripper`'s default policy is narrower (429/502/503/504); pass this predicate through `TransportConfig.CheckRetry` to widen it.
- `ParseRetryAfter` / `ParseRetryAfterResponse`: parse a Retry-After header (capped at `RetryAfterCap` / raw)

### Status checking

`CheckHTTPStatus(resp)` returns `nil` for **exactly 2xx** (200-299) and an error for every other status:

| Status | Result |
| --- | --- |
| 2xx | `nil` |
| 3xx | `*HTTPStatusError{Code}` — not transient, not a client or server error |
| 401 / 403 | `*AuthError` (the message carries `(401)` / `(403)`) |
| 429 | `*RateLimitError` with the raw, uncapped `Retry-After` hint |
| other 4xx / 5xx | `*HTTPStatusError{Code}` — transient for 502/503/504 |
| 1xx | `*HTTPStatusError{Code}` — not a completed response |

The 2xx-only window is a **v4 breaking change** (v3 returned nil for the whole 200-399 band). A 3xx only ever reaches a caller when the client is configured _not_ to follow redirects — `RefuseAllRedirects`, or any `CheckRedirect` returning `http.ErrUseLastResponse`, which net/http hands back as the 3xx response itself with a **nil error**. Under the old window that redirect stub classified as success, so a token-bearing client that deliberately refuses the hop (exactly the client `RefuseAllRedirects` exists for) then treated the unfollowed redirect as a completed request. `CheckHTTPStatus` is the status handling that policy delegates to, and it now reports the 3xx as the failure it is.

A 3xx is deliberately an `*HTTPStatusError` rather than a new type, so it flows through the existing plumbing unchanged: `IsTransient` is false (only 502/503/504 are transient), `IsServerError` and `IsClientError` are both false, and `LogSafeError` and the redaction helpers pass it through (it embeds no URL). There is no second, stricter classifier — this is the only one. See [Migrating from v3](#migrating-from-v3) for the migration.

### Error Control

- `Permanent(err)`: wrap an error to signal "do not retry" (mirrors cenkalti/backoff)
- `IsPermanent(err)`: check whether an error is wrapped as permanent
- `PermanentError`: the wrapper type (supports `errors.Is`/`errors.As`/`Unwrap`)
- `MarkTransient(err)`: wrap an error to signal "retry this" — the mirror of `Permanent`, for a failure your operation knows is self-healing where the shared policy cannot know (a server-side fault delivered inside a 200 envelope, an upstream whose plain 500 always clears). It saves you declaring a one-method `Transient` wrapper, and cannot forget `Unwrap` the way a hand-rolled one does. The mark is the outermost verdict, so it overrides a non-transient verdict already on the error, but `IsTransient`'s standing rejections still win: `Permanent`, an `*AuthError`, a `*RateLimitError` (retry a rate limit by naming a wait budget instead — `WithRateLimitRetry`), and a caller-context error stay terminal. For a per-attempt timeout carrying a deadline, use `AttemptTimeout`.

### Nesting a door in your own retry loop

Running a door with `WithMaxAttempts(1)` inside your own retry loop is the sanctioned way to avoid multiplying the two attempt counts (a 3-attempt door inside a 3-attempt loop is 9 requests). The door then makes no retry decision, so you make it — and the two exported predicates are the door's own rule, so your loop and the built-in one classify identically:

```go
// One attempt per outer attempt; this loop owns the budget.
body, err := httpx.GetBytes(ctx, client, url, httpx.WithMaxAttempts(1))
if err != nil {
    // A self-healing status is worth another of MY attempts. GetBytes
    // deliberately does NOT mark its exhaustion error transient: after
    // WithMaxAttempts(1) that decision is the caller's policy.
    if se, ok := errors.AsType[*httpx.StatusError](err); ok && httpx.IsRetryableStatus(se.Code) {
        return httpx.MarkTransient(err)
    }
    return err // auth/config failure: terminal, fail on the first attempt
}
```

The exhaustion error still implements `RetryAfterHint`, so the upstream's already-capped `Retry-After` survives into the enclosing `Do` and is waited instead of the jittered backoff.

### Backoff Primitives

- `JitteredBackoff`: equal jitter, `[backoff/2, backoff]`
- `SafeDouble` / `SleepCtx`: overflow-safe doubling, context-aware sleep

### Body Helpers

- `Drain` / `DrainClose`: drain a body for connection reuse (64 KB limit). A failed drain logs a bare Debug line and never the read error itself — that text is written by the far end (see [URL redaction in logs and errors](#url-redaction-in-logs-and-errors))
- `LimitedBody`: wrap a response body with a size cap
- `ReadLimitedBody`: read a body to a cap (closing it) with overflow detection, returning `*ResponseTooLargeError` instead of a silently truncated body

### Conditional GET

- `Validators{ETag, LastModified}`: the cache validators captured from a previous 200, replayed on the next request
- `ConditionalResult{Validators, Body, NotModified}`: one conditional-request outcome
- `DoConditional(client, req, v, maxBodyBytes)`: one conditional attempt; `v` alone decides what is replayed (pre-existing conditional headers are cleared, empty fields unsent). A 304 returns `NotModified` with zero `Validators` (keep the ones you sent); a 200 returns the bounded body plus fresh validators; anything else is an error, with transport errors reduced via `LogSafeError` so no raw URL reaches caller error text. Single-shot by design: wrap it in `Do` for retry, rebuild the request per attempt, persist body and validators together, and send zero `Validators` when the cached body is unusable. Validators are checked in both directions (header field-value grammar, 1 KiB cap): an invalid upstream value is captured as empty and an invalid replayed field is unsent, so a poisoned validator degrades to an unconditional GET and self-heals on the next clean 200. Full semantics in the [godoc](https://pkg.go.dev/github.com/cplieger/httpx/v5#DoConditional).

### Redirect Policies

- `DefaultRedirectPolicy`: same-host-only (used by `NewClient`); refuses a same-host `https`->`http` downgrade, allows an `http`->`https` upgrade.
- `RefuseAllRedirects`: follows **no** redirect; returns `http.ErrUseLastResponse`, so the client surfaces the 3xx response itself (nil error) and `CheckHTTPStatus` reports it as an error. The policy for a token-bearing client of an API that issues no redirects: Go forwards custom headers (`X-Plex-Token`, `X-Api-Key`) across redirects, so a hostile 302 would exfiltrate the credential.
- `DockerGitHubRedirectPolicy`: example allowlist policy for docker.com/github.com.
- `RedirectPolicyFunc`: build a custom redirect allowlist from functional options: `WithAllowedHosts`, `WithAllowedSuffixes`, `WithSameHost(true)` (also allow the original request's host; the same-origin building block — `false` is the no-option default), `WithMaxHops`, `WithAllowSchemeDowngrade`, and `WithPreserveMethod`. Every policy refuses an `https`->`http` downgrade by default, even to an allowlisted or same-host target, so an auth header is never forwarded onto a cleartext hop; an `http`->`https` upgrade is always allowed, and `WithAllowSchemeDowngrade(true)` opts out of the refusal.
- `WithPreserveMethod(true)`: **refuses** a hop that would change the request method instead of rewriting the method back (`false` is the no-option default: the hop is followed as net/http rewrites it). net/http downgrades a POST/PUT/PATCH/DELETE to a GET across a 301/302/303 and drops the body (RFC 9110 §15.4, Go issue 18570); only 307/308 carry the method forward. The refusal returns `http.ErrUseLastResponse`, so the 3xx surfaces to the caller (nil error) and errors under `CheckHTTPStatus` — the same pairing `RefuseAllRedirects` relies on. The comparison is against the **original** request, so a POST kept by a 307 and then downgraded by a 302 is refused at the second hop; an empty `via` chain fails closed. The hop cap, allowlist, and scheme-downgrade refusals (hard errors) keep precedence, and the option grants nothing on its own — with no allowlist and no `WithSameHost(true)` the policy still refuses everything.
- `CheckRedirect`: the `http.Client.CheckRedirect` function shape as a type alias; every shipped policy is one.

### Secret Redaction

- `RedactTransportError` / `RedactSecret` / `RedactSecretString`: secret redaction (error- and string-level). `RedactSecretString` and `RedactTransportError` take the secret as the named `Secret` type, so the value-to-hide and the text-to-scan cannot be transposed — a reversed call would turn the redactor into a leak, and it no longer compiles.
- `LogSafeError`: reduce a URL-embedding transport `*url.Error` to its underlying cause (everything else passes through, `errors.Is`/`As` preserved). The same reduction httpx applies to every transport error it logs; equivalent to `RedactTransportError(err, "", "")`.

### Error Types

- `AuthError` / `RateLimitError` / `HTTPStatusError` / `StatusError`
- `ResponseTooLargeError`: returned by `GetBytes` when the response exceeds `WithMaxBodyBytes` (carries `Limit`; no body is returned)
- `ErrRateLimited` / `ErrServerError`: sentinel errors
- `PermanentError`: do-not-retry sentinel wrapper

## Logging

`Do` and `GetBytes` log via `log/slog` and accept `WithLogger` to override the default logger per call. Per-attempt "retrying" lines are logged at **Debug**; a retry that recovers is normal operation, not a degraded state. The terminal "retries exhausted" / "rate limit retries exhausted" lines are at **Warn** — except under `WithMaxAttempts(1)`, where they drop to **Debug**: a one-attempt budget retried nothing, so the door is a single attempt inside the caller's own retry loop, and that loop owns both the retry policy and the warning. `WithExhaustedLevel(level)` overrides that line's level outright, for callers whose own failure log carries strictly more context than the library's can (the tracker, the item, an onset latch): demoting the library's copy keeps one report of one event without discarding the per-attempt Debug diagnostics a discard logger would also throw away. It applies to a multi-attempt budget too, and is the only way to raise the line above Warn. `GetBytes` also emits a **Warn** "slow upstream response" when a single attempt's response takes longer than 10s (timed per attempt, so backoff sleeps are not counted as upstream latency). The `RetryRoundTripper` logs nothing itself; observe its retries through the `OnRetry` hook, where redaction is the caller's responsibility. `Drain`/`DrainClose` emit one **Debug** line, `failed to drain response body`, carrying no attributes — a failed drain only forfeits connection reuse, and the read error that caused it is remote-authored text this package refuses to log (see below).

### URL redaction in logs and errors

To avoid leaking credentials into logs (CWE-532, the class of [go-retryablehttp CVE-2024-6104](https://discuss.hashicorp.com/t/hcsec-2024-12-go-retryablehttp-can-leak-basic-auth-credentials-to-log-files/68027)), `GetBytes` never logs or returns a raw request URL:

- Every logged `url` attribute is redacted: the whole userinfo component is replaced with `REDACTED` (stronger than `url.URL.Redacted`, which masks only the password and would leave a username-only API token in the clear) and query values are replaced with `REDACTED` (query values commonly carry API keys and tokens). Query keys, scheme, host, and path are kept for debugging.
- `StatusError.Error()` renders that same redacted URL, so the secret stays out of returned errors too; the raw `StatusError.URL` field remains available for programmatic use.
- Transport errors (`*url.Error`, which embed the full URL) are reduced to their underlying cause before logging. The reduction is exported as `LogSafeError` so callers wrapping transport errors into their own messages can apply the same one.

That reduction is **type-based** — it strips an envelope this library itself added — so it cannot sanitize text the far end wrote. `Drain`/`DrainClose` therefore drop the body-read error instead of logging it: net/http renders a malformed chunked trailer as `malformed MIME header: missing colon: "<remote bytes>"`, and for a URL that carries its credential in the path (a webhook token) an edge echoing the request URI puts that credential in those bytes. The drain site logs on the package-level default logger, which no option can reroute, so the value is dropped there rather than left for callers to handle — nothing an operator can act on is lost, because a drain runs only where the body is already being discarded and its outcome is reported by the path that discarded it.

## Retry exhaustion

`GetBytes` and the `RetryRoundTripper` report exhaustion differently; match your error handling to the one you use:

- **`GetBytes`** returns `nil` body and a wrapped error: `retries exhausted after <elapsed>: <lastErr>` (unwrap with `errors.Is`/`errors.As`). A response that overflows `WithMaxBodyBytes` returns `*ResponseTooLargeError` (no body). When the last attempt carried a `Retry-After`, the exhaustion error also implements `RetryAfterHint` with that capped wait, so a caller running `GetBytes` with `WithMaxAttempts(1)` inside its own `Do` loop (the pattern that avoids multiplying the two attempt budgets) still honors the upstream-requested delay instead of falling back to jittered backoff. It deliberately does **not** implement `Transient`: whether an exhausted GET is worth another outer attempt stays the caller's policy.
- **`RetryRoundTripper`** returns the **last response with a nil error**, even when that response is a retryable 5xx (e.g. a 503), mirroring how a non-retried request behaves. A caller that checks only `err != nil` will treat an exhausted 503 as success, so **inspect `resp.StatusCode` and close the body**. (A budget abort via `MaxElapsedTime` does return an error.)

## Timeouts and deadlines

httpx retries transient failures, not budget expiry. `IsTransient` classifies a caller's expired or canceled context as **non-transient**, while a connection reset, a DNS error, a `net.Error` timeout, and a 429/5xx are transient. A caller's context deadline means "the budget is exhausted, stop"; the expiry of a bound over ONE attempt means "this attempt failed, try again".

The two are told apart by whether the `context.DeadlineExceeded` **value** is in the error's unwrap chain, not by `errors.Is`: net/http's own timeout error reports `errors.Is(err, context.DeadlineExceeded) == true` without ever carrying the sentinel, so `errors.Is` cannot distinguish the bounds net/http installs from the caller's own. Where the difference is genuinely unknowable — a bound the `net` package reports, since it maps every expired context onto one shared `i/o timeout` value — the verdict stays terminal, and `AttemptTimeout(err)` is how the code that installed the bound opts back in.

- **Total budget: a context deadline.** Pass a `context.WithTimeout` (or a caller-supplied deadline) as the single authoritative bound. It spans every attempt and every backoff sleep (`SleepCtx` caps the backoff by it); on expiry the call ends, terminal, not retried.
- **Per-attempt bound under `Do`: `WithAttemptTimeout(d)`.** Each attempt runs under a context bounded by `d`, and that bound's expiry is retried — the gRPC per-try-timeout model. A caller deadline nearer than `d` still governs (context keeps the earlier deadline), so `d` caps one attempt and never extends the total. The expiry is marked as the attempt's only while the caller's own context is still live, so a caller out of budget stays terminal. The attempt context is canceled when `fn` returns, so read or drain the body inside `fn`.
- **Per-attempt bound on the client or transport: `http.Client.Timeout` and `ResponseHeaderTimeout`, retried.** net/http reports both through its own timeout error, which claims to match `context.DeadlineExceeded` but never carries it, so both classify as per-attempt and are retried — including under the `RetryRoundTripper`'s default policy. Use `Permanent(err)` in a `TransportConfig.CheckRetry` if you want one of them to stop the loop instead.
- **Per-attempt bound anywhere else: mark it with `AttemptTimeout(err)`.** A `context.WithTimeout` a caller derives itself inside `fn` carries the real sentinel, and a net-level bound (a `net.Dialer` Timeout) is indistinguishable from a caller deadline the `net` package mapped; both stay terminal until marked. `WithAttemptTimeout` is the first case done for you.
- **`GetBytes`** takes no per-attempt option: the door owns its request and has no callback to bound. Bound it with the `Client.Timeout` of the client you pass, or run it as one attempt inside `Do` — `WithMaxAttempts(1)` on the GET, `WithAttemptTimeout(d)` on the `Do` — which is also the composition that avoids multiplying two attempt budgets.

Under `NewRetryRoundTripper` / `NewRetryClient` the retry loop runs _inside_ `client.Do`, so `http.Client.Timeout` is not per-attempt at all: it caps the whole retry sequence and a slow attempt that trips it aborts the remaining retries, which is why `NewRetryClient` sets none. Put the per-attempt bound on the base transport (`ResponseHeaderTimeout`, retried) and the total on the caller's context or `TransportConfig.MaxElapsedTime`. Neither the between-attempt `MaxElapsedTime` check nor an expired context can interrupt an attempt already stalled inside the base transport; only a transport-level timeout can.

## Unsupported by Design

The following features are intentionally not provided:

| Feature | Rationale |
| --- | --- |
| Circuit breaker | Orthogonal pattern excluded by all comparables. Compose externally with sony/gobreaker. |
| Retry budget / token bucket | None of the comparables implement it. Disproportionate complexity (~150 LOC + shared mutable state) for a focused library. |
| Multiple jitter strategies (full, decorrelated) | Equal jitter is the recommended default per AWS Builders' Library. Full jitter risks near-zero delays. |
| `ErrorHandler` for exhaustion | Current `fmt.Errorf("retries exhausted: %w", lastErr)` is sufficient. Callers unwrap. |
| Response body on error | Adds API complexity (ownership of body close). Use `Do[T]` with custom logic. |
| Idempotency key injection | Application-level concern, not a retry library's responsibility. |
| Configurable Retry-After cap | A raisable cap would regress the fixed-60s DoS ceiling (`ParseRetryAfter`); rate-limit waits are capped by the caller-owned `maxWait` arguments. |

## Contributing

Issues and PRs are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for the
conventions and how to run the checks locally.

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

Apache-2.0. See [LICENSE](LICENSE).
