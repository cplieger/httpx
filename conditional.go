package httpx

import (
	"fmt"
	"net/http"
)

// --- Conditional GET (ETag / Last-Modified revalidation) ---

// Validators carries the cache validators captured from a previous 200
// response, replayed on the next conditional request so an unchanged resource
// is a cheap 304 instead of a re-download. Persist them alongside the cached
// body; the zero value sends no conditional headers (forcing a full 200).
// DoConditional validates both fields in both directions (see its validator
// hygiene contract), so values it captured are always safe to persist and
// replay verbatim.
type Validators struct {
	// ETag is replayed as If-None-Match.
	ETag string
	// LastModified is replayed as If-Modified-Since.
	LastModified string
}

// ConditionalResult is one conditional-request outcome. Fields are ordered
// largest-alignment-first for govet fieldalignment.
type ConditionalResult struct {
	// Validators are the fresh validators captured from a 200 response's ETag /
	// Last-Modified headers (either may be empty when the server sent none).
	// Zero on a 304: the caller keeps the validators it already holds.
	Validators Validators
	// Body is the full response body of a 200, bounded by the maxBodyBytes
	// given to DoConditional. Nil on a 304.
	Body []byte
	// NotModified reports a 304: the cached representation is still current.
	NotModified bool
}

// DoConditional executes req as a single conditional request: it owns both
// conditional headers — any pre-existing If-None-Match / If-Modified-Since on
// req is removed, then each is set from v (an empty field is not sent), so v
// alone decides what is replayed — performs the request on client, and
// classifies the response.
//
//   - 304 -> NotModified=true (body drained and closed; Validators zero — keep
//     the ones you sent).
//   - 200 -> the bounded body plus the response's fresh validators. A body over
//     maxBodyBytes fails loud with *ResponseTooLargeError rather than being
//     silently truncated; maxBodyBytes <= 0 means DefaultMaxBodyBytes.
//   - Anything else -> an error: the CheckHTTPStatus mapping for >= 400
//     (*AuthError, *RateLimitError — non-transient; *HTTPStatusError —
//     transient for 502/503/504), or a plain non-transient error for a status
//     that is neither usable content nor a revalidation (a 204, a 3xx from a
//     redirect-refusing client). The body is always closed.
//
// A transport error from the request itself is reduced via LogSafeError
// before it is returned: a *url.Error embeds the full request URL, so the
// reduction keeps query-string secrets out of caller error text (the same
// contract GetBytes applies to every error it returns), while preserving the
// cause for transient classification when composed with Do.
//
// It is deliberately a SINGLE attempt so the caller owns the retry and cache
// policy: wrap it in Do (transient classification composes through the
// returned errors), rebuild req per attempt, and decide app-side
// when a cached copy may be reused on failure (stale-on-error) and whether
// validators may be sent at all (send the zero Validators when the cached body
// is unusable, so an empty cache can never be "revalidated" into a 304 with
// nothing to reuse). Intended for GET (or HEAD, where Body stays empty).
//
// Validator hygiene: validators are validated in BOTH directions against the
// header field-value grammar (RFC 9110: no control bytes other than HTAB, no
// DEL) plus a 1 KiB per-value cap. A 200's ETag / Last-Modified that fails the
// check is captured as empty, so a hostile or corrupt upstream value can never
// enter the caller's persisted cache state; a replayed v field that fails it
// is not sent, so a validator poisoned in a store outside this package
// degrades to an unconditional GET and self-heals through the next 200's
// clean capture instead of failing at net/http's request-write validation on
// every subsequent attempt. Both drops are silent - the cost is one full
// re-download, never a correctness fault.
func DoConditional(client *http.Client, req *http.Request, v Validators, maxBodyBytes int64) (ConditionalResult, error) {
	if maxBodyBytes <= 0 {
		maxBodyBytes = DefaultMaxBodyBytes
	}
	req.Header.Del("If-None-Match")
	req.Header.Del("If-Modified-Since")
	if v.ETag != "" && validValidator(v.ETag) {
		req.Header.Set("If-None-Match", v.ETag)
	}
	if v.LastModified != "" && validValidator(v.LastModified) {
		req.Header.Set("If-Modified-Since", v.LastModified)
	}
	//nolint:bodyclose,gosec // bodyclose: closed on every path below (ReadLimitedBody on 200, DrainClose otherwise); G704: the request is caller-built, so URL/SSRF policy is the caller's, as at every httpx entry point
	resp, err := client.Do(req)
	if err != nil {
		return ConditionalResult{}, LogSafeError(err)
	}
	switch resp.StatusCode {
	case http.StatusNotModified:
		DrainClose(resp.Body)
		return ConditionalResult{NotModified: true}, nil
	case http.StatusOK:
		body, err := ReadLimitedBody(resp.Body, maxBodyBytes)
		if err != nil {
			return ConditionalResult{}, err
		}
		return ConditionalResult{
			Body: body,
			Validators: Validators{
				ETag:         captureValidator(resp.Header.Get("ETag")),
				LastModified: captureValidator(resp.Header.Get("Last-Modified")),
			},
		}, nil
	default:
		DrainClose(resp.Body)
		if statusErr := CheckHTTPStatus(resp); statusErr != nil {
			return ConditionalResult{}, statusErr
		}
		return ConditionalResult{}, fmt.Errorf("unexpected status %d on conditional request", resp.StatusCode)
	}
}

// maxValidatorBytes bounds one validator value accepted by DoConditional in
// either direction (capture or replay). Real ETags and HTTP-dates are tens of
// bytes; anything larger is a corrupt or hostile value that would bloat every
// caller's persisted cache state on capture.
const maxValidatorBytes = 1 << 10

// validValidator reports whether v is safe to persist and replay as an HTTP
// validator header value: within maxValidatorBytes and free of bytes illegal
// in a header field value (RFC 9110 field-value grammar: control characters
// other than HTAB, or DEL). A value with a CR/LF would otherwise be rejected
// by net/http at request-write time on every replay, and one with other
// control bytes or an absurd length is corrupt or hostile either way.
func validValidator(v string) bool {
	if len(v) > maxValidatorBytes {
		return false
	}
	for i := range len(v) {
		if c := v[i]; (c < 0x20 && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}

// captureValidator returns v, or empty when it fails validValidator, so an
// invalid upstream validator is captured as absent (the next request is an
// unconditional GET) rather than handed to the caller to persist.
func captureValidator(v string) string {
	if !validValidator(v) {
		return ""
	}
	return v
}
