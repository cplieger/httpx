package httpx

import (
	"context"
	"testing"
	"time"
)

// gremlins_kill_httpx_fix_test.go — FIXUP pass over the still-living httpx
// mutants. Only ONE of the 15 survivors is killable with a deterministic test
// (SleepCtx d==0 boundary, below). The other 14 are genuinely unkillable; the
// per-mutant reasoning is recorded here so the verdicts can be audited against
// the source rather than taken on faith.
//
// CONDITIONALS_BOUNDARY flips a relational operator (`<`<->`<=`, `>`<->`>=`), so
// the original and mutant disagree on EXACTLY ONE input value. A mutant is
// equivalent when, at that single boundary input, both code paths produce the
// identical observable result — then no test can distinguish them.
//
// EQUIVALENT (no test can ever kill these — boundary input yields identical output):
//
//   httpx.go:246:8  `n <= 0` -> `n < 0` (ParseRetryAfter). Diff only at n==0.
//       n==0: original returns 0; mutant falls through, capSecs=60, 0>60 false,
//       returns time.Duration(0)*time.Second == 0. Same.
//   httpx.go:251:8  `n > capSecs` -> `n >= capSecs` (ParseRetryAfter). capSecs==60,
//       RetryAfterCap==60*time.Second. Diff only at n==60: original falls through
//       returning 60*time.Second; mutant returns RetryAfterCap==60*time.Second. Same.
//   httpx.go:257:28 `d > 0` -> `d >= 0` (ParseRetryAfter, http-date branch). Diff
//       only at d==0: original returns 0; mutant returns min(0,RetryAfterCap)==0. Same.
//   httpx.go:274:11 `secs <= 0` -> `secs < 0` (ParseRetryAfterResponse). Diff only
//       at secs==0: original returns 0; mutant falls through, returns
//       time.Duration(0)*time.Second == 0. Same.
//   httpx.go:279:11 `secs > maxSecs` -> `secs >= maxSecs` (ParseRetryAfterResponse).
//       Diff only at secs==maxSecs: original falls through returning
//       time.Duration(maxSecs)*time.Second; mutant returns the same expression. Same.
//   httpx.go:286:8  `d < 0` -> `d <= 0` (ParseRetryAfterResponse, http-date branch).
//       Diff only at d==0: original returns d==0; mutant returns 0. Same.
//   httpx.go:300:21 `resp.StatusCode >= 200` -> `> 200` (CONDITIONALS_BOUNDARY) and
//       -> `< 200` (CONDITIONALS_NEGATION) in CheckHTTPStatus. The first `if` is a
//       redundant early-return: its only job is to return nil for <400 codes, which
//       the function tail already does (switch matches only 401/403/429, all >=400;
//       the >=400 branch is gated by the INDEPENDENT `< 400` operand, which is false
//       for every >=400 code regardless of the first operand). Mutating the first
//       operand cannot change any output. Both mutants equivalent.
//   httpx.go:325:13 `backoff <= 0` -> `backoff < 0` (JitteredBackoff). Diff only at
//       backoff==0: original returns 0; mutant falls through: half=0,
//       jitter=rand.Int64N(1)==0 (only value in [0,1)), returns 0. Same.
//   httpx.go:335:7  `d <= 0` -> `d < 0` (SafeDouble). Diff only at d==0: original
//       returns 0; mutant falls through: doubled=0, `0<0` false, returns 0. Same.
//   httpx.go:339:13 `doubled < d` -> `doubled <= d` (SafeDouble overflow guard).
//       Differs only when doubled==d. Reachable domain here is d>0 (d<=0 returned at
//       335). For d>0, d*2==d mod 2^64 implies d==0, impossible. So the operators
//       agree on every reachable input. Equivalent.
//
// HARD-SKIP (real behavioral mutants, but the distinguishing input is an exact
// equality on a wall-/monotonic-clock reading that advances between setup and the
// comparison, so the boundary is unreachable deterministically):
//
//   httpx.go:199:53 `time.Since(b.startTime) >= b.maxElapsedTime` -> `>`
//       (ExponentialBackoff.NextBackOff). Differs only when time.Since==maxElapsedTime
//       exactly; the clock is read live, so that point is unreachable.
//   roundtripper.go:295:48 `time.Since(start) >= rt.maxElapsedTime` -> `>`
//       (sleepBeforeRetry). Same clock seam; `start` is settable but the comparison
//       still reads time.Now() live, so equality is unreachable.
//   httpx.go:575:45 `elapsed > 10*time.Second` -> `>=` (Retry slow-response log).
//       Differs only when elapsed==10s exactly (clock seam); `start` is an internal
//       local and unsettable, and the sole effect is a log.Warn line.

// TestGkHttpxFix_SleepCtx_zeroDuration_returnsNilWithoutConsultingCtx kills the
// CONDITIONALS_BOUNDARY mutant at httpx.go:347:7:
//
//	func SleepCtx(ctx context.Context, d time.Duration) error {
//		if d <= 0 {          // 347:7   <=  ->  <
//			return nil
//		}
//		t := time.NewTimer(d)
//		select {
//		case <-ctx.Done():
//			t.Stop()
//			return ctx.Err()
//		case <-t.C:
//			return nil
//		}
//	}
//
// `<=` and `<` disagree on exactly one input: d == 0, which is supplied exactly
// here (d is an integer-valued parameter under full test control — unlike the
// time.Since clock seams above). With an already-cancelled context:
//
//   - original (d <= 0): 0<=0 == true  -> returns nil immediately; ctx ignored,
//     no timer, no select -> deterministic.
//   - mutant   (d <  0): 0<0  == false -> NewTimer(0) + select. ctx.Done() is a
//     closed channel (immediately ready); the 0-timer's send is delivered
//     asynchronously by the runtime timer goroutine, which has not run by the
//     time select evaluates, so select takes ctx.Done() and returns
//     context.Canceled (non-nil).
//
// Asserting nil therefore passes on the original every time (it never reaches
// the select, so there is no scheduler race on the original) and fails on the
// mutant. The loop drives the mutant-side failure to a certainty even under an
// adversarial select scheduler; it is flake-free on the original because that
// path short-circuits before the timer/select are ever created.
func TestGkHttpxFix_SleepCtx_zeroDuration_returnsNilWithoutConsultingCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so the mutant's select sees a ready ctx.Done()

	for range 200 {
		if got := SleepCtx(ctx, time.Duration(0)); got != nil {
			t.Fatalf("SleepCtx(cancelledCtx, 0) = %v, want nil "+
				"(original returns nil for d<=0 without consulting the context)", got)
		}
	}
}
