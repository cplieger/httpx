window.BENCHMARK_DATA = {
  "lastUpdate": 1787310766516,
  "repoUrl": "https://github.com/cplieger/ci",
  "entries": {
    "Benchmark": [
      {
        "commit": {
          "author": {
            "name": "cplieger",
            "username": "cplieger",
            "email": "917744+cplieger@users.noreply.github.com"
          },
          "committer": {
            "name": "Christopher Plieger",
            "username": "cplieger",
            "email": "917744+cplieger@users.noreply.github.com"
          },
          "id": "a66dd3d4479d96bf77d84ed08b78651e2477d1f4",
          "message": "fix: measure the weekly benchmarks instead of reporting an empty run green\n\nThe fanout discovered repos with a jq filter that emits one name per line, then tested enrolment with a space-delimited substring match. A newline is not a space, so every enrolled repo was rejected as not live, the matrix came out empty, the run job skipped on its non-empty guard, and the leg reported success having measured nothing. Confirmed by the absence of a benchmarks branch on all four enrolled repos despite three consecutive green runs.\n\nFlattens the discovery output, then makes the two silent paths fail closed: a hardcoded enrolment list resolving to zero live repos is a defect rather than a weekly state, and an empty matrix now fails instead of skipping the run job. Also guards the HEAD lookup, which had the same unguarded shape that took down the sibling mutation-testing fanout in August.",
          "timestamp": "2026-08-21T11:04:22Z",
          "url": "https://github.com/cplieger/ci/commit/a66dd3d4479d96bf77d84ed08b78651e2477d1f4"
        },
        "date": 1787310765767,
        "tool": "customSmallerIsBetter",
        "benches": [
          {
            "name": "BenchmarkIsTransient_Nil - B/op",
            "value": 0,
            "range": "± 0.0",
            "unit": "B/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkIsTransient_Nil - allocs/op",
            "value": 0,
            "range": "± 0.0",
            "unit": "allocs/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkIsTransient_Nil",
            "value": 1,
            "range": "± 0.0214",
            "unit": "ns/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkIsTransient_PermanentError - B/op",
            "value": 0,
            "range": "± 0.0",
            "unit": "B/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkIsTransient_PermanentError - allocs/op",
            "value": 0,
            "range": "± 0.0",
            "unit": "allocs/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkIsTransient_PermanentError",
            "value": 2.9475,
            "range": "± 0.418",
            "unit": "ns/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkIsTransient_UnexpectedEOF - B/op",
            "value": 0,
            "range": "± 0.0",
            "unit": "B/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkIsTransient_UnexpectedEOF - allocs/op",
            "value": 0,
            "range": "± 0.0",
            "unit": "allocs/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkIsTransient_UnexpectedEOF",
            "value": 96.315,
            "range": "± 5.73",
            "unit": "ns/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkJitteredBackoff - B/op",
            "value": 0,
            "range": "± 0.0",
            "unit": "B/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkJitteredBackoff - allocs/op",
            "value": 0,
            "range": "± 0.0",
            "unit": "allocs/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkJitteredBackoff",
            "value": 5.88,
            "range": "± 0.143",
            "unit": "ns/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkParseRetryAfter_Empty - B/op",
            "value": 0,
            "range": "± 0.0",
            "unit": "B/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkParseRetryAfter_Empty - allocs/op",
            "value": 0,
            "range": "± 0.0",
            "unit": "allocs/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkParseRetryAfter_Empty",
            "value": 1.86,
            "range": "± 0.259",
            "unit": "ns/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkParseRetryAfter_HTTPDate - B/op",
            "value": 80,
            "range": "± 0.0",
            "unit": "B/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkParseRetryAfter_HTTPDate - allocs/op",
            "value": 2,
            "range": "± 0.0",
            "unit": "allocs/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkParseRetryAfter_HTTPDate",
            "value": 253.8,
            "range": "± 23.2",
            "unit": "ns/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkParseRetryAfter_Seconds - B/op",
            "value": 0,
            "range": "± 0.0",
            "unit": "B/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkParseRetryAfter_Seconds - allocs/op",
            "value": 0,
            "range": "± 0.0",
            "unit": "allocs/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkParseRetryAfter_Seconds",
            "value": 11.115,
            "range": "± 3.56",
            "unit": "ns/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkRetryRoundTripper_RetryThenSuccess - B/op",
            "value": 1292,
            "range": "± 1.0",
            "unit": "B/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkRetryRoundTripper_RetryThenSuccess - allocs/op",
            "value": 11,
            "range": "± 0.0",
            "unit": "allocs/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkRetryRoundTripper_RetryThenSuccess",
            "value": 639.15,
            "range": "± 11.6",
            "unit": "ns/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkRetryRoundTripper_Success - B/op",
            "value": 512,
            "range": "± 0.0",
            "unit": "B/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkRetryRoundTripper_Success - allocs/op",
            "value": 3,
            "range": "± 0.0",
            "unit": "allocs/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkRetryRoundTripper_Success",
            "value": 225.15,
            "range": "± 16.3",
            "unit": "ns/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkSafeDouble - B/op",
            "value": 0,
            "range": "± 0.0",
            "unit": "B/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkSafeDouble - allocs/op",
            "value": 0,
            "range": "± 0.0",
            "unit": "allocs/op",
            "extra": "10 samples, median"
          },
          {
            "name": "BenchmarkSafeDouble",
            "value": 0.6589,
            "range": "± 0.002",
            "unit": "ns/op",
            "extra": "10 samples, median"
          }
        ]
      }
    ]
  }
}