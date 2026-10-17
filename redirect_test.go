package httpx_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/httpx/v5"
)

func redirectReq(host string) *http.Request {
	u, _ := url.Parse("https://" + host + "/some/path")
	return &http.Request{URL: u}
}

func redirectVia(n int) []*http.Request {
	via := make([]*http.Request, n)
	for i := range n {
		via[i] = &http.Request{}
	}
	return via
}

func TestDockerGitHubRedirectPolicy(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		viaLen  int
		wantErr bool
	}{
		{"hub.docker.com allowed", "hub.docker.com", 0, false},
		{"subdomain of docker.com allowed", "auth.docker.com", 0, false},
		{"github.com allowed", "github.com", 0, false},
		{"subdomain of github.com allowed", "api.github.com", 0, false},
		{"githubusercontent.com allowed", "raw.githubusercontent.com", 0, false},
		{"evil.com refused", "evil.com", 0, true},
		{"localhost refused", "localhost", 0, true},
		{"127.0.0.1 refused", "127.0.0.1", 0, true},
		{"too many redirects", "hub.docker.com", 6, true},
		{"5 redirects still ok", "hub.docker.com", 5, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := httpx.DockerGitHubRedirectPolicy(redirectReq(tt.host), redirectVia(tt.viaLen))
			if tt.wantErr && err == nil {
				t.Errorf("DockerGitHubRedirectPolicy(%q, via=%d) = nil, want error", tt.host, tt.viaLen)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("DockerGitHubRedirectPolicy(%q, via=%d) = %v, want nil", tt.host, tt.viaLen, err)
			}
		})
	}
}

func TestRedirectPolicyFunc(t *testing.T) {
	policy := httpx.RedirectPolicyFunc(
		httpx.WithAllowedHosts("example.com"),
		httpx.WithAllowedSuffixes(".example.org"),
		httpx.WithMaxHops(3),
	)

	tests := []struct {
		name    string
		host    string
		viaLen  int
		wantErr bool
	}{
		{"exact host allowed", "example.com", 0, false},
		{"suffix allowed", "sub.example.org", 0, false},
		{"unknown refused", "evil.com", 0, true},
		{"too many hops", "example.com", 4, true},
		{"3 hops ok", "example.com", 3, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := policy(redirectReq(tt.host), redirectVia(tt.viaLen))
			if tt.wantErr && err == nil {
				t.Errorf("want error for %s via=%d", tt.host, tt.viaLen)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error for %s via=%d: %v", tt.host, tt.viaLen, err)
			}
		})
	}

	// no options refuses all
	nilPolicy := httpx.RedirectPolicyFunc()
	if err := nilPolicy(redirectReq("example.com"), nil); err == nil {
		t.Error("no-options policy should refuse all redirects")
	}
}

// TestRedirectPolicyFunc_hosts_only_allows_configured_host pins that an
// allowlist of hosts (no suffixes) still selects the allow path, not the
// refuse-all branch (which only applies when BOTH lists are empty).
func TestRedirectPolicyFunc_hosts_only_allows_configured_host(t *testing.T) {
	policy := httpx.RedirectPolicyFunc(httpx.WithAllowedHosts("example.com"))
	if err := policy(redirectReq("example.com"), nil); err != nil {
		t.Errorf("RedirectPolicyFunc(hosts only) to example.com = %v, want nil", err)
	}
}

// TestRedirectPolicyFunc_suffixes_only_allows_configured_suffix is the suffix
// twin of the hosts-only case.
func TestRedirectPolicyFunc_suffixes_only_allows_configured_suffix(t *testing.T) {
	policy := httpx.RedirectPolicyFunc(httpx.WithAllowedSuffixes(".example.org"))
	if err := policy(redirectReq("sub.example.org"), nil); err != nil {
		t.Errorf("RedirectPolicyFunc(suffixes only) to sub.example.org = %v, want nil", err)
	}
}

// TestRedirectPolicyFunc_default_max_hops verifies the default hop cap when
// WithMaxHops is not set: the 5th redirect is followed, the 6th refused.
func TestRedirectPolicyFunc_default_max_hops(t *testing.T) {
	policy := httpx.RedirectPolicyFunc(httpx.WithAllowedHosts("example.com"))
	if err := policy(redirectReq("example.com"), redirectVia(5)); err != nil {
		t.Errorf("the 5th redirect should be followed: %v", err)
	}
	if err := policy(redirectReq("example.com"), redirectVia(6)); err == nil {
		t.Error("the 6th redirect should be refused (default cap 5)")
	}
}

func TestDefaultRedirectPolicy_same_host_allowed(t *testing.T) {
	origURL, _ := url.Parse("https://example.com/start")
	redirURL, _ := url.Parse("https://example.com/other")
	via := []*http.Request{{URL: origURL}}
	if err := httpx.DefaultRedirectPolicy(&http.Request{URL: redirURL}, via); err != nil {
		t.Errorf("same-host redirect should be allowed, got %v", err)
	}
}

func TestDefaultRedirectPolicy_cross_host_refused(t *testing.T) {
	origURL, _ := url.Parse("https://example.com/start")
	redirURL, _ := url.Parse("https://evil.com/x")
	via := []*http.Request{{URL: origURL}}
	if err := httpx.DefaultRedirectPolicy(&http.Request{URL: redirURL}, via); err == nil {
		t.Error("cross-host redirect should be refused")
	}
}

func TestDefaultRedirectPolicy_first_redirect_no_via(t *testing.T) {
	redirURL, _ := url.Parse("https://anywhere.com/x")
	if err := httpx.DefaultRedirectPolicy(&http.Request{URL: redirURL}, nil); err != nil {
		t.Errorf("first redirect (no via) should be allowed, got %v", err)
	}
}

func TestDefaultRedirectPolicy_too_many_hops(t *testing.T) {
	origURL, _ := url.Parse("https://example.com/start")
	redirURL, _ := url.Parse("https://example.com/x")
	via := make([]*http.Request, 6)
	for i := range via {
		via[i] = &http.Request{URL: origURL}
	}
	if err := httpx.DefaultRedirectPolicy(&http.Request{URL: redirURL}, via); err == nil {
		t.Error("should refuse the 6th redirect")
	}
}

func TestNewClient_wires_timeout_and_redirect_policy(t *testing.T) {
	c := httpx.NewClient(42 * time.Second)
	if c.Timeout != 42*time.Second {
		t.Errorf("Timeout = %v, want 42s", c.Timeout)
	}
	if c.CheckRedirect == nil {
		t.Fatal("CheckRedirect is nil")
	}
	// DefaultRedirectPolicy denies cross-host redirects.
	origURL, _ := url.Parse("https://example.com/start")
	redirURL, _ := url.Parse("https://evil.com/x")
	via := []*http.Request{{URL: origURL}}
	if err := c.CheckRedirect(&http.Request{URL: redirURL}, via); err == nil {
		t.Error("CheckRedirect(evil.com) = nil, want error")
	}
	// Same-host redirect is allowed.
	sameURL, _ := url.Parse("https://example.com/other")
	if err := c.CheckRedirect(&http.Request{URL: sameURL}, via); err != nil {
		t.Errorf("CheckRedirect(same host) = %v, want nil", err)
	}
}

func TestRedirect_case_insensitive_host_matching(t *testing.T) {
	// RFC 3986 6.2.2.1 host comparison is case-insensitive; url.Parse preserves
	// host case, so these uppercase/mixed-case targets drive the asciiLower fold
	// the (all-lowercase) other redirect tests never reach.
	for _, host := range []string{"HUB.DOCKER.COM", "API.GITHUB.COM", "Raw.GitHubUserContent.com"} {
		if err := httpx.DockerGitHubRedirectPolicy(redirectReq(host), redirectVia(0)); err != nil {
			t.Errorf("DockerGitHubRedirectPolicy(%q) = %v, want nil (case-insensitive match)", host, err)
		}
	}
	policy := httpx.RedirectPolicyFunc(
		httpx.WithAllowedHosts("example.com"),
		httpx.WithAllowedSuffixes(".example.org"),
	)
	if err := policy(redirectReq("EXAMPLE.COM"), nil); err != nil {
		t.Errorf("RedirectPolicyFunc allowed-host uppercase EXAMPLE.COM = %v, want nil", err)
	}
	if err := policy(redirectReq("Sub.Example.ORG"), nil); err != nil {
		t.Errorf("RedirectPolicyFunc suffix mixed-case Sub.Example.ORG = %v, want nil", err)
	}
}

// TestDockerGitHubRedirectPolicy_substring_and_bare_domain_refused pins the
// dot-anchoring of the allowlist suffixes: a host that only CONTAINS an allowed
// domain as a substring, a bare allowed domain, or an allowed domain used as a
// left label must all be refused. DockerGitHubRedirectPolicy inlines its own
// strings.HasSuffix checks (it shares no code with the fuzzed RedirectPolicyFunc),
// so without these a regression dropping a leading dot (".docker.com" ->
// "docker.com") would let maliciousdocker.com through and no other
// DockerGitHubRedirectPolicy case would fail.
func TestDockerGitHubRedirectPolicy_substring_and_bare_domain_refused(t *testing.T) {
	for _, host := range []string{
		"maliciousdocker.com",
		"notgithub.com",
		"evilgithubusercontent.com",
		"docker.com",
		"hub.docker.com.attacker.example",
		"api.github.com.attacker.example",
	} {
		if err := httpx.DockerGitHubRedirectPolicy(redirectReq(host), redirectVia(0)); err == nil {
			t.Errorf("DockerGitHubRedirectPolicy(%q) = nil, want refused (substring/bare-domain must not match a dot-anchored suffix)", host)
		}
	}
}

// TestRedirectPolicyFunc_empty_suffix_fails_closed pins the fail-closed guard
// in normalizeSuffixes: an empty, bare-dot, or whitespace-only allowed suffix
// is DROPPED rather than dot-anchored to a bare ".", so a policy configured
// with only such a suffix (and no hosts) refuses every redirect -- including a
// trailing-dot FQDN, which a surviving "." suffix would otherwise match via
// hostMatchesSuffix's strings.HasSuffix(host, ".") branch (the documented
// redirect-allowlist bypass). FuzzRedirectPolicyFunc skips empty suffixes, so
// this branch is otherwise unexercised.
func TestRedirectPolicyFunc_empty_suffix_fails_closed(t *testing.T) {
	for _, suffix := range []string{"", ".", "   "} {
		policy := httpx.RedirectPolicyFunc(httpx.WithAllowedSuffixes(suffix))
		if err := policy(redirectReq("evil.example."), nil); err == nil {
			t.Errorf("RedirectPolicyFunc(WithAllowedSuffixes(%q)) allowed a trailing-dot FQDN, want refused (empty/bare-dot suffix must be dropped, failing closed)", suffix)
		}
		if err := policy(redirectReq("anything.example"), nil); err == nil {
			t.Errorf("RedirectPolicyFunc(WithAllowedSuffixes(%q)) allowed anything.example, want refused (no usable suffix, no hosts)", suffix)
		}
	}
}

// reqTo builds a redirect target request for the given URL.
func reqTo(t *testing.T, rawURL string) *http.Request {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", rawURL, err)
	}
	return &http.Request{URL: u}
}

// viaWithOrigin builds a one-element via chain whose original request carries
// the given URL, so same-host and scheme-downgrade checks have an origin to
// compare against.
func viaWithOrigin(t *testing.T, rawURL string) []*http.Request {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", rawURL, err)
	}
	return []*http.Request{{URL: u}}
}

// viaWithOriginMethod is viaWithOrigin with the origin's method set, so
// WithPreserveMethod checks have a method to compare against.
func viaWithOriginMethod(t *testing.T, rawURL, method string) []*http.Request {
	t.Helper()
	via := viaWithOrigin(t, rawURL)
	via[0].Method = method
	return via
}

// TestRedirectPolicyFunc_sameHost pins the WithSameHost behavior: same-host
// redirects (including an http->https upgrade) are followed, a cross-host hop
// and a same-host https->http downgrade are refused, host matching is
// ASCII-case-insensitive, and a differing port on the same host is still same
// host.
func TestRedirectPolicyFunc_sameHost(t *testing.T) {
	policy := httpx.RedirectPolicyFunc(httpx.WithSameHost(true), httpx.WithMaxHops(10))
	tests := []struct {
		name    string
		orig    string
		target  string
		wantErr bool
	}{
		{"same host same scheme", "https://arr.example/a", "https://arr.example/b", false},
		{"same host http->https upgrade", "http://arr.example/a", "https://arr.example/b", false},
		{"same host https->http downgrade refused", "https://arr.example/a", "http://arr.example/b", true},
		{"same host case-insensitive", "https://ARR.Example/a", "https://arr.example/b", false},
		{"cross host refused", "https://arr.example/a", "https://other.example/b", true},
		{"same host differing port allowed", "https://arr.example:8989/a", "https://arr.example:9090/b", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := policy(reqTo(t, tt.target), viaWithOrigin(t, tt.orig))
			if tt.wantErr != (err != nil) {
				t.Errorf("policy(%s -> %s) err=%v, wantErr=%v", tt.orig, tt.target, err, tt.wantErr)
			}
		})
	}
}

// TestRedirectPolicyFunc_sameHost_nil_via_refuses pins the nil-origin guard on
// the WithSameHost clause: with no via chain there is no origin to match
// against, so the policy must refuse gracefully rather than panic. Every other
// same-host test supplies a non-nil origin, leaving this branch undriven.
func TestRedirectPolicyFunc_sameHost_nil_via_refuses(t *testing.T) {
	policy := httpx.RedirectPolicyFunc(httpx.WithSameHost(true))
	if err := policy(reqTo(t, "https://arr.example/b"), nil); err == nil {
		t.Error("WithSameHost policy with nil via = nil, want refusal (no origin to match against)")
	}
}

// TestRedirectPolicyFunc_allowSchemeDowngrade confirms WithAllowSchemeDowngrade
// opts back into following a same-host https->http downgrade.
func TestRedirectPolicyFunc_allowSchemeDowngrade(t *testing.T) {
	policy := httpx.RedirectPolicyFunc(httpx.WithSameHost(true), httpx.WithAllowSchemeDowngrade(true))
	if err := policy(reqTo(t, "http://arr.example/b"), viaWithOrigin(t, "https://arr.example/a")); err != nil {
		t.Errorf("WithAllowSchemeDowngrade(true): same-host https->http should be allowed, got %v", err)
	}
}

// TestRedirectPolicyFunc_downgrade_refused_for_allowlist confirms the
// scheme-downgrade guard also applies to an allowlisted host, not only the
// same-host path: a custom auth header must not follow onto a cleartext hop
// even to an allowed host.
func TestRedirectPolicyFunc_downgrade_refused_for_allowlist(t *testing.T) {
	policy := httpx.RedirectPolicyFunc(httpx.WithAllowedHosts("cdn.example"))
	if err := policy(reqTo(t, "http://cdn.example/x"), viaWithOrigin(t, "https://api.example/start")); err == nil {
		t.Error("https->http redirect to an allowlisted host should be refused by default")
	}
}

// TestDefaultRedirectPolicy_scheme pins the hardened DefaultRedirectPolicy:
// a same-host http->https upgrade is allowed, a same-host https->http downgrade
// is refused.
func TestDefaultRedirectPolicy_scheme(t *testing.T) {
	if err := httpx.DefaultRedirectPolicy(reqTo(t, "https://arr.example/b"), viaWithOrigin(t, "http://arr.example/a")); err != nil {
		t.Errorf("same-host http->https upgrade should be allowed, got %v", err)
	}
	if err := httpx.DefaultRedirectPolicy(reqTo(t, "http://arr.example/b"), viaWithOrigin(t, "https://arr.example/a")); err == nil {
		t.Error("same-host https->http downgrade should be refused")
	}
}

// TestDefaultRedirectPolicy_nil_origin_url_refuses pins the graceful-refusal
// path inherited from the compiled policy it delegates to: a hand-built via
// chain whose original request carries no URL yields a refusal, not a panic
// (the previous hand-rolled implementation dereferenced via[0].URL
// unconditionally). net/http always populates via[0].URL, so only hand-built
// chains reach this branch.
func TestDefaultRedirectPolicy_nil_origin_url_refuses(t *testing.T) {
	if err := httpx.DefaultRedirectPolicy(reqTo(t, "https://example.com/x"), redirectVia(1)); err == nil {
		t.Error("DefaultRedirectPolicy with nil origin URL = nil, want refusal (no origin to match against)")
	}
}

// TestDockerGitHubRedirectPolicy_scheme_downgrade_refused pins the downgrade
// guard on the example allowlist policy: an https->http redirect is refused
// even when the target host is allowlisted, an http->https upgrade and a
// same-scheme hop stay allowed, and a via entry carrying no URL (as the
// hand-built table tests construct) skips the guard rather than tripping it.
func TestDockerGitHubRedirectPolicy_scheme_downgrade_refused(t *testing.T) {
	tests := []struct {
		name    string
		orig    string
		target  string
		wantErr bool
	}{
		{"downgrade to allowlisted host refused", "https://hub.docker.com/a", "http://hub.docker.com/b", true},
		{"downgrade across allowlisted hosts refused", "https://github.com/a", "http://api.github.com/b", true},
		{"http->https upgrade allowed", "http://hub.docker.com/a", "https://hub.docker.com/b", false},
		{"same scheme allowed", "https://github.com/a", "https://raw.githubusercontent.com/b", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := httpx.DockerGitHubRedirectPolicy(reqTo(t, tt.target), viaWithOrigin(t, tt.orig))
			if tt.wantErr != (err != nil) {
				t.Errorf("policy(%s -> %s) err=%v, wantErr=%v", tt.orig, tt.target, err, tt.wantErr)
			}
		})
	}
	// A via entry with no URL must not trip the downgrade guard.
	if err := httpx.DockerGitHubRedirectPolicy(reqTo(t, "http://hub.docker.com/b"), redirectVia(1)); err != nil {
		t.Errorf("nil-URL via entry should skip the downgrade guard, got %v", err)
	}
}

// TestRefuseAllRedirects_identity pins the mechanism: the policy is exactly
// http.ErrUseLastResponse, which makes the client surface the redirect
// response itself rather than fail the request with an error.
func TestRefuseAllRedirects_identity(t *testing.T) {
	if err := httpx.RefuseAllRedirects(redirectReq("anywhere.example"), redirectVia(3)); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("RefuseAllRedirects = %v, want http.ErrUseLastResponse", err)
	}
}

// TestRefuseAllRedirects_surfaced_3xx_is_a_CheckHTTPStatus_error closes the
// loop the v4 strict window exists for: the response RefuseAllRedirects hands
// back carries a nil error, so the caller's status handling is the only thing
// standing between an unfollowed redirect and a "success". Since v4
// CheckHTTPStatus reports it as *HTTPStatusError; under the old 200-399 window
// it returned nil and the redirect stub passed as a completed request.
func TestRefuseAllRedirects_surfaced_3xx_is_a_CheckHTTPStatus_error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/hop", http.StatusFound)
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: httpx.RefuseAllRedirects}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	statusErr := httpx.CheckHTTPStatus(resp)
	var se *httpx.HTTPStatusError
	if !errors.As(statusErr, &se) || se.Code != http.StatusFound {
		t.Fatalf("CheckHTTPStatus(surfaced 302) = %v, want *HTTPStatusError{302}", statusErr)
	}
	if httpx.IsTransient(statusErr) {
		t.Error("IsTransient(surfaced 302) = true, want false")
	}
}

// methodReq builds a redirect-target request carrying a method, for the
// WithPreserveMethod checks (the other helpers leave Method empty).
func methodReq(t *testing.T, method, rawURL string) *http.Request {
	t.Helper()
	r := reqTo(t, rawURL)
	r.Method = method
	return r
}

// methodVia builds a via chain whose entries carry the given methods, all
// pointing at rawURL (the origin URL every entry shares in these tests).
func methodVia(t *testing.T, rawURL string, methods ...string) []*http.Request {
	t.Helper()
	via := make([]*http.Request, 0, len(methods))
	for _, m := range methods {
		via = append(via, methodReq(t, m, rawURL))
	}
	return via
}

// TestRedirectPolicyFunc_preserveMethod pins the option's decision table at the
// policy level: a method-changing hop is refused with http.ErrUseLastResponse
// (so the 3xx surfaces to the caller rather than failing the request), a
// same-method hop is untouched, an empty Method is read as GET (net/http's own
// reading), the comparison is against the ORIGINAL request so a 307-then-302
// chain is caught, and an empty via chain fails closed.
func TestRedirectPolicyFunc_preserveMethod(t *testing.T) {
	policy := httpx.RedirectPolicyFunc(httpx.WithSameHost(true), httpx.WithPreserveMethod(true))
	const origin = "https://api.example/start"
	const target = "https://api.example/next"
	tests := []struct {
		name      string
		method    string
		via       []string
		wantRefus bool
	}{
		{name: "GET chain unchanged", method: http.MethodGet, via: []string{http.MethodGet}},
		{name: "POST kept through 307/308", method: http.MethodPost, via: []string{http.MethodPost}},
		{
			name: "POST downgraded to GET refused", method: http.MethodGet,
			via: []string{http.MethodPost}, wantRefus: true,
		},
		{
			name: "PUT downgraded to GET refused", method: http.MethodGet,
			via: []string{http.MethodPut}, wantRefus: true,
		},
		{
			// via[0] is POST, hop 1 kept it (307), hop 2 downgraded it (302):
			// comparing against via[0] catches it, comparing against the
			// previous hop would not.
			name: "307 then 302 refused at the second hop", method: http.MethodGet,
			via: []string{http.MethodPost, http.MethodPost}, wantRefus: true,
		},
		{
			name: "307 then 307 still allowed", method: http.MethodPost,
			via: []string{http.MethodPost, http.MethodPost},
		},
		{name: "empty origin method reads as GET", method: http.MethodGet, via: []string{""}},
		{name: "empty target method reads as GET", method: "", via: []string{http.MethodGet}},
		{
			name: "empty target method against POST origin refused", method: "",
			via: []string{http.MethodPost}, wantRefus: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := policy(methodReq(t, tt.method, target), methodVia(t, origin, tt.via...))
			if tt.wantRefus {
				if !errors.Is(err, http.ErrUseLastResponse) {
					t.Errorf("policy(%s via %v) = %v, want http.ErrUseLastResponse", tt.method, tt.via, err)
				}
				return
			}
			if err != nil {
				t.Errorf("policy(%s via %v) = %v, want nil (method unchanged)", tt.method, tt.via, err)
			}
		})
	}
}

// TestRedirectPolicyFunc_preserveMethod_empty_via_fails_closed pins the
// fail-closed branch: with no via chain the original method is unknowable, so
// the hop is not followed (net/http never calls CheckRedirect this way; a
// hand-built chain can).
func TestRedirectPolicyFunc_preserveMethod_empty_via_fails_closed(t *testing.T) {
	policy := httpx.RedirectPolicyFunc(httpx.WithAllowedHosts("api.example"), httpx.WithPreserveMethod(true))
	for _, via := range [][]*http.Request{nil, {}} {
		err := policy(methodReq(t, http.MethodGet, "https://api.example/next"), via)
		if err == nil {
			t.Fatalf("policy(via=%v) = nil, want a refusal (no original method to verify against)", via)
		}
		if !errors.Is(err, http.ErrUseLastResponse) {
			t.Errorf("policy(via=%v) = %v, want http.ErrUseLastResponse", via, err)
		}
	}
}

// TestRedirectPolicyFunc_preserveMethod_precedence pins the ORDER inside the
// policy: the hop cap, the target allowlist, and the scheme-downgrade guard are
// the stronger refusals and must win, because they fail the request with a hard
// error while WithPreserveMethod returns ErrUseLastResponse (a nil-error 3xx the
// caller must classify). A hop that both leaves the allowlist and changes the
// method must be reported as the allowlist violation.
func TestRedirectPolicyFunc_preserveMethod_precedence(t *testing.T) {
	const origin = "https://api.example/start"
	tests := []struct {
		name    string
		policy  httpx.CheckRedirect
		target  string
		via     []string
		wantMsg string
	}{
		{
			name:    "cross-host refusal wins",
			policy:  httpx.RedirectPolicyFunc(httpx.WithAllowedHosts("api.example"), httpx.WithPreserveMethod(true)),
			target:  "https://evil.example/x",
			via:     []string{http.MethodPost},
			wantMsg: "refusing redirect to https://evil.example",
		},
		{
			name:    "scheme-downgrade refusal wins",
			policy:  httpx.RedirectPolicyFunc(httpx.WithSameHost(true), httpx.WithPreserveMethod(true)),
			target:  "http://api.example/x",
			via:     []string{http.MethodPost},
			wantMsg: "refusing scheme downgrade to http://api.example",
		},
		{
			name:    "hop cap wins",
			policy:  httpx.RedirectPolicyFunc(httpx.WithSameHost(true), httpx.WithMaxHops(2), httpx.WithPreserveMethod(true)),
			target:  "https://api.example/x",
			via:     []string{http.MethodPost, http.MethodPost, http.MethodPost},
			wantMsg: "too many redirects",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.policy(methodReq(t, http.MethodGet, tt.target), methodVia(t, origin, tt.via...))
			if err == nil {
				t.Fatalf("policy = nil, want %q", tt.wantMsg)
			}
			if errors.Is(err, http.ErrUseLastResponse) {
				t.Fatalf("policy = ErrUseLastResponse, want the hard refusal %q to take precedence", tt.wantMsg)
			}
			if err.Error() != tt.wantMsg {
				t.Errorf("policy = %q, want %q", err.Error(), tt.wantMsg)
			}
		})
	}
}

// TestRedirectPolicyFunc_preserveMethod_grants_nothing pins that the option only
// ever narrows: on its own (no allowlist, WithSameHost or WithSameOrigin) the
// policy still refuses every redirect via the fail-closed no-target branch.
func TestRedirectPolicyFunc_preserveMethod_grants_nothing(t *testing.T) {
	policy := httpx.RedirectPolicyFunc(httpx.WithPreserveMethod(true))
	err := policy(methodReq(t, http.MethodGet, "https://api.example/next"),
		methodVia(t, "https://api.example/start", http.MethodGet))
	if err == nil {
		t.Fatal("WithPreserveMethod alone allowed a redirect; want the no-target refusal")
	}
	if err.Error() != "redirects not allowed" {
		t.Errorf("err = %q, want the no-target refusal", err.Error())
	}
}

// preserveMethodChain starts an httptest server that redirects /start to /hop
// with the given status and records the method /hop was reached with. The
// returned pointer is nil-safe to read after the request completes.
func preserveMethodChain(t *testing.T, status int) (srv *httptest.Server, hopMethod *atomic.Pointer[string]) {
	t.Helper()
	hopMethod = &atomic.Pointer[string]{}
	mux := http.NewServeMux()
	mux.HandleFunc("/hop", func(w http.ResponseWriter, r *http.Request) {
		m := r.Method
		hopMethod.Store(&m)
		_, _ = w.Write([]byte("hop reached"))
	})
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/hop", status)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, hopMethod
}

// TestWithPreserveMethod_end_to_end drives the option through the real net/http
// redirect machinery, which is what actually rewrites the method: a POST is
// downgraded to GET across 301/302/303 (RFC 9110 §15.4 / Go issue 18570) and
// carried across 307/308. The refused hop must surface the 3xx itself with a
// nil error (never a rewritten method, never a followed hop), and a GET chain
// must be unaffected.
func TestWithPreserveMethod_end_to_end(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		status     int
		wantHop    bool
		wantStatus int
	}{
		{"POST through 301 refused", http.MethodPost, http.StatusMovedPermanently, false, http.StatusMovedPermanently},
		{"POST through 302 refused", http.MethodPost, http.StatusFound, false, http.StatusFound},
		{"POST through 303 refused", http.MethodPost, http.StatusSeeOther, false, http.StatusSeeOther},
		{"POST through 307 followed", http.MethodPost, http.StatusTemporaryRedirect, true, http.StatusOK},
		{"POST through 308 followed", http.MethodPost, http.StatusPermanentRedirect, true, http.StatusOK},
		{"GET through 302 followed", http.MethodGet, http.StatusFound, true, http.StatusOK},
		{"GET through 307 followed", http.MethodGet, http.StatusTemporaryRedirect, true, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, hopMethod := preserveMethodChain(t, tt.status)
			client := srv.Client()
			client.CheckRedirect = httpx.RedirectPolicyFunc(httpx.WithSameHost(true), httpx.WithPreserveMethod(true))

			var body io.Reader = http.NoBody
			if tt.method == http.MethodPost {
				// A *strings.Reader gives NewRequest a GetBody, so a 307/308
				// can replay the body (without it net/http declines the hop
				// itself and this test would pass for the wrong reason).
				body = strings.NewReader("payload")
			}
			req, err := http.NewRequestWithContext(t.Context(), tt.method, srv.URL+"/start", body)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("Do: %v (a refused method-changing hop must surface the 3xx, not fail the request)", err)
			}
			defer resp.Body.Close()
			httpx.Drain(resp.Body)

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			got := hopMethod.Load()
			if !tt.wantHop {
				if got != nil {
					t.Errorf("hop was reached with %s; the method-changing hop must not be followed", *got)
				}
				if loc := resp.Header.Get("Location"); loc == "" {
					t.Error("Location header missing from the surfaced redirect response")
				}
				return
			}
			if got == nil {
				t.Fatal("hop was never reached, want the redirect followed")
			}
			if *got != tt.method {
				t.Errorf("hop method = %s, want %s (the option never rewrites the method)", *got, tt.method)
			}
		})
	}
}

// TestWithPreserveMethod_end_to_end_307_then_302 is the chained case through
// real net/http: the 307 keeps POST (followed), the following 302 downgrades it
// to GET (refused), so the caller receives the second hop's 302 and the final
// target is never contacted.
func TestWithPreserveMethod_end_to_end_307_then_302(t *testing.T) {
	var endHit atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/end", func(http.ResponseWriter, *http.Request) { endHit.Store(true) })
	mux.HandleFunc("/mid", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/end", http.StatusFound)
	})
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/mid", http.StatusTemporaryRedirect)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := srv.Client()
	client.CheckRedirect = httpx.RedirectPolicyFunc(httpx.WithSameHost(true), httpx.WithPreserveMethod(true))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/start", strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	httpx.Drain(resp.Body)

	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302 (the second hop's response, surfaced)", resp.StatusCode)
	}
	if endHit.Load() {
		t.Error("final target was contacted; the method-changing second hop must not be followed")
	}
}

// TestRefuseAllRedirects_surfaces_3xx_and_never_follows is the end-to-end
// contract: a client configured with the policy hands back the 302 itself
// (nil error), and the redirect target — which would have received the
// token-bearing headers — is never contacted.
func TestRefuseAllRedirects_surfaces_3xx_and_never_follows(t *testing.T) {
	var targetHit atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/hop", func(http.ResponseWriter, *http.Request) { targetHit.Store(true) })
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/hop", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: httpx.RefuseAllRedirects}
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/start", http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("X-Api-Token", "supersecret")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v (the refused redirect must surface as a response, not an error)", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want %d (the redirect response itself)", resp.StatusCode, http.StatusFound)
	}
	if loc := resp.Header.Get("Location"); loc == "" {
		t.Error("Location header missing from the surfaced redirect response")
	}
	if targetHit.Load() {
		t.Error("redirect target was contacted; the hop must never be followed")
	}
}

// The false arm of each parameterised option is the no-option default and the
// last value wins — both pinned here because nothing else passes false: a
// mutant that hard-wires either config field to true survives every other
// redirect test.
func TestRedirectPolicyFunc_false_equals_absent_and_last_wins(t *testing.T) {
	t.Run("WithSameHost(false) refuses like no option", func(t *testing.T) {
		policy := httpx.RedirectPolicyFunc(httpx.WithSameHost(false), httpx.WithMaxHops(10))
		err := policy(reqTo(t, "https://arr.example/b"), viaWithOrigin(t, "https://arr.example/a"))
		if err == nil {
			t.Error("policy(same-host hop) with WithSameHost(false) = nil err, want refusal (false must equal leaving the option out)")
		}
	})
	t.Run("later WithSameHost(false) overrides earlier true", func(t *testing.T) {
		policy := httpx.RedirectPolicyFunc(httpx.WithSameHost(true), httpx.WithSameHost(false), httpx.WithMaxHops(10))
		err := policy(reqTo(t, "https://arr.example/b"), viaWithOrigin(t, "https://arr.example/a"))
		if err == nil {
			t.Error("policy(same-host hop) with WithSameHost(true) then (false) = nil err, want refusal (last value wins)")
		}
	})
	t.Run("WithPreserveMethod(false) follows a method-rewriting hop like no option", func(t *testing.T) {
		policy := httpx.RedirectPolicyFunc(httpx.WithSameHost(true), httpx.WithPreserveMethod(false), httpx.WithMaxHops(10))
		req := reqTo(t, "https://arr.example/b")
		via := viaWithOriginMethod(t, "https://arr.example/a", "POST")
		if err := policy(req, via); err != nil {
			t.Errorf("policy(POST 302 hop) with WithPreserveMethod(false) = %v, want nil (false must equal leaving the option out)", err)
		}
	})
}

// refusal asserts err is a *RedirectRefusedError with the given reason and
// origin.
func refusal(t *testing.T, err error, reason httpx.RedirectReason, origin string) {
	t.Helper()
	refused, ok := errors.AsType[*httpx.RedirectRefusedError](err)
	if !ok {
		t.Fatalf("err = %v (%T), want *httpx.RedirectRefusedError", err, err)
	}
	if refused.Reason != reason {
		t.Errorf("Reason = %v, want %v", refused.Reason, reason)
	}
	if refused.Origin != origin {
		t.Errorf("Origin = %q, want %q", refused.Origin, origin)
	}
}

// TestSameOriginRefusesOtherPort pins the exact-origin rule (RFC 6454): under
// WithSameOrigin(true) a hop is followed only to the original scheme, host
// (ASCII-folded) and effective port, and WithSameHost(true) follows the
// first two hops this refuses, which is why a key-bearing client needs it.
func TestSameOriginRefusesOtherPort(t *testing.T) {
	refusedHops := []struct {
		name, orig, target, origin string
	}{
		{"same host other port", "https://arr.example:8989/a", "https://arr.example:9090/b", "https://arr.example:9090"},
		{"http to https on the same host", "http://arr.example/a", "https://arr.example/b", "https://arr.example"},
		{"other host", "https://arr.example/a", "https://other.example/b", "https://other.example"},
	}
	sameOrigin := httpx.RedirectPolicyFunc(httpx.WithSameOrigin(true))
	sameHost := httpx.RedirectPolicyFunc(httpx.WithSameHost(true))
	for _, tt := range refusedHops {
		t.Run(tt.name, func(t *testing.T) {
			err := sameOrigin(reqTo(t, tt.target), viaWithOrigin(t, tt.orig))
			refusal(t, err, httpx.RedirectOtherOrigin, tt.origin)
			if err != nil && err.Error() != "refusing redirect to "+tt.origin {
				t.Errorf("Error() = %q, want %q", err.Error(), "refusing redirect to "+tt.origin)
			}
		})
	}
	for _, tt := range refusedHops[:2] {
		t.Run(tt.name+" under WithSameHost", func(t *testing.T) {
			if err := sameHost(reqTo(t, tt.target), viaWithOrigin(t, tt.orig)); err != nil {
				t.Errorf("WithSameHost(true) %s -> %s = %v, want followed", tt.orig, tt.target, err)
			}
		})
	}
	followed := []struct{ name, orig, target string }{
		{"same origin", "https://arr.example:8989/a", "https://arr.example:8989/b"},
		{"host case folded", "https://ARR.Example/a", "https://arr.example/b"},
		{"explicit 443 equals https default", "https://arr.example:443/a", "https://arr.example/b"},
		{"https default equals explicit 443", "https://arr.example/a", "https://arr.example:443/b"},
		{"explicit 80 equals http default", "http://arr.example/a", "http://Arr.Example:80/b"},
	}
	for _, tt := range followed {
		t.Run(tt.name, func(t *testing.T) {
			if err := sameOrigin(reqTo(t, tt.target), viaWithOrigin(t, tt.orig)); err != nil {
				t.Errorf("WithSameOrigin(true) %s -> %s = %v, want followed", tt.orig, tt.target, err)
			}
		})
	}
}

// TestRedirectRefusalsTyped pins that every hard refusal of a shipped policy
// is a *RedirectRefusedError carrying the hop target's origin and the rule
// that refused it, so a caller tells them apart with errors.As alone; the
// origin never carries the target's userinfo, path or query, and parses.
func TestRedirectRefusalsTyped(t *testing.T) {
	const target = "https://user:pw@evil.example:8443/key/path?apikey=k"
	tests := []struct {
		name    string
		policy  httpx.CheckRedirect
		target  string
		via     []*http.Request
		reason  httpx.RedirectReason
		origin  string
		message string
	}{
		{
			name:   "no allowlist",
			policy: httpx.RedirectPolicyFunc(),
			target: target, via: viaWithOrigin(t, "https://api.example/a"),
			reason: httpx.RedirectNotAllowed, origin: "https://evil.example:8443",
			message: "redirects not allowed",
		},
		{
			name:   "cross host",
			policy: httpx.RedirectPolicyFunc(httpx.WithSameHost(true)),
			target: target, via: viaWithOrigin(t, "https://api.example/a"),
			reason: httpx.RedirectOtherOrigin, origin: "https://evil.example:8443",
			message: "refusing redirect to https://evil.example:8443",
		},
		{
			name:   "downgrade",
			policy: httpx.RedirectPolicyFunc(httpx.WithAllowedHosts("cdn.example")),
			target: "http://cdn.example/x?apikey=k", via: viaWithOrigin(t, "https://api.example/a"),
			reason: httpx.RedirectDowngrade, origin: "http://cdn.example",
			message: "refusing scheme downgrade to http://cdn.example",
		},
		{
			name:   "hop cap names the hop's target",
			policy: httpx.RedirectPolicyFunc(httpx.WithAllowedHosts("api.example", "cdn.example"), httpx.WithMaxHops(1)),
			target: "https://cdn.example:8443/c?apikey=k", via: append(viaWithOrigin(t, "https://api.example/a"), &http.Request{}),
			reason: httpx.RedirectTooMany, origin: "https://cdn.example:8443",
			message: "too many redirects",
		},
		{
			name:   "zoned IPv6 target keeps its zone escaped",
			policy: httpx.RedirectPolicyFunc(httpx.WithSameOrigin(true)),
			target: "http://[fe80::1%25eth1]:8080/b", via: viaWithOrigin(t, "http://[fe80::1%25eth0]:8080/a"),
			reason: httpx.RedirectOtherOrigin, origin: "http://[fe80::1%25eth1]:8080",
			message: "refusing redirect to http://[fe80::1%25eth1]:8080",
		},
		{
			name:   "DockerGitHubRedirectPolicy other host",
			policy: httpx.DockerGitHubRedirectPolicy,
			target: target, via: viaWithOrigin(t, "https://hub.docker.com/a"),
			reason: httpx.RedirectOtherOrigin, origin: "https://evil.example:8443",
			message: "refusing redirect to https://evil.example:8443",
		},
		{
			name:   "DockerGitHubRedirectPolicy downgrade",
			policy: httpx.DockerGitHubRedirectPolicy,
			target: "http://hub.docker.com/b", via: viaWithOrigin(t, "https://hub.docker.com/a"),
			reason: httpx.RedirectDowngrade, origin: "http://hub.docker.com",
			message: "refusing scheme downgrade to http://hub.docker.com",
		},
		{
			name:   "DockerGitHubRedirectPolicy hop cap",
			policy: httpx.DockerGitHubRedirectPolicy,
			target: "https://hub.docker.com/b", via: redirectVia(6),
			reason: httpx.RedirectTooMany, origin: "https://hub.docker.com",
			message: "too many redirects",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.policy(reqTo(t, tt.target), tt.via)
			refusal(t, err, tt.reason, tt.origin)
			if err != nil && err.Error() != tt.message {
				t.Errorf("Error() = %q, want %q", err.Error(), tt.message)
			}
			if _, perr := url.Parse(tt.origin); perr != nil {
				t.Errorf("url.Parse(Origin %q) = %v, want a URL", tt.origin, perr)
			}
		})
	}
}

// TestRedirectRefusalsHandBuilt pins the policy on requests net/http never
// builds: a nil or host-less URL refuses with an empty Origin instead of
// panicking, and a scheme written in upper case still reads as its origin.
func TestRedirectRefusalsHandBuilt(t *testing.T) {
	tests := []struct {
		name   string
		policy httpx.CheckRedirect
		req    *http.Request
		via    []*http.Request
		reason httpx.RedirectReason // zero: followed
		origin string
	}{
		{
			name:   "nil target URL",
			policy: httpx.RedirectPolicyFunc(),
			req:    &http.Request{},
			reason: httpx.RedirectNotAllowed,
		},
		{
			name:   "host-less target and nil original URL under WithSameOrigin",
			policy: httpx.RedirectPolicyFunc(httpx.WithSameOrigin(true)),
			req:    &http.Request{URL: &url.URL{Path: "/b"}},
			via:    []*http.Request{{}},
			reason: httpx.RedirectOtherOrigin,
		},
		{
			name:   "upper-case target scheme",
			policy: httpx.RedirectPolicyFunc(),
			req:    &http.Request{URL: &url.URL{Scheme: "HTTPS", Host: "evil.example"}},
			reason: httpx.RedirectNotAllowed, origin: "https://evil.example",
		},
		{
			name:   "upper-case original scheme is the same origin",
			policy: httpx.RedirectPolicyFunc(httpx.WithSameOrigin(true)),
			req:    &http.Request{URL: &url.URL{Scheme: "https", Host: "arr.example"}},
			via:    []*http.Request{{URL: &url.URL{Scheme: "HTTPS", Host: "arr.example"}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.policy(tt.req, tt.via)
			if tt.reason == 0 {
				if err != nil {
					t.Errorf("policy = %v, want followed", err)
				}
				return
			}
			refusal(t, err, tt.reason, tt.origin)
		})
	}
}

// TestSameOriginComposition pins how WithSameOrigin composes with the other
// options: the original host is followed only at its own scheme and effective
// port whatever WithSameHost says, allowlisted hosts are still followed, the
// downgrade rule governs only those other hosts, and the later option wins.
func TestSameOriginComposition(t *testing.T) {
	const orig = "https://arr.example:8989/a"
	tests := []struct {
		name   string
		opts   []httpx.RedirectOption
		target string
		reason httpx.RedirectReason // zero: followed
		origin string
	}{
		{
			name:   "WithSameHost does not widen the original host's port",
			opts:   []httpx.RedirectOption{httpx.WithSameOrigin(true), httpx.WithSameHost(true)},
			target: "https://arr.example:9090/b", reason: httpx.RedirectOtherOrigin, origin: "https://arr.example:9090",
		},
		{
			name:   "WithSameHost placed first does not widen it either",
			opts:   []httpx.RedirectOption{httpx.WithSameHost(true), httpx.WithSameOrigin(true)},
			target: "https://arr.example:9090/b", reason: httpx.RedirectOtherOrigin, origin: "https://arr.example:9090",
		},
		{
			name:   "same origin with WithSameHost",
			opts:   []httpx.RedirectOption{httpx.WithSameOrigin(true), httpx.WithSameHost(true)},
			target: "https://arr.example:8989/b",
		},
		{
			name:   "an allowed host stays followed on any port",
			opts:   []httpx.RedirectOption{httpx.WithSameOrigin(true), httpx.WithAllowedHosts("cdn.example")},
			target: "https://cdn.example:444/b",
		},
		{
			name:   "an allowed suffix stays followed",
			opts:   []httpx.RedirectOption{httpx.WithSameOrigin(true), httpx.WithAllowedSuffixes(".cdn.example")},
			target: "https://edge.cdn.example/b",
		},
		{
			name:   "allowlisting the original host does not widen its origin",
			opts:   []httpx.RedirectOption{httpx.WithSameOrigin(true), httpx.WithAllowedHosts("arr.example")},
			target: "https://arr.example:9090/b", reason: httpx.RedirectOtherOrigin, origin: "https://arr.example:9090",
		},
		{
			name:   "a downgrade to an allowed host is refused by default",
			opts:   []httpx.RedirectOption{httpx.WithSameOrigin(true), httpx.WithAllowedHosts("cdn.example")},
			target: "http://cdn.example/b", reason: httpx.RedirectDowngrade, origin: "http://cdn.example",
		},
		{
			name: "WithAllowSchemeDowngrade lets an allowed host downgrade",
			opts: []httpx.RedirectOption{
				httpx.WithSameOrigin(true), httpx.WithAllowedHosts("cdn.example"), httpx.WithAllowSchemeDowngrade(true),
			},
			target: "http://cdn.example/b",
		},
		{
			name: "WithAllowSchemeDowngrade does not let the original host downgrade",
			opts: []httpx.RedirectOption{
				httpx.WithSameOrigin(true), httpx.WithAllowedHosts("cdn.example"), httpx.WithAllowSchemeDowngrade(true),
			},
			target: "http://arr.example:8989/b", reason: httpx.RedirectOtherOrigin, origin: "http://arr.example:8989",
		},
		{
			name:   "later WithSameOrigin(false) turns the permission off",
			opts:   []httpx.RedirectOption{httpx.WithSameOrigin(true), httpx.WithSameOrigin(false)},
			target: "https://arr.example:8989/b", reason: httpx.RedirectNotAllowed, origin: "https://arr.example:8989",
		},
		{
			name:   "later WithSameOrigin(true) turns it on",
			opts:   []httpx.RedirectOption{httpx.WithSameOrigin(false), httpx.WithSameOrigin(true)},
			target: "https://arr.example:8989/b",
		},
		{
			name:   "WithSameOrigin(false) leaves WithSameHost as it was",
			opts:   []httpx.RedirectOption{httpx.WithSameHost(true), httpx.WithSameOrigin(false)},
			target: "https://arr.example:9090/b",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := httpx.RedirectPolicyFunc(tt.opts...)(reqTo(t, tt.target), viaWithOrigin(t, orig))
			if tt.reason == 0 {
				if err != nil {
					t.Errorf("%s -> %s = %v, want followed", orig, tt.target, err)
				}
				return
			}
			refusal(t, err, tt.reason, tt.origin)
		})
	}
}

// redirectChain serves /hop/<i> as a same-origin redirect to /hop/<i+1> until
// /hop/<n>, which answers 200, and counts the requests it answered.
func redirectChain(t *testing.T, n int) (srv *httptest.Server, hits *atomic.Int32) {
	t.Helper()
	hits = &atomic.Int32{}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var i int
		if _, err := fmt.Sscanf(r.URL.Path, "/hop/%d", &i); err != nil {
			http.NotFound(w, r)
			return
		}
		if i >= n {
			_, _ = w.Write([]byte("end"))
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/hop/%d", i+1), http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

// TestMaxHopsFollowsExactlyN pins the hop cap through net/http: WithMaxHops(n)
// follows a chain of exactly n redirects and refuses the (n+1)th with
// RedirectTooMany, and leaving the option out (or n <= 0) means 5.
func TestMaxHopsFollowsExactlyN(t *testing.T) {
	tests := []struct {
		name string
		opts []httpx.RedirectOption
		n    int
	}{
		{"WithMaxHops(1)", []httpx.RedirectOption{httpx.WithMaxHops(1)}, 1},
		{"WithMaxHops(3)", []httpx.RedirectOption{httpx.WithMaxHops(3)}, 3},
		{"default", nil, 5},
		{"WithMaxHops(0) is the default", []httpx.RedirectOption{httpx.WithMaxHops(0)}, 5},
	}
	get := func(t *testing.T, policy httpx.CheckRedirect, url string) (*http.Response, error) {
		t.Helper()
		client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: policy}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		return client.Do(req)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := httpx.RedirectPolicyFunc(append([]httpx.RedirectOption{httpx.WithSameOrigin(true)}, tt.opts...)...)

			srv, hits := redirectChain(t, tt.n)
			resp, err := get(t, policy, srv.URL+"/hop/0")
			if err != nil {
				t.Fatalf("a chain of %d redirects: %v, want followed", tt.n, err)
			}
			httpx.Drain(resp.Body)
			resp.Body.Close()
			if got := hits.Load(); got != int32(tt.n+1) {
				t.Errorf("a chain of %d redirects reached the server %d times, want %d", tt.n, got, tt.n+1)
			}

			longer, longerHits := redirectChain(t, tt.n+1)
			resp, err = get(t, policy, longer.URL+"/hop/0")
			if err == nil {
				httpx.Drain(resp.Body)
				resp.Body.Close()
				t.Fatalf("a chain of %d redirects was followed, want the redirect after the %dth refused", tt.n+1, tt.n)
			}
			refusal(t, err, httpx.RedirectTooMany, longer.URL)
			if httpx.IsTransient(err) {
				t.Errorf("IsTransient(%v) = true, want false: a refused redirect is not retried", err)
			}
			if got := longerHits.Load(); got != int32(tt.n+1) {
				t.Errorf("a chain of %d redirects reached the server %d times, want %d", tt.n+1, got, tt.n+1)
			}
		})
	}
}
