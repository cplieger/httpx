package httpx

import (
	"math"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"
)

func TestJitteredBackoff_property(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		d := time.Duration(rapid.Int64Range(1, math.MaxInt64/2).Draw(t, "duration"))
		result := JitteredBackoff(d)
		half := d / 2
		if result < half {
			t.Fatalf("JitteredBackoff(%v) = %v, want >= %v", d, result, half)
		}
		if result > d {
			t.Fatalf("JitteredBackoff(%v) = %v, want <= %v", d, result, d)
		}
	})
}

func TestSafeDouble_property(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		d := time.Duration(rapid.Int64Range(1, math.MaxInt64).Draw(t, "duration"))
		result := SafeDouble(d)
		if result < d {
			t.Fatalf("SafeDouble(%v) = %v, want >= %v", d, result, d)
		}
		doubled := d * 2
		if doubled >= d {
			if result != doubled {
				t.Fatalf("SafeDouble(%v) = %v, want %v", d, result, doubled)
			}
		} else {
			if result != time.Duration(math.MaxInt64) {
				t.Fatalf("SafeDouble(%v) = %v, want MaxInt64 on overflow", d, result)
			}
		}
	})
}

func FuzzParseRetryAfter(f *testing.F) {
	f.Add("")
	f.Add("60")
	f.Add("-1")
	f.Add("0")
	f.Add("3600")
	f.Add("garbage!@#$%")
	f.Add("  30  ")
	f.Add("\t60\n")
	f.Add("Mon, 02 Jan 2006 15:04:05 GMT")

	f.Fuzz(func(t *testing.T, val string) {
		d := ParseRetryAfter(val)
		if d < 0 {
			t.Fatalf("ParseRetryAfter(%q) returned negative: %v", val, d)
		}
		if d > RetryAfterCap {
			t.Fatalf("ParseRetryAfter(%q) = %v exceeds cap %v", val, d, RetryAfterCap)
		}
	})
}

func FuzzParseRetryAfterResponse(f *testing.F) {
	f.Add("")
	f.Add("60")
	f.Add("-1")
	f.Add("0")
	f.Add("3600")
	f.Add("garbage!@#$%")
	f.Add("  30  ")
	f.Add("\t60\n")
	f.Add("Mon, 02 Jan 2006 15:04:05 GMT")

	f.Fuzz(func(t *testing.T, val string) {
		resp := &http.Response{Header: http.Header{}}
		if val != "" {
			resp.Header.Set("Retry-After", val)
		}
		d := ParseRetryAfterResponse(resp)
		if d < 0 {
			t.Fatalf("ParseRetryAfterResponse(%q) returned negative: %v", val, d)
		}
		// Cross-function consistency invariant (cycle-1 l-f4 merged both onto
		// the shared parseRetryAfterValue, so they must differ only by the cap):
		// ParseRetryAfter(val) == min(ParseRetryAfterResponse(resp), cap).
		// A tolerance absorbs the sub-microsecond clock drift between the two
		// time.Until evaluations an HTTP-date value triggers; a real regression
		// (either function ceasing to delegate, or the cap being dropped) moves
		// the value by whole seconds, far past the margin.
		capped := min(d, RetryAfterCap)
		pra := ParseRetryAfter(val)
		diff := pra - capped
		if diff < 0 {
			diff = -diff
		}
		if diff > 2*time.Second {
			t.Fatalf("ParseRetryAfter(%q)=%v but min(ParseRetryAfterResponse, cap)=%v (diff %v): consistency broken",
				val, pra, capped, diff)
		}
	})
}

func FuzzRedactTransportError(f *testing.F) {
	f.Add("connection refused", "prefix", "mykey123")
	f.Add("", "prefix", "")
	f.Add("error with mykey123 inside", "fetch", "mykey123")
	f.Add("no secret here", "download", "absent")

	f.Fuzz(func(t *testing.T, errMsg, prefix, secret string) {
		var err error
		if errMsg != "" {
			err = &testError{msg: errMsg}
		}
		// Panic-safety: exercise redaction with the arbitrary fuzzed secret.
		result := RedactTransportError(err, prefix, secret)
		if err == nil {
			if result != nil {
				t.Fatal("expected nil for nil input")
			}
			return
		}
		// Leak property: a distinctive sentinel embedded in the fuzzed message
		// must never survive redaction. Asserting "output contains the arbitrary
		// fuzzed secret" is unsound — short/degenerate secrets coincidentally
		// recur in the error structure (separators, the REDACTED marker, text
		// re-joined when a marker is removed) even though every real occurrence is
		// replaced. A distinctive token cannot be coincidentally re-formed.
		const sentinel = "AKIAIOSFODNN7EXAMPLE"
		sres := RedactTransportError(&testError{msg: errMsg + sentinel}, prefix, sentinel)
		if sres != nil && strings.Contains(sres.Error(), sentinel) {
			t.Fatalf("sentinel secret survived redaction: %q", sres.Error())
		}
	})
}

func FuzzSafeDouble(f *testing.F) {
	f.Add(int64(0))
	f.Add(int64(1))
	f.Add(int64(-1))
	f.Add(int64(time.Hour))
	f.Add(int64(1<<62 - 1))
	f.Add(int64(1<<63 - 1))

	f.Fuzz(func(t *testing.T, ns int64) {
		d := time.Duration(ns)
		result := SafeDouble(d)
		if d > 0 && result < 0 {
			t.Fatalf("SafeDouble(%v) returned negative: %v", d, result)
		}
		if d > 0 && result < d {
			t.Fatalf("SafeDouble(%v) returned smaller: %v", d, result)
		}
	})
}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

func FuzzRedirectPolicyFunc(f *testing.F) {
	f.Add("sub.docker.com", ".docker.com", "exact.host.com")
	f.Add("maliciousdocker.com", ".docker.com", "")
	f.Add("docker.com", "docker.com", "")
	f.Add("evil.com", ".example.org", "good.com")

	f.Fuzz(func(t *testing.T, host, suffix, allowedHost string) {
		if host == "" || suffix == "" {
			return
		}
		// Skip hosts that would produce invalid URLs.
		u, err := url.Parse("http://" + host + "/path")
		if err != nil || u.Hostname() == "" {
			return
		}
		host = u.Hostname() // use normalized hostname

		var opts []RedirectOption
		if allowedHost != "" {
			opts = append(opts, WithAllowedHosts(allowedHost))
		}
		opts = append(opts, WithAllowedSuffixes(suffix))
		policy := RedirectPolicyFunc(opts...)

		req := &http.Request{URL: u}
		via := []*http.Request{{URL: &url.URL{Host: "origin.com"}}}
		err = policy(req, via)

		if err == nil {
			// Policy accepted — verify invariant.
			if host == allowedHost {
				return // exact match is fine
			}
			// Must match the normalized suffix.
			norm := suffix
			if !strings.HasPrefix(norm, ".") {
				norm = "." + norm
			}
			if !hostMatchesSuffix(host, norm) {
				t.Fatalf("policy accepted host %q but it doesn't match suffix %q or allowedHost %q", host, suffix, allowedHost)
			}
		}
	})
}
