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

// DoConditional executes req as a single conditional request: it sets
// If-None-Match / If-Modified-Since from v (an empty field is not sent),
// performs the request on client, and classifies the response.
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
// It is deliberately a SINGLE attempt so the caller owns the retry and cache
// policy: wrap it in RetryWithBackoff (transient classification composes
// through the returned errors), rebuild req per attempt, and decide app-side
// when a cached copy may be reused on failure (stale-on-error) and whether
// validators may be sent at all (send the zero Validators when the cached body
// is unusable, so an empty cache can never be "revalidated" into a 304 with
// nothing to reuse). Intended for GET (or HEAD, where Body stays empty).
func DoConditional(client *http.Client, req *http.Request, v Validators, maxBodyBytes int64) (ConditionalResult, error) {
	if maxBodyBytes <= 0 {
		maxBodyBytes = DefaultMaxBodyBytes
	}
	if v.ETag != "" {
		req.Header.Set("If-None-Match", v.ETag)
	}
	if v.LastModified != "" {
		req.Header.Set("If-Modified-Since", v.LastModified)
	}
	//nolint:bodyclose,gosec // bodyclose: closed on every path below (ReadLimitedBody on 200, DrainClose otherwise); G704: the request is caller-built, so URL/SSRF policy is the caller's, as at every httpx entry point
	resp, err := client.Do(req)
	if err != nil {
		return ConditionalResult{}, err
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
				ETag:         resp.Header.Get("ETag"),
				LastModified: resp.Header.Get("Last-Modified"),
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
