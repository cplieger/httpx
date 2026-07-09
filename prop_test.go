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
			// Policy accepted — verify the invariant using the SAME ASCII
			// case-normalization the policy applies (RFC 3986 §6.2.2.1). url.Parse
			// preserves host case and the policy matches case-insensitively, so the
			// oracle must lowercase host, allowed host, and suffix identically.
			lhost := asciiLower(host)
			if lhost == asciiLower(allowedHost) {
				return // exact allowed-host match
			}
			// Must match the normalized (dot-anchored, ASCII-lowercased) suffix.
			norm := suffix
			if !strings.HasPrefix(norm, ".") {
				norm = "." + norm
			}
			norm = asciiLower(norm)
			if !hostMatchesSuffix(lhost, norm) {
				t.Fatalf("policy accepted host %q but it doesn't match suffix %q or allowedHost %q", host, suffix, allowedHost)
			}
		}
	})
}

func TestAsciiLower_invariants_property(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		s := string(rapid.SliceOf(rapid.Byte()).Draw(t, "bytes"))
		got := asciiLower(s)
		// Length is preserved. strings.ToLower would fold each invalid-UTF-8 byte
		// to the 3-byte U+FFFD, changing length and collapsing distinct hosts into
		// one allowlist-matching class (the documented bypass).
		if len(got) != len(s) {
			t.Fatalf("asciiLower(%q) len = %d, want %d", s, len(got), len(s))
		}
		// Only ASCII A-Z change (to a-z); every other byte is byte-identical.
		for i := range len(s) {
			in := s[i]
			want := in
			if 'A' <= in && in <= 'Z' {
				want += 'a' - 'A'
			}
			if got[i] != want {
				t.Fatalf("asciiLower(%q) byte %d = %#x, want %#x", s, i, got[i], want)
			}
		}
		// Idempotent.
		if again := asciiLower(got); again != got {
			t.Fatalf("asciiLower not idempotent: %q -> %q -> %q", s, got, again)
		}
	})
}

func TestAsciiLower_distinct_non_ascii_bytes_never_collapse_property(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		b1 := byte(rapid.IntRange(0x80, 0xFF).Draw(t, "b1"))
		b2 := byte(rapid.IntRange(0x80, 0xFF).Draw(t, "b2"))
		if b1 == b2 {
			return
		}
		// Distinct non-ASCII bytes must stay distinct after folding — the
		// no-collapse invariant strings.ToLower violates (both would become
		// U+FFFD and compare equal, bypassing the redirect allowlist).
		if asciiLower(string(b1)) == asciiLower(string(b2)) {
			t.Fatalf("asciiLower collapsed distinct bytes %#x and %#x", b1, b2)
		}
	})
}

func FuzzRedactURL(f *testing.F) {
	f.Add("apikey")
	f.Add("weird key&=#%")
	f.Add("")
	f.Add("user:pw@evil")
	f.Fuzz(func(t *testing.T, rawKey string) {
		// Panic-safety on arbitrary URL input.
		_ = redactURL("https://h.example/p?" + rawKey)
		// Security invariant (CWE-532): a distinctive sentinel placed as a query
		// VALUE never survives redaction. rawKey is QueryEscaped into the KEY
		// position so it can neither inject extra params nor turn the sentinel into
		// a (kept) query key — the sound sentinel form FuzzRedactTransportError uses
		// to avoid short-secret coincidences.
		const sentinel = "AKIAIOSFODNN7EXAMPLE"
		out := redactURL("https://h.example/p?" + url.QueryEscape(rawKey) + "=" + sentinel)
		if strings.Contains(out, sentinel) {
			t.Fatalf("sentinel query value survived redaction: %q", out)
		}
	})
}

// FuzzSameOriginRedirect exercises the WithSameHost + WithAllowSchemeDowngrade
// invariants (white-box, so it can reuse the policy's own asciiLower /
// isSchemeDowngrade oracle): with WithSameHost and downgrades refused (the
// default), a redirect is accepted iff its target host equals the origin host
// AND it is not an https->http downgrade; with downgrades allowed, only the
// same-host check gates acceptance. FuzzRedirectPolicyFunc seeds an empty origin
// scheme, so it never drives the scheme-downgrade branch this covers.
func FuzzSameOriginRedirect(f *testing.F) {
	f.Add("https", "arr.example", "https", "arr.example")   // same origin
	f.Add("http", "arr.example", "https", "arr.example")    // upgrade
	f.Add("https", "arr.example", "http", "arr.example")    // downgrade
	f.Add("https", "arr.example", "https", "other.example") // cross-host
	f.Add("https", "ARR.example", "https", "arr.example")   // case-fold

	f.Fuzz(func(t *testing.T, origScheme, origHost, tgtScheme, tgtHost string) {
		if (origScheme != "http" && origScheme != "https") || (tgtScheme != "http" && tgtScheme != "https") {
			return
		}
		orig, err := url.Parse(origScheme + "://" + origHost + "/a")
		if err != nil || orig.Hostname() == "" {
			return
		}
		tgt, err := url.Parse(tgtScheme + "://" + tgtHost + "/b")
		if err != nil || tgt.Hostname() == "" {
			return
		}
		via := []*http.Request{{URL: orig}}
		sameHost := asciiLower(tgt.Hostname()) == asciiLower(orig.Hostname())
		downgrade := isSchemeDowngrade(orig.Scheme, tgt.Scheme)

		// Default: downgrades refused.
		gotErr := RedirectPolicyFunc(WithSameHost())(&http.Request{URL: tgt}, via)
		if want := sameHost && !downgrade; want != (gotErr == nil) {
			t.Fatalf("WithSameHost %s->%s: err=%v (sameHost=%v downgrade=%v, wantAllowed=%v)", orig, tgt, gotErr, sameHost, downgrade, want)
		}

		// Downgrades allowed: only the host gates acceptance.
		gotAllow := RedirectPolicyFunc(WithSameHost(), WithAllowSchemeDowngrade(true))(&http.Request{URL: tgt}, via)
		if sameHost != (gotAllow == nil) {
			t.Fatalf("WithSameHost+AllowDowngrade %s->%s: err=%v (sameHost=%v)", orig, tgt, gotAllow, sameHost)
		}
	})
}
