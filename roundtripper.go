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
			if bo == nil {
				backoff = SafeDouble(backoff)
			}
		}

		tryReq := req.Clone(ctx)
		if attempt > 0 && req.GetBody != nil {
			body, bodyErr := req.GetBody()
			if bodyErr != nil {
				return nil, fmt.Errorf("rewind request body: %w", bodyErr)
			}
			tryReq.Body = body
		}
		if attempt > 0 && rt.prepareRetry != nil {
			if prepErr := rt.prepareRetry(tryReq); prepErr != nil {
				return nil, fmt.Errorf("prepare retry: %w", prepErr)
			}
		}

		resp, err = rt.transport().RoundTrip(tryReq)

		shouldRetry, checkErr := check(ctx, resp, err)
		if checkErr != nil {
			drainResp(resp)
			return nil, checkErr
		}
		if !shouldRetry {
			return resp, err
		}
		if ctx.Err() != nil {
			drainResp(resp)
			return nil, ctx.Err()
		}
	}

	return resp, err
}

// sleepBeforeRetry handles the pre-retry logic: hook, compute wait, honor
// Retry-After, enforce the elapsed-time budget, drain, sleep.
func (rt *RetryRoundTripper) sleepBeforeRetry(ctx context.Context, attempt int, req *http.Request, resp *http.Response, lastErr error, backoff time.Duration, bo Backoff, start time.Time) error {
	if rt.onRetry != nil {
		rt.onRetry(attempt, req, resp, lastErr)
	}

	var wait time.Duration
	if bo != nil {
		// bo is a per-request instance (from rt.backoffFunc), so advancing it
		// here needs no lock — no other goroutine shares it.
		w := bo.NextBackOff()
		if w == BackoffStop {
			drainResp(resp)
			if lastErr != nil {
				return lastErr
			}
			return errors.New("backoff stopped")
		}
		wait = w
	} else {
		wait = JitteredBackoff(backoff)
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

	// maxElapsedTime is a hard ceiling. Check it AFTER computing the final wait
	// (including any honored Retry-After): if sleeping would push total retry
	// time to or past the budget, abort now rather than overshoot it. An
	// honored Retry-After can dwarf the remaining budget, so the pre-wait
	// elapsed alone is not a sufficient guard. This subsumes the
	// already-exceeded case (wait >= 0).
	// Compare without adding so a near-MaxInt64 wait cannot overflow the sum
	// (CWE-190): abort if the elapsed time already meets the budget, or if the
	// remaining budget is <= wait. maxElapsedTime-elapsed is computed only when
	// elapsed < maxElapsedTime, so it stays positive and cannot underflow.
	if elapsed := time.Since(start); rt.maxElapsedTime > 0 &&
		(elapsed >= rt.maxElapsedTime || wait >= rt.maxElapsedTime-elapsed) {
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
