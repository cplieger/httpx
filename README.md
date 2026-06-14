# httpx

[![CI](https://github.com/cplieger/httpx/actions/workflows/ci.yaml/badge.svg)](https://github.com/cplieger/httpx/actions/workflows/ci.yaml)
[![Go Reference](https://pkg.go.dev/badge/github.com/cplieger/httpx.svg)](https://pkg.go.dev/github.com/cplieger/httpx)
[![Go Report Card](https://goreportcard.com/badge/github.com/cplieger/httpx)](https://goreportcard.com/report/github.com/cplieger/httpx)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/httpx/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/httpx)
[![Coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/httpx/badges/coverage.json)](https://github.com/cplieger/httpx/actions/workflows/coverage.yml)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13213/badge)](https://www.bestpractices.dev/projects/13213)

> Resilient outbound-HTTP toolkit for Go: retry, backoff, transient-error classification, and more.

A resilient outbound-HTTP toolkit for Go providing jittered exponential backoff, transient-error classification, Retry-After parsing, HTTP status mapping, secret redaction, body draining, a transparent retrying `http.RoundTripper` with body replay, and a configurable redirect allowlist. Zero dependencies beyond the Go standard library and pgregory.net/rapid (test only).

## Install

`go get github.com/cplieger/httpx@latest`

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

// Transparent retrying RoundTripper (mirrors hashicorp/go-retryablehttp)
rt := httpx.NewRetryRoundTripper(http.DefaultTransport,
    httpx.WithMaxRetries(3),
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

// Retry POST/PUT with body replay (opt-in, mirrors go-retryablehttp)
rt := httpx.NewRetryRoundTripper(http.DefaultTransport,
    httpx.WithMaxRetries(3),
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

// Pluggable backoff strategy
rt := httpx.NewRetryRoundTripper(http.DefaultTransport,
    httpx.WithBackoff(httpx.NewExponentialBackoff(
        httpx.WithInitialInterval(500*time.Millisecond),
        httpx.WithMaxElapsedTime(30*time.Second),
    )),
)

// Custom redirect policy
policy := httpx.RedirectPolicyFunc(
    httpx.WithAllowedHosts("api.example.com"),
    httpx.WithAllowedSuffixes(".cdn.example.com"),
    httpx.WithMaxHops(3),
)

// Transient error classification
if httpx.IsTransient(err) { /* safe to retry */ }

// Limit response body size
rc := httpx.LimitedBody(resp, 1<<20) // 1 MB cap
defer rc.Close()
```

## API

### Retry

- `Retry` — HTTP GET with exponential backoff on 429/5xx (functional options: `WithMaxAttempts`, `WithBaseDelay`, `WithMaxBodyBytes`, `WithHeaders`, `WithLogger`)
- `RetryWithBackoff[T]` — generic retry with jittered exponential backoff
- `RetryOnRateLimit` — retry on `*RateLimitError` only (passes ctx to fn)
- `NewRetryRoundTripper` — create a retrying `http.RoundTripper` (functional options: `WithMaxRetries`, `WithRTBaseDelay`, `WithRTMaxElapsedTime`, `WithBackoff`, `WithCheckRetry`, `WithOnRetry`, `WithPrepareRetry`, `WithRetryNonIdempotent`)
- `StandardClient()` — returns `*http.Client` using the `RetryRoundTripper`

### Hooks & Policies

- `CheckRetry` — pluggable retry policy: `func(ctx, resp, err) (bool, error)`
- `OnRetry` — per-attempt callback for observability/metrics
- `PrepareRetry` — mutate request before retry (e.g., re-sign tokens)

### Backoff Strategy

- `Backoff` — pluggable backoff interface: `NextBackOff() time.Duration` + `Reset()` (mirrors cenkalti/backoff)
- `NewExponentialBackoff` — create jittered exponential backoff (functional options: `WithInitialInterval`, `WithMaxElapsedTime`)
- `BackoffStop` — sentinel value to signal "stop retrying"

### Error Control

- `Permanent(err)` — wrap error to signal "do not retry" (mirrors cenkalti/backoff)
- `IsPermanent(err)` — check if error is wrapped as permanent
- `PermanentError` — the wrapper type (supports `errors.Is`/`errors.As`/`Unwrap`)

### Classification & Parsing

- `IsTransient` — classify errors as transient (retryable); respects `PermanentError`
- `CheckHTTPStatus` — map HTTP status to typed errors
- `ParseRetryAfter` / `ParseRetryAfterResponse` — parse Retry-After header

### Backoff Primitives

- `JitteredBackoff` — equal jitter `[backoff/2, backoff]`
- `SafeDouble` / `SleepCtx` — overflow-safe doubling, context-aware sleep

### Body Helpers

- `Drain` / `DrainClose` — body drain for connection reuse (64 KB limit)
- `LimitedBody` — wrap response body with a size cap

### Redirect Policies

- `DefaultRedirectPolicy` — same-host-only redirect policy (used by `NewClient`)
- `DockerGitHubRedirectPolicy` — optional example policy for docker.com/github.com
- `RedirectPolicyFunc` — build a custom redirect allowlist (functional options: `WithAllowedHosts`, `WithAllowedSuffixes`, `WithMaxHops`)

### Client Helpers

- `NewClient` / `Close` — preconfigured HTTP client

### Secret Redaction

- `RedactTransportError` / `RedactSecret` — secret redaction

### Error Types

- `AuthError` / `RateLimitError` / `HTTPStatusError` / `StatusError`
- `ErrRateLimited` / `ErrServerError` — sentinel errors
- `PermanentError` — do-not-retry sentinel wrapper

## Logging

`Retry` logs via `log/slog`. Pass `WithLogger` to override the default logger for `Retry` calls. `RetryWithBackoff` and `Drain` use `slog.Default()` and cannot be overridden per-call.

### URL redaction in logs and errors

To avoid leaking credentials into logs (CWE-532, the class of [go-retryablehttp CVE-2024-6104](https://discuss.hashicorp.com/t/hcsec-2024-12-go-retryablehttp-can-leak-basic-auth-credentials-to-log-files/68027)), `Retry` never logs or returns a raw request URL:

- Every logged `url` attribute is redacted — the userinfo password is masked (like `url.URL.Redacted`) and query values are replaced with `REDACTED` (query values commonly carry api keys, tokens, and signatures — the same default [.NET 9's `IHttpClientFactory`](https://learn.microsoft.com/en-us/dotnet/core/compatibility/networking/9.0/query-redaction-logs) adopted). Query keys, scheme, host, and path are kept for debugging.
- `StatusError.Error()` renders that same redacted URL, so the secret stays out of returned errors too; the raw `StatusError.URL` field remains available for programmatic use.
- Transport errors (`*url.Error`, which embed the full URL) are reduced to their underlying cause before logging.

The `RoundTripper` performs no URL logging of its own — wire any logging through its `WithOnRetry` hook, where redaction is the caller's responsibility.

## Unsupported by Design (SKIP List)

The following features are intentionally not provided:

| Feature | Rationale |
|---------|-----------|
| Circuit breaker | Orthogonal pattern excluded by all comparables. Compose externally with sony/gobreaker. |
| Retry budget / token bucket | None of the comparables implement it. Disproportionate complexity (~150 LOC + shared mutable state) for a focused library. |
| Multiple jitter strategies (full, decorrelated) | Equal jitter is the recommended default per AWS Builders' Library. Full jitter risks near-zero delays. |
| `ErrorHandler` for exhaustion | Current `fmt.Errorf("retries exhausted: %w", lastErr)` is sufficient. Callers unwrap. |
| Response body on error | Adds API complexity (ownership of body close). Use `RetryWithBackoff[T]` with custom logic. |
| Idempotency key injection | Application-level concern, not a retry library's responsibility. |

## License

GPL-3.0 — see [LICENSE](LICENSE).
