package config_test

import (
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The public edge is the OTHER place this project logs a request, and nothing in the Go build
// parses its config, so these tests do.
//
// The API's own loggers refuse to log query parameters (middleware/logger.go) or record only an
// allow-listed SHAPE (middleware/slow_query_shape.go). Caddy sits in front of both and writes
// request.uri (query string included) and every request and response header into journald, on
// to promtail and Loki. A filter naming only one query parameter (`token`) would log
// /v1/account/admin/lookup?email=<customer email> verbatim; X-API-Key must be filtered too; and
// the trailing-slash 308's Location header carries the query back out on the response side.
//
// Pinned: no query value leaves the edge on the request side, none on the response side,
// credential-bearing headers are deleted under the exact name net/http gives them (a mismatched
// case is a SILENT no-op), and the hand-kept Caddyfile copy has not drifted from the ansible
// template that reaches the host.
var caddyfiles = []string{
	filepath.Join("..", "..", "configs", "caddy", "Caddyfile.api"),
	filepath.Join("..", "..", "configs", "ansible", "roles", "archival-node", "templates", "Caddyfile.j2"),
}

// caddyURIFilterPattern extracts the `request>uri regexp` pattern and replacement from a Caddyfile. Caddy's regexp
// filter uses Go's regexp, so applying the pattern here exercises production behaviour rather than a re-implementation.
var caddyURIFilterPattern = regexp.MustCompile(`request>uri regexp "([^"]*)" "([^"]*)"`)

func readCaddyfile(t *testing.T, path string) string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(src)
}

func caddyURIFilter(t *testing.T, path string) (*regexp.Regexp, string) {
	t.Helper()
	m := caddyURIFilterPattern.FindStringSubmatch(readCaddyfile(t, path))
	if m == nil {
		t.Fatalf("%s: no `request>uri regexp` access-log filter — the edge is logging query strings verbatim", path)
	}
	re, err := regexp.Compile(m[1])
	if err != nil {
		t.Fatalf("%s: access-log filter pattern does not compile: %v", path, err)
	}
	return re, m[2]
}

// TestCaddyAccessLogRedactsEveryQueryValue: a parameter nobody thought of when writing the filter must be redacted
// anyway. Redacting only `?token=` would leak customer emails to Loki.
func TestCaddyAccessLogRedactsEveryQueryValue(t *testing.T) {
	for _, path := range caddyfiles {
		t.Run(filepath.Base(path), func(t *testing.T) {
			re, repl := caddyURIFilter(t, path)

			// Every value here is a credential or PII; the last few are shapes a per-parameter pattern gets wrong:
			// an unencoded `&` (legal in an email local-part) or `?` inside a value, and a `&` in the path rather than the query.
			const secret = "PII-OR-CREDENTIAL"
			for _, uri := range []string{
				"/v1/account/admin/lookup?email=" + secret,
				"/v1/auth/callback?token=" + secret,
				"/v1/signup/verify?token=" + secret,
				"/v1/prices?api_key=" + secret,
				"/v1/assets?q=" + secret,
				"/v1/assets?cursor=" + secret,
				"/v1/whatever?invented_tomorrow=" + secret,
				"/v1/assets?limit=10&national_id=" + secret + "&order=desc",
				"/v1/account/admin/lookup?email=a&" + secret,
				"/v1/account/admin/lookup?email=a?" + secret,
				"/some&path=weird/v1/assets?email=" + secret,
			} {
				got := re.ReplaceAllString(uri, repl)
				if strings.Contains(got, secret) {
					t.Errorf("query value survived redaction\n  in:  %s\n  out: %s", uri, got)
				}
				// The path is the edge log's primary diagnostic. A
				// filter that eats it is not a safe over-redaction.
				if reqPath, _, _ := strings.Cut(uri, "?"); !strings.HasPrefix(got, reqPath) {
					t.Errorf("path lost to redaction\n  in:  %s\n  out: %s", uri, got)
				}
			}

			// A request with no query string must pass through
			// untouched — most of the log is these.
			for _, uri := range []string{"/v1/healthz", "/v1/assets/native"} {
				if got := re.ReplaceAllString(uri, repl); got != uri {
					t.Errorf("query-less uri rewritten: %q -> %q", uri, got)
				}
			}
		})
	}
}

// Pins the headers Caddy does not redact itself: X-API-Key is our own name, and Location carries the query string out on
// every TrailingSlashRedirect 308. Request-header names must be in canonical MIME form, the key Caddy logs under;
// `X-API-Key delete` in the wrong form matches nothing and fails open.
func TestCaddyAccessLogDropsHeadersCaddyDoesNotRedact(t *testing.T) {
	for _, path := range caddyfiles {
		t.Run(filepath.Base(path), func(t *testing.T) {
			src := readCaddyfile(t, path)
			for _, header := range []string{
				"X-API-Key", // the documented customer API key
				"Referer",   // can carry another origin's ?token=
				"X-Reason",  // staff free text on the admin routes
			} {
				want := "request>headers>" + textproto.CanonicalMIMEHeaderKey(header) + " delete"
				if !strings.Contains(src, want) {
					t.Errorf("%s: access log does not delete the request %s header (want a `%s` field filter)",
						path, header, want)
				}
			}
			// Response side. The 308 that TrailingSlashRedirect answers
			// /v1/account/admin/lookup/?email=… with echoes the query
			// into Location, so a request-only filter still leaks.
			if want := "resp_headers>Location delete"; !strings.Contains(src, want) {
				t.Errorf("%s: access log does not delete the response Location header (want a `%s` field filter) — "+
					"the trailing-slash 308 carries the request query into it", path, want)
			}
		})
	}
}

// Caddyfile.api is the reference copy and Caddyfile.j2 is what ansible renders onto the host; they differ elsewhere by
// design. A redaction rule in only one makes the repo-side fix cosmetic, as the edge keeps logging what the repo says it
// does not.
func TestCaddyfilesShareAccessLogBlock(t *testing.T) {
	first := caddyLogBlock(t, caddyfiles[0])
	for _, path := range caddyfiles[1:] {
		if got := caddyLogBlock(t, path); got != first {
			t.Errorf("access-log block differs between\n  %s\nand\n  %s\n\n--- %s ---\n%s\n--- %s ---\n%s",
				caddyfiles[0], path, caddyfiles[0], first, path, got)
		}
	}
}

// caddyLogBlock returns the site block's `log { ... }` stanza verbatim. It opens at one tab of indentation with deeper
// nested blocks, so the first line that is exactly a tab and a closing brace ends it.
func caddyLogBlock(t *testing.T, path string) string {
	t.Helper()
	src := readCaddyfile(t, path)

	// Take the last `\tlog {`, not the first: the site block's logger writes nearly all access lines, while the global
	// options block has an identical filter. Taking the first would leave the site logger uncovered with tests green; the
	// global block is asserted separately by TestCaddyAccessLogFilterIsGlobalNotJustTheSite.
	start := strings.LastIndex(src, "\tlog {\n")
	if start < 0 {
		t.Fatalf("%s: no access-log block", path)
	}
	if strings.Index(src, "\tlog {\n") == start {
		t.Fatalf("%s: only ONE `log {` block found — the global options block and the site block must BOTH carry the filter (#346 F2b); a single block means one of the two loggers is unfiltered", path)
	}
	rest := src[start:]
	end := strings.Index(rest, "\n\t}\n")
	if end < 0 {
		t.Fatalf("%s: access-log block is not closed", path)
	}
	return rest[:end]
}

// TestCaddyAccessLogFilterIsGlobalNotJustTheSite: a site-only filter leaves reverse_proxy warnings and the catch-all
// default access logger emitting raw request objects. A site-block `log` replaces the default rather than layering, so
// the redaction line must appear at least twice per file.
func TestCaddyAccessLogFilterIsGlobalNotJustTheSite(t *testing.T) {
	for _, path := range caddyfiles {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		body := string(src)

		if got := strings.Count(body, `request>uri regexp`); got < 2 {
			t.Errorf("%s has %d uri-redaction line(s), want >= 2 (one in the global options block, one in the site block) — the global logger is what covers reverse_proxy warnings and the catch-all :80 access log", path, got)
		}

		// The global block must be the one that opens the file, so the
		// filter cannot have been satisfied by two site-level copies.
		globalEnd := strings.Index(body, "\n}\n")
		if globalEnd < 0 {
			t.Fatalf("%s: could not find the end of the global options block", path)
		}
		global := body[:globalEnd]
		for _, want := range []string{
			`request>uri regexp "[?].*" "?<redacted>"`,
			"request>headers>X-Api-Key delete",
			"request>headers>Referer delete",
			"request>headers>X-Reason delete",
			"resp_headers>Location delete",
		} {
			if !strings.Contains(global, want) {
				t.Errorf("%s global options block is missing %q — the default logger stays unfiltered", path, want)
			}
		}
	}
}

// Response compression has the same two-file drift hazard. Bare `encode` is unsafe: its default matcher allows `text/*`,
// including text/event-stream, and buffering serves SSE consumers zero bytes. So stream paths are excluded at request
// level and the matcher narrowed to JSON/atom in both files; dropping either gives a 200 with no data.

// Extracts the compression exclusion's path regexp from a Caddyfile.
// Caddy's `path_regexp` runs on Go's regexp, so compiling the pattern here exercises the production matcher.
var caddyEncodeExclusion = regexp.MustCompile(`@compressible not path_regexp \S+ (\S+)`)

// caddySSEFlushMatcher pulls the `@sse` matcher — the one
// `flush_interval -1` hangs off — so a test can assert the two
// definitions of "this is a stream" have not drifted apart.
var caddySSEFlushMatcher = regexp.MustCompile(`@sse path_regexp \S+ (\S+)`)

// caddySSERoutes is every server-sent-event endpoint the API serves
// (internal/api/v1/server.go:1763, 1860, 1869, 1874). Each answers
// `Content-Type: text/event-stream` and must never be encoded.
var caddySSERoutes = []string{
	"/v1/price/stream",
	"/v1/price/tip/stream",
	"/v1/ledger/stream",
	"/v1/observations/stream",
}

// caddyCredentialRoutes carry a session cookie, magic-link/verification token, SEP-10 challenge or live API key.
// Compressing a secret mixed with attacker-influenced input leaks it through compressed length (BREACH); the bodies are
// small, and all are already private, no-store in cachecontrol.go.
var caddyCredentialRoutes = []string{
	"/v1/auth/login",
	"/v1/auth/callback",
	"/v1/auth/logout",
	"/v1/auth/verify-code",
	"/v1/auth/sep10/challenge",
	"/v1/auth/sep10/token",
	"/v1/auth/passkey/begin-login",
	"/v1/dashboard/keys",
	"/v1/dashboard/webhooks",
	"/v1/dashboard/price-alerts",
	"/v1/account/me",
	"/v1/account/keys",
	"/v1/account/usage",
	"/v1/signup",
	"/v1/signup/verify",
}

// caddyCompressibleRoutes are the large public JSON bodies compression exists for. Two traps: /v1/accounts/{G...} is the
// public explorer and must stay compressed (only singular /v1/account/ is credential), so an exclusion `account` not
// `account/` would break it. /v1/oracle/streams is a plain JSON listing, not SSE; the exclusion is anchored `/stream$`.
var caddyCompressibleRoutes = []string{
	"/v1/assets",
	"/v1/assets/native",
	"/v1/markets",
	"/v1/ledgers",
	"/v1/operations",
	"/v1/pools",
	"/v1/issuers",
	"/v1/contracts",
	"/v1/oracle/streams",
	"/v1/accounts/GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ",
	"/v1/accounts/GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ/trades",
	"/v1/incidents.atom",
	"/v1/healthz",
}

// caddyEncodeStanza returns the `@compressible` matcher line plus the `encode ... { ... }` block verbatim. As with
// caddyLogBlock, it opens at one tab and the first line that is exactly a tab and a closing brace ends it.
func caddyEncodeStanza(t *testing.T, path string) string {
	t.Helper()
	src := readCaddyfile(t, path)
	start := strings.Index(src, "\t@compressible ")
	if start < 0 {
		t.Fatalf("%s: no `@compressible` matcher — responses ship uncompressed (#331 F2)", path)
	}
	rest := src[start:]
	end := strings.Index(rest, "\n\t}\n")
	if end < 0 {
		t.Fatalf("%s: compression stanza is not closed", path)
	}
	return rest[:end]
}

// caddyEncodeExcludes compiles the exclusion pattern out of a
// Caddyfile and reports whether it would keep `reqPath` uncompressed.
func caddyEncodeExcludes(t *testing.T, path string) func(string) bool {
	t.Helper()
	m := caddyEncodeExclusion.FindStringSubmatch(readCaddyfile(t, path))
	if m == nil {
		t.Fatalf("%s: no `@compressible not path_regexp …` matcher on the encode directive — "+
			"a bare `encode` compresses text/event-stream under Caddy's default matcher", path)
	}
	re, err := regexp.Compile(m[1])
	if err != nil {
		t.Fatalf("%s: compression exclusion pattern does not compile: %v", path, err)
	}
	return re.MatchString
}

// TestCaddyEncodesPublicJSON: both files must offer zstd and gzip, and the response matcher must allow-list only
// JSON/atom bodies. Caddy's matcher can name Content-Type values but not exclude one, so any `text/` entry in the match
// block is the bug.
func TestCaddyEncodesPublicJSON(t *testing.T) {
	for _, path := range caddyfiles {
		t.Run(filepath.Base(path), func(t *testing.T) {
			stanza := caddyEncodeStanza(t, path)

			if !strings.Contains(stanza, "encode @compressible zstd gzip") {
				t.Errorf("%s: expected `encode @compressible zstd gzip`, got stanza:\n%s", path, stanza)
			}
			if !strings.Contains(stanza, "header Content-Type application/json*") {
				t.Errorf("%s: encode's response matcher does not allow application/json — "+
					"the whole public read path stays uncompressed", path)
			}
			// The load-bearing negative. `text/*` is in Caddy's default
			// list and matches text/event-stream.
			if strings.Contains(stanza, "text/") {
				t.Errorf("%s: encode's response matcher names a `text/` Content-Type — "+
					"`text/*` and `text/event-stream` are exactly what must not be encoded:\n%s", path, stanza)
			}

			isExcluded := caddyEncodeExcludes(t, path)
			for _, reqPath := range caddyCompressibleRoutes {
				if isExcluded(reqPath) {
					t.Errorf("%s: %s is excluded from compression but is a public JSON body", path, reqPath)
				}
			}
		})
	}
}

// TestCaddyEncodeExcludesEventStreams: `encode` buffers, and a buffered SSE response is a 200 with the right Content-
// Type and no bytes, invisible to status smoke checks. Every SSE route must be excluded at request level, agreeing with
// the `@sse` matcher the `flush_interval -1` block uses.
func TestCaddyEncodeExcludesEventStreams(t *testing.T) {
	for _, path := range caddyfiles {
		t.Run(filepath.Base(path), func(t *testing.T) {
			isExcluded := caddyEncodeExcludes(t, path)
			for _, reqPath := range caddySSERoutes {
				if !isExcluded(reqPath) {
					t.Errorf("%s: %s is NOT excluded from `encode` — Caddy would buffer the event stream "+
						"and every SSE consumer gets a 200 with no data", path, reqPath)
				}
			}

			// The two definitions of "this is a stream" must not drift.
			m := caddySSEFlushMatcher.FindStringSubmatch(readCaddyfile(t, path))
			if m == nil {
				t.Fatalf("%s: no `@sse path_regexp` matcher", path)
			}
			sse, err := regexp.Compile(m[1])
			if err != nil {
				t.Fatalf("%s: @sse pattern does not compile: %v", path, err)
			}
			for _, reqPath := range append(append([]string{}, caddySSERoutes...),
				caddyCompressibleRoutes...) {
				if sse.MatchString(reqPath) && !isExcluded(reqPath) {
					t.Errorf("%s: %s gets `flush_interval -1` (so it is a stream) but is still "+
						"handed to `encode` — the two matchers have drifted apart", path, reqPath)
				}
			}
		})
	}
}

// TestCaddyEncodeExcludesCredentialSurfaces pins the BREACH exclusion. A response carrying a session cookie, magic-link
// token or live API key beside attacker-influenced input leaks the secret through its compressed length.
func TestCaddyEncodeExcludesCredentialSurfaces(t *testing.T) {
	for _, path := range caddyfiles {
		t.Run(filepath.Base(path), func(t *testing.T) {
			isExcluded := caddyEncodeExcludes(t, path)
			for _, reqPath := range caddyCredentialRoutes {
				if !isExcluded(reqPath) {
					t.Errorf("%s: %s is compressed — it carries a credential, and compressing a secret "+
						"beside attacker-influenced input leaks it through the response length (BREACH)",
						path, reqPath)
				}
			}
		})
	}
}

// TestCaddyfilesShareCompressionStanza is the drift guard, like TestCaddyfilesShareAccessLogBlock: Caddyfile.api is the
// reference, Caddyfile.j2 is what ansible renders. An exclusion in only one means the edge does something the repo says
// it does not.
func TestCaddyfilesShareCompressionStanza(t *testing.T) {
	first := caddyEncodeStanza(t, caddyfiles[0])
	for _, path := range caddyfiles[1:] {
		if got := caddyEncodeStanza(t, path); got != first {
			t.Errorf("compression stanza differs between\n  %s\nand\n  %s\n\n--- %s ---\n%s\n--- %s ---\n%s",
				caddyfiles[0], path, caddyfiles[0], first, path, got)
		}
	}
}

// caddyReferrerPolicyPattern captures the leading `header` block token
// in front of `Referrer-Policy`, if any.
var caddyReferrerPolicyPattern = regexp.MustCompile(`(?m)^\s*(\S*)Referrer-Policy\s+"`)

// TestCaddyReferrerPolicyDoesNotOverrideTheAPI: the API sets `Referrer-Policy: no-referrer` itself since
// /v1/auth/callback?token= carries a credential. The site `header` block deletes `-Server`, deferring the whole block to
// write-time, so an unconditional Referrer-Policy would win; `?` (set only if absent) prevents it.
func TestCaddyReferrerPolicyDoesNotOverrideTheAPI(t *testing.T) {
	for _, path := range caddyfiles {
		t.Run(filepath.Base(path), func(t *testing.T) {
			src := readCaddyfile(t, path)
			m := caddyReferrerPolicyPattern.FindStringSubmatch(src)
			if m == nil {
				t.Fatalf("%s: no `Referrer-Policy \"...\"` line found in the header block", path)
			}
			if m[1] != "?" {
				t.Errorf("%s: Referrer-Policy is set unconditionally (prefix %q) — "+
					"it must be `?Referrer-Policy` so the edge only fills in a default "+
					"when the API hasn't already set the header, not overwrite it at write-time",
					path, m[1])
			}
		})
	}
}

// The test-net edge (configs/libvirt/host-Caddyfile) is separate from the production template, so the production
// redaction tests do not cover it. These pin the same properties: no query string, Referer, X-Api-Key, X-Reason or
// Location in any logger, plus baseline security headers on every site.
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
