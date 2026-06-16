package httpx_test

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/httpx"
)

// recordingBackoff records, per instance, the ordered sequence of NextBackOff
// step indices it served. NextBackOff returns a zero delay so the test runs
// fast; the recorded sequence is what we assert on. Each instance is touched by
// exactly one goroutine (the request that the factory minted it for), so
// reading seq after wg.Wait() is race-free.
type recordingBackoff struct {
	step int
	seq  []int
}

func (b *recordingBackoff) NextBackOff() time.Duration {
	b.seq = append(b.seq, b.step)
	b.step++
	return 0
}

func (b *recordingBackoff) Reset() {
	b.step = 0
	b.seq = nil
}

// TestPerRequestBackoffFactory_IndependentProgression is the A3 fix proof:
// many goroutines drive ONE StandardClient built with a custom WithBackoffFunc,
// and every request must observe its OWN backoff progression starting from
// step 0. With the old shared-instance model, a single Backoff would have been
// advanced/rewound across all concurrent requests (and, without the deleted
// mutex, raced). Must pass under -race.
func TestPerRequestBackoffFactory_IndependentProgression(t *testing.T) {
	t.Parallel()

	const (
		goroutines = 50
		maxRetries = 5
	)

	// Transport always 503 so each request exhausts maxRetries retries,
	// producing exactly maxRetries NextBackOff calls on its own instance.
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     http.Header{},
		}, nil
	})

	var mu sync.Mutex
	var instances []*recordingBackoff

	rt := httpx.NewRetryRoundTripper(transport,
		httpx.WithMaxRetries(maxRetries),
		httpx.WithBackoffFunc(func() httpx.Backoff {
			b := &recordingBackoff{}
			mu.Lock()
			instances = append(instances, b)
			mu.Unlock()
			return b
		}),
	)
	client := rt.StandardClient()

	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com/indep", http.NoBody)
			resp, err := rt.RoundTrip(req)
			if err == nil && resp != nil {
				resp.Body.Close()
			}
			_ = client
		})
	}
	wg.Wait()

	// The factory must have been invoked exactly once per request: one fresh
	// Backoff instance per goroutine, never shared.
	if len(instances) != goroutines {
		t.Fatalf("backoff factory invoked %d times, want %d (one fresh instance per request)", len(instances), goroutines)
	}

	// Each instance must show an INDEPENDENT progression: its own ordered
	// sequence [0,1,...,maxRetries-1]. A shared instance would instead show one
	// instance with goroutines*maxRetries calls (and interleaved steps).
	want := make([]int, maxRetries)
	for i := range want {
		want[i] = i
	}
	for i, b := range instances {
		if !slices.Equal(b.seq, want) {
			t.Fatalf("instance %d progression = %v, want %v (independent per-request progression)", i, b.seq, want)
		}
	}
}
