package httpx

import (
	"math"
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

	f.Fuzz(func(t *testing.T, val string) {
		d := ParseRetryAfter(val)
		if d < 0 {
			t.Fatalf("ParseRetryAfter(%q) returned negative: %v", val, d)
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
		result := RedactTransportError(err, prefix, secret)
		if err == nil {
			if result != nil {
				t.Fatal("expected nil for nil input")
			}
			return
		}
		if secret != "" && result != nil {
			if strings.Contains(result.Error(), secret) {
				t.Fatalf("output contains secret: %q", result.Error())
			}
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
