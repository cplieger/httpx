# httpx

[![Go Reference](https://pkg.go.dev/badge/github.com/cplieger/httpx/v2.svg)](https://pkg.go.dev/github.com/cplieger/httpx/v2)
[![Go version](https://img.shields.io/github/go-mod/go-version/cplieger/httpx)](https://github.com/cplieger/httpx/blob/main/go.mod)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/httpx/badges/coverage.json)](https://github.com/cplieger/httpx/actions/workflows/coverage.yml)
[![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/httpx/badges/mutation.json)](https://github.com/cplieger/httpx/issues?q=label%3Agremlins-tracker)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13213/badge)](https://www.bestpractices.dev/projects/13213)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/httpx/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/httpx)

> Resilient outbound-HTTP toolkit for Go: retry, backoff, transient-error classification, and more.

A resilient outbound-HTTP toolkit for Go providing jittered exponential backoff, transient-error classification, Retry-After parsing, HTTP status mapping, secret redaction, body draining, a transparent retrying `http.RoundTripper` with body replay, and a configurable redirect allowlist. Zero dependencies beyond the Go standard library and pgregory.net/rapid (test only).

## Install

`go get github.com/cplieger/httpx/v2@latest`

## Usage

```go
// Simple GET with retry
body, err := httpx.Retry(ctx, http.DefaultClient, url,
    httpx.WithMaxAttempts(3),
    httpx.WithBaseDelay(time.Second),
)

// Generic retry with backoff
result, err := httpx.RetryWithBackoff(ctx, 3, time.Second, "fetch", func(ctx context.Context) (T, error) {
    return doWork(ctx)
})

// Transparent retrying RoundTripper (inspired by hashicorp/go-retryablehttp)
rt := httpx.NewRetryRoundTripper(http.DefaultTransport,
    httpx.WithRTMaxAttempts(4),
    httpx.WithRTBaseDelay(time.Second),
    httpx.WithOnRetry(func(attempt int, req *http.Request, resp *http.Response, err error) {
        log.Printf("retry #%d for %s", attempt, req.URL)
    }),
    httpx.WithPrepareRetry(func(req *http.Request) error {
        req.Header.Set("Authorization", "Bearer "+freshToken())
        return nil
    }),
)
client := rt.StandardClient()

// Retry POST/PUT with body replay (opt-in, inspired by go-retryablehttp)
rt := httpx.NewRetryRoundTripper(http.DefaultTransport,
    httpx.WithRTMaxAttempts(4),
    httpx.WithRetryNonIdempotent(true),
)
client := rt.StandardClient()
payload := []byte(`{"key":"value"}`)
req, _ := http.NewRequest("POST", url, bytes.NewReader(payload))
req.GetBody = func() (io.ReadCloser, error) {
    return io.NopCloser(bytes.NewReader(payload)), nil
}
resp, err := client.Do(req)

// PermanentError — signal "do not retry" (mirrors cenkalti/backoff)
if configErr != nil {
    return httpx.Permanent(configErr) // will not be retried
}

// Pluggable backoff strategy (factory invoked once per request, so each
// request gets its own independent backoff progression)
rt := httpx.NewRetryRoundTripper(http.DefaultTransport,
    httpx.WithBackoffFunc(func() httpx.Backoff {
        return httpx.NewExponentialBackoff(
            httpx.WithInitialInterval(500*time.Millisecond),
            httpx.WithMaxElapsedTime(30*time.Second),
        )
    }),
)

// Custom redirect policy
policy := httpx.RedirectPolicyFunc(
    httpx.WithAllowedHosts("api.example.com"),
    httpx.WithAllowedSuffixes(".cdn.example.com"),
    httpx.WithMaxHops(3),
)

// Pin a private / self-signed CA as the SOLE trust anchor (verification stays
// ON, TLS 1.2 minimum). The caller reads the PEM bytes (file, secret, env),
// keeping the helper I/O-free.
tr, err := httpx.CATransport(pemBytes)
client := &http.Client{Transport: tr}
// ...or compose the pinned transport with retry:
client = httpx.NewRetryRoundTripper(tr, httpx.WithRTMaxAttempts(3)).StandardClient()

// Transient error classification
if httpx.IsTransient(err) { /* safe to retry */ }

// Limit response body size
rc := httpx.LimitedBody(resp, 1<<20) // 1 MB cap
defer rc.Close()
```

## API

### Retry

- `Retry` — HTTP GET with exponential backoff on 429/5xx **and transient transport errors** (timeouts, connection resets, DNS failures — see `IsTransient`); 4xx (non-429) and non-transient transport errors return immediately (functional options: `WithMaxAttempts`, `WithBaseDelay`, `WithMaxBodyBytes`, `WithHeaders`, `WithLogger`). Counts **total** attempts (a non-positive count clamps to 1).
- `RetryWithBackoff[T]` — generic retry with jittered exponential backoff; when a transient error implements `RetryAfterHint`, its pre-capped duration replaces the backoff for the next wait (the exponential base keeps advancing)
- `RetryOnRateLimit` — retry on `*RateLimitError` only (passes ctx to fn)
- `NewRetryRoundTripper` — create a retrying `http.RoundTripper` (functional options: `WithRTMaxAttempts`, `WithRTBaseDelay`, `WithRTMaxElapsedTime`, `WithBackoffFunc`, `WithCheckRetry`, `WithOnRetry`, `WithPrepareRetry`, `WithRetryNonIdempotent`)
- `StandardClient()` — returns `*http.Client` using the `RetryRoundTripper`

### TLS transports

- `CATransport(pem)` — build an `*http.Transport` (cloned from `http.DefaultTransport`, so pooling/timeouts/proxy are preserved) that pins the CA certificate(s) in `pem` as the **sole** trust anchors. Verification stays **on** (`InsecureSkipVerify` is never set) with a TLS 1.2 minimum. Returns the concrete, mutable transport so it composes with `NewRetryRoundTripper`.
- `ErrNoCertsInPEM` — returned by `CATransport` when `pem` yields no certificates (a loud error instead of a silently-empty pool). The caller reads the PEM bytes, keeping the helper I/O-free.
- `CloneDefaultTransport()` — a private clone of `http.DefaultTransport` (pooling/timeouts/HTTP2/proxy preserved) that is yours to mutate — set a per-attempt `ResponseHeaderTimeout`, tune `MaxIdleConnsPerHost`, or use it as the base of `NewRetryRoundTripper` — without reconfiguring every other client in the process. Errors when `http.DefaultTransport` has been replaced by a non-`*http.Transport` (nothing concrete to clone). The building block `CATransport` is assembled on.

### Test helpers (`certtest` subpackage)

The `github.com/cplieger/httpx/v2/certtest` subpackage supplies throwaway self-signed CA material for tests — the companion to `CATransport`. It lives in a separate package so the certificate-generation code is never linked into a production binary (only the `_test.go` files that import it pull it in, exactly as the standard library ships `net/http/httptest` alongside `net/http`).

- `certtest.SelfSignedCA(tb)` — a fresh, throwaway self-signed CA certificate, PEM-encoded (`[]byte`); feed it to `CATransport` or an `x509.CertPool`. A new key each call, so two certs are mutually untrusted (handy for asserting a pin is enforced).
- `certtest.WriteSelfSignedCA(tb)` — the same certificate written to a `ca.pem` file (mode `0o600`) under `tb.TempDir()`, returning the path — for code under test that reads its CA from a file path.

### Hooks & Policies

- `CheckRetry` — pluggable retry policy: `func(ctx, resp, err) (bool, error)`
- `OnRetry` — per-attempt callback for observability/metrics
- `PrepareRetry` — mutate request before retry (e.g., re-sign tokens)

### Backoff Strategy

- `Backoff` — pluggable backoff interface: `NextBackOff() time.Duration` + `Reset()` (mirrors cenkalti/backoff)
- `WithBackoffFunc(func() Backoff)` — supply a factory that is called per-request to produce a fresh `Backoff` instance (each request gets an independent backoff progression)
- `NewExponentialBackoff` — create jittered exponential backoff (functional options: `WithInitialInterval`, `WithMaxElapsedTime`)
- `BackoffStop` — sentinel value to signal "stop retrying"

### Error Control

- `Permanent(err)` — wrap error to signal "do not retry" (mirrors cenkalti/backoff)
- `IsPermanent(err)` — check if error is wrapped as permanent
- `PermanentError` — the wrapper type (supports `errors.Is`/`errors.As`/`Unwrap`)

### Classification & Parsing

- `IsTransient` — classify errors as transient (retryable); respects `PermanentError`
- `RetryAfterHint` is an interface (`RetryAfterHint() time.Duration`) an error implements to supply the next retry wait; `RetryWithBackoff` honors it when the error is transient and the duration is positive (the implementer must cap the value, since httpx applies no ceiling of its own here)
- `CheckHTTPStatus` — map HTTP status to typed errors
- `ParseRetryAfter` / `ParseRetryAfterResponse` — parse Retry-After header

### Backoff Primitives

- `JitteredBackoff` — equal jitter `[backoff/2, backoff]`
- `SafeDouble` / `SleepCtx` — overflow-safe doubling, context-aware sleep

### Body Helpers

- `Drain` / `DrainClose` — body drain for connection reuse (64 KB limit)
- `LimitedBody` — wrap response body with a size cap
- `ReadLimitedBody` — read a body to a cap (closing it) with overflow detection, returning `*ResponseTooLargeError` instead of a silently truncated body

### Redirect Policies

- `DefaultRedirectPolicy` — same-host-only redirect policy (used by `NewClient`). It also refuses a same-host `https`->`http` scheme downgrade and allows an `http`->`https` upgrade, which makes it equivalent to `RedirectPolicyFunc(WithSameHost())`.
- `RefuseAllRedirects` — follows **no** redirect: returns `http.ErrUseLastResponse`, so the client surfaces the 3xx response itself (nil error) instead of following. The policy for a token-bearing client of an API that issues no redirects — Go forwards custom headers (`X-Plex-Token`, `X-Api-Key`) across redirects, so a hostile 302 would exfiltrate the credential.
- `DockerGitHubRedirectPolicy` — optional example policy for docker.com/github.com
- `RedirectPolicyFunc` — build a custom redirect allowlist (functional options: `WithAllowedHosts`, `WithAllowedSuffixes`, `WithSameHost`, `WithAllowSchemeDowngrade`, `WithMaxHops`)
  - `WithSameHost` additionally allows a redirect whose target host equals the original request's host (layered on any allowlisted hosts or suffixes); it is the building block for a same-origin policy.
  - `WithAllowSchemeDowngrade(bool)` permits an `https`->`http` downgrade redirect. The default `false` refuses it, so a custom auth header is never forwarded onto a cleartext hop; an `http`->`https` upgrade is always allowed. The downgrade is judged against the original request's scheme, and the guard applies to allowlisted and same-host targets alike.

### Client Helpers

- `NewClient` / `Close` — preconfigured HTTP client

### Secret Redaction

- `RedactTransportError` / `RedactSecret` / `RedactSecretString` — secret redaction (error- and string-level)
- `LogSafeError` — reduce a URL-embedding transport `*url.Error` to its underlying cause (everything else passes through, `errors.Is`/`As` preserved). The same reduction httpx applies to every transport error it logs; equivalent to `RedactTransportError(err, "", "")`.

### Error Types

- `AuthError` / `RateLimitError` / `HTTPStatusError` / `StatusError`
- `ResponseTooLargeError` — returned by `Retry` when the response exceeds `WithMaxBodyBytes` (carries `Limit`; no body is returned)
- `ErrRateLimited` / `ErrServerError` — sentinel errors
- `PermanentError` — do-not-retry sentinel wrapper

## Logging

`Retry` logs via `log/slog`. Pass `WithLogger` to override the default logger for `Retry` calls. `RetryWithBackoff`, `RetryOnRateLimit`, and `Drain` use `slog.Default()` and cannot be overridden per-call.

Per-attempt "retrying" lines are logged at **Debug** — a retry that recovers is normal operation, not a degraded state. Only the terminal "retries exhausted" / "rate limit retries exhausted" lines are at **Warn**. `Retry` also emits a **Warn** "slow upstream response" when a single attempt's response takes longer than 10s (timed per attempt, so backoff sleeps are not counted as upstream latency). The `RetryRoundTripper` logs nothing itself — observe its retries through the `WithOnRetry` hook.

### URL redaction in logs and errors

To avoid leaking credentials into logs (CWE-532, the class of [go-retryablehttp CVE-2024-6104](https://discuss.hashicorp.com/t/hcsec-2024-12-go-retryablehttp-can-leak-basic-auth-credentials-to-log-files/68027)), `Retry` never logs or returns a raw request URL:

- Every logged `url` attribute is redacted — the userinfo password is masked (like `url.URL.Redacted`) and query values are replaced with `REDACTED` (query values commonly carry api keys, tokens, and signatures — the same default [.NET 9's `IHttpClientFactory`](https://learn.microsoft.com/en-us/dotnet/core/compatibility/networking/9.0/query-redaction-logs) adopted). Query keys, scheme, host, and path are kept for debugging.
- `StatusError.Error()` renders that same redacted URL, so the secret stays out of returned errors too; the raw `StatusError.URL` field remains available for programmatic use.
- Transport errors (`*url.Error`, which embed the full URL) are reduced to their underlying cause before logging — the reduction is exported as `LogSafeError` so callers wrapping transport errors into their own messages can apply the same one.

The `RoundTripper` performs no URL logging of its own — wire any logging through its `WithOnRetry` hook, where redaction is the caller's responsibility.

## Retry exhaustion

`Retry` and the `RetryRoundTripper` report exhaustion differently — match your error handling to the one you use:

- **`Retry`** returns `nil` body and a wrapped error: `retries exhausted after <elapsed>: <lastErr>` (unwrap with `errors.Is`/`errors.As`). A response that overflows `WithMaxBodyBytes` returns `*ResponseTooLargeError` (no body).
- **`RetryRoundTripper`** returns the **last response with a nil error**, even when that response is a retryable 5xx (e.g. a 503) — mirroring how a non-retried request behaves. A caller that checks only `err != nil` will treat an exhausted 503 as success, so **inspect `resp.StatusCode` and close the body**. (A budget abort via `WithRTMaxElapsedTime` or a `BackoffStop` does return an error.)

## Timeouts and deadlines

httpx retries transient failures, not budget expiry, and that distinction drives how you should bound a retried call. `IsTransient` classifies `context.DeadlineExceeded` and `context.Canceled` as **non-transient** (checked first), while a transport-level `net.Error` timeout, a connection reset, a DNS error, and a 429/5xx are transient. So a context deadline means "the budget is exhausted, stop", and a transport timeout means "this attempt failed, try again".

- **Total budget: a context deadline.** Pass a `context.WithTimeout` (or a caller-supplied deadline) as the single authoritative bound over the whole operation. `Retry` / `RetryWithBackoff` stop the moment `ctx` is done, and `SleepCtx` caps the backoff by it, so the deadline spans every attempt and every backoff sleep. On expiry the call ends; it is terminal, not retried.
- **Per-attempt bound.** Where a per-attempt cap lives depends on the retry entry point, because that is what decides whether `http.Client.Timeout` is per-attempt or total:
  - With the one-shot `Retry` / `RetryWithBackoff` (the retry loop runs _outside_ `client.Do`), an `http.Client.Timeout` bounds each attempt and fires as a `net.Error` timeout, so it is **retried**; a `context.WithTimeout` wrapped inside the retry `fn` is instead a context deadline and is **terminal**. Choose the stall behavior you want.
  - With `NewRetryRoundTripper` (the retry loop runs _inside_ `client.Do`), `http.Client.Timeout` is NOT per-attempt: it caps the whole retry sequence, and because it is a context deadline a slow attempt that trips it aborts the remaining retries. For a per-attempt bound here, set a transport timeout such as `ResponseHeaderTimeout` on the base transport (it fires as a retryable `net.Error`); bound the total with the caller's context or `WithRTMaxElapsedTime`.
- **RoundTripper budget.** `NewRetryRoundTripper` takes `WithRTMaxElapsedTime` as a hard total-time ceiling across retries (computed after any honored `Retry-After`). Pair it with a per-attempt **transport** timeout (e.g. `ResponseHeaderTimeout`); do **not** wrap the client in an `http.Client.Timeout`, which is a total cap in disguise and defeats the retries it sits above.

**Recommended:** give the operation a context deadline as its total budget (honored end-to-end, and it keeps a slow attempt from running unbounded), and add a per-attempt bound only when a single try needs its own cap. Through the one-shot `Retry` helper a bare `http.Client.Timeout` with no context deadline is fine for a simple call to a trusted or local endpoint (there it is per-attempt, so a retried call can run up to `maxAttempts` times its value); under `NewRetryRoundTripper` reach for `ResponseHeaderTimeout` plus a total from the context or `WithRTMaxElapsedTime` instead.

A per-attempt timeout that is **itself retried** (a stalled attempt abandoned and a fresh one tried within the remaining total budget, the gRPC per-try-timeout model) is not built into the retry primitives today. Approximate it by pairing a per-attempt bound (a `context.WithTimeout` inside the one-shot `fn`, or `ResponseHeaderTimeout` under the RoundTripper) with an outer context deadline or `WithRTMaxElapsedTime` for the total.

## Unsupported by Design (SKIP List)

The following features are intentionally not provided:

| Feature                                                              | Rationale                                                                                                                                               |
| -------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Circuit breaker                                                      | Orthogonal pattern excluded by all comparables. Compose externally with sony/gobreaker.                                                                 |
| Retry budget / token bucket                                          | None of the comparables implement it. Disproportionate complexity (~150 LOC + shared mutable state) for a focused library.                              |
| Multiple jitter strategies (full, decorrelated)                      | Equal jitter is the recommended default per AWS Builders' Library. Full jitter risks near-zero delays.                                                  |
| `ErrorHandler` for exhaustion                                        | Current `fmt.Errorf("retries exhausted: %w", lastErr)` is sufficient. Callers unwrap.                                                                   |
| Response body on error                                               | Adds API complexity (ownership of body close). Use `RetryWithBackoff[T]` with custom logic.                                                             |
| Idempotency key injection                                            | Application-level concern, not a retry library's responsibility.                                                                                        |
| Configurable Retry-After cap / per-call `WithLogger` on the generics | A raisable cap would regress the fixed-60s DoS ceiling; a per-call logger on the positional generics forces an options refactor for no consumer demand. |

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude Opus](https://www.anthropic.com/claude) and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

GPL-3.0 — see [LICENSE](LICENSE).
