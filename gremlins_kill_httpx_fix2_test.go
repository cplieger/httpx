package httpx

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// Orchestrator fixup: deterministic kills for the still-living boundary/negation
// mutants in CheckHTTPStatus / ParseRetryAfter / ParseRetryAfterResponse /
// SafeDouble that the prior passes missed. Test-only.

func gkHttpxFix2Resp(code int, retryAfter string) *http.Response {
	h := http.Header{}
	if retryAfter != "" {
		h.Set("Retry-After", retryAfter)
	}
	return &http.Response{StatusCode: code, Header: h}
}

//nolint:bodyclose // synthetic responses passed to pure status/header funcs; bodies never read
func TestGkHttpxFix2_CheckHTTPStatus_classBoundaries(t *testing.T) {
	if err := CheckHTTPStatus(gkHttpxFix2Resp(200, "")); err != nil {
		t.Errorf("CheckHTTPStatus(200) = %v, want nil", err)
	}
	if err := CheckHTTPStatus(gkHttpxFix2Resp(399, "")); err != nil {
		t.Errorf("CheckHTTPStatus(399) = %v, want nil", err)
	}
	// status==400 is the boundary that distinguishes `code < 400` (kills >=)
	// and `code >= 400` (kills >): it must be a plain *HTTPStatusError.
	err400 := CheckHTTPStatus(gkHttpxFix2Resp(400, ""))
	var hse *HTTPStatusError
	if !errors.As(err400, &hse) {
		t.Fatalf("CheckHTTPStatus(400) = %v, want *HTTPStatusError", err400)
	}
	if hse.Code != 400 {
		t.Errorf("CheckHTTPStatus(400).Code = %d, want 400", hse.Code)
	}
	var ae *AuthError
	if !errors.As(CheckHTTPStatus(gkHttpxFix2Resp(401, "")), &ae) {
		t.Errorf("CheckHTTPStatus(401) want *AuthError")
	}
	if !errors.As(CheckHTTPStatus(gkHttpxFix2Resp(403, "")), &ae) {
		t.Errorf("CheckHTTPStatus(403) want *AuthError")
	}
	var rle *RateLimitError
	if !errors.As(CheckHTTPStatus(gkHttpxFix2Resp(429, "120")), &rle) {
		t.Fatalf("CheckHTTPStatus(429) want *RateLimitError")
	}
	if rle.RetryAfter != 120*time.Second {
		t.Errorf("CheckHTTPStatus(429).RetryAfter = %v, want 120s", rle.RetryAfter)
	}
	if err := CheckHTTPStatus(gkHttpxFix2Resp(500, "")); err == nil {
		t.Errorf("CheckHTTPStatus(500) = nil, want error")
	}
}

func TestGkHttpxFix2_ParseRetryAfter_values(t *testing.T) {
	if d := ParseRetryAfter(""); d != 0 {
		t.Errorf("ParseRetryAfter(\"\") = %v, want 0", d)
	}
	if d := ParseRetryAfter("0"); d != 0 {
		t.Errorf("ParseRetryAfter(\"0\") = %v, want 0", d)
	}
	if d := ParseRetryAfter("-7"); d != 0 {
		t.Errorf("ParseRetryAfter(\"-7\") = %v, want 0", d)
	}
	if d := ParseRetryAfter("12"); d != 12*time.Second {
		t.Errorf("ParseRetryAfter(\"12\") = %v, want 12s", d)
	}
	// Future HTTP-date must yield a positive duration: kills the `d > 0`
	// negation (mutant returns 0 for a real future deadline).
	future := time.Now().Add(45 * time.Second).UTC().Format(http.TimeFormat)
	if d := ParseRetryAfter(future); d <= 0 {
		t.Errorf("ParseRetryAfter(future) = %v, want > 0", d)
	}
	past := time.Now().Add(-45 * time.Second).UTC().Format(http.TimeFormat)
	if d := ParseRetryAfter(past); d != 0 {
		t.Errorf("ParseRetryAfter(past) = %v, want 0", d)
	}
}

//nolint:bodyclose // synthetic responses passed to pure status/header funcs; bodies never read
func TestGkHttpxFix2_ParseRetryAfterResponse_values(t *testing.T) {
	if d := ParseRetryAfterResponse(gkHttpxFix2Resp(429, "")); d != 0 {
		t.Errorf("ParseRetryAfterResponse(no header) = %v, want 0", d)
	}
	if d := ParseRetryAfterResponse(gkHttpxFix2Resp(429, "0")); d != 0 {
		t.Errorf("ParseRetryAfterResponse(\"0\") = %v, want 0", d)
	}
	if d := ParseRetryAfterResponse(gkHttpxFix2Resp(429, "9")); d != 9*time.Second {
		t.Errorf("ParseRetryAfterResponse(\"9\") = %v, want 9s", d)
	}
	// l-f4: ParseRetryAfterResponse now trims whitespace via the shared
	// parseRetryAfterValue ("  30  " previously parsed to 0 via a failed Atoi).
	if d := ParseRetryAfterResponse(gkHttpxFix2Resp(429, "  30  ")); d != 30*time.Second {
		t.Errorf("ParseRetryAfterResponse(\"  30  \") = %v, want 30s", d)
	}
	// Future date positive (kills the `d < 0` negation at line 300).
	future := time.Now().Add(45 * time.Second).UTC().Format(http.TimeFormat)
	if d := ParseRetryAfterResponse(gkHttpxFix2Resp(429, future)); d <= 0 {
		t.Errorf("ParseRetryAfterResponse(future) = %v, want > 0", d)
	}
	past := time.Now().Add(-45 * time.Second).UTC().Format(http.TimeFormat)
	if d := ParseRetryAfterResponse(gkHttpxFix2Resp(429, past)); d != 0 {
		t.Errorf("ParseRetryAfterResponse(past) = %v, want 0", d)
	}
}

func TestGkHttpxFix2_SafeDouble_overflow(t *testing.T) {
	if d := SafeDouble(0); d != 0 {
		t.Errorf("SafeDouble(0) = %v, want 0", d)
	}
	if d := SafeDouble(-3 * time.Second); d != -3*time.Second {
		t.Errorf("SafeDouble(-3s) = %v, want -3s", d)
	}
	if d := SafeDouble(2 * time.Second); d != 4*time.Second {
		t.Errorf("SafeDouble(2s) = %v, want 4s", d)
	}
	// No overflow at 2^61 -> 2^62.
	if d := SafeDouble(time.Duration(1 << 61)); d != time.Duration(1<<62) {
		t.Errorf("SafeDouble(2^61) = %v, want 2^62", d)
	}
	// Overflow path: doubling the max duration wraps negative -> capped to max.
	maxd := time.Duration(1<<63 - 1)
	if d := SafeDouble(maxd); d != maxd {
		t.Errorf("SafeDouble(maxDuration) = %v, want maxDuration (overflow cap)", d)
	}
}
