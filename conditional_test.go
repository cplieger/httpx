package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// revalidatingServer serves body with the given validators: a request whose
// If-None-Match matches etag (or whose If-Modified-Since equals lastModified)
// gets a 304, everything else a 200 with fresh validator headers.
func revalidatingServer(t *testing.T, body, etag, lastModified string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (etag != "" && r.Header.Get("If-None-Match") == etag) ||
			(lastModified != "" && r.Header.Get("If-Modified-Since") == lastModified) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if etag != "" {
			w.Header().Set("ETag", etag)
		}
		if lastModified != "" {
			w.Header().Set("Last-Modified", lastModified)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newConditionalReq(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return req
}

func TestDoConditionalFullFetchCapturesValidators(t *testing.T) {
	t.Parallel()
	const lm = "Mon, 02 Jan 2006 15:04:05 GMT"
	srv := revalidatingServer(t, "payload", `"v1"`, lm)

	res, err := DoConditional(srv.Client(), newConditionalReq(t, srv.URL), Validators{}, 0)
	if err != nil {
		t.Fatalf("DoConditional: %v", err)
	}
	if res.NotModified {
		t.Error("NotModified = true on a 200")
	}
	if string(res.Body) != "payload" {
		t.Errorf("Body = %q, want payload", res.Body)
	}
	if res.Validators.ETag != `"v1"` || res.Validators.LastModified != lm {
		t.Errorf("Validators = %+v, want the response's ETag and Last-Modified", res.Validators)
	}
}

func TestDoConditionalNotModified(t *testing.T) {
	t.Parallel()
	srv := revalidatingServer(t, "payload", `"v1"`, "")

	res, err := DoConditional(srv.Client(), newConditionalReq(t, srv.URL), Validators{ETag: `"v1"`}, 0)
	if err != nil {
		t.Fatalf("DoConditional: %v", err)
	}
	if !res.NotModified {
		t.Error("NotModified = false, want true for a matching ETag")
	}
	if res.Body != nil {
		t.Errorf("Body = %q, want nil on a 304", res.Body)
	}
	if res.Validators != (Validators{}) {
		t.Errorf("Validators = %+v, want zero on a 304 (caller keeps its own)", res.Validators)
	}
}

func TestDoConditionalSendsOnlySetValidators(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		v       Validators
		wantINM string
		wantIMS string
	}{
		{name: "zero validators send no conditional headers", v: Validators{}},
		{name: "etag only", v: Validators{ETag: `"e"`}, wantINM: `"e"`},
		{name: "last-modified only", v: Validators{LastModified: "yesterday"}, wantIMS: "yesterday"},
		{name: "both", v: Validators{ETag: `"e"`, LastModified: "yesterday"}, wantINM: `"e"`, wantIMS: "yesterday"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gotINM, gotIMS string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotINM = r.Header.Get("If-None-Match")
				gotIMS = r.Header.Get("If-Modified-Since")
				_, _ = w.Write([]byte("x"))
			}))
			t.Cleanup(srv.Close)
			if _, err := DoConditional(srv.Client(), newConditionalReq(t, srv.URL), tc.v, 0); err != nil {
				t.Fatalf("DoConditional: %v", err)
			}
			if gotINM != tc.wantINM || gotIMS != tc.wantIMS {
				t.Errorf("sent (If-None-Match=%q, If-Modified-Since=%q), want (%q, %q)",
					gotINM, gotIMS, tc.wantINM, tc.wantIMS)
			}
		})
	}
}

func TestDoConditionalStatusMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		status    int
		check     func(t *testing.T, err error)
		transient bool
	}{
		{
			name: "503 maps to transient HTTPStatusError", status: http.StatusServiceUnavailable,
			check: func(t *testing.T, err error) {
				t.Helper()
				var se *HTTPStatusError
				if !errors.As(err, &se) || se.Code != http.StatusServiceUnavailable {
					t.Errorf("err = %v, want *HTTPStatusError{503}", err)
				}
			},
			transient: true,
		},
		{
			name: "429 maps to RateLimitError", status: http.StatusTooManyRequests,
			check: func(t *testing.T, err error) {
				t.Helper()
				var rl *RateLimitError
				if !errors.As(err, &rl) {
					t.Errorf("err = %v, want *RateLimitError", err)
				}
			},
		},
		{
			name: "401 maps to AuthError", status: http.StatusUnauthorized,
			check: func(t *testing.T, err error) {
				t.Helper()
				var ae *AuthError
				if !errors.As(err, &ae) {
					t.Errorf("err = %v, want *AuthError", err)
				}
			},
		},
		{
			name: "204 is an unexpected-status error", status: http.StatusNoContent,
			check: func(t *testing.T, err error) {
				t.Helper()
				if err == nil {
					t.Error("err = nil, want an unexpected-status error for 204")
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(srv.Close)
			_, err := DoConditional(srv.Client(), newConditionalReq(t, srv.URL), Validators{}, 0)
			if err == nil {
				t.Fatal("err = nil, want an error")
			}
			tc.check(t, err)
			if got := IsTransient(err); got != tc.transient {
				t.Errorf("IsTransient = %v, want %v (composability with Do)", got, tc.transient)
			}
		})
	}
}

func TestDoConditionalOversizedBodyFailsLoud(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 64))
	}))
	t.Cleanup(srv.Close)
	_, err := DoConditional(srv.Client(), newConditionalReq(t, srv.URL), Validators{}, 16)
	var tooLarge *ResponseTooLargeError
	if !errors.As(err, &tooLarge) || tooLarge.Limit != 16 {
		t.Fatalf("err = %v, want *ResponseTooLargeError{16}", err)
	}
}

func TestDoConditionalTransportErrorPassesThrough(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // refuse the connection
	_, err := DoConditional(http.DefaultClient, newConditionalReq(t, srv.URL), Validators{}, 0)
	if err == nil {
		t.Fatal("err = nil, want a transport error from a closed server")
	}
}

// TestDoConditionalComposesWithDo pins the documented idiom: a
// transient 503 retried by Do, the request rebuilt per attempt, succeeding on
// the second try.
func TestDoConditionalComposesWithDo(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("ETag", `"v2"`)
		_, _ = w.Write([]byte("fresh"))
	}))
	t.Cleanup(srv.Close)

	res, err := Do(t.Context(),
		func(ctx context.Context) (ConditionalResult, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)
			if err != nil {
				return ConditionalResult{}, err
			}
			return DoConditional(srv.Client(), req, Validators{ETag: `"v1"`}, 0)
		}, WithMaxAttempts(3), WithBaseDelay(time.Millisecond), WithLabel("conditional"))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if string(res.Body) != "fresh" || res.Validators.ETag != `"v2"` {
		t.Errorf("result = %+v, want the second attempt's fresh body and validators", res)
	}
	if calls.Load() != 2 {
		t.Errorf("server calls = %d, want 2 (one 503, one 200)", calls.Load())
	}
}
