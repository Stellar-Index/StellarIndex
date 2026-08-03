package metadata

import "testing"

// isValidDomainOrHostPort guards the URL builder from injection
// attempts (query strings, fragments, whitespace, slashes) and from
// malformed DNS labels. Pin every branch — getting this wrong means
// SEP-1 resolution would emit a URL the caller didn't intend, with
// follow-on impact on outbound HTTP requests + SSRF surface area.

func TestIsValidDomainOrHostPort_validInputs(t *testing.T) {
	// Production mode (allowAnyPort=false): bare hosts + an explicit
	// :443 (the SEP-1 HTTPS port) are the only accepted shapes.
	for _, in := range []string{
		"example.com",
		"sub.example.com",
		"a.b.c.example.com",
		"127.0.0.1",
		"localhost",
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
		// Finding 2: a non-443 port on an issuer home_domain must be
		// REJECTED — otherwise the sep1-refresh cron would open a blind
		// TLS+GET to an arbitrary port on a public host. These three
		// used to be BLESSED (encoding the vulnerable behaviour).
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

// TestIsValidDomainOrHostPort_portRestriction pins Finding 2 directly:
// in production a :port suffix is accepted only when it is 443; the
// test escape hatch (allowAnyPort=true, used for httptest servers on
// ephemeral ports) accepts any syntactic 1-65535 port.
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
