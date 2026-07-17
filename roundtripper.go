package httpx

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// CheckRetry is the signature for a retry policy function.
// It receives the context, the response (may be nil on transport error), and
// the error (nil on successful response). It returns whether to retry and an
// optional error to short-circuit with.
// Mirrors hashicorp/go-retryablehttp CheckRetry.
type CheckRetry func(ctx context.Context, resp *http.Response, err error) (bool, error)

// OnRetry is called before each retry attempt (not the initial request).
// attempt is 1-indexed (first retry = 1). resp may be nil on transport errors.
type OnRetry func(attempt int, req *http.Request, resp *http.Response, err error)

// PrepareRetry is called before each retry to allow request mutation
// (e.g., re-signing with a fresh token). Mirrors go-retryablehttp PrepareRetry.
type PrepareRetry func(req *http.Request) error

// TransportConfig configures a RetryRoundTripper (and, through NewRetryClient,
// the retrying client). The zero value is ready to use and behaves exactly
// like an unconfigured v2 round-tripper: three total attempts, one-second
// base delay, the default retry policy, no hooks, no elapsed ceiling, no
// non-idempotent replay.
type TransportConfig struct {
	// CheckRetry overrides the retry policy. nil means the default policy:
	// transient transport errors plus 429/502/503/504 responses (deliberately
	// narrower than GetBytes, which retries every 5xx; supply a custom policy
	// to broaden the set when an upstream returns transient 500s).
	CheckRetry CheckRetry
	// OnRetry is a hook called before each retry attempt, the transport's only
	// observability seam (it logs nothing itself).
	OnRetry OnRetry
	// PrepareRetry is called before each retry to mutate the cloned request
	// (e.g. refresh an auth token).
	PrepareRetry PrepareRetry
	// MaxAttempts is the TOTAL attempt count including the initial request.
	// Zero means unset and takes DefaultMaxAttempts (3); a NEGATIVE value
	// means exactly one attempt (the "try once" configuration — v2 expressed
	// it as WithRTMaxAttempts(0), but a zero struct field cannot distinguish
	// absent from zero, so v3 moves try-once to negatives).
	MaxAttempts int
	// BaseDelay is the initial backoff delay; non-positive means
	// DefaultBaseDelay (1s). Waits are equal-jitter with overflow-safe
	// doubling, and a retryable response's Retry-After header (capped at
	// RetryAfterCap) overrides the computed wait.
	BaseDelay time.Duration
	// MaxElapsedTime is a hard ceiling on total time across retries,
	// including any honored Retry-After: when now+wait would meet or pass it,
	// the round-tripper aborts with an error instead of sleeping. It is
	// checked BETWEEN attempts and cannot interrupt a stalled in-flight
	// attempt; bound single attempts on the base transport (e.g.
	// ResponseHeaderTimeout) and the whole call with the request context.
	// Zero means no ceiling.
	MaxElapsedTime time.Duration
	// RetryNonIdempotent enables retry of non-idempotent methods (POST, PUT,
	// PATCH, DELETE) when the request has a GetBody function for body replay.
	RetryNonIdempotent bool
}

// RetryRoundTripper implements http.RoundTripper with automatic retry.
//
// By default, only idempotent methods (GET, HEAD, OPTIONS, TRACE) are retried.
// Use TransportConfig.RetryNonIdempotent to also retry POST/PUT/PATCH/DELETE
// when the request has a GetBody function for body replay.
//
// Inspired by hashicorp/go-retryablehttp, but operates directly on stdlib
// *http.Request without a custom request type, and counts TOTAL attempts
// (TransportConfig.MaxAttempts) rather than go-retryablehttp's
// retries-beyond-first.
type RetryRoundTripper struct {
	next               http.RoundTripper
	checkRetry         CheckRetry
	onRetry            OnRetry
	prepareRetry       PrepareRetry
	baseDelay          time.Duration
	maxAttempts        int
	maxElapsedTime     time.Duration
	retryNonIdempotent bool
}

// NewRetryRoundTripper creates a RetryRoundTripper wrapping next with the
// given configuration. If next is nil, http.DefaultTransport is used.
// TransportConfig{} gives the defaults (see its field docs).
func NewRetryRoundTripper(next http.RoundTripper, cfg TransportConfig) *RetryRoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return &RetryRoundTripper{
		next:               next,
		checkRetry:         cfg.CheckRetry,
		onRetry:            cfg.OnRetry,
		prepareRetry:       cfg.PrepareRetry,
		baseDelay:          cfg.BaseDelay,
		maxAttempts:        cfg.MaxAttempts,
		maxElapsedTime:     cfg.MaxElapsedTime,
		retryNonIdempotent: cfg.RetryNonIdempotent,
	}
}

// isIdempotent reports whether the HTTP method is safe to retry.
func isIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}

// canRetry reports whether this request is eligible for retry.
func (rt *RetryRoundTripper) canRetry(req *http.Request) bool {
	if isIdempotent(req.Method) {
		return true
	}
	if !rt.retryNonIdempotent {
		return false
	}
	if req.Body == nil || req.Body == http.NoBody {
		return true
	}
	return req.GetBody != nil
}

// defaultCheckRetry is the built-in retry policy for RetryRoundTripper.
// It retries transient transport errors and the following HTTP status codes:
// 429 (Too Many Requests), 502 (Bad Gateway), 503 (Service Unavailable),
// 504 (Gateway Timeout).
//
// This is deliberately narrower than GetBytes, which retries every 5xx
// (including 500), and narrower than hashicorp/go-retryablehttp (all 5xx
// except 501). A 500 Internal Server Error is NOT retried by default; supply
// TransportConfig.CheckRetry to broaden the set when an upstream returns
// transient 500s.
func defaultCheckRetry(_ context.Context, resp *http.Response, err error) (bool, error) {
	if err != nil {
		return IsTransient(err), nil
	}
	if resp != nil {
		switch resp.StatusCode {
		case http.StatusTooManyRequests,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout:
			return true, nil
		}
	}
	return false, nil
}

// getMaxAttempts returns the total attempt count: zero means unset and takes
// DefaultMaxAttempts; a negative value means exactly one attempt.
func (rt *RetryRoundTripper) getMaxAttempts() int {
	if rt.maxAttempts == 0 {
		return DefaultMaxAttempts
	}
	if rt.maxAttempts < 1 {
		return 1
	}
	return rt.maxAttempts
}

func (rt *RetryRoundTripper) getBaseDelay() time.Duration {
	if rt.baseDelay > 0 {
		return rt.baseDelay
	}
	return DefaultBaseDelay
}

func (rt *RetryRoundTripper) transport() http.RoundTripper {
	if rt.next != nil {
		return rt.next
	}
	return http.DefaultTransport
}

func (rt *RetryRoundTripper) getCheckRetry() CheckRetry {
	if rt.checkRetry != nil {
		return rt.checkRetry
	}
	return defaultCheckRetry
}

// RoundTrip implements http.RoundTripper. It retries eligible requests on
// transient failures with jittered exponential backoff, honoring Retry-After.
// Per the http.RoundTripper contract, the caller's request is never mutated.
//
// When retries are exhausted the final response is returned, not an error:
// if the last attempt produced a retryable response (e.g. a 503), RoundTrip
// returns that *http.Response with a nil error, exactly as a non-retried
// request would. The caller owns the returned body (must Close it) and MUST
// inspect resp.StatusCode - a nil error does not imply a 2xx response.
func (rt *RetryRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if !rt.canRetry(req) {
		return rt.transport().RoundTrip(req)
	}

	ctx := req.Context()
	check := rt.getCheckRetry()
	maxAttempts := rt.getMaxAttempts()

	backoff := rt.getBaseDelay()
	start := time.Now()

	var resp *http.Response
	var err error

	for attempt := range maxAttempts {
		if attempt > 0 {
			if abortErr := rt.sleepBeforeRetry(ctx, attempt, req, resp, err, backoff, start); abortErr != nil {
				return nil, abortErr
			}
			backoff = SafeDouble(backoff)
		}

		tryReq, buildErr := rt.buildAttemptRequest(ctx, req, attempt)
		if buildErr != nil {
			return nil, buildErr
		}

		resp, err = rt.transport().RoundTrip(tryReq)

		if done, finalResp, finalErr := rt.evaluateAttempt(ctx, check, resp, err); done {
			return finalResp, finalErr
		}
	}

	return resp, err
}

// buildAttemptRequest clones req for a single attempt. The caller's request is
// never mutated (RoundTripper contract). On retries (attempt > 0) it rewinds the
// body via GetBody and applies the PrepareRetry hook.
func (rt *RetryRoundTripper) buildAttemptRequest(ctx context.Context, req *http.Request, attempt int) (*http.Request, error) {
	tryReq := req.Clone(ctx)
	if attempt == 0 {
		return tryReq, nil
	}
	if req.GetBody != nil {
		body, bodyErr := req.GetBody()
		if bodyErr != nil {
			return nil, fmt.Errorf("rewind request body: %w", bodyErr)
		}
		tryReq.Body = body
	}
	if rt.prepareRetry != nil {
		if prepErr := rt.prepareRetry(tryReq); prepErr != nil {
			return nil, fmt.Errorf("prepare retry: %w", prepErr)
		}
	}
	return tryReq, nil
}

// evaluateAttempt applies the retry policy after an attempt. done==true means
// RoundTrip stops and returns (finalResp, finalErr); done==false means retry.
// A retryable response is drained only when stopping early on a policy or
// context error; the success/no-retry path hands the body back to the caller.
func (rt *RetryRoundTripper) evaluateAttempt(ctx context.Context, check CheckRetry, resp *http.Response, err error) (done bool, finalResp *http.Response, finalErr error) {
	shouldRetry, checkErr := check(ctx, resp, err)
	if checkErr != nil {
		drainResp(resp)
		return true, nil, checkErr
	}
	if !shouldRetry {
		return true, resp, err
	}
	if ctx.Err() != nil {
		drainResp(resp)
		return true, nil, ctx.Err()
	}
	return false, nil, nil
}

// sleepBeforeRetry handles the pre-retry logic: hook, compute wait, honor
// Retry-After, enforce the elapsed-time budget, drain, sleep.
func (rt *RetryRoundTripper) sleepBeforeRetry(ctx context.Context, attempt int, req *http.Request, resp *http.Response, lastErr error, backoff time.Duration, start time.Time) error {
	if rt.onRetry != nil {
		rt.onRetry(attempt, req, resp, lastErr)
	}

	wait := JitteredBackoff(backoff)

	// Honor Retry-After on any retryable response (429, 502, 503, 504).
	// ParseRetryAfter caps at RetryAfterCap, so a hostile header cannot force
	// an unbounded wait. Honored per RFC for 503/429; 502/504 is a pragmatic,
	// harmless extension.
	if resp != nil {
		if ra := ParseRetryAfter(resp.Header.Get("Retry-After")); ra > 0 {
			wait = ra
		}
	}

	// maxElapsedTime is a hard ceiling. The check (computed AFTER any honored
	// Retry-After) aborts now rather than overshoot the budget by sleeping.
	if rt.elapsedBudgetExceeded(wait, start) {
		drainResp(resp)
		if lastErr != nil {
			return fmt.Errorf("max elapsed time %s exceeded: %w", rt.maxElapsedTime, lastErr)
		}
		return fmt.Errorf("max elapsed time %s exceeded", rt.maxElapsedTime)
	}

	drainResp(resp)

	if sleepErr := SleepCtx(ctx, wait); sleepErr != nil {
		return sleepErr
	}

	return nil
}

// elapsedBudgetExceeded reports whether sleeping for wait now would meet or pass
// the maxElapsedTime ceiling. The comparison tests the remaining budget rather
// than summing elapsed+wait, so a near-MaxInt64 wait cannot overflow (CWE-190);
// maxElapsedTime-elapsed is evaluated only when elapsed < maxElapsedTime, so it
// stays positive. A non-positive maxElapsedTime means "no ceiling".
func (rt *RetryRoundTripper) elapsedBudgetExceeded(wait time.Duration, start time.Time) bool {
	if rt.maxElapsedTime <= 0 {
		return false
	}
	elapsed := time.Since(start)
	return elapsed >= rt.maxElapsedTime || wait >= rt.maxElapsedTime-elapsed
}

// drainResp drains and closes a response body if present.
func drainResp(resp *http.Response) {
	if resp != nil && resp.Body != nil {
		DrainClose(resp.Body)
	}
}

// NewRetryClient returns an *http.Client whose Transport is a
// RetryRoundTripper over base (nil base means http.DefaultTransport) and
// whose CheckRedirect is policy. It is the one-call form of the composition
// plexapi-style consumers assemble by hand: retry transport plus an explicit
// redirect policy.
//
// policy is REQUIRED and must be non-nil: NewRetryClient panics on a nil
// policy, because a nil CheckRedirect silently means net/http's default
// follow-anywhere behavior (up to 10 hops to any host, custom auth headers
// forwarded) — exactly the unsafe omission this constructor exists to
// prevent. Pass DefaultRedirectPolicy (same-host), RefuseAllRedirects, or a
// RedirectPolicyFunc allowlist.
//
// The returned client sets no Client.Timeout: a Client.Timeout above a
// retrying transport caps the WHOLE retry sequence and defeats the retries
// beneath it. Note that neither MaxElapsedTime nor the request context can
// interrupt a stalled in-flight attempt from between attempts: bound single
// attempts on the base transport (e.g. ResponseHeaderTimeout on a
// CloneDefaultTransport()) and bound the total with a context deadline
// (http.NewRequestWithContext) or TransportConfig.MaxElapsedTime.
func NewRetryClient(base http.RoundTripper, policy CheckRedirect, cfg TransportConfig) *http.Client {
	if policy == nil {
		panic("httpx.NewRetryClient: nil redirect policy (pass DefaultRedirectPolicy, RefuseAllRedirects, or a RedirectPolicyFunc allowlist)")
	}
	return &http.Client{
		Transport:     NewRetryRoundTripper(base, cfg),
		CheckRedirect: policy,
	}
}
