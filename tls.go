package httpx

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
)

// ErrNoCertsInPEM is returned by CACertPool and CATransport when the supplied
// PEM data contains no parseable certificates. A misconfigured or empty CA
// file therefore fails loudly rather than silently producing a transport that
// trusts nothing (which would reject every connection with an opaque error).
var ErrNoCertsInPEM = errors.New("httpx: no PEM-encoded certificates found")

// caCfg holds the resolved options for CACertPool / CATransport.
type caCfg struct {
	systemRoots bool
}

// CAOption configures CACertPool and CATransport.
type CAOption func(*caCfg)

// WithSystemRoots trusts the supplied CA certificate(s) IN ADDITION to the
// host's system trust store, rather than pinning them as the sole anchors.
// Use it for mixed trust — for example a corporate MITM CA alongside the
// public CAs a service also talks to. Omit it (the default) to pin ONLY the
// supplied CA(s), the tighter setup for a known self-hosted endpoint with a
// private or self-signed certificate. If the system pool cannot be loaded,
// the pool falls back to the supplied certificates only.
func WithSystemRoots() CAOption {
	return func(c *caCfg) { c.systemRoots = true }
}

// CACertPool builds an *x509.CertPool trusting the CA certificate(s) in pem.
// By default the pool contains ONLY those certificates (pinning); pass
// WithSystemRoots to add them to a copy of the system trust store instead.
//
// It returns ErrNoCertsInPEM when pem yields no certificates, so an empty or
// malformed cert file is a loud error rather than a silently-empty pool.
// This is the lower-level primitive CATransport is built on; use it directly
// when you need a pool for your own *tls.Config, gRPC transport credentials,
// or similar.
func CACertPool(pem []byte, opts ...CAOption) (*x509.CertPool, error) {
	var cfg caCfg
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	pool := x509.NewCertPool()
	if cfg.systemRoots {
		// SystemCertPool returns a COPY, so appending to it never mutates the
		// process-wide roots. On failure (e.g. a minimal container image with
		// no CA bundle) fall back to a fresh pool holding only pem.
		if sys, err := x509.SystemCertPool(); err == nil && sys != nil {
			pool = sys
		}
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, ErrNoCertsInPEM
	}
	return pool, nil
}

// CATransport builds an *http.Transport that trusts the CA certificate(s) in
// pem for TLS verification. It is cloned from http.DefaultTransport, so it
// keeps the standard connection pooling, dial/keepalive timeouts, and proxy
// support (ProxyFromEnvironment); only the TLS trust configuration is set.
// Verification stays ENABLED — InsecureSkipVerify is never set — with a
// TLS 1.2 minimum (an existing stricter minimum, e.g. TLS 1.3, is preserved).
//
// By default the supplied CA(s) are the SOLE trust anchors: the transport
// rejects any host not chaining to them, including public-CA hosts. This is
// the right setup for a known self-hosted endpoint presenting a private or
// self-signed certificate (a Plex server, an internal API). Pass
// WithSystemRoots to trust the CA(s) in ADDITION to the system trust store.
//
// The returned *http.Transport is concrete and mutable, so callers may tune
// fields such as MaxIdleConnsPerHost or pass it as the base RoundTripper of
// NewRetryRoundTripper:
//
//	tr, err := httpx.CATransport(pem)
//	client := httpx.NewRetryRoundTripper(tr, httpx.WithRTMaxAttempts(3)).StandardClient()
//
// It returns ErrNoCertsInPEM when pem yields no certificates. The caller owns
// reading the PEM bytes (from a file, a secret, an env var), which keeps this
// function I/O-free and lets the caller bound the read as it sees fit.
func CATransport(pem []byte, opts ...CAOption) (*http.Transport, error) {
	pool, err := CACertPool(pem, opts...)
	if err != nil {
		return nil, err
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// http.DefaultTransport is always *http.Transport in the standard
		// library; this guard is defensive against a program that replaced it
		// with a different RoundTripper before calling CATransport.
		return nil, errors.New("httpx: http.DefaultTransport is not *http.Transport")
	}
	tr := base.Clone()
	// Preserve any TLS settings already on the cloned transport (NextProtos,
	// cipher preferences) and layer the pinned roots + TLS 1.2 floor on top,
	// rather than discarding them with a fresh *tls.Config. Clone is nil-safe.
	tlsCfg := tr.TLSClientConfig.Clone()
	if tlsCfg == nil {
		tlsCfg = &tls.Config{}
	}
	tlsCfg.RootCAs = pool
	if tlsCfg.MinVersion < tls.VersionTLS12 {
		tlsCfg.MinVersion = tls.VersionTLS12
	}
	tr.TLSClientConfig = tlsCfg
	return tr, nil
}
