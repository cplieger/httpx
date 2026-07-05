package httpx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testCAPEM generates a throwaway self-signed CA certificate and returns it
// PEM-encoded. Used to exercise the pool/transport builders without touching
// disk or the system trust store.
func testCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "httpx-test-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestCACertPool exercises the internal caCertPool primitive directly (this is
// an in-package test so it can reach the unexported builder).
func TestCACertPool(t *testing.T) {
	t.Run("valid PEM builds a pool", func(t *testing.T) {
		pool, err := caCertPool(testCAPEM(t))
		if err != nil {
			t.Fatalf("caCertPool: %v", err)
		}
		if pool == nil {
			t.Fatal("pool is nil")
		}
	})

	t.Run("no certs is a loud error", func(t *testing.T) {
		for _, bad := range [][]byte{nil, {}, []byte("not a pem"), []byte("-----BEGIN CERTIFICATE-----\nnonsense\n-----END CERTIFICATE-----\n")} {
			pool, err := caCertPool(bad)
			if !errors.Is(err, ErrNoCertsInPEM) {
				t.Errorf("caCertPool(%q) err = %v, want ErrNoCertsInPEM", bad, err)
			}
			if pool != nil {
				t.Errorf("caCertPool(%q) pool = non-nil, want nil on error", bad)
			}
		}
	})

	t.Run("valid cert alongside a junk block still builds", func(t *testing.T) {
		// AppendCertsFromPEM returns true if it parsed at least one cert, so a
		// valid cert followed by an unparseable block yields a usable pool.
		mixed := append(testCAPEM(t), "\n-----BEGIN CERTIFICATE-----\nnotvalidbase64\n-----END CERTIFICATE-----\n"...)
		if pool, err := caCertPool(mixed); err != nil || pool == nil {
			t.Errorf("caCertPool(valid+junk) = (%v, %v), want (pool, nil)", pool, err)
		}
	})
}

func TestCATransport(t *testing.T) {
	t.Run("pins CA with verification on and TLS 1.2 floor", func(t *testing.T) {
		tr, err := CATransport(testCAPEM(t))
		if err != nil {
			t.Fatalf("CATransport: %v", err)
		}
		if tr.TLSClientConfig == nil {
			t.Fatal("TLSClientConfig is nil")
		}
		if tr.TLSClientConfig.RootCAs == nil {
			t.Error("RootCAs not set")
		}
		if tr.TLSClientConfig.InsecureSkipVerify {
			t.Error("InsecureSkipVerify must never be set")
		}
		if tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
			t.Errorf("MinVersion = %#x, want TLS 1.2 (%#x)", tr.TLSClientConfig.MinVersion, tls.VersionTLS12)
		}
		// Confirms it was cloned from http.DefaultTransport (which carries
		// ProxyFromEnvironment) rather than built as a bare &http.Transport{}.
		if tr.Proxy == nil {
			t.Error("Proxy is nil; transport was not cloned from http.DefaultTransport")
		}
	})

	t.Run("no certs is a loud error", func(t *testing.T) {
		tr, err := CATransport([]byte("garbage"))
		if !errors.Is(err, ErrNoCertsInPEM) {
			t.Errorf("err = %v, want ErrNoCertsInPEM", err)
		}
		if tr != nil {
			t.Error("transport non-nil on error")
		}
	})
}

// TestCATransport_verification is the end-to-end check: a transport pinning the
// server's CA connects; a transport pinning a DIFFERENT CA is rejected by TLS
// verification (proving the pin is enforced, not bypassed).
func TestCATransport_verification(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	serverCAPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})

	trusting, err := CATransport(serverCAPEM)
	if err != nil {
		t.Fatalf("CATransport(serverCA): %v", err)
	}
	resp, err := (&http.Client{Transport: trusting, Timeout: 5 * time.Second}).Get(srv.URL)
	if err != nil {
		t.Fatalf("request with pinned server CA failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}

	// A transport pinning an unrelated CA must reject the server with a
	// certificate/authority error — proving the pin is enforced, not bypassed.
	wrong, err := CATransport(testCAPEM(t))
	if err != nil {
		t.Fatalf("CATransport(wrongCA): %v", err)
	}
	badResp, err := (&http.Client{Transport: wrong, Timeout: 5 * time.Second}).Get(srv.URL)
	if err == nil {
		badResp.Body.Close()
		t.Fatal("request with an unpinned CA succeeded; TLS verification was not enforced")
	}
	var unknownAuthority x509.UnknownAuthorityError
	if !errors.As(err, &unknownAuthority) && !strings.Contains(err.Error(), "certificate") {
		t.Errorf("expected a certificate verification error, got: %v", err)
	}
}

func FuzzCACertPool(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte("garbage"))
	f.Add([]byte("-----BEGIN CERTIFICATE-----\nnotbase64\n-----END CERTIFICATE-----\n"))
	f.Fuzz(func(t *testing.T, pemBytes []byte) {
		pool, err := caCertPool(pemBytes)
		// Contract: exactly one of (pool, err) is set — never a nil pool with a
		// nil error, and never a pool alongside an error.
		if (err == nil) == (pool == nil) {
			t.Errorf("caCertPool invariant violated: pool=%v err=%v", pool, err)
		}
		if err != nil && !errors.Is(err, ErrNoCertsInPEM) {
			t.Errorf("unexpected error type: %v", err)
		}
	})
}

func TestCATransport_errors_when_DefaultTransport_replaced(t *testing.T) {
	// Documented contract: if http.DefaultTransport has been replaced by a
	// wrapping RoundTripper, CATransport returns an error. Swaps the process
	// global (stubRT is the package-internal RoundTripper from bench_test.go),
	// so this must NOT run in parallel with the TLS tests that clone it.
	orig := http.DefaultTransport
	http.DefaultTransport = &stubRT{}
	defer func() { http.DefaultTransport = orig }()

	tr, err := CATransport(testCAPEM(t))
	if tr != nil {
		t.Errorf("CATransport = %v, want nil transport when DefaultTransport is not *http.Transport", tr)
	}
	if err == nil || !strings.Contains(err.Error(), "not *http.Transport") {
		t.Errorf("CATransport err = %v, want an error containing 'not *http.Transport'", err)
	}
}
