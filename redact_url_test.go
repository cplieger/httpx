package httpx

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestRedactURL(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		absent  []string
		present []string
	}{
		{
			name:    "query value redacted, keys and path kept",
			in:      "https://tautulli.example/api/v2?apikey=supersecret&cmd=get_history",
			absent:  []string{"supersecret", "get_history"},
			present: []string{"apikey=REDACTED", "cmd=REDACTED", "tautulli.example", "/api/v2"},
		},
		{
			name:    "userinfo password masked, username kept",
			in:      "https://user:hunter2@example.com/path",
			absent:  []string{"hunter2"},
			present: []string{"user:xxxxx@", "example.com", "/path"},
		},
		{
			name:    "fragment dropped",
			in:      "https://example.com/p?token=abc#secretfrag",
			absent:  []string{"abc", "secretfrag"},
			present: []string{"token=REDACTED"},
		},
		{
			name:    "no query left unchanged",
			in:      "https://example.com/healthz",
			present: []string{"https://example.com/healthz"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := redactURL(tc.in)
			for _, s := range tc.absent {
				if strings.Contains(got, s) {
					t.Errorf("redactURL(%q) = %q, must not contain %q", tc.in, got, s)
				}
			}
			for _, s := range tc.present {
				if !strings.Contains(got, s) {
					t.Errorf("redactURL(%q) = %q, want substring %q", tc.in, got, s)
				}
			}
		})
	}
}

func TestRedactURL_unparseableYieldsPlaceholder(t *testing.T) {
	got := redactURL("://\x7f-not-a-url")
	if got != "[unparseable url]" {
		t.Errorf("redactURL(bad) = %q, want %q", got, "[unparseable url]")
	}
}

func TestLogSafeError(t *testing.T) {
	if logSafeError(nil) != nil {
		t.Fatal("logSafeError(nil) should be nil")
	}

	// A transport *url.Error embeds the full URL; logSafeError drops it.
	ue := &url.Error{
		Op:  "Get",
		URL: "https://example.com/p?apikey=supersecret",
		Err: errors.New("dial tcp: connection refused"),
	}
	got := logSafeError(ue)
	if strings.Contains(got.Error(), "supersecret") || strings.Contains(got.Error(), "example.com") {
		t.Errorf("logSafeError leaked URL: %q", got.Error())
	}
	if !strings.Contains(got.Error(), "connection refused") {
		t.Errorf("logSafeError dropped the cause: %q", got.Error())
	}

	// A *StatusError passes through (its Error() already redacts) and the
	// errors.As chain is preserved.
	se := &StatusError{Code: 429, URL: "https://example.com/p?apikey=supersecret"}
	got2 := logSafeError(se)
	var asSE *StatusError
	if !errors.As(got2, &asSE) {
		t.Error("logSafeError(*StatusError) broke the errors.As chain")
	}
	if strings.Contains(got2.Error(), "supersecret") {
		t.Errorf("StatusError still leaked the secret: %q", got2.Error())
	}
}

func TestRedactTransportError_unwraps_url_error_and_drops_url(t *testing.T) {
	ue := &url.Error{
		Op:  "Get",
		URL: "https://user:pw@host.example/api?apikey=supersecret",
		Err: errors.New("dial tcp: connection refused"),
	}
	got := RedactTransportError(ue, "fetch", "supersecret")
	if got == nil {
		t.Fatal("RedactTransportError(*url.Error) = nil, want error")
	}
	msg := got.Error()
	for _, leak := range []string{"supersecret", "host.example", "apikey", "pw"} {
		if strings.Contains(msg, leak) {
			t.Errorf("RedactTransportError leaked %q: %q", leak, msg)
		}
	}
	if !strings.Contains(msg, "connection refused") {
		t.Errorf("RedactTransportError = %q, want cause preserved", msg)
	}
}

func TestRedactSecret_replaces_secret_occurrences(t *testing.T) {
	got := RedactSecret(errors.New("auth failed: apikey=supersecret123 rejected"), "supersecret123")
	if got == nil {
		t.Fatal("RedactSecret = nil, want error")
	}
	msg := got.Error()
	if strings.Contains(msg, "supersecret123") {
		t.Errorf("RedactSecret = %q, must not contain the secret", msg)
	}
	if !strings.Contains(msg, "REDACTED") {
		t.Errorf("RedactSecret = %q, want REDACTED in place of secret", msg)
	}
}

// TestRedactTransportError_prefix_controls_wrapping pins the prefix branch: an
// empty prefix returns the cause verbatim, while a non-empty prefix renders
// "prefix: cause". No *url.Error and no secret are involved, isolating the
// prefix decision from the unwrap and redact steps.
func TestRedactTransportError_prefix_controls_wrapping(t *testing.T) {
	base := errors.New("connection refused")
	if got := RedactTransportError(base, "", ""); got.Error() != "connection refused" {
		t.Errorf(`RedactTransportError(err, "", "") = %q, want %q`, got.Error(), "connection refused")
	}
	if got := RedactTransportError(base, "fetch", ""); got.Error() != "fetch: connection refused" {
		t.Errorf(`RedactTransportError(err, "fetch", "") = %q, want %q`, got.Error(), "fetch: connection refused")
	}
}
