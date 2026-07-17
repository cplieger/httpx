package httpx_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/cplieger/httpx/v3"
)

func ExampleGetBytes() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "hello")
	}))
	defer srv.Close()

	body, err := httpx.GetBytes(context.Background(), srv.Client(), srv.URL,
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

func ExampleDo() {
	attempts := 0
	result, err := httpx.Do(context.Background(), func(_ context.Context) (string, error) {
		attempts++
		if attempts < 2 {
			return "", &httpx.HTTPStatusError{Code: 503}
		}
		return "done", nil
	}, httpx.WithMaxAttempts(3), httpx.WithBaseDelay(time.Millisecond), httpx.WithLabel("example"))
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(result)
	// Output: done
}
