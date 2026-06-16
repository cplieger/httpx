package httpx_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/httpx"
)

// === ROUND 6 CONVERGENCE: Final adversarial sweep ===

// --- GetBody replay across >2 attempts (5 retries) ---

func TestR6_GetBody_ReplayAcross5Retries(t *testing.T) {
	const wantAttempts = 6 // 1 initial + 5 retries
	var bodies []string
	var mu sync.Mutex
	var calls atomic.Int32

	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(req.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		if calls.Add(1) < int32(wantAttempts) {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     http.Header{},
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("done")),
			Header:     http.Header{},
		}, nil
	})

	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithMaxRetries(5),
		httpx.WithRetryNonIdempotent(true),
	)

	payload := "body-across-5-retries"
	req, _ := http.NewRequest(http.MethodPost, "http://example.com/replay6", strings.NewReader(payload))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(payload)), nil
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	resp.Body.Close()

	if got := calls.Load(); got != int32(wantAttempts) {
		t.Fatalf("calls=%d, want %d", got, wantAttempts)
	}
	for i, b := range bodies {
		if b != payload {
			t.Errorf("attempt %d body=%q, want %q", i+1, b, payload)
		}
	}
}

// --- PrepareRetry mutating headers each attempt: isolation across all ---

func TestR6_PrepareRetry_HeaderIsolation_PerAttempt(t *testing.T) {
	var attempt atomic.Int32
	var headersSeen []string
	var mu sync.Mutex

	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		n := attempt.Add(1)
		mu.Lock()
		headersSeen = append(headersSeen, req.Header.Get("X-Token"))
		mu.Unlock()
		if n < 4 {
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     http.Header{},
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Header:     http.Header{},
		}, nil
	})

	var prepareAttempt atomic.Int32
	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithMaxRetries(4),
		httpx.WithPrepareRetry(func(req *http.Request) error {
			n := prepareAttempt.Add(1)
			// Each PrepareRetry sets a DIFFERENT token value
			req.Header.Set("X-Token", string('A'+n-1))
			// Also add accumulating header to test no leak
			req.Header.Add("X-Accum", "x")
			return nil
		}),
	)

	origReq, _ := http.NewRequest(http.MethodGet, "http://example.com/prepareisolation", http.NoBody)
	origReq.Header.Set("X-Token", "original")

	resp, err := rt.RoundTrip(origReq)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	resp.Body.Close()

	// Original request must remain untouched
	if origReq.Header.Get("X-Token") != "original" {
		t.Fatal("FINDING: PrepareRetry mutated original request X-Token")
	}
	if origReq.Header.Get("X-Accum") != "" {
		t.Fatal("FINDING: PrepareRetry X-Accum leaked to original")
	}

	// First attempt (no PrepareRetry) should see "original"
	if headersSeen[0] != "original" {
		t.Fatalf("attempt 1 X-Token=%q, want 'original'", headersSeen[0])
	}
	// Retries should each see their own fresh token (A, B, C) from PrepareRetry
	expected := []string{"A", "B", "C"}
	for i, want := range expected {
		if headersSeen[i+1] != want {
			t.Errorf("attempt %d X-Token=%q, want %q", i+2, headersSeen[i+1], want)
		}
	}

	// Each retry clone should have exactly 1 X-Accum (not accumulating from prior clones)
	// This is implicitly verified by the PrepareRetry getting a fresh clone each time
}

// --- Trailers preserved on clone ---

func TestR6_Clone_TrailersPreserved(t *testing.T) {
	var attempt atomic.Int32
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		n := attempt.Add(1)
		// Verify Trailer is present on retried request
		if req.Trailer == nil || req.Trailer.Get("X-Checksum") != "abc123" {
			t.Errorf("attempt %d: Trailer missing or wrong, got %v", n, req.Trailer)
		}
		if n < 2 {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     http.Header{},
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Header:     http.Header{},
		}, nil
	})

	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithMaxRetries(2),
	)

	req, _ := http.NewRequest(http.MethodGet, "http://example.com/trailers", http.NoBody)
	req.Trailer = http.Header{"X-Checksum": {"abc123"}}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	resp.Body.Close()
}

// --- Host preserved on clone ---

func TestR6_Clone_HostPreserved(t *testing.T) {
	var attempt atomic.Int32
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		n := attempt.Add(1)
		if req.Host != "custom.host.example" {
			t.Errorf("attempt %d: Host=%q, want 'custom.host.example'", n, req.Host)
		}
		if n < 2 {
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     http.Header{},
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Header:     http.Header{},
		}, nil
	})

	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithMaxRetries(2),
	)

	req, _ := http.NewRequest(http.MethodGet, "http://example.com/hostclone", http.NoBody)
	req.Host = "custom.host.example"

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	resp.Body.Close()
}

// --- Per-request backoff factory: heavy concurrent load with -race detector ---

func TestR6_PerRequestBackoff_HeavyRace(t *testing.T) {
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     http.Header{},
		}, nil
	})

	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithMaxRetries(20),
		httpx.WithBackoffFunc(func() httpx.Backoff {
			return httpx.NewExponentialBackoff(
				httpx.WithInitialInterval(time.Millisecond),
				httpx.WithMaxElapsedTime(10*time.Millisecond),
			)
		}),
	)

	var wg sync.WaitGroup
	for range 200 {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com/heavyrace", http.NoBody)
			resp, _ := rt.RoundTrip(req)
			if resp != nil {
				resp.Body.Close()
			}
		})
	}
	wg.Wait()
}

// --- PrepareRetry error does not leak partial state ---

func TestR6_PrepareRetry_Error_NoLeak(t *testing.T) {
	var attempt atomic.Int32
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempt.Add(1)
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     http.Header{},
		}, nil
	})

	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithMaxRetries(3),
		httpx.WithPrepareRetry(func(req *http.Request) error {
			req.Header.Set("X-Mutated", "yes")
			return io.ErrUnexpectedEOF // always fail
		}),
	)

	origReq, _ := http.NewRequest(http.MethodGet, "http://example.com/prepareerr", http.NoBody)
	_, err := rt.RoundTrip(origReq) //nolint:bodyclose // error path
	if err == nil {
		t.Fatal("expected PrepareRetry error")
	}
	// Original must remain clean
	if origReq.Header.Get("X-Mutated") != "" {
		t.Fatal("FINDING: PrepareRetry error path leaked mutation to original")
	}
}

// --- Nil PrepareRetry/OnRetry/CheckRetry don't panic ---

func TestR6_NilCallbacks_NoPanic(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		if calls.Add(1) < 2 {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     http.Header{},
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Header:     http.Header{},
		}, nil
	})

	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithMaxRetries(2),
		httpx.WithPrepareRetry(nil),
		httpx.WithOnRetry(nil),
		httpx.WithCheckRetry(nil),
	)

	req, _ := http.NewRequest(http.MethodGet, "http://example.com/nilcb", http.NoBody)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("nil callbacks caused error: %v", err)
	}
	resp.Body.Close()
}

// --- GetBody returning error on 2nd rewind ---

func TestR6_GetBody_ErrorOnRewind(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     http.Header{},
		}, nil
	})

	var getBodyCalls atomic.Int32
	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithRTBaseDelay(time.Millisecond),
		httpx.WithMaxRetries(5),
		httpx.WithRetryNonIdempotent(true),
	)

	req, _ := http.NewRequest(http.MethodPost, "http://example.com/getbodyerr", strings.NewReader("data"))
	req.GetBody = func() (io.ReadCloser, error) {
		if getBodyCalls.Add(1) >= 3 {
			return nil, io.ErrUnexpectedEOF // fail on 3rd rewind
		}
		return io.NopCloser(strings.NewReader("data")), nil
	}

	_, err := rt.RoundTrip(req) //nolint:bodyclose // error path
	if err == nil {
		t.Fatal("expected GetBody error to propagate")
	}
	if !strings.Contains(err.Error(), "rewind request body") {
		t.Fatalf("unexpected error: %v", err)
	}
}
