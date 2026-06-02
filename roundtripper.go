package httpx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
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
	backoff            Backoff
	baseDelay          time.Duration
	maxRetries         int
	maxElapsedTime     time.Duration
	retryNonIdempotent bool
}

// RTOption configures a RetryRoundTripper via NewRetryRoundTripper.
type RTOption func(*rtCfg)

// WithMaxRetries sets the maximum number of retries (not counting the initial
// request). Default: 2 (3 total attempts).
func WithMaxRetries(n int) RTOption {
	return func(c *rtCfg) { c.maxRetries = n }
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

// WithBackoff sets a custom backoff strategy. When set, BaseDelay is ignored.
func WithBackoff(b Backoff) RTOption {
	return func(c *rtCfg) { c.backoff = b }
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
// request has a GetBody function for body replay (mirrors go-retryablehttp).
//
// Mirrors hashicorp/go-retryablehttp RoundTripper but operates directly on
// stdlib *http.Request without requiring a custom request type.
type RetryRoundTripper struct {
	next               http.RoundTripper
	checkRetry         CheckRetry
	onRetry            OnRetry
	prepareRetry       PrepareRetry
	backoff            Backoff
	baseDelay          time.Duration
	maxRetries         int
	maxElapsedTime     time.Duration
	retryNonIdempotent bool
	mu                 sync.Mutex
}

// NewRetryRoundTripper creates a RetryRoundTripper wrapping next with the
// given options. If next is nil, http.DefaultTransport is used.
func NewRetryRoundTripper(next http.RoundTripper, opts ...RTOption) *RetryRoundTripper {
	cfg := rtCfg{
		baseDelay:  DefaultBaseDelay,
		maxRetries: DefaultMaxAttempts - 1,
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
		backoff:            cfg.backoff,
		baseDelay:          cfg.baseDelay,
		maxRetries:         cfg.maxRetries,
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

// defaultCheckRetry is the built-in retry policy: retry on transient transport
// errors and retryable HTTP status codes (429, 502, 503, 504).
func defaultCheckRetry(_ context.Context, resp *http.Response, err error) (bool, error) {
	if err != nil {
		if IsPermanent(err) {
			return false, nil
		}
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

func (rt *RetryRoundTripper) getMaxRetries() int {
	if rt.maxRetries < 0 {
		return DefaultMaxAttempts - 1
	}
	return rt.maxRetries
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
func (rt *RetryRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if !rt.canRetry(req) {
		return rt.transport().RoundTrip(req)
	}

	ctx := req.Context()
	check := rt.getCheckRetry()
	maxRetries := rt.getMaxRetries()

	var bo Backoff
	if rt.backoff != nil {
		bo = rt.backoff
		rt.mu.Lock()
		bo.Reset()
		rt.mu.Unlock()
	}
	backoff := rt.getBaseDelay()
	start := time.Now()

	var resp *http.Response
	var err error

	for attempt := 0; attempt <= maxRetries; attempt++ {
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
		if !shouldRetry || ctx.Err() != nil {
			if !shouldRetry {
				return resp, err
			}
			drainResp(resp)
			return nil, ctx.Err()
		}
	}

	return resp, err
}

// sleepBeforeRetry handles the pre-retry logic: hook, drain, wait, prepare.
func (rt *RetryRoundTripper) sleepBeforeRetry(ctx context.Context, attempt int, req *http.Request, resp *http.Response, lastErr error, backoff time.Duration, bo Backoff, start time.Time) error {
	if rt.onRetry != nil {
		rt.onRetry(attempt, req, resp, lastErr)
	}

	if rt.maxElapsedTime > 0 && time.Since(start) >= rt.maxElapsedTime {
		drainResp(resp)
		return fmt.Errorf("max elapsed time %s exceeded: %w", rt.maxElapsedTime, lastErr)
	}

	var wait time.Duration
	if bo != nil {
		rt.mu.Lock()
		w := bo.NextBackOff()
		rt.mu.Unlock()
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

	if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
		if ra := ParseRetryAfter(resp.Header.Get("Retry-After")); ra > 0 {
			wait = ra
		}
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
		Drain(resp.Body)
		resp.Body.Close()
	}
}

// StandardClient returns an *http.Client using this RetryRoundTripper as its
// Transport. Mirrors hashicorp/go-retryablehttp StandardClient().
func (rt *RetryRoundTripper) StandardClient() *http.Client {
	return &http.Client{Transport: rt}
}
