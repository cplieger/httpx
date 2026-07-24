# Contributing to httpx

Notes specific to this library. For org-wide defaults (workflow, PR
template) see the
[generic guide](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md);
this file covers what makes httpx different.

## What this library is

A zero-runtime-dependency Go toolkit for resilient outbound HTTP: jittered
exponential backoff, transient-error classification, Retry-After parsing, HTTP
status→typed-error mapping, secret redaction, body draining, a transparent
retrying `http.RoundTripper` with body replay, a configurable redirect
allowlist, and a custom-CA TLS transport. The production toolkit is a single
package: everything lives in the root package `httpx` across five files plus
`doc.go` (the package overview): `httpx.go` (errors, backoff primitives,
parsing, classification, redirect policies, redaction), `loop.go` (the shared
retry loop behind `Do` and `GetBytes`, and the per-door options),
`roundtripper.go` (the `RetryRoundTripper` and `NewRetryClient`),
`conditional.go` (`DoConditional`), and `tls.go` (the custom-CA TLS transport
and `ErrNoCertsInPEM`). A test-support subpackage, `certtest`, adds throwaway
self-signed CA helpers for consumers' TLS tests; it is imported only from
`_test.go` files, so its certificate-generation code never links into a
production binary.

## The SKIP list is a contract, not a backlog

`README.md` has an
"[Unsupported by Design](README.md#unsupported-by-design)"
table of deliberate non-goals with documented rationale; **do not implement
them**. If you believe one belongs in scope, open an issue to change the
contract first; don't send a PR that quietly adds it.

## Design invariants to preserve

- **One retry-count model: total attempts, minimum 1.** The loop doors (`Do`,
  `GetBytes`) count the TOTAL number of executions including the first; a
  non-positive count clamps to 1 (try exactly once), never a silent
  zero-attempt no-op and never a coercion to the default. The transport is the
  one deliberate variation: `TransportConfig.MaxAttempts` zero means unset
  (default 3) and a NEGATIVE value means exactly one attempt, because a zero
  struct field cannot distinguish "unset" from "try once". `contract_test.go`
  pins the exact counts for both; keep them green.
- **Equal jitter only.** `JitteredBackoff` returns `[backoff/2, backoff]` (AWS
  Builders' Library default). Full jitter risks near-zero delays and is
  excluded on purpose.
- **Zero runtime dependencies.** `go.mod` requires only `pgregory.net/rapid`,
  and that is test-only. Don't add a runtime dependency.
- **Mirror the reference APIs.** `RetryRoundTripper` mirrors
  hashicorp/go-retryablehttp; `Permanent`/`IsPermanent` mirror
  cenkalti/backoff. Keep names and semantics aligned with those so the library
  stays a drop-in mental model.
- **The RoundTripper never mutates the caller's request.** `RoundTrip` clones
  via `req.Clone(ctx)` per attempt; body replay goes through `req.GetBody`.
  Retrying non-idempotent methods is opt-in
  (`TransportConfig{RetryNonIdempotent: true}` plus a `GetBody`).
- **Per-request backoff, no shared state.** Each `RoundTrip` drives its own
  backoff progression; `RetryRoundTripper` holds no shared backoff state or
  mutex. Don't reintroduce a shared backoff instance; it corrupts the
  progression across goroutines that share one client.
- **RoundTrip exhaustion returns the last response, not an error.** When retries
  are exhausted, `RoundTrip` returns the last `*http.Response` with a nil error
  (even a retryable 503), mirroring stdlib (a 5xx is not a transport error);
  only a `TransportConfig.MaxElapsedTime` abort returns an error. `GetBytes`,
  by contrast, returns `retries exhausted after <elapsed>: %w`. Preserve both
  contracts; consumers branch on them.
- **Overflow- and context-safety.** `SafeDouble` guards against `int64`
  overflow; `SleepCtx` is cancellation-aware; `ParseRetryAfter` caps at
  `RetryAfterCap`. Preserve these guards when touching backoff/parse code.

## Never log or wrap a raw URL

`GetBytes` must never emit a raw request URL into a log attribute or a returned
error (CWE-532, the bug class behind go-retryablehttp's CVE-2024-6104). The
unexported `redactURL` masks userinfo and query values on every logged `url`
attribute and in `StatusError.Error()`, while the raw `StatusError.URL` field
stays available for programmatic use. `LogSafeError` reduces URL-bearing
`*url.Error`s to their cause before they are logged or returned; `GetBytes` and
`DoConditional` both apply it, and it is exported so callers can apply the same
reduction. Hardening fixes that add no public API ship as a `sec:` commit, not
a `feat:`.

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
`gofumpt` extra-rules, and `gci` (standard → third-party import grouping); let
`golangci-lint fmt` settle imports rather than hand-ordering them.

## Tests, properties, and fuzzing

Tests double as the spec, so match the existing style when adding behavior:

- **Examples** (`example_test.go`) are runnable docs; keep `ExampleGetBytes`
  and `ExampleDo` compiling and their `// Output:` accurate.
- **Property tests** (`prop_test.go`) use `pgregory.net/rapid`; invariants like
  backoff bounds and parse round-trips belong here.
- **Fuzz targets** exist for the parsing/redaction/backoff/redirect/validator
  surface (`FuzzParseRetryAfter`, `FuzzParseRetryAfterResponse`,
  `FuzzRedactTransportError`, `FuzzRedactURL`, `FuzzSafeDouble`,
  `FuzzRedirectPolicyFunc`, `FuzzSameOriginRedirect`, `FuzzCaptureValidator`,
  `FuzzCACertPool`). Run one with, e.g.,
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
[security policy](https://github.com/cplieger/.github/blob/main/SECURITY.md);
never in a public issue.
