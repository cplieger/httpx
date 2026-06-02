package httpx_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/cplieger/httpx"
)

func ExampleRetry() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "hello")
	}))
	defer srv.Close()

	body, err := httpx.Retry(context.Background(), srv.Client(), srv.URL,
		httpx.WithMaxAttempts(3),
		httpx.WithBaseDelay(100*time.Millisecond),
	)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(string(body))
	// Output: hello
}

func ExampleRetryWithBackoff() {
	attempts := 0
	result, err := httpx.RetryWithBackoff(context.Background(), 3, time.Millisecond, "example", func(_ context.Context) (string, error) {
		attempts++
		if attempts < 2 {
			return "", &httpx.HTTPStatusError{Code: 503}
		}
		return "done", nil
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(result)
	// Output: done
}
