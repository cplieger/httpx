# Status codes, errors and bodies

This page is for a developer handling what comes back from a call. It covers how httpx turns a status into an error, which error types to match, how bodies are capped and drained, and how a conditional GET works.

## Status checking

`CheckHTTPStatus(resp)` returns nil for a 2xx status, 200 to 299, and an error for every other one.

| Status | Result |
| --- | --- |
| 2xx | `nil` |
| 3xx | `*HTTPStatusError{Code}`, not transient, neither a client nor a server error |
| 401, 403 | `*AuthError`, whose message carries `(401)` or `(403)` |
| 429 | `*RateLimitError` with the raw, uncapped `Retry-After` value |
| other 4xx and 5xx | `*HTTPStatusError{Code}`, transient for 502, 503 and 504 |
| 1xx | `*HTTPStatusError{Code}`, not a completed response |

A 3xx reaches your code only when the client does not follow redirects. That happens under `RefuseAllRedirects`, or any `CheckRedirect` returning `http.ErrUseLastResponse`, and net/http then returns the 3xx response itself with a nil error. A followed redirect never surfaces a 3xx, so a client on `DefaultRedirectPolicy` or an allowlist sees no difference.

Treating that 3xx as an error matters for a client that carries a token and refuses redirects on purpose. Otherwise the unfollowed redirect would pass as a completed request. If one call site must accept a 3xx, check `resp.StatusCode` there.

A 3xx is an ordinary `*HTTPStatusError`, so the rest of the package handles it unchanged. `IsTransient` reports false, `IsServerError` and `IsClientError` both report false, and `LogSafeError` and the redaction helpers pass it through because it carries no URL. `CheckHTTPStatus` is the only status classifier in the package.

## Error types

- `*AuthError` for 401 and 403. Tell them apart by the `(401)` or `(403)` in its message.
- `*RateLimitError` for 429. Its `RetryAfter` field is the raw header value, so bound it before sleeping on it. The rate-limit options of `Do` do.
- `*HTTPStatusError` for every other non-2xx status, with the code in `Code`. It implements `Transient`, true for 502, 503 and 504.
- `*StatusError` is what `GetBytes` returns for a status it does not retry. For a retried status it sits inside the exhaustion error. Its `Error()` text shows the redacted URL. The raw URL stays in the `URL` field for your code to use, so do not log or serialize that field.
- `*ResponseTooLargeError` means a body went over its cap. It carries `Limit`, and no body is returned with it.
- `*PermanentError` wraps an error that must not be retried. It works with `errors.Is`, `errors.As` and `Unwrap`.
- `ErrRateLimited` and `ErrServerError` are sentinel errors.

## Body helpers

| Helper | What it does |
| --- | --- |
| `ReadLimitedBody(body, limit)` | Reads up to `limit` bytes and closes the body. Returns `*ResponseTooLargeError` instead of a cut body when there is more |
| `LimitedBody(resp, limit)` | Wraps `resp.Body` so reads stop at `limit` bytes. Reading and overflow handling stay with you |
| `Drain(body)` | Reads and discards up to 64 KB so the connection can be reused |
| `DrainClose(body)` | `Drain`, then `Close` |

`GetBytes` and `DoConditional` read with the same cap-plus-one check as `ReadLimitedBody`, 10 MB unless you set another limit.

`Drain` still matters on Go 1.27, where an HTTP/1 response body drains itself on `Close`. The standard library does not do this for HTTP/2, which every transport httpx builds attempts, and `Drain` also works on a body that is not an `*http.Response` body.

A failed drain only costs connection reuse. `Drain` logs one Debug line, `failed to drain response body`, with no attributes, on `slog.Default()`. It never logs the read error, because the far end writes that text and it can echo a credential from the request URL. [Redirects, TLS and redaction](security.md) explains the case.

## Conditional GET

`DoConditional(client, req, v, maxBodyBytes)` sends one request with the cache validators from a previous 200, so an unchanged resource comes back as a short 304 instead of a full download.

- `Validators{ETag, LastModified}` holds the validators. `ETag` is sent as `If-None-Match` and `LastModified` as `If-Modified-Since`. An empty field is not sent.
- `ConditionalResult{Validators, Body, NotModified}` is one outcome.

`DoConditional` owns both conditional headers. It removes any already on `req`, so `v` alone decides what is sent.

| Response | Result |
| --- | --- |
| 304 | `NotModified` is true and `Validators` is zero, so keep the ones you sent |
| 200 | the body, capped at `maxBodyBytes`, plus the response's fresh validators |
| any other non-2xx | the `CheckHTTPStatus` error for that status |
| any other 2xx, such as 204 or 206 | a plain error, because it carries nothing to cache |

A transport error is reduced with `LogSafeError`, so its text never holds the raw URL. Its cause still classifies as transient when `Do` wraps the call.

Validators are checked in both directions against the HTTP header value rules, with a 1 KiB limit per value. An invalid value from the server is captured as empty, so it never enters your stored cache. An invalid value you replay is not sent, so the request becomes an unconditional GET and the next clean 200 repairs the cache.

`DoConditional` is a single attempt, so the retry and cache policy are yours:

- wrap it in `Do` for retries, and build a new request for each attempt
- store the body and its validators together
- send zero `Validators` when the cached body cannot be used, so an empty cache is never answered with a 304

It is meant for GET, or HEAD, where `Body` stays empty. The full contract is on [pkg.go.dev](https://pkg.go.dev/github.com/cplieger/httpx/v5#DoConditional).
