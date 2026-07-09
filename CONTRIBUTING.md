# Contributing to httpx

Notes specific to this library. For org-wide defaults (workflow, PR
template) see the
[generic guide](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md);
this file covers what makes httpx different.

## What this library is

A single-package, zero-runtime-dependency Go toolkit for resilient outbound
HTTP: jittered exponential backoff, transient-error classification, Retry-After
parsing, HTTP status→typed-error mapping, secret redaction, body draining, a
transparent retrying `http.RoundTripper` with body replay, and a configurable
redirect allowlist, and a custom-CA TLS transport. Everything lives in the root
package `httpx` across three files: `httpx.go` (errors, backoff, retry, parsing,
redirect, redaction), `roundtripper.go` (the `RetryRoundTripper`), and `tls.go`
(the custom-CA TLS transport and `ErrNoCertsInPEM`).

## The SKIP list is a contract, not a backlog

`README.md` has an
"[Unsupported by Design (SKIP List)](README.md#unsupported-by-design-skip-list)"
table of deliberate non-goals with documented rationale; **do not implement
them**. If you believe one belongs in scope, open an issue to change the
contract first; don't send a PR that quietly adds it.

## Design invariants to preserve

- **One retry-count model: total attempts, minimum 1.** Every entry point
  (`Retry`, `RetryWithBackoff`, `RetryOnRateLimit`, `RetryRoundTripper`) counts
  the TOTAL number of executions including the first; a non-positive count
  clamps to 1 (try exactly once) — never a silent zero-attempt no-op and never a
  coercion to the default. `v2_test.go` pins the exact counts for
  `maxAttempts ∈ {0,1,2,3}`; keep them green.
- **Equal jitter only.** `JitteredBackoff` returns `[backoff/2, backoff]` (AWS
  Builders' Library default). Full jitter risks near-zero delays and is
  excluded on purpose.
- **Zero runtime dependencies.** `go.mod` requires only `pgregory.net/rapid`,
  and that is test-only. Don't add a runtime dependency.
- **Mirror the reference APIs.** `RetryRoundTripper` mirrors
  hashicorp/go-retryablehttp; `Permanent`/`IsPermanent` and the `Backoff`
  interface mirror cenkalti/backoff. Keep names and semantics aligned with
  those so the library stays a drop-in mental model.
- **The RoundTripper never mutates the caller's request.** `RoundTrip` clones
  via `req.Clone(ctx)` per attempt; body replay goes through `req.GetBody`.
  Retrying non-idempotent methods is opt-in (`WithRetryNonIdempotent(true)` plus
  a `GetBody`).
- **Per-request backoff, no shared state.** `WithBackoffFunc` is a factory
  invoked once per `RoundTrip`, so each request drives its own `Backoff`
  instance; `RetryRoundTripper` holds no shared `Backoff` or mutex. Don't
  reintroduce a shared backoff instance — it corrupts progression across
  goroutines that share one `StandardClient()`.
- **RoundTrip exhaustion returns the last response, not an error.** When retries
  are exhausted, `RoundTrip` returns the last `*http.Response` with a nil error
  (even a retryable 503), mirroring stdlib (a 5xx is not a transport error); only
  a `WithRTMaxElapsedTime` abort or a `BackoffStop` returns an error. `Retry`, by
  contrast, returns `retries exhausted: %w`. Preserve both contracts — consumers
  branch on them.
- **Overflow- and context-safety.** `SafeDouble` guards against `int64`
  overflow; `SleepCtx` is cancellation-aware; `ParseRetryAfter` caps at
  `RetryAfterCap`. Preserve these guards when touching backoff/parse code.

## Never log or wrap a raw URL

`Retry` must never emit a raw request URL into a log attribute or a returned
error (CWE-532 — the bug class behind go-retryablehttp's CVE-2024-6104). The
unexported `redactURL` masks userinfo and query values, and `logSafeError`
reduces URL-bearing `*url.Error`s to their cause. Both are intentionally
unexported, so adding redaction does not grow the public surface — ship it as a
`sec:` commit, not a `feat:`. `StatusError.Error()` is redacted too, while the
raw `StatusError.URL` field stays available for programmatic use.

## Local checks

Standard Go toolchain; no Makefile. Run from the repo root:

```sh
go build ./...
go test ./...
go test -race ./...
golangci-lint run
golangci-lint fmt        # applies gofumpt + gci; `run` also flags unformatted files
```

CI is centralized (`.github/workflows/ci.yaml` calls `cplieger/ci`); these are
the same gates it enforces. `.golangci.yaml` is v2 with `govet` enable-all,
`gofumpt` extra-rules, and `gci` (standard → third-party import grouping) — let
`golangci-lint fmt` settle imports rather than hand-ordering them.

## Tests, properties, and fuzzing

Tests double as the spec, so match the existing style when adding behavior:

- **Examples** (`example_test.go`) are runnable docs — keep `ExampleRetry`
  et al. compiling and their `// Output:` accurate.
- **Property tests** (`prop_test.go`) use `pgregory.net/rapid`; invariants like
  backoff bounds and parse round-trips belong here.
- **Fuzz targets** exist for the parsing/redaction/backoff/redirect surface
  (`FuzzParseRetryAfter`, `FuzzParseRetryAfterResponse`,
  `FuzzRedactTransportError`, `FuzzSafeDouble`, `FuzzRedirectPolicyFunc`,
  `FuzzSameOriginRedirect`). Run one with, e.g.,
  `go test -run=^$ -fuzz=FuzzParseRetryAfter -fuzztime=30s`.

New parsing, classification, or redirect logic should land with a property test
or fuzz target, not just table tests.

## Commits & PRs

Conventional Commits, parsed by git-cliff for release notes and version bumps
(see `cliff.toml`): `feat:` → minor, `fix:`/`refactor:`/`perf:` → patch under
Changed, `sec:` → Security. Use `sec:` for redaction/hardening fixes that add no
public API. Branch from `main`, keep the change focused, and open a PR.

## Conduct & security

By participating you agree to the
[Code of Conduct](https://github.com/cplieger/.github/blob/main/CODE_OF_CONDUCT.md).
Report vulnerabilities through the
[security policy](https://github.com/cplieger/.github/blob/main/SECURITY.md) —
never in a public issue.
