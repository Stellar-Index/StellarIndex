package metadata

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestIsBlocked_CoversCloudMetadataAndReservedRanges pins the deny-list against real SSRF pivot addresses: the stdlib
// helpers cover most classes and extraBlockedNets closes the cloud-metadata gaps outside RFC 1918 / 3927 / 4193. A
// shrinking table opens a new target.
func TestIsBlocked_CoversCloudMetadataAndReservedRanges(t *testing.T) {
	d := &ssrfDialer{}
	cases := []struct {
		ip   string
		want bool
		why  string
	}{
		// Loopback — IsLoopback.
		{"127.0.0.1", true, "IPv4 loopback"},
		{"127.255.255.1", true, "IPv4 loopback /8"},
		{"::1", true, "IPv6 loopback"},

		// Link-local — IsLinkLocalUnicast (includes cloud metadata).
		{"169.254.0.1", true, "IPv4 link-local /16"},
		{"169.254.169.254", true, "AWS/Azure/GCP/DO metadata"},
		{"fe80::1", true, "IPv6 link-local"},

		// RFC 1918 + RFC 4193 — IsPrivate.
		{"10.0.0.1", true, "RFC 1918 10/8"},
		{"172.16.0.1", true, "RFC 1918 172.16/12"},
		{"192.168.0.1", true, "RFC 1918 192.168/16"},
		{"fc00::1", true, "RFC 4193 ULA /7"},
		{"fd00::1", true, "RFC 4193 ULA /7 upper half"},

		// Unspecified.
		{"0.0.0.0", true, "IPv4 unspecified"},
		{"::", true, "IPv6 unspecified"},

		// extraBlockedNets — these FAIL without our additions.
		{"100.64.0.1", true, "RFC 6598 CGNAT /10"},
		{"100.100.100.200", true, "Alibaba Cloud metadata (CGNAT)"},
		{"192.0.0.192", true, "Oracle Cloud metadata"},
		{"192.0.0.8", true, "IETF Protocol Assignments /24"},
		{"198.18.0.1", true, "RFC 2544 benchmarking /15"},
		{"198.19.255.254", true, "RFC 2544 benchmarking upper"},

		// Allowed — real public Internet IPs. These must NOT be
		// blocked or the resolver can't fetch anything.
		{"8.8.8.8", false, "public DNS"},
		{"1.1.1.1", false, "public DNS"},
		{"93.184.216.34", false, "example.com"},
		{"2606:4700:4700::1111", false, "Cloudflare IPv6"},

		// 192.0.0.0/24 is blocked, but 192.0.1.0+ is public again.
		{"192.0.1.1", false, "public (outside IETF /24)"},
		// 100.63.x.x is below CGNAT; 100.128.x.x is above. Both public.
		{"100.63.255.255", false, "public (just below CGNAT)"},
		{"100.128.0.1", false, "public (just above CGNAT)"},
	}

	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("test bug: bad IP %q", c.ip)
		}
		got := d.isBlocked(ip)
		if got != c.want {
			t.Errorf("isBlocked(%s) = %v, want %v — %s", c.ip, got, c.want, c.why)
		}
	}
}

// TestResolverTransportDisablesProxy pins Proxy == nil: with HTTP(S)_PROXY honoured, the transport dials the proxy
// (passing our SSRF guard) and hands it the issuer-controlled host, so the real target is never validated. Every fetch
// must use our SSRF-checked dialer.
func TestResolverTransportDisablesProxy(t *testing.T) {
	r := NewResolver(Options{})
	tr, ok := r.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("resolver transport is %T, want *http.Transport", r.client.Transport)
	}
	if tr.Proxy != nil {
		t.Error("SEP-1 transport.Proxy must be nil (F-1336): a configured " +
			"HTTP(S)_PROXY would dial the proxy and bypass the SSRF guard on the target host")
	}
}

// TestIsBlocked_AllowPrivateIPsBypass confirms the test-only escape
// hatch still works — necessary for httptest-backed tests that need
// to dial 127.0.0.1.
func TestIsBlocked_AllowPrivateIPsBypass(t *testing.T) {
	d := &ssrfDialer{allowPrivateIPs: true}
	for _, s := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.100.100.200"} {
		if d.isBlocked(net.ParseIP(s)) {
			t.Errorf("AllowPrivateIPs=true must not block %s", s)
		}
	}
}

// TestDialContext_EmptyResolutionErrorsNotPanics pins that a resolver returning (empty, nil) yields an error, not an
// ips[0] panic that would abort the whole sep1-refresh batch. The injected lookupIP reproduces it; the len(ips)==0
// guard is defensive.
func TestDialContext_EmptyResolutionErrorsNotPanics(t *testing.T) {
	d := &ssrfDialer{
		inner: &net.Dialer{},
		lookupIP: func(_ context.Context, _, _ string) ([]net.IP, error) {
			return nil, nil // resolved to zero addresses, no error
		},
	}
	conn, err := d.DialContext(context.Background(), "tcp", "example.com:443")
	if conn != nil {
		_ = conn.Close()
		t.Fatal("expected no connection on empty resolution")
	}
	if err == nil {
		t.Fatal("expected an error on empty resolution, got nil")
	}
	if !strings.Contains(err.Error(), "no addresses") {
		t.Errorf("expected a 'no addresses' error, got: %v", err)
	}
}

// TestDialContext_BlocksResolvedLoopback pins that the dial-layer SSRF guard blocks whatever IP the resolver returns,
// whatever the hostname syntax. A plain hostname via injected lookupIP bypasses the syntax gate; without the isBlocked
// check this connects instead of ErrSSRFBlocked.
func TestDialContext_BlocksResolvedLoopback(t *testing.T) {
	d := &ssrfDialer{
		inner: &net.Dialer{},
		lookupIP: func(_ context.Context, _, _ string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		},
	}
	conn, err := d.DialContext(context.Background(), "tcp", "attacker.example.org:443")
	if conn != nil {
		_ = conn.Close()
	}
	if !errors.Is(err, ErrSSRFBlocked) {
		t.Errorf("DialContext(attacker.example.org → 127.0.0.1) = %v, want ErrSSRFBlocked", err)
	}
}

// TestResolverTransportUsesSSRFGuardedDialer pins the wiring, not just the predicate: deleting `DialContext:
// (&ssrfDialer{...}).DialContext` would leave `go test ./...` green otherwise. home_domain is attacker-controlled
// on-chain, so this dialer stands between a malicious issuer and a GET to 169.254.169.254.
func TestResolverTransportUsesSSRFGuardedDialer(t *testing.T) {
	r := NewResolver(Options{})
	tr, ok := r.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("resolver transport is %T, want *http.Transport", r.client.Transport)
	}
	if tr.DialContext == nil {
		t.Fatal("transport has a nil DialContext — the SSRF-guarded dialer is not wired, " +
			"so every private-IP block in this package is decorative")
	}

	// The wired dialer must refuse a loopback literal itself, before the
	// stdlib dialer is ever reached.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := tr.DialContext(ctx, "tcp", "127.0.0.1:9")
	if conn != nil {
		_ = conn.Close()
	}
	if !errors.Is(err, ErrSSRFBlocked) {
		t.Errorf("transport.DialContext(127.0.0.1:9) = %v, want ErrSSRFBlocked", err)
	}
}

// TestResolverRedirectPolicy_RejectsPortChange pins that a redirect must keep the original port, not just the
// hostname: home_domain is limited to 443, and a same-host Location on ":6379" would reopen the port the validator
// closed.
func TestResolverRedirectPolicy_RejectsPortChange(t *testing.T) {
	check := NewResolver(Options{}).client.CheckRedirect
	orig, err := http.NewRequest(http.MethodGet, "https://example.com/.well-known/stellar.toml", nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		location string
		allow    bool
	}{
		{"https://example.com:6379/.well-known/stellar.toml", false},
		{"https://example.com:8443/.well-known/stellar.toml", false},
		{"https://EXAMPLE.com:443/.well-known/stellar.toml", true},
		{"https://example.com/.well-known/stellar.toml?v=2", true},
		{"https://evil.example.com/.well-known/stellar.toml", false},
	}
	for _, tc := range cases {
		req, err := http.NewRequest(http.MethodGet, tc.location, nil)
		if err != nil {
			t.Fatal(err)
		}
		err = check(req, []*http.Request{orig})
		if tc.allow && err != nil {
			t.Errorf("redirect to %s refused: %v", tc.location, err)
		}
		if !tc.allow && err == nil {
			t.Errorf("redirect to %s allowed, want refusal", tc.location)
		}
	}
}

// TestDialContext_ProductionRefusesNon443Port pins the dialer-level port guard, which covers every hop however the URL
// was built. The context is pre-cancelled so nothing reaches the network: an allowed port fails with a context error,
// a refused one with ErrSSRFBlocked.
func TestDialContext_ProductionRefusesNon443Port(t *testing.T) {
	d := &ssrfDialer{
		inner: &net.Dialer{},
		lookupIP: func(_ context.Context, _, _ string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("93.184.216.34")}, nil // public
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, addr := range []string{"example.com:6379", "example.com:80", "example.com:8443"} {
		conn, err := d.DialContext(ctx, "tcp", addr)
		if conn != nil {
			_ = conn.Close()
		}
		if !errors.Is(err, ErrSSRFBlocked) {
			t.Errorf("DialContext(%s) = %v, want ErrSSRFBlocked", addr, err)
		}
	}
	conn, err := d.DialContext(ctx, "tcp", "example.com:443")
	if conn != nil {
		_ = conn.Close()
	}
	if errors.Is(err, ErrSSRFBlocked) {
		t.Errorf("DialContext(example.com:443) = %v, must not be port-blocked", err)
	}

	d.allowPrivateIPs = true // test mode: httptest binds ephemeral ports
	if _, err := d.DialContext(ctx, "tcp", "example.com:6379"); errors.Is(err, ErrSSRFBlocked) {
		t.Errorf("test-mode DialContext(:6379) = %v, must not be port-blocked", err)
	}
}

// TestDialContext_FailsOverToNextResolvedAddress pins that one dead A/AAAA record does not fail the fetch: the dialer
// falls through to the next validated address. [::1] is refused because the listener is IPv4-only.
func TestDialContext_FailsOverToNextResolvedAddress(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		if c, aerr := ln.Accept(); aerr == nil {
			_ = c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	d := &ssrfDialer{
		inner:           &net.Dialer{Timeout: 2 * time.Second},
		allowPrivateIPs: true, // loopback listener
		lookupIP: func(_ context.Context, _, _ string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("::1"), net.ParseIP("127.0.0.1")}, nil
		},
	}
	conn, err := d.DialContext(context.Background(), "tcp", net.JoinHostPort("issuer.example", port))
	if err != nil {
		t.Fatalf("DialContext = %v, want fail-over to 127.0.0.1", err)
	}
	defer func() { _ = conn.Close() }()
	if got := conn.RemoteAddr().String(); got != ln.Addr().String() {
		t.Errorf("connected to %s, want %s", got, ln.Addr())
	}
}

// isValidDomainOrHostPort guards the URL builder against injection (query, fragment, whitespace, slashes) and
// malformed DNS labels. Pin every branch: a mistake makes SEP-1 resolution emit an unintended URL with SSRF impact.

func TestIsValidDomainOrHostPort_validInputs(t *testing.T) {
	// Production mode (allowAnyPort=false): bare hosts + an explicit
	// :443 (the SEP-1 HTTPS port) are the only accepted shapes.
	for _, in := range []string{
		"example.com",
		"sub.example.com",
		"a.b.c.example.com",
		"127.0.0.1",
		"example.com:443",
	} {
		if !isValidDomainOrHostPort(in, false) {
			t.Errorf("expected %q to be valid", in)
		}
	}
}

func TestIsValidDomainOrHostPort_invalidInputs(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty string", ""},
		{"trailing colon no port", "example.com:"},
		{"port with non-digit", "example.com:80a"},
		{"port too long", "example.com:123456"},
		{"empty host with port", ":80"},
		{"host with slash", "example.com/path"},
		{"host with query", "example.com?x=1"},
		{"host with fragment", "example.com#frag"},
		{"host with whitespace", "example.com "},
		{"host with underscore", "ex_ample.com"},
		{"label starts with hyphen", "-leading.com"},
		{"label ends with hyphen", "trailing-.com"},
		{"empty label (leading dot)", ".example.com"},
		{"empty label (double dot)", "ex..ample.com"},
		{"label too long (>63)", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.com"}, // 65 a's
		{"host too long (>253)", longHost(t, 254)},
		// Reserved TLDs (RFC 2606/6761) never resolve to a real
		// destination; an on-chain home_domain claiming one is rejected
		// at the syntax gate, not just by the dial-layer SSRF guard.
		{"reserved TLD: localhost", "localhost"},
		{"reserved TLD: bare example", "example"},
		{"reserved TLD: subdomain of test", "foo.test"},
		// A non-443 port on an issuer home_domain must be REJECTED, or the sep1-refresh cron would open a blind
		// TLS+GET to an arbitrary port on a public host. These three must not be blessed.
		{"redis port", "host:6379"},
		{"http-alt port", "example.com:8080"},
		{"max non-standard port", "example.com:65535"},
		{"low non-standard port", "x.y:1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if isValidDomainOrHostPort(c.in, false) {
				t.Errorf("expected %q to be invalid", c.in)
			}
		})
	}
}

// TestIsValidDomainOrHostPort_portRestriction pins that in production a :port suffix is accepted only when it is 443,
// while allowAnyPort=true (httptest servers on ephemeral ports) accepts any syntactic port 1-65535.
func TestIsValidDomainOrHostPort_portRestriction(t *testing.T) {
	// Production: only 443 passes.
	if !isValidDomainOrHostPort("example.com:443", false) {
		t.Error("example.com:443 must be valid in production mode")
	}
	for _, in := range []string{"host:6379", "example.com:8080", "example.com:65535", "x.y:1"} {
		if isValidDomainOrHostPort(in, false) {
			t.Errorf("%q must be REJECTED in production mode (non-443 port)", in)
		}
	}
	// Test mode: ephemeral ports (httptest) are permitted.
	for _, in := range []string{"127.0.0.1:8080", "example.com:65535", "x.y:1", "example.com:443"} {
		if !isValidDomainOrHostPort(in, true) {
			t.Errorf("%q must be permitted in test mode (allowAnyPort)", in)
		}
	}
	// Even in test mode, a malformed port is still rejected.
	for _, in := range []string{"example.com:", "example.com:80a", "example.com:123456"} {
		if isValidDomainOrHostPort(in, true) {
			t.Errorf("%q must be rejected even in test mode (malformed port)", in)
		}
	}
}

func longHost(t *testing.T, total int) string {
	t.Helper()
	// 50-char labels separated by dots — keeps each label legal
	// while pushing the total length past 253.
	const labelLen = 50
	out := make([]byte, 0, total)
	for len(out) < total {
		if len(out) > 0 {
			out = append(out, '.')
		}
		for i := 0; i < labelLen && len(out) < total; i++ {
			out = append(out, 'a')
		}
	}
	return string(out[:total])
}
