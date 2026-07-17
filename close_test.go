package httpx_test

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/cplieger/httpx/v3"
)

// TestClose_closes_idle_connections_on_transport pins that httpx.Close forwards
// to the client transport's CloseIdleConnections (a command to the unmanaged
// connection pool). Without it, an emptied Close body leaks idle connections and
// no test fails.
func TestClose_closes_idle_connections_on_transport(t *testing.T) {
	var closed atomic.Bool
	client := &http.Client{Transport: idleCloserTransport{onClose: func() { closed.Store(true) }}}
	httpx.Close(client)
	if !closed.Load() {
		t.Error("Close did not propagate CloseIdleConnections to the client transport")
	}
}

// idleCloserTransport records whether CloseIdleConnections was invoked; RoundTrip
// is never reached because Close performs no request.
type idleCloserTransport struct{ onClose func() }

func (idleCloserTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("RoundTrip not used by Close")
}

func (t idleCloserTransport) CloseIdleConnections() { t.onClose() }
