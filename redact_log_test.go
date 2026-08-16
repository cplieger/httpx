package httpx_test

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/httpx/v4"
)

func TestStatusError_ErrorRedactsURL(t *testing.T) {
	se := &httpx.StatusError{Code: 429, URL: "https://user:pw@host.example/api?apikey=supersecret"}
	msg := se.Error()
	for _, s := range []string{"supersecret", "pw"} {
		if strings.Contains(msg, s) {
			t.Errorf("StatusError.Error() = %q, leaked %q", msg, s)
		}
	}
	if !strings.Contains(msg, "HTTP 429") {
		t.Errorf("StatusError.Error() = %q, want status code", msg)
	}
	// The raw URL field is still available for programmatic access.
	if !strings.Contains(se.URL, "supersecret") {
		t.Error("raw StatusError.URL should be preserved for callers")
	}
}

func TestGetBytes_doesNotLeakSecretInLogsOrError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	_, err := httpx.GetBytes(t.Context(), srv.Client(), srv.URL+"?apikey=supersecret",
		httpx.WithMaxAttempts(2),
		httpx.WithBaseDelay(time.Microsecond),
		httpx.WithLogger(logger),
	)
	if err == nil {
		t.Fatal("expected error from exhausted retries")
	}
	if strings.Contains(err.Error(), "supersecret") {
		t.Errorf("returned error leaked secret: %v", err)
	}
	// errors.Is must still resolve through the (redacted) StatusError chain.
	if !errors.Is(err, httpx.ErrServerError) {
		t.Errorf("errors.Is(err, ErrServerError) = false; chain not preserved: %v", err)
	}
	logged := buf.String()
	if strings.Contains(logged, "supersecret") {
		t.Errorf("retry logging leaked secret:\n%s", logged)
	}
	if !strings.Contains(logged, "retries exhausted") {
		t.Errorf("expected retry diagnostics in logs, got:\n%s", logged)
	}
}

// normalizeForTest reproduces the one behavior the ordering contract needs from
// a normalizing transform: an invalid UTF-8 byte becomes U+FFFD, exactly as
// runesafe's strings.Map and encoding/json both render it. It is built on
// strings.ToValidUTF8 rather than importing runesafe because httpx carries no
// runtime dependencies.
func normalizeForTest(s string) string {
	return strings.ToValidUTF8(s, "\uFFFD")
}

// TestRedactSecretStringOrderingContract pins the composition documented on
// RedactSecretString: redact, normalize, redact, cap. Legs (a) and (b)
// reproduce the two single-sided failures, and leg (c) shows the canonical
// order clean, asserting at both redaction boundaries so dropping either call
// fails here rather than silently reopening the trap.
func TestRedactSecretStringOrderingContract(t *testing.T) {
	// An invalid UTF-8 byte is the cheapest value whose raw and normalized forms
	// differ, and it is the shape of the seed case this contract came from.
	secret := "tok" + "\xff" + "en"
	normalized := normalizeForTest(secret)
	if secret == normalized {
		t.Fatalf("fixture is vacuous: raw and normalized secret are both %q", secret)
	}

	// The haystack carries the value twice: once as the exact bytes the caller
	// holds, and once with a different invalid byte, which no byte-exact needle
	// matches and which the transform maps onto the secret's normalized form.
	variant := "tok" + "\xfe" + "en"
	if strings.Contains(variant, secret) || normalizeForTest(variant) != normalized {
		t.Fatalf("fixture is vacuous: variant %q must miss the raw needle and normalize onto it", variant)
	}
	haystack := "upstream rejected token=" + secret + " retried token=" + variant + " (401)"

	// (a) Shape A: redaction before the transform and none after. The transform
	// builds the secret's normalized form out of text the needle never matched,
	// so the value shows up after the mask already ran.
	beforeOnly := normalizeForTest(httpx.RedactSecretString(haystack, secret))
	if !strings.Contains(beforeOnly, normalized) {
		t.Errorf("shape A no longer reproduces, fixture is stale: %q", beforeOnly)
	}

	// (b) Shape D: redaction only after the transform, with the needle in the raw
	// representation. The transform rewrote the byte inside the value, so the
	// byte-exact needle matches nothing and the normalized value survives.
	afterOnly := httpx.RedactSecretString(normalizeForTest(haystack), secret)
	if !strings.Contains(afterOnly, normalized) {
		t.Errorf("shape D no longer reproduces, fixture is stale: %q", afterOnly)
	}

	// (c) The canonical composition: redact, normalize, redact. Each side passes
	// the needle in the representation that side carries.
	redacted := httpx.RedactSecretString(haystack, secret)
	if strings.Contains(redacted, secret) {
		t.Errorf("redaction before the transform left the raw value in place: %q", redacted)
	}
	safe := httpx.RedactSecretString(normalizeForTest(redacted), normalized)
	if strings.Contains(safe, secret) || strings.Contains(safe, normalized) {
		t.Errorf("canonical composition leaked the secret: %q", safe)
	}
	if strings.Count(safe, "REDACTED") != 2 {
		t.Errorf("canonical composition redacted %d of 2 occurrences: %q", strings.Count(safe, "REDACTED"), safe)
	}
}
