package httpx_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/httpx"
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

func TestRetry_doesNotLeakSecretInLogsOrError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	_, err := httpx.Retry(context.Background(), srv.Client(), srv.URL+"?apikey=supersecret",
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
