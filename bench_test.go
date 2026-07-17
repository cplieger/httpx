package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- Stub RoundTripper helpers ---

// stubRT always returns the given response/error.
type stubRT struct {
	resp *http.Response
	err  error
}

func (s *stubRT) RoundTrip(*http.Request) (*http.Response, error) {
	return s.resp, s.err
}

// failThenSucceedRT fails the first N calls, then succeeds.
type failThenSucceedRT struct {
	successResp *http.Response
	failCount   int64
	calls       atomic.Int64
}

func (f *failThenSucceedRT) RoundTrip(*http.Request) (*http.Response, error) {
	n := f.calls.Add(1)
	if n <= f.failCount {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     http.Header{},
		}, nil
	}
	return f.successResp, nil
}

func (f *failThenSucceedRT) reset() { f.calls.Store(0) }

// --- RetryRoundTripper benchmarks ---

func BenchmarkRetryRoundTripper_Success(b *testing.B) {
	okResp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("ok")),
		Header:     http.Header{},
	}
	rt := NewRetryRoundTripper(&stubRT{resp: okResp}, TransportConfig{MaxAttempts: 3})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com", http.NoBody)

	b.ResetTimer()
	for range b.N {
		resp, err := rt.RoundTrip(req)
		if err != nil {
			b.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b.Fatal("unexpected status")
		}
	}
}

func BenchmarkRetryRoundTripper_RetryThenSuccess(b *testing.B) {
	okResp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("ok")),
		Header:     http.Header{},
	}
	inner := &failThenSucceedRT{failCount: 1, successResp: okResp}
	// A 1ns base delay keeps the benchmark measuring the retry machinery, not
	// sleep (the jittered wait from a 1ns base is at most 1ns).
	rt := NewRetryRoundTripper(inner, TransportConfig{MaxAttempts: 4, BaseDelay: time.Nanosecond})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com", http.NoBody)

	b.ResetTimer()
	for range b.N {
		inner.reset()
		resp, err := rt.RoundTrip(req)
		if err != nil {
			b.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b.Fatal("unexpected status")
		}
	}
}

// --- JitteredBackoff benchmark ---

func BenchmarkJitteredBackoff(b *testing.B) {
	base := time.Second
	for range b.N {
		_ = JitteredBackoff(base)
	}
}

// --- SafeDouble benchmark ---

func BenchmarkSafeDouble(b *testing.B) {
	d := 500 * time.Millisecond
	for range b.N {
		d = SafeDouble(d)
		if d < 0 {
			d = 500 * time.Millisecond // reset to prevent trivial ops at max
		}
	}
}

// --- ParseRetryAfter benchmarks ---

func BenchmarkParseRetryAfter_Seconds(b *testing.B) {
	for range b.N {
		_ = ParseRetryAfter("30")
	}
}

func BenchmarkParseRetryAfter_HTTPDate(b *testing.B) {
	// A fixed date string to exercise the HTTP-date parsing path.
	header := "Tue, 03 Jun 2025 08:00:00 GMT"
	for range b.N {
		_ = ParseRetryAfter(header)
	}
}

func BenchmarkParseRetryAfter_Empty(b *testing.B) {
	for range b.N {
		_ = ParseRetryAfter("")
	}
}

// --- IsTransient benchmarks ---

func BenchmarkIsTransient_UnexpectedEOF(b *testing.B) {
	// io.ErrUnexpectedEOF is the canonical transient network error path.
	err := fmt.Errorf("read body: %w", io.ErrUnexpectedEOF)
	b.ResetTimer()
	for range b.N {
		_ = IsTransient(err)
	}
}

func BenchmarkIsTransient_Nil(b *testing.B) {
	for range b.N {
		_ = IsTransient(nil)
	}
}

func BenchmarkIsTransient_PermanentError(b *testing.B) {
	err := Permanent(errors.New("bad request"))
	for range b.N {
		_ = IsTransient(err)
	}
}
