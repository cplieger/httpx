package httpx

import (
	"context"
	"errors"
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

// --- RetryRoundTripper functional options ---

// rtCfg holds the configuration for a RetryRoundTripper.
type rtCfg struct {
	checkRetry         CheckRetry
	onRetry            OnRetry
	prepareRetry       PrepareRetry
	backoffFunc        func() Backoff
	baseDelay          time.Duration
	maxAttempts        int
	maxElapsedTime     time.Duration
	retryNonIdempotent bool
}

// RTOption configures a RetryRoundTripper via NewRetryRoundTripper.
type RTOption func(*rtCfg)

// WithRTMaxAttempts sets the maximum number of attempts (TOTAL, including the
// initial request). Default: DefaultMaxAttempts (3). A value below 1 is treated
// as 1, so the request is always sent at least once. This counts total
// attempts, not retries-beyond-first.
func WithRTMaxAttempts(n int) RTOption {
	return func(c *rtCfg) { c.maxAttempts = n }
}

// WithRTBaseDelay sets the initial backoff delay for the round-tripper
// (used when no custom Backoff is provided). Default: DefaultBaseDelay (1s).
func WithRTBaseDelay(d time.Duration) RTOption {
	return func(c *rtCfg) { c.baseDelay = d }
}

// WithRTMaxElapsedTime caps total time spent retrying. Zero means no cap.
func WithRTMaxElapsedTime(d time.Duration) RTOption {
	return func(c *rtCfg) { c.maxElapsedTime = d }
}

// WithBackoffFunc sets a factory that returns a fresh custom backoff strategy
// for each request. When set, the round-tripper's base delay is ignored.
//
// The factory is invoked once per RoundTrip, so every request drives its own
// independent Backoff instance. This is required for correctness under the
// documented shared-transport pattern (one StandardClient fanned across
// goroutines): a single long-lived Backoff would have its progression rewound
// and advanced concurrently by unrelated requests. Return a new instance (e.g.
// NewExponentialBackoff(...)) from the factory; the fresh instance needs no
// Reset.
func WithBackoffFunc(f func() Backoff) RTOption {
	return func(c *rtCfg) { c.backoffFunc = f }
}

// WithCheckRetry sets a custom retry policy. If nil, the default policy
// retries on transient transport errors and 429/5xx responses.
func WithCheckRetry(cr CheckRetry) RTOption {
	return func(c *rtCfg) { c.checkRetry = cr }
}

// WithOnRetry sets a hook called before each retry attempt for observability.
func WithOnRetry(fn OnRetry) RTOption {
	return func(c *rtCfg) { c.onRetry = fn }
}

// WithPrepareRetry sets a hook called before each retry to mutate the request
// (e.g., refresh auth tokens).
func WithPrepareRetry(fn PrepareRetry) RTOption {
	return func(c *rtCfg) { c.prepareRetry = fn }
}

// WithRetryNonIdempotent enables retry of non-idempotent methods (POST, PUT,
// PATCH, DELETE) when the request has a GetBody function for body replay.
func WithRetryNonIdempotent(enable bool) RTOption {
	return func(c *rtCfg) { c.retryNonIdempotent = enable }
}

// RetryRoundTripper implements http.RoundTripper with automatic retry.
//
// By default, only idempotent methods (GET, HEAD, OPTIONS, TRACE) are retried.
// Use WithRetryNonIdempotent to also retry POST/PUT/PATCH/DELETE when the
// request has a GetBody function for body replay.
//
// Inspired by hashicorp/go-retryablehttp, but operates directly on stdlib
// *http.Request without a custom request type, and counts TOTAL attempts
// (WithRTMaxAttempts) rather than go-retryablehttp's retries-beyond-first.
type RetryRoundTripper struct {
	next               http.RoundTripper
	checkRetry         CheckRetry
	onRetry            OnRetry
	prepareRetry       PrepareRetry
	backoffFunc        func() Backoff
	baseDelay          time.Duration
	maxAttempts        int
	maxElapsedTime     time.Duration
	retryNonIdempotent bool
}

// NewRetryRoundTripper creates a RetryRoundTripper wrapping next with the
// given options. If next is nil, http.DefaultTransport is used.
func NewRetryRoundTripper(next http.RoundTripper, opts ...RTOption) *RetryRoundTripper {
	cfg := rtCfg{
		baseDelay:   DefaultBaseDelay,
		maxAttempts: DefaultMaxAttempts,
	}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	if next == nil {
		next = http.DefaultTransport
	}
	return &RetryRoundTripper{
		next:               next,
		checkRetry:         cfg.checkRetry,
		onRetry:            cfg.onRetry,
		prepareRetry:       cfg.prepareRetry,
		backoffFunc:        cfg.backoffFunc,
		baseDelay:          cfg.baseDelay,
		maxAttempts:        cfg.maxAttempts,
		maxElapsedTime:     cfg.maxElapsedTime,
		retryNonIdempotent: cfg.retryNonIdempotent,
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
// This is deliberately narrower than the one-shot Retry helper, which retries
// every 5xx (including 500), and narrower than hashicorp/go-retryablehttp
// (all 5xx except 501). A 500 Internal Server Error is NOT retried by default;
// supply WithCheckRetry to broaden the set when an upstream returns transient
// 500s.
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

// getMaxAttempts returns the total attempt count, clamped to a minimum of 1 so
// the request is always sent at least once (never a silent zero-attempt no-op).
func (rt *RetryRoundTripper) getMaxAttempts() int {
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

	var bo Backoff
	if rt.backoffFunc != nil {
		bo = rt.backoffFunc()
	}
	backoff := rt.getBaseDelay()
	start := time.Now()

	var resp *http.Response
	var err error

	for attempt := range maxAttempts {
		if attempt > 0 {
			if abortErr := rt.sleepBeforeRetry(ctx, attempt, req, resp, err, backoff, bo, start); abortErr != nil {
				return nil, abortErr
			}
			// Advancing the default backoff unconditionally is harmless when a
			// custom Backoff is in use (its value is then unused by sleepBeforeRetry).
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
func (rt *RetryRoundTripper) sleepBeforeRetry(ctx context.Context, attempt int, req *http.Request, resp *http.Response, lastErr error, backoff time.Duration, bo Backoff, start time.Time) error {
	if rt.onRetry != nil {
		rt.onRetry(attempt, req, resp, lastErr)
	}

	wait, stop := nextWait(bo, backoff)
	if stop {
		drainResp(resp)
		if lastErr != nil {
			return lastErr
		}
		return errors.New("backoff stopped")
	}

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

// nextWait returns the base wait for this retry. With a custom Backoff it
// advances that instance (per-request, so no lock is needed); stop==true
// signals the Backoff is exhausted (BackoffStop). Without one it derives an
// equal-jitter wait from the doubling default backoff.
func nextWait(bo Backoff, backoff time.Duration) (wait time.Duration, stop bool) {
	if bo == nil {
		return JitteredBackoff(backoff), false
	}
	if w := bo.NextBackOff(); w != BackoffStop {
		return w, false
	}
	return 0, true
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

// StandardClient returns an *http.Client using this RetryRoundTripper as its
// Transport. Mirrors hashicorp/go-retryablehttp StandardClient().
//
// The returned client sets no Client.Timeout. Bound every request with a context
// deadline (http.NewRequestWithContext) or configure the wrapped transport's own
// timeouts: without one, a stalled upstream blocks RoundTrip indefinitely, the
// retry loop never advances (it is suspended inside the transport), and because
// the RoundTripper logs nothing itself the stall is silent. A Client.Timeout is
// intentionally NOT set here because it would cap total time across all retries,
// conflicting with WithRTMaxElapsedTime.
func (rt *RetryRoundTripper) StandardClient() *http.Client {
	return &http.Client{Transport: rt}
}
