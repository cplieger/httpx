# httpx
> Resilient outbound-HTTP toolkit for Go: retry, backoff, transient-error classification, and more.

A standalone library extracted from subflux and registry-stats providing jittered exponential backoff, transient-error classification, Retry-After parsing, HTTP status mapping, secret redaction, body draining, and a configurable redirect allowlist. Zero dependencies beyond the Go standard library and pgregory.net/rapid (test only).

## Install
<!-- TODO: registry/pull link -->
`go get github.com/cplieger/httpx@latest`

## Usage
```go
// Simple GET with retry
body, err := httpx.Retry(ctx, http.DefaultClient, url, httpx.Options{
    MaxAttempts: 3,
    BaseDelay:   time.Second,
})

// Generic retry with backoff
result, err := httpx.RetryWithBackoff(ctx, 3, time.Second, "fetch", func(ctx context.Context) (T, error) {
    return doWork(ctx)
})

// Transient error classification
if httpx.IsTransient(err) { /* safe to retry */ }
```

## API
- `Retry` — HTTP GET with exponential backoff on 429/5xx
- `RetryWithBackoff[T]` — generic retry with jittered exponential backoff
- `RetryOnRateLimit` — retry on `*RateLimitError` only
- `IsTransient` — classify errors as transient (retryable)
- `CheckHTTPStatus` — map HTTP status to typed errors
- `ParseRetryAfter` / `ParseRetryAfterResponse` — parse Retry-After header
- `JitteredBackoff` / `SafeDouble` / `SleepCtx` — backoff primitives
- `Drain` / `DrainClose` — body drain for connection reuse
- `RedirectPolicy` / `RedirectPolicyFunc` — configurable redirect allowlist
- `NewClient` / `Close` — preconfigured HTTP client
- `RedactTransportError` / `RedactSecret` — secret redaction
- `AuthError` / `RateLimitError` / `HTTPStatusError` / `StatusError` — error types
- `ErrRateLimited` / `ErrServerError` — sentinel errors

## License
GPL-3.0 — see [LICENSE](LICENSE).
