package config_test

import (
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The public edge is the OTHER place this project logs a request, and
// nothing in the Go build parses its config — so these tests do.
//
// The API's own logger refuses to log query parameters at all
// (internal/api/v1/middleware/logger.go) and its slow-request logger
// records only an allow-listed SHAPE
// (internal/api/v1/middleware/slow_query_shape.go). Caddy sits in front
// of both and writes request.uri — query string included — every
// request header, and every response header straight into journald, on
// to promtail and Loki. Its redaction filter used to name one query
// parameter (`token`), so /v1/account/admin/lookup?email=<customer
// email> was logged verbatim; X-API-Key, the customer credential
// documented in docs/getting-started.md, was never filtered at all; and
// the trailing-slash 308's Location header carried the same query back
// out on the response side.
//
// Four properties are pinned here: no query value leaves the edge on
// the request side, none leaves on the response side, the
// credential-bearing headers are deleted under the exact name net/http
// gives them (a mismatched case is a SILENT no-op), and the hand-kept
// copy of the Caddyfile has not drifted from the ansible template that
// is what actually reaches the host.
var caddyfiles = []string{
	filepath.Join("..", "..", "configs", "caddy", "Caddyfile.api"),
	filepath.Join("..", "..", "configs", "ansible", "roles", "archival-node", "templates", "Caddyfile.j2"),
}

// caddyURIFilterPattern pulls the `request>uri regexp "<pattern>"
// "<replacement>"` line out of a Caddyfile. Caddy's regexp log filter
// runs on Go's regexp package, so applying the extracted pattern here
// exercises the production behaviour rather than a re-implementation.
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

// TestCaddyAccessLogRedactsEveryQueryValue holds the property the
// enumeration lacked: a parameter nobody thought of when the filter was
// written must redact anyway. `?token=` redacted and everything else
// left alone is what shipped customer emails to Loki.
func TestCaddyAccessLogRedactsEveryQueryValue(t *testing.T) {
	for _, path := range caddyfiles {
		t.Run(filepath.Base(path), func(t *testing.T) {
			re, repl := caddyURIFilter(t, path)

			// Every value here is a credential or PII, and the last few
			// are the shapes a per-parameter pattern gets wrong: an
			// unencoded `&` (legal in an email local-part) or `?` inside
			// a value, and a `&` in the PATH rather than the query.
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

// TestCaddyAccessLogDropsHeadersCaddyDoesNotRedact pins the headers
// Caddy does NOT redact on its own. Its `log_credentials` default
// blanks Authorization / Cookie / Set-Cookie / Proxy-Authorization,
// which covers Bearer keys and the dashboard session cookie — but
// X-API-Key is our own header name and Caddy has never heard of it, and
// Location carries the query string back out on the response side of
// every TrailingSlashRedirect 308.
//
// Request-header names must be in Go's canonical MIME form, because
// that is how net/http parses the header and therefore the key Caddy
// logs it under: `request>headers>X-API-Key delete` would match nothing
// and fail open, silently.
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

// TestCaddyfilesShareAccessLogBlock guards the drift that would make a
// repo-side fix cosmetic: Caddyfile.api is the reference copy,
// Caddyfile.j2 is what ansible renders onto the host. The two files
// differ elsewhere by design (site address, an ansible banner), but a
// redaction rule that lives in only one of them means the edge keeps
// logging what the repo says it does not.
func TestCaddyfilesShareAccessLogBlock(t *testing.T) {
	first := caddyLogBlock(t, caddyfiles[0])
	for _, path := range caddyfiles[1:] {
		if got := caddyLogBlock(t, path); got != first {
			t.Errorf("access-log block differs between\n  %s\nand\n  %s\n\n--- %s ---\n%s\n--- %s ---\n%s",
				caddyfiles[0], path, caddyfiles[0], first, path, got)
		}
	}
}

// caddyLogBlock returns the site block's `log { … }` stanza verbatim.
// The stanza opens at one tab of indentation and every nested block is
// deeper, so the first line that is exactly a tab and a closing brace
// ends it.
func caddyLogBlock(t *testing.T, path string) string {
	t.Helper()
	src := readCaddyfile(t, path)

	// Take the LAST `\tlog {`, not the first — the SITE block's logger.
	//
	// This helper fed every assertion in this file, and when b44cda44
	// added an identical filter to the GLOBAL options block, "first" started
	// resolving to that one instead. The site logger then had no coverage at
	// all: stripping its four header/Location deletes left every test in
	// this file green, while that logger writes 15,450 of r1's 15,473 access
	// lines — so an X-Api-Key would have reached Loki with CI passing.
	//
	// A guard that silently stops guarding is worse than no guard, because
	// the green run is taken as evidence. The global block is asserted
	// separately by TestCaddyAccessLogFilterIsGlobalNotJustTheSite, which
	// reads the file's head, so the two never collapse onto the same text.
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

// TestCaddyAccessLogFilterIsGlobalNotJustTheSite pins #346 F2b. Attaching
// the filter to the SITE's access logger leaves two other loggers in the
// same process emitting a raw `request` object, and both were caught doing
// it on r1: reverse_proxy runtime warnings (which embed request.uri and
// headers when a client disconnects mid-response) and the catch-all
// default access logger serving bare-IP / legacy-host traffic (which
// echoed Location). Filtering the DEFAULT logger covers both.
//
// The site block keeps its own copy on purpose — `log` inside a site
// block REPLACES the default for that site rather than layering onto it —
// so the redaction line must appear at least twice per file. One
// occurrence means someone moved it rather than adding it, and half the
// loggers went unfiltered again.
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

// ─── Response compression (#331 F2) ─────────────────────────────────
//
// The edge is also where response compression lives, and the same
// two-file drift hazard applies. Measured against the live API on
// 2026-09-02: every JSON route answered `Accept-Encoding: gzip, br,
// zstd` with NO `content-encoding` at all — 15–34 KB of raw JSON per
// page load, of which ~77% is compressible (170,924 B of
// representative payloads → 40,554 B gzip / 39,399 B zstd).
//
// `encode` is not safe bare here. Caddy's DEFAULT response matcher
// allow-lists `text/*`, which INCLUDES `text/event-stream`, and the
// handler wraps the response writer and buffers up to
// `minimum_length` before it can decide whether to encode. Buffering
// an event stream is precisely the failure the `flush_interval -1`
// block in the same file exists for — r1 2026-08-03 served ZERO bytes
// over 25 s to every SSE consumer. So the stream paths are excluded
// from `encode` at the REQUEST level (the handler never wraps them)
// AND the response matcher is narrowed to the JSON/atom bodies the
// API actually serves.
//
// The tests below pin both halves of that exclusion in both files. A
// future edit that drops either one compresses an event stream and
// takes streaming down silently — the failure mode is a 200 with the
// right Content-Type and no data, which no smoke check catches.

// caddyEncodeExclusion pulls the compression exclusion's path regexp
// out of a Caddyfile. Caddy's `path_regexp` matcher runs on Go's
// regexp package, so compiling the extracted pattern here exercises
// the production matcher rather than a re-implementation of it.
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

// caddyCredentialRoutes carry a dashboard session cookie, a
// magic-link / email-verification token, a SEP-10 challenge or a live
// API key. Compressing a body that mixes a secret with
// attacker-influenced input leaks the secret through the compressed
// length (BREACH), and these bodies are small and low-volume, so
// there is nothing to win by compressing them. Every one is already
// `private, no-store` in
// internal/api/v1/middleware/cachecontrol.go.
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

// caddyCompressibleRoutes are the large public JSON bodies the fix
// exists for. Two traps are pinned here deliberately:
//
//   - `/v1/accounts/{G…}` is the PUBLIC explorer surface and must stay
//     compressed; only the singular `/v1/account/…` dashboard prefix is
//     a credential surface. An exclusion written `account` rather than
//     `account/` would silently stop compressing the busiest family of
//     pages on the site.
//   - `/v1/oracle/streams` is an ordinary JSON listing, not SSE. The
//     exclusion is anchored `/stream$` precisely so the plural does not
//     get caught by it.
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

// caddyEncodeStanza returns the compression stanza verbatim — the
// `@compressible` matcher line plus the `encode … { … }` block. Same
// shape as caddyLogBlock: the stanza opens at one tab of indentation
// and every nested block is deeper, so the first line that is exactly
// a tab and a closing brace ends it.
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

// TestCaddyEncodesPublicJSON pins the directive itself: both files
// must offer zstd and gzip, and the response matcher must allow-list
// only the JSON/atom bodies. Caddy's response matcher can NAME
// Content-Type values to encode but cannot exclude one, so
// "never text/event-stream" is only expressible as "only these" —
// which means any `text/` entry appearing in the match block is the
// bug, not a widening.
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

// TestCaddyEncodeExcludesEventStreams is the one that matters. Caddy's
// `encode` buffers, and an SSE response that is buffered is a 200 with
// the right Content-Type and no bytes — the exact r1 2026-08-03
// outage, which is invisible to a status-code smoke check. Every SSE
// route must be excluded at the REQUEST level so `encode` never wraps
// it, and the exclusion must agree with the `@sse` matcher that the
// `flush_interval -1` block already hangs off.
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

// TestCaddyEncodeExcludesCredentialSurfaces pins the BREACH exclusion.
// A response that carries a session cookie, a magic-link token or a
// live API key next to attacker-influenced input leaks the secret
// through its compressed length.
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

// TestCaddyfilesShareCompressionStanza is the drift guard, the same
// one TestCaddyfilesShareAccessLogBlock applies to the log block:
// Caddyfile.api is the reference copy, Caddyfile.j2 is what ansible
// renders onto the host. An exclusion that lives in only one of them
// means the edge does something the repo says it does not.
func TestCaddyfilesShareCompressionStanza(t *testing.T) {
	first := caddyEncodeStanza(t, caddyfiles[0])
	for _, path := range caddyfiles[1:] {
		if got := caddyEncodeStanza(t, path); got != first {
			t.Errorf("compression stanza differs between\n  %s\nand\n  %s\n\n--- %s ---\n%s\n--- %s ---\n%s",
				caddyfiles[0], path, caddyfiles[0], first, path, got)
		}
	}
}
