package config_test

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The test-net edge (configs/libvirt/host-Caddyfile) is a separate file
// from the production template, so the production redaction tests do not
// cover it. These pin the same properties: no query string, Referer,
// X-Api-Key, X-Reason or Location in any logger, plus the baseline
// security headers on every site.
var hostCaddyfile = filepath.Join("..", "..", "configs", "libvirt", "host-Caddyfile")

var hostLogFilterFields = []string{
	`request>uri regexp "[?].*" "?<redacted>"`,
	"request>headers>X-Api-Key delete",
	"request>headers>Referer delete",
	"request>headers>X-Reason delete",
	"resp_headers>Location delete",
}

// hostBlock returns the body of the top-level block opened by header.
func hostBlock(t *testing.T, src, header string) string {
	t.Helper()
	_, rest, ok := strings.Cut(src, "\n"+header+" {\n")
	if !ok {
		t.Fatalf("%s: no %q block", hostCaddyfile, header)
	}
	body, _, ok := strings.Cut(rest, "\n}\n")
	if !ok {
		t.Fatalf("%s: %q block is not closed", hostCaddyfile, header)
	}
	return body
}

func TestHostCaddyfileLogsAreRedacted(t *testing.T) {
	src := readCaddyfile(t, hostCaddyfile)
	global := strings.TrimPrefix(src[strings.Index(src, "\n{\n"):], "\n{\n")
	global, _, _ = strings.Cut(global, "\n}\n")
	for name, body := range map[string]string{
		"global options": global,
		"(edge) snippet": hostBlock(t, src, "(edge)"),
	} {
		if !strings.Contains(body, "\tlog {") {
			t.Errorf("%s has no log block — its logger is unfiltered", name)
		}
		for _, want := range hostLogFilterFields {
			if !strings.Contains(body, want) {
				t.Errorf("%s is missing %q", name, want)
			}
		}
	}

	re, repl := caddyURIFilter(t, hostCaddyfile)
	if got := re.ReplaceAllString("/v1/auth/callback?token=SECRET", repl); strings.Contains(got, "SECRET") {
		t.Errorf("query value survived redaction: %s", got)
	}
}

func TestHostCaddyfileSetsSecurityHeaders(t *testing.T) {
	edge := hostBlock(t, readCaddyfile(t, hostCaddyfile), "(edge)")
	for _, want := range []string{
		"Strict-Transport-Security",
		`X-Content-Type-Options "nosniff"`,
		"-Server",
	} {
		if !strings.Contains(edge, want) {
			t.Errorf("(edge) snippet is missing %q", want)
		}
	}
}

// Every site must import the snippet: a `log` in a site block replaces the
// default logger, and a site without the import has neither filter nor headers.
func TestHostCaddyfileSitesImportEdge(t *testing.T) {
	src := readCaddyfile(t, hostCaddyfile)
	sites := regexp.MustCompile(`(?m)^([a-z0-9.-]+) \{$`).FindAllStringSubmatch(src, -1)
	if len(sites) == 0 {
		t.Fatalf("%s: no site blocks found", hostCaddyfile)
	}
	for _, m := range sites {
		if !strings.Contains(hostBlock(t, src, m[1]), "\timport edge\n") {
			t.Errorf("site %s does not `import edge`", m[1])
		}
	}
}
