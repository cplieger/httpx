# Retries

This page is for a developer choosing between the three ways httpx retries a call and tuning one of them. It covers the options, which failures each one retries, how waits are computed, what happens when attempts run out, and what gets logged.

## Three ways to retry

Each has a different owner for the request, the body and the response. They share one backoff progression.

| Call | You give it | It returns | Use it when |
| --- | --- | --- | --- |
| `Do[T]` | a function returning `(T, error)` | the typed result | you build the request and decode the response yourself |
| `GetBytes` | a client and a URL | the body bytes, capped | you want a GET whose body comes back as `[]byte` |
| `NewRetryRoundTripper` | a base transport and a `TransportConfig` | an `http.RoundTripper` | code you do not control already takes an `*http.Client` |

`NewRetryClient(base, policy, cfg)` builds an `*http.Client` from a `RetryRoundTripper` and the redirect policy you pass. The policy is required and a nil one panics, because a nil `CheckRedirect` means net/http follows redirects to any host. The client sets no `Client.Timeout`, for the reason in [Timeouts and deadlines](timeouts.md).

`NewClient(timeout)` is a plain client with that timeout and `DefaultRedirectPolicy` installed. It does not retry by itself, so it is the client to hand to `GetBytes`.

`ContextWithDefaultTimeout(ctx, def)` bounds `ctx` by `def` only when the caller set no deadline. A caller's deadline is never shortened or extended, a `def` of zero or less means no default, and the returned cancel function is never nil.

## Options for `Do` and `GetBytes`

`Do` and `GetBytes` take functional options. They are typed, so passing a `Do` option to `GetBytes`, or the reverse, does not compile.

| Option | Accepted by | Default | Effect |
| --- | --- | --- | --- |
| `WithMaxAttempts(n)` | both | 3 | Total attempts, the first included. A value below 1 means exactly one attempt |
| `WithBaseDelay(d)` | both | 1s | First backoff delay. A value of zero or less takes the default |
| `WithLogger(l)` | both | `slog.Default()` | Logger for the retry lines. A nil logger takes the default |
| `WithExhaustedLevel(level)` | both | see [Logging](#logging) | Level of the final "retries exhausted" line |
| `WithLabel(s)` | `Do` | `operation` | Name used in `Do`'s log lines |
| `WithAttemptTimeout(d)` | `Do` | none | Bounds each attempt and makes its expiry retryable |
| `WithRateLimitRetry(maxWait)` | `Do` | off | Retries `*RateLimitError` as well as transient errors |
| `WithRateLimitOnly(maxWait)` | `Do` | off | Retries `*RateLimitError` and nothing else |
| `WithHeaders(fn)` | `GetBytes` | none | Called to set headers on each request |
| `WithMaxBodyBytes(n)` | `GetBytes` | 10 MB | Body cap. A value of zero or less takes the default |

`WithRateLimitRetry` and `WithRateLimitOnly` together are a configuration error, which `Do` returns before calling your function.

## Options for the transport

`TransportConfig{}` is ready to use. It gives three attempts, a one-second base delay and the default retry policy.

| Field | Effect |
| --- | --- |
| `MaxAttempts` | Total attempts. Zero means 3, and a negative value means exactly one attempt |
| `BaseDelay` | First backoff delay. Zero or less means 1s |
| `CheckRetry` | Replaces the retry policy, `func(ctx, resp, err) (bool, error)`. A returned error stops the loop |
| `OnRetry` | Called before each retry, with a 1-based attempt number. The transport logs nothing, so this is where to observe it |
| `PrepareRetry` | Changes the cloned request before each retry, for example to set a fresh token |
| `MaxElapsedTime` | Ceiling on total time across retries, honored `Retry-After` waits included. Checked between attempts. Zero means none |
| `RetryNonIdempotent` | Also retries POST, PUT, PATCH and DELETE, replaying the body through `req.GetBody` |

The transport never changes your request. It clones it for each attempt. By default it retries only GET, HEAD, OPTIONS and TRACE. With `RetryNonIdempotent`, a request with a body is retried only when it has a `GetBody` function.

Each `RoundTrip` keeps its own backoff, so one `RetryRoundTripper` is safe to share between goroutines.

## What each one retries

| Failure | `Do` | `GetBytes` | Transport |
| --- | --- | --- | --- |
| Transient network error | yes | yes | yes |
| 408 | sees no status | yes | no |
| 429 | only with a rate-limit option | yes | yes |
| 500, 501 and 505 to 599 | sees no status | yes | no |
| 502, 503, 504 | sees no status | yes | yes |
| Other 4xx | sees no status | no | no |

`Do` sees only the error your function returns, and `IsTransient` decides whether it is worth another attempt. When your function returns the error from `CheckHTTPStatus`, `Do` retries a 502, 503 or 504 and nothing else, because only those `*HTTPStatusError` values report themselves transient. A transient error is a network timeout, a connection reset, refused or broken pipe, an unexpected end of body, a DNS failure, or an error whose `IsTransient()` method says so. `IsTransient` never accepts a `*PermanentError`, an `*AuthError`, a `*RateLimitError` or an expired or canceled caller context.

Pass `IsRetryableStatus` through `TransportConfig.CheckRetry` to give the transport the same status set as `GetBytes`. `IsRetryableStatus(code)` is true for 408, 429 and any 5xx, and `GetBytes` calls it itself, so the two cannot drift apart.

## Waits and Retry-After

The wait before the next attempt is a random time between half and all of the current delay, which starts at `BaseDelay` and doubles after each retry. This is equal jitter, the only strategy httpx provides.

`GetBytes` and the transport replace that wait with the response's `Retry-After` header when it has one, capped at 60 seconds. The transport honors it on any response it retries.

In `Do`, an error that implements `RetryAfterHint` with a positive duration replaces the wait instead. httpx does not cap that value, so the type that returns it must. The doubling delay keeps advancing underneath either override.

`ParseRetryAfter` reads a header value in seconds or as an HTTP date and caps it at `RetryAfterCap`, 60 seconds. `ParseRetryAfterResponse` reads it from a response and does not cap it. A missing, malformed or past value is zero.

## Rate limits

`Do` does not retry a `*RateLimitError` by default, so a generic operation never re-fires a rate-limited call on its own.

- `WithRateLimitRetry(maxWait)` adds rate limits to the retryable set. Before a rate-limit retry, `Do` waits the error's `Retry-After` when it has one, but never longer than `maxWait`. Without a hint it waits `maxWait`.
- `WithRateLimitOnly(maxWait)` retries rate limits with the same waits and returns every other error at once, transient ones included. Its final log line reads "rate limit retries exhausted". The error of the last attempt is returned even when the context was already canceled.

A `maxWait` of zero or less becomes 60 seconds, so the wait between attempts is never zero.

## Marking errors

- `Permanent(err)` makes `IsTransient` reject `err`, so `Do` and the default transport policy never retry it. It is the same idea as cenkalti/backoff. `IsPermanent` tests for it, and `*PermanentError` works with `errors.Is`, `errors.As` and `Unwrap`.
- `MarkTransient(err)` tells `Do` to retry an error the shared rules would not, such as a server fault inside a 200 envelope. It overrides a non-transient verdict already on the error. It cannot override `Permanent`, an `*AuthError`, a `*RateLimitError` or a caller context error. For a rate limit, name a wait budget with `WithRateLimitRetry` instead.
- `AttemptTimeout(err)` marks a timeout as the end of one attempt, so it is retried. `IsAttemptTimeout` tests for it. [Timeouts and deadlines](timeouts.md) explains when to use it.
- Your own error type can implement `Transient`, an `error` with `IsTransient() bool`, or `RetryAfterHint`, an `error` with `RetryAfterHint() time.Duration`. Both embed `error`, so `errors.AsType[httpx.Transient](err)` compiles.

## When attempts run out

`Do` returns the last error, or `ctx.Err()` when the context ended after a failed attempt.

`GetBytes` returns a nil body and an error reading `retries exhausted after <elapsed>: <last error>`, which `errors.Is` and `errors.As` can unwrap. A body over the cap is a `*ResponseTooLargeError` and returns no body. When the last attempt carried a `Retry-After`, the error also implements `RetryAfterHint` with that capped wait. It does not implement `Transient`, because whether an exhausted GET deserves another outer attempt is your decision.

The transport returns the last response with a nil error, even when that response is a retryable 503. A caller that checks only `err != nil` treats an exhausted 503 as success, so check `resp.StatusCode` and close the body. Only a `MaxElapsedTime` stop returns an error.

## Running GetBytes inside your own retry loop

A 3-attempt `GetBytes` inside a 3-attempt loop makes up to 9 requests. Run it with `WithMaxAttempts(1)` and let your loop decide:

```go
body, err := httpx.GetBytes(ctx, client, url, httpx.WithMaxAttempts(1))
if err != nil {
    // A status that clears by itself is worth another of this loop's attempts.
    if se, ok := errors.AsType[*httpx.StatusError](err); ok && httpx.IsRetryableStatus(se.Code) {
        return httpx.MarkTransient(err)
    }
    return err // an auth or configuration failure ends the loop on its first attempt
}
```

Because the exhaustion error keeps the `Retry-After` hint, an enclosing `Do` waits what the server asked for instead of its own backoff.

## Logging

`Do` and `GetBytes` log through `log/slog`, to `WithLogger` or `slog.Default()`.

| Line | Level |
| --- | --- |
| Each failed attempt that will be retried | Debug |
| A `Do` success after a retry | Debug |
| Attempts ran out, with more than one attempt allowed | Warn |
| Attempts ran out under `WithMaxAttempts(1)` | Debug |
| A successful `GetBytes` attempt that took more than 10 seconds | Warn |

With a single attempt nothing was retried, so the loop around the call owns the warning. `WithExhaustedLevel(level)` sets the level of the final line for any attempt count, and it is the only way to raise it above Warn. Use it to demote the line when your own failure log carries more context, without losing the Debug lines a discard logger would also drop.

The slow-response timer runs per attempt, so backoff sleeps never count as upstream latency. The transport logs nothing, so observe its retries through `OnRetry`, where redacting the URL is up to you. Every URL these lines carry is redacted as [Redirects, TLS and redaction](security.md) describes.

## Backoff building blocks

- `JitteredBackoff(d)` returns a random duration in `[d/2, d]`.
- `SafeDouble(d)` doubles `d` and stops at the largest duration instead of overflowing.
- `SleepCtx(ctx, d)` sleeps for `d` and returns early with `ctx.Err()` when the context ends.
