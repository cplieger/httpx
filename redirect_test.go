package httpx_test

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/cplieger/httpx/v2"
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
		{"too many redirects", "hub.docker.com", 5, true},
		{"4 redirects still ok", "hub.docker.com", 4, false},
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
		{"too many hops", "example.com", 3, true},
		{"2 hops ok", "example.com", 2, false},
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

// TestRedirectPolicyFunc_default_max_hops verifies the default hop cap is
// redirectCap (5) when WithMaxHops is not set: 4 hops allowed, 5 refused.
func TestRedirectPolicyFunc_default_max_hops(t *testing.T) {
	policy := httpx.RedirectPolicyFunc(httpx.WithAllowedHosts("example.com"))
	if err := policy(redirectReq("example.com"), redirectVia(4)); err != nil {
		t.Errorf("4 hops should be allowed: %v", err)
	}
	if err := policy(redirectReq("example.com"), redirectVia(5)); err == nil {
		t.Error("5 hops should be refused (default maxHops=5)")
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
	via := make([]*http.Request, 5)
	for i := range via {
		via[i] = &http.Request{URL: origURL}
	}
	if err := httpx.DefaultRedirectPolicy(&http.Request{URL: redirURL}, via); err == nil {
		t.Error("should refuse after 5 hops")
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

// TestRedirectPolicyFunc_sameHost pins the WithSameHost behavior: same-host
// redirects (including an http->https upgrade) are followed, a cross-host hop
// and a same-host https->http downgrade are refused, host matching is
// ASCII-case-insensitive, and a differing port on the same host is still same
// host.
func TestRedirectPolicyFunc_sameHost(t *testing.T) {
	policy := httpx.RedirectPolicyFunc(httpx.WithSameHost(), httpx.WithMaxHops(10))
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
	policy := httpx.RedirectPolicyFunc(httpx.WithSameHost())
	if err := policy(reqTo(t, "https://arr.example/b"), nil); err == nil {
		t.Error("WithSameHost policy with nil via = nil, want refusal (no origin to match against)")
	}
}

// TestRedirectPolicyFunc_allowSchemeDowngrade confirms WithAllowSchemeDowngrade
// opts back into following a same-host https->http downgrade.
func TestRedirectPolicyFunc_allowSchemeDowngrade(t *testing.T) {
	policy := httpx.RedirectPolicyFunc(httpx.WithSameHost(), httpx.WithAllowSchemeDowngrade(true))
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
