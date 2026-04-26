package metadata

import "testing"

// isValidDomainOrHostPort guards the URL builder from injection
// attempts (query strings, fragments, whitespace, slashes) and from
// malformed DNS labels. Pin every branch — getting this wrong means
// SEP-1 resolution would emit a URL the caller didn't intend, with
// follow-on impact on outbound HTTP requests + SSRF surface area.

func TestIsValidDomainOrHostPort_validInputs(t *testing.T) {
	for _, in := range []string{
		"example.com",
		"sub.example.com",
		"a.b.c.example.com",
		"127.0.0.1",
		"localhost",
		"example.com:8080",
		"x.y:1",
		"example.com:65535",
	} {
		if !isValidDomainOrHostPort(in) {
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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if isValidDomainOrHostPort(c.in) {
				t.Errorf("expected %q to be invalid", c.in)
			}
		})
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
