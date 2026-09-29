package metadata

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/Stellar-Index/StellarIndex/internal/nettools"
)

// SEP1 is the parsed subset of a stellar.toml document we care
// about. The full raw content is preserved in [SEP1.Raw] for
// callers that need the uncommon fields (FEDERATION_SERVER,
// SIGNING_KEY, HORIZON_URL, etc.) — we don't pre-struct those
// until we have a use for them.
type SEP1 struct {
	// OrgName is the issuer's human-readable organisation name —
	// typically `DOCUMENTATION.ORG_NAME` in the TOML.
	OrgName string

	// Version is the TOML's declared SEP-1 version.
	Version string

	// Currencies is the [[CURRENCIES]] array — asset-specific
	// metadata per SEP-1 §Currencies. Limited to the fields we
	// surface via /v1/assets today; more land as needed.
	Currencies []Currency

	// Documentation maps each documented field to its value —
	// ORG_NAME, ORG_DBA, ORG_URL, ORG_LOGO, etc. Raw values so
	// callers can selectively surface them.
	Documentation map[string]string

	// Raw is the full parsed TOML as a map — callers that need
	// fields this package doesn't expose can grep.
	Raw map[string]any

	// FetchedAt is when the fetch happened (UTC). Populated by
	// [Resolver.Resolve] — not part of the TOML itself.
	FetchedAt time.Time

	// RecoveredSections is non-empty when the document did not parse
	// whole and was read section by section instead. Each entry names
	// a top-level table this index could NOT read, and the parser
	// error that refused it.
	//
	// It is the visible form of a partial read. A caller that treats an
	// absent field as "the issuer declared nothing" would be wrong
	// about a document whose section carrying that field is listed
	// here, so the list has to travel with the result rather than being
	// logged and dropped. Empty on every document that parsed whole,
	// which is almost all of them.
	RecoveredSections []SkippedSection
}

// SkippedSection is one top-level table a recovered parse could not
// read. Header is the table line verbatim ("[[CURRENCIES]]",
// "[DOCUMENTATION]"), Err the parser's own message.
type SkippedSection struct {
	Header string
	Err    string
}

// Currency is one entry from the [[CURRENCIES]] array. Subset of
// SEP-1 §Currencies. We preserve numeric values as strings to
// avoid precision loss on `max_supply` + `fixed_number` fields
// (ADR-0003).
type Currency struct {
	Code            string
	Issuer          string
	Decimals        int
	DisplayDecimals int
	Name            string
	Description     string
	Conditions      string
	Image           string
	FixedNumber     string
	MaxNumber       string
	IsUnlimited     bool
	AnchorAsset     string
	AnchorAssetType string
	Status          string
}

// Resolver fetches + parses stellar.toml for a home-domain. Safe
// for concurrent use. Stateless across calls (no cache).
type Resolver struct {
	client *http.Client

	// allowPrivateIPs mirrors [Options.AllowPrivateIPs]. It is the
	// single "test mode" switch: production leaves it false. As well
	// as relaxing the SSRF IP guard, it relaxes the home_domain port
	// restriction so httptest servers (which bind ephemeral ports)
	// resolve. Production requires the SEP-1 well-known port (443).
	allowPrivateIPs bool
}

// Options configures a [Resolver].
type Options struct {
	// Timeout is the per-request budget (connect + transfer).
	// Default 10 s matches the Phase-1 design.
	Timeout time.Duration

	// AllowPrivateIPs disables the SSRF guard. Tests set this to
	// true so httptest.Server (which listens on 127.0.0.1) is
	// reachable. Production MUST leave it false.
	AllowPrivateIPs bool
}

// WithClient replaces the Resolver's HTTP client. Test-only —
// production callers use NewResolver's built-in client with its
// SSRF-guarded transport + timeouts.
//
// Preserves our CheckRedirect policy on the incoming client so
// redirect safety (cross-host + downgrade rejection) survives the
// swap. Without this, tests that replace the client would also
// drop security policy — making redirect-related regressions
// invisible to the test suite.
//
// Use case: httptest.Server listens on 127.0.0.1 with a
// self-signed cert; tests need a client that trusts it and allows
// the loopback address.
func WithClient(r *Resolver, c *http.Client) *Resolver {
	if r.client != nil && r.client.CheckRedirect != nil {
		c.CheckRedirect = r.client.CheckRedirect
	}
	r.client = c
	return r
}

// NewResolver constructs a SEP-1 resolver.
func NewResolver(opts Options) *Resolver {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		// Proxy is DISABLED (nil) — these are direct-egress fetches of
		// an issuer-controlled URL, and the SSRF guard lives in
		// DialContext below.
		//
		// SECURITY (F-1336): if we honoured HTTP(S)_PROXY here, the
		// transport would dial the PROXY's address (which passes the
		// guard) and hand it the issuer-controlled target host in the
		// CONNECT line / request URL. The guard would then NEVER see —
		// let alone validate — the real target, because the proxy
		// resolves and connects to it for us. An attacker who can set
		// a home-domain could exfiltrate to internal hosts straight
		// through a configured proxy. Pinning Proxy: nil forces every
		// fetch through our own SSRF-checked dialer.
		Proxy: nil,
		DialContext: (&ssrfDialer{
			inner:           dialer,
			allowPrivateIPs: opts.AllowPrivateIPs,
		}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		// Bounds the RESPONSE HEADERS only (net/http's own doc for this
		// field) — it does not cap the body. stellar.toml files should
		// be a few KB at most; body size is enforced separately by the
		// caller reading with io.LimitReader, not by this transport
		// setting.
		MaxResponseHeaderBytes: 1 << 20,
	}

	return &Resolver{
		allowPrivateIPs: opts.AllowPrivateIPs,
		client: &http.Client{
			Timeout:       timeout,
			Transport:     transport,
			CheckRedirect: checkRedirect,
		},
	}
}

// checkRedirect is the SEP-1 redirect policy:
//
//  1. Cap at 5 hops so a malicious domain can't burn our request
//     budget with a redirect loop.
//  2. Reject scheme downgrade (https → http). An attacker controlling
//     the domain MUST NOT force plaintext transit.
//  3. Reject cross-origin redirects: host AND effective port must match
//     the original request. SEP-1 is origin-scoped trust, and a
//     same-host hop to another port would bypass the 443-only rule
//     that [isValidDomainOrHostPort] applies to the on-chain domain.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("sep1: stopped after 5 redirects")
	}
	if req.URL.Scheme != "https" {
		return fmt.Errorf("sep1: refusing redirect to %s:// (downgrade)", req.URL.Scheme)
	}
	orig, next := redirectOrigin(via[0].URL), redirectOrigin(req.URL)
	if orig != next {
		return fmt.Errorf("sep1: refusing cross-host redirect %q → %q", orig, next)
	}
	return nil
}

// redirectOrigin returns the case-folded host:port a URL dials, with
// the scheme's default port filled in so "EXAMPLE.com:443" and
// "example.com" compare equal.
func redirectOrigin(u *url.URL) string {
	port := u.Port()
	if port == "" && u.Scheme == "https" {
		port = standardTLSPort
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// ErrSSRFBlocked is returned when a SEP-1 URL resolves to a
// private or loopback IP, or (in production) targets a port other than 443.
var ErrSSRFBlocked = errors.New("sep1: target IP is in a private or reserved range")

// ErrTOMLTooLarge is returned when the response body exceeds
// [maxBodyBytes].
var ErrTOMLTooLarge = errors.New("sep1: TOML body exceeds 100 KiB limit")

// maxBodyBytes caps the stellar.toml body size at SEP-1's own limit;
// the reference client (go-stellar-sdk clients/stellartoml
// StellarTomlMaxSize) enforces the same 100 * 1024.
const maxBodyBytes = 100 << 10

// ErrTOMLTooDeep is returned when a stellar.toml nests structural
// brackets, or tables through dotted keys and headers, deeper than
// [maxTOMLNestingDepth].
//
// # Why a byte cap is not enough
//
// [maxBodyBytes] bounds the INPUT; it does not bound the WORK the
// decoder does on that input, and the decoder's cost is superlinear in
// nesting depth. Measured against the pinned decoder on this host:
// 250 nested inline tables (1,005 bytes) allocate 8 MiB, 1,000 (4 KB)
// allocate 117 MiB, 4,000 (16 KB) allocate 1.81 GiB in 0.72s — roughly
// quadratic, and a 100 KiB body admits ~26,000 levels. The refresh unit
// runs under MemoryMax=2G, so a 16 KB document any 1-XLM account can
// publish from its home_domain is enough to have the whole worker
// SIGKILLed by the cgroup. Depth is the one dimension that does this:
// the same measurement over nested ARRAYS and over 10,000 sibling
// [[CURRENCIES]] tables is linear (0.6 MiB in 55ms).
//
// So the depth is bounded BEFORE the body reaches the decoder, and a
// document over the bound is refused the way any other unparseable
// document is — the issuer is marked failed and the run moves on.
var ErrTOMLTooDeep = errors.New("sep1: TOML nests tables deeper than the parse budget allows")

// Nesting bounds enforced by [checkTOMLNesting].
//
// maxTOMLNestingDepth is the real limit, measured by a scan that knows
// where strings and comments are. 32 is far past anything SEP-1
// describes — the deepest construct the spec has is an inline table
// inside an array of tables, two levels — and bounds the decoder's work
// on a full-size body to a few hundred KB of allocation.
//
// maxRawTOMLNestingDepth bounds a second, context-free count that
// ignores strings and comments, so opening brackets and dots in data
// raise it. It is a backstop for a string-aware scan that loses track of
// a string, not a guarantee: a closing bracket in data lowers it, so a
// body that pairs every hidden opener with a closer in a comment or
// string defeats it. The guarantee is the string-aware scans ending
// every string and comment where the decoder's lexer does, which
// TestSkipTOMLStringMatchesDecoder checks exhaustively over short
// strings. A document trips the raw bound only with 256 unclosed
// brackets of literal text; at that depth the decoder allocates ~8 MiB.
//
// Both bounds also apply to table nesting spelled with dots, which
// brackets never see: every segment of `a.b.c = 1` or `[a.b.c]` is a
// table the decoder creates, and its cost is quadratic in the length of
// the full key path. The path a key reaches — its table header, the keys
// owning every inline table it sits in, and its own segments — is held
// to maxTOMLNestingDepth; SEP-1's deepest is two (`[[CURRENCIES]]` then
// `code`). A full body of 32-segment paths costs ~110 MiB of transient
// allocation where one unbounded 16,000-segment key cost 4.75 GiB. The
// raw bound is looser for dots than for brackets: a full body of
// 251-segment keys, which only it would stop, allocates ~1 GiB.
const (
	maxTOMLNestingDepth    = 32
	maxRawTOMLNestingDepth = 256
)

// checkTOMLNesting refuses a document whose structural nesting would
// make the decode superlinearly expensive. See [ErrTOMLTooDeep].
func checkTOMLNesting(body []byte) error {
	body = stripTOMLBOM(body)
	if rawBracketDepth(body) > maxRawTOMLNestingDepth ||
		tomlNestingDepth(body) > maxTOMLNestingDepth ||
		rawKeyPathDepth(body) > maxRawTOMLNestingDepth ||
		tomlKeyPathDepth(body) > maxTOMLNestingDepth {
		return ErrTOMLTooDeep
	}
	return nil
}

// stripTOMLBOM drops the byte-order mark the pinned decoder drops before
// lexing (toml parse.go), so a header on the first line still starts it.
func stripTOMLBOM(body []byte) []byte {
	switch {
	case bytes.HasPrefix(body, []byte("\xff\xfe")), bytes.HasPrefix(body, []byte("\xfe\xff")):
		return body[2:]
	case bytes.HasPrefix(body, []byte("\xef\xbb\xbf")):
		return body[3:]
	}
	return body
}

// rawKeyPathDepth over-estimates the longest key path in body, in
// segments, without knowing where strings or comments are: every dot
// counts. At each dot and each `=` the estimate is the current table
// header, plus one entry per unclosed bracket holding the dots that
// preceded it on its line, plus every dot so far on the current line.
// Inline tables may span lines, so it is this accumulation, not the dots
// on any one line, that bounds a path the string-aware
// [tomlKeyPathDepth] cannot see.
//
// The header is taken from a line whose first non-blank byte is `[`: it
// replaces the previous header when no bracket is open, and can only
// raise it when one is (the open bracket may be data the decoder never
// saw). Like [rawBracketDepth], a closing bracket that is data cancels
// an opening one, so this is a backstop, not the bound itself.
func rawKeyPathDepth(body []byte) int {
	var owners []int
	inside, header, line, run, deepest := 0, 0, 0, 0, 0
	lineStart := true
	headerAt := 0 // 0: not a header line; 1: `[` with nothing open; 2: `[` inside a bracket
	endLine := func() {
		switch headerAt {
		case 1:
			header = line + 1
		case 2:
			header = max(header, line+1)
		}
		line, run, headerAt, lineStart = 0, 0, 0, true
	}
	for _, c := range body {
		switch c {
		case ' ', '\t':
			continue
		case '\n':
			endLine()
			continue
		case '.':
			line++
			run++
			deepest = max(deepest, header+inside+line+1)
		case '=':
			deepest = max(deepest, header+inside+line+1)
		case '{', '[':
			if c == '[' && lineStart {
				headerAt = 1
				if len(owners) > 0 {
					headerAt = 2
				}
			}
			owners = append(owners, run+1)
			inside += run + 1
			run = 0
		case '}', ']':
			if n := len(owners); n > 0 {
				inside -= owners[n-1]
				owners = owners[:n-1]
			}
		}
		lineStart = false
	}
	return deepest
}

// tomlKeyPathDepth returns the longest key path, in segments, that the
// decoder would build for any key or table header in body: the current
// table header's segments, plus the segments of the key owning each
// enclosing inline table or array, plus the key's own. Strings and
// comments are skipped as in [tomlNestingDepth], so dots in data (and in
// quoted key segments) do not count.
//
// A dot counts toward a key only while every byte since the last
// separator could belong to a key: a bare-key character, a space or
// tab, a dot, or a quoted segment. Any other byte ends the run, which
// is what keeps floats and dates in values out of the count.
func tomlKeyPathDepth(body []byte) int {
	s := keyPathScan{lineStart: true}
	for i := 0; i < len(body); {
		switch c := body[i]; c {
		case '#':
			i = skipTOMLComment(body, i)
			s.newline()
		case '"', '\'':
			i = skipTOMLString(body, i)
			s.lineStart = false
		default:
			s.step(c)
			i++
		}
	}
	return s.deepest
}

// keyPathScan is the state of [tomlKeyPathDepth] between bytes.
type keyPathScan struct {
	owners    []int // segments each open value bracket adds to the path inside it
	inside    int   // sum of owners
	header    int   // segments of the current table header
	dots      int   // dots in the current run of key bytes
	pending   int   // segments of the key whose value comes next
	deepest   int
	inHeader  bool
	lineStart bool // only spaces and tabs since the last newline
}

func (s *keyPathScan) step(c byte) {
	switch {
	case c == '\n':
		s.newline()
	case c == ' ' || c == '\t':
	case c == '.':
		s.dots++
		s.lineStart = false
	case isTOMLBareKeyByte(c):
		s.lineStart = false
	case c == '=':
		s.pending = s.dots + 1
		s.deepest = max(s.deepest, s.header+s.inside+s.pending)
		s.dots, s.lineStart = 0, false
	case c == '[' || c == '{':
		s.open(c)
	case c == ']' || c == '}':
		s.close()
	default:
		s.dots, s.pending, s.lineStart = 0, 0, false
	}
}

// open handles an opening bracket: a table header when it starts a line
// outside any value, otherwise a value whose contents sit under the key
// that owns it.
func (s *keyPathScan) open(c byte) {
	if c == '[' && len(s.owners) == 0 && (s.lineStart || s.inHeader) {
		s.inHeader = true
	} else {
		s.owners = append(s.owners, s.pending)
		s.inside += s.pending
	}
	s.dots, s.pending, s.lineStart = 0, 0, false
}

// close stops counting down at an empty stack, as [tomlNestingDepth] does.
func (s *keyPathScan) close() {
	if s.inHeader {
		s.endHeader()
	} else if n := len(s.owners); n > 0 {
		s.inside -= s.owners[n-1]
		s.owners = s.owners[:n-1]
	}
	s.dots, s.pending = 0, 0
}

func (s *keyPathScan) newline() {
	if s.inHeader {
		s.endHeader()
	}
	s.dots, s.pending, s.lineStart = 0, 0, true
}

func (s *keyPathScan) endHeader() {
	s.header = s.dots + 1
	s.deepest = max(s.deepest, s.header)
	s.inHeader = false
}

// isTOMLBareKeyByte reports whether c may appear in a TOML bare key.
func isTOMLBareKeyByte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// rawBracketDepth returns the greatest excess of opening over closing
// brackets at any point in body, counting every byte — inside strings
// and comments included. Deliberately context-free, so an opening
// bracket in data raises it; a closing bracket in data lowers it just
// the same, which is why it backs up [tomlNestingDepth] rather than
// replacing it.
func rawBracketDepth(body []byte) int {
	depth, deepest := 0, 0
	for _, c := range body {
		switch c {
		case '{', '[':
			depth++
			if depth > deepest {
				deepest = depth
			}
		case '}', ']':
			if depth > 0 {
				depth--
			}
		}
	}
	return deepest
}

// tomlNestingDepth returns the deepest structural bracket nesting in
// body, skipping comments and every TOML string form so that brackets
// which are DATA are not counted as structure.
//
// It stops counting down at zero rather than going negative: a stray
// closing bracket in a malformed document must not let a later opening
// run start from below zero and hide its depth.
func tomlNestingDepth(body []byte) int {
	depth, deepest := 0, 0
	for i := 0; i < len(body); {
		switch c := body[i]; c {
		case '#':
			i = skipTOMLComment(body, i)
		case '"', '\'':
			i = skipTOMLString(body, i)
		case '{', '[':
			depth++
			if depth > deepest {
				deepest = depth
			}
			i++
		case '}', ']':
			if depth > 0 {
				depth--
			}
			i++
		default:
			i++
		}
	}
	return deepest
}

// skipTOMLComment returns the index just past the newline that ends the
// comment starting at i, or len(body) when the comment runs to EOF.
func skipTOMLComment(body []byte, i int) int {
	if n := bytes.IndexByte(body[i:], '\n'); n >= 0 {
		return i + n + 1
	}
	return len(body)
}

// skipTOMLString returns the index just past the string literal opening
// at body[i], which must be a quote byte. Handles all four TOML string
// forms: the double-quoted basic and single-quoted literal forms, and
// the triple-quoted multi-line counterpart of each. Backslash escapes
// are honoured in the basic forms only, which is where TOML defines
// them.
//
// An unterminated string ends the scan at the newline (single-line
// forms) or at EOF (multi-line forms) — the decoder rejects such a
// document anyway; this only has to leave the scan in the same state
// the decoder's lexer is in.
func skipTOMLString(body []byte, i int) int {
	quote := body[i]
	width := 1
	if i+2 < len(body) && body[i+1] == quote && body[i+2] == quote {
		width = 3
	}
	escaped := quote == '"'
	afterBackslash := -1 // index just past the last `\\` escape
	for j := i + width; j < len(body); {
		switch {
		case escaped && body[j] == '\\':
			// A single-line string cannot continue past a newline, escaped
			// or not; a multi-line one can (line-ending backslash).
			if width == 1 && j+1 < len(body) && body[j+1] == '\n' {
				return j + 1
			}
			if j+1 < len(body) && body[j+1] == '\\' {
				afterBackslash = j + 2
			}
			j += 2
		case width == 1 && body[j] == '\n':
			return j
		case body[j] == quote && closesTOMLString(body, j, quote, width):
			return endOfTOMLStringClose(body, j, quote, width, afterBackslash == j)
		default:
			j++
		}
	}
	return len(body)
}

// closesTOMLString reports whether the quote run at body[j] is the
// closing delimiter of a string opened with `width` quote bytes.
func closesTOMLString(body []byte, j int, quote byte, width int) bool {
	if width == 1 {
		return true
	}
	return j+2 < len(body) && body[j+1] == quote && body[j+2] == quote
}

// endOfTOMLStringClose returns the index just past the closing delimiter
// found at body[j]. A multi-line string may end with up to two quote
// bytes of content before its closing three (`"""x"""""` is `x""`), which
// the decoder's lexer includes in the string. Right after an escaped
// backslash the lexer takes three: its six-quote check exempts a run
// preceded by `\` without asking whether that `\` was itself escaped
// (toml v1.6.0 lex.go lexMultilineString).
func endOfTOMLStringClose(body []byte, j int, quote byte, width int, afterBackslash bool) int {
	maxExtra := 2
	if afterBackslash {
		maxExtra = 3
	}
	end := j + width
	for extra := 0; width == 3 && extra < maxExtra && end < len(body) && body[end] == quote; extra++ {
		end++
	}
	return end
}

// Per-field length caps. The 100 KiB body cap bounds the whole TOML,
// but a single field (e.g. DOCUMENTATION.ORG_NAME) can still be ~100 KiB
// of one string — and we store + serve these values verbatim (every
// /v1/issuers response, the explorer's issuer <h1>). Cap each copied
// string at parse time so a hostile issuer can't bloat our storage or
// our clients' DOM. Short fields (names, codes, handles, tickers) get
// maxShortFieldRunes; free-text fields (descriptions, conditions) get
// maxLongFieldRunes. Caps are in RUNES and truncation lands on a UTF-8
// rune boundary (never mid-codepoint) — see [truncateRunes].
const (
	maxShortFieldRunes = 256
	maxLongFieldRunes  = 4096
)

// maxDocumentationFields and maxCurrencies bound FIELD/ENTRY COUNT,
// not byte size: the 100 KiB body cap and the per-field rune caps above
// bound the bytes of any one value, but nothing stopped a document
// with an unbounded number of short DOCUMENTATION keys or CURRENCIES
// entries from growing the parsed struct — and every field/entry is
// stored and served verbatim. SEP-1 declares under a dozen
// DOCUMENTATION fields; real anchors list at most a few hundred
// currencies.
const (
	maxDocumentationFields = 64
	maxCurrencies          = 1000
)

// truncateRunes returns s limited to at most maxRunes runes, cutting
// on a UTF-8 rune boundary so the result is never invalid mid-encoding
// UTF-8. Returns s unchanged when it already fits (the common case).
func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	// Byte length is an upper bound on rune count: if the bytes fit,
	// the runes fit, and no counting is needed.
	if len(s) <= maxRunes {
		return s
	}
	n := 0
	for i := range s { // ranging a string yields rune-start byte indices
		if n == maxRunes {
			return s[:i]
		}
		n++
	}
	return s
}

// sep1DocFieldCap returns the rune cap for a DOCUMENTATION field by
// key. Only ORG_DESCRIPTION is free-text; every other documented field
// (names, URLs, handles, addresses) is short.
func sep1DocFieldCap(key string) int {
	if key == "ORG_DESCRIPTION" {
		return maxLongFieldRunes
	}
	return maxShortFieldRunes
}

// Resolve fetches the stellar.toml for domain and returns the
// parsed SEP1 record.
//
// domain is the bare home-domain (e.g. "circle.com"), NOT a URL.
// The resolver constructs `https://<domain>/.well-known/stellar.toml`.
// Domain is lowercased before use.
func (r *Resolver) Resolve(ctx context.Context, domain string) (*SEP1, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return nil, errors.New("sep1: empty domain")
	}
	// Friendlier error first: a full URL is a very common user
	// mistake ("pass just the hostname"). Other bad characters get
	// the generic hostname-validation error.
	if strings.Contains(domain, "://") || strings.Contains(domain, "/") ||
		strings.HasPrefix(domain, "http:") || strings.HasPrefix(domain, "https:") {
		return nil, fmt.Errorf("sep1: %q looks like a URL; pass just the hostname", domain)
	}
	if !isValidDomainOrHostPort(domain, r.allowPrivateIPs) {
		return nil, fmt.Errorf("sep1: %q is not a valid hostname (or host:port)", domain)
	}

	rawURL := "https://" + domain + "/.well-known/stellar.toml"
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("sep1: bad URL %q: %w", rawURL, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("sep1: new request: %w", err)
	}
	req.Header.Set("Accept", "text/plain, application/toml, */*;q=0.1")
	req.Header.Set("User-Agent", "stellar-index/metadata (+https://stellarindex.io)")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sep1: %s: %w", domain, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sep1: %s: HTTP %d", domain, resp.StatusCode)
	}
	if resp.ContentLength > maxBodyBytes {
		return nil, ErrTOMLTooLarge
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("sep1: %s: read body: %w", domain, err)
	}
	if int64(len(body)) > maxBodyBytes {
		return nil, ErrTOMLTooLarge
	}

	return parseSEP1(body)
}

// parseSEP1 decodes TOML bytes into a SEP1 struct. Separated from
// the HTTP path so tests can exercise the parser directly.
func parseSEP1(body []byte) (*SEP1, error) {
	// Refuse a pathologically nested document BEFORE handing it to the
	// decoder: the byte cap bounds the input, not the work the decoder
	// does on it. See [ErrTOMLTooDeep].
	if err := checkTOMLNesting(body); err != nil {
		return nil, err
	}
	raw := map[string]any{}
	var skipped []SkippedSection
	if err := toml.Unmarshal(body, &raw); err != nil {
		var rerr error
		raw, skipped, rerr = recoverSEP1Sections(body)
		if rerr != nil {
			return nil, fmt.Errorf("sep1: parse TOML: %w", err)
		}
	}

	sep := &SEP1{
		Raw:               raw,
		FetchedAt:         time.Now().UTC(),
		Documentation:     map[string]string{},
		RecoveredSections: skipped,
	}

	if v, ok := raw["VERSION"].(string); ok {
		sep.Version = truncateRunes(v, maxShortFieldRunes)
	}

	applySEP1Documentation(sep, raw)
	appendSEP1Currencies(sep, raw)

	return sep, nil
}

// applySEP1Documentation copies the [DOCUMENTATION] table onto sep,
// capping each field on a rune boundary, each key's own length, and
// the total number of fields copied (see [maxDocumentationFields]).
// Keys are sorted first so the fields kept under the cap are
// deterministic rather than depending on Go's randomised map order.
func applySEP1Documentation(sep *SEP1, raw map[string]any) {
	doc, ok := raw["DOCUMENTATION"].(map[string]any)
	if !ok {
		return
	}
	keys := make([]string, 0, len(doc))
	for k := range doc {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if len(sep.Documentation) >= maxDocumentationFields {
			break
		}
		if s, ok := doc[k].(string); ok {
			sep.Documentation[truncateRunes(k, maxShortFieldRunes)] = truncateRunes(s, sep1DocFieldCap(k))
		}
	}
	if name, ok := doc["ORG_NAME"].(string); ok {
		sep.OrgName = truncateRunes(name, maxShortFieldRunes)
	}
}

// appendSEP1Currencies reads the [[CURRENCIES]] array onto sep,
// capped at [maxCurrencies] entries.
//
// Two shapes, because the decoder produces both: an array-of-tables
// normally arrives as []map[string]any, and sometimes as []any of
// map[string]any. Reading only one silently drops every currency in a
// document that happened to decode as the other.
func appendSEP1Currencies(sep *SEP1, raw map[string]any) {
	if currencies, ok := raw["CURRENCIES"].([]map[string]any); ok {
		for _, c := range currencies {
			if len(sep.Currencies) >= maxCurrencies {
				break
			}
			sep.Currencies = append(sep.Currencies, parseCurrency(c))
		}
	}
	if arr, ok := raw["CURRENCIES"].([]any); ok {
		for _, entry := range arr {
			if len(sep.Currencies) >= maxCurrencies {
				break
			}
			if m, ok := entry.(map[string]any); ok {
				sep.Currencies = append(sep.Currencies, parseCurrency(m))
			}
		}
	}
}

func parseCurrency(m map[string]any) Currency {
	c := Currency{}
	// getString reads a string field and caps it to maxRunes on a
	// rune boundary (see [truncateRunes]) so a hostile issuer can't
	// smuggle a ~100 KiB currency field past the whole-body cap.
	getString := func(k string, maxRunes int) string {
		if v, ok := m[k].(string); ok {
			return truncateRunes(v, maxRunes)
		}
		return ""
	}
	getInt := func(k string) int {
		switch v := m[k].(type) {
		case int:
			return v
		case int64:
			return int(v)
		}
		return 0
	}
	getBool := func(k string) bool {
		if v, ok := m[k].(bool); ok {
			return v
		}
		return false
	}

	c.Code = getString("code", maxShortFieldRunes)
	c.Issuer = getString("issuer", maxShortFieldRunes)
	c.Decimals = getInt("decimals")
	c.DisplayDecimals = getInt("display_decimals")
	c.Name = getString("name", maxShortFieldRunes)
	c.Description = getString("desc", maxLongFieldRunes)
	c.Conditions = getString("conditions", maxLongFieldRunes)
	c.Image = getString("image", maxShortFieldRunes)
	// fixed_number + max_number are NUMERIC-scale values; TOML
	// might decode them as int64 or string — normalise to string,
	// then cap (a string variant could be attacker-sized).
	c.FixedNumber = truncateRunes(normaliseNumeric(m["fixed_number"]), maxShortFieldRunes)
	c.MaxNumber = truncateRunes(normaliseNumeric(m["max_number"]), maxShortFieldRunes)
	c.IsUnlimited = getBool("is_unlimited")
	c.AnchorAsset = getString("anchor_asset", maxShortFieldRunes)
	c.AnchorAssetType = getString("anchor_asset_type", maxShortFieldRunes)
	c.Status = getString("status", maxShortFieldRunes)
	return c
}

// normaliseNumeric accepts whatever TOML gave us (int, int64,
// string, nil) and returns a decimal string.
func normaliseNumeric(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int:
		return fmt.Sprintf("%d", x)
	case int64:
		return fmt.Sprintf("%d", x)
	}
	return ""
}

// ─── SSRF guard ───────────────────────────────────────────────────

// ssrfDialer wraps net.Dialer + blocks dials to private / loopback
// / link-local / multicast addresses and, outside test mode, any port
// other than [standardTLSPort]. The block happens AFTER DNS
// resolution + BEFORE TCP connect, so it catches rebind attacks
// where a hostname resolves differently each call.
type ssrfDialer struct {
	inner           *net.Dialer
	allowPrivateIPs bool // tests only

	// lookupIP resolves host → IPs. nil means net.DefaultResolver's
	// LookupIP (the production path); tests override it to exercise
	// the blocked-IP and empty-result branches deterministically.
	lookupIP func(ctx context.Context, network, host string) ([]net.IP, error)
}

func (d *ssrfDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("sep1 dialer: bad address %q: %w", address, err)
	}

	// Resolve to a specific IP ourselves so the guard acts on the
	// exact address we're about to connect to — not just the
	// hostname's first resolution. This closes DNS-rebinding races.
	lookupIP := d.lookupIP
	if lookupIP == nil {
		lookupIP = net.DefaultResolver.LookupIP
	}
	ips, err := lookupIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("sep1 dialer: resolve %q: %w", host, err)
	}
	// Defensive: a resolver returning (empty, nil) would otherwise
	// panic on ips[0] below and abort the whole sep1-refresh batch.
	if len(ips) == 0 {
		return nil, fmt.Errorf("sep1 dialer: resolve %q: no addresses returned", host)
	}
	for _, ip := range ips {
		if d.isBlocked(ip) {
			return nil, fmt.Errorf("%w: %s → %s", ErrSSRFBlocked, host, ip)
		}
	}
	// Port is enforced here, at the one chokepoint every connection
	// (initial fetch and each redirect hop) passes through.
	if !d.allowPrivateIPs && port != standardTLSPort {
		return nil, fmt.Errorf("%w: %s port %s (SEP-1 is served on %s only)",
			ErrSSRFBlocked, host, port, standardTLSPort)
	}
	return nettools.DialFirstReachable(ctx, d.inner, network, ips, port)
}

// isBlocked reports whether ip is in a range we refuse to dial.
//
// standardTLSPort is the only port a production SEP-1 fetch will dial.
// SEP-1's stellar.toml is served over HTTPS on 443, and a Stellar
// home_domain is a bare domain (no port) in practice. An issuer whose
// on-chain home_domain carries any other port (":6379", ":8080") is
// rejected up front, and ssrfDialer refuses any other port on every hop,
// so neither a `<public-host>:<arbitrary-port>` home_domain nor a
// redirect to one lets the sep1-refresh cron open a blind TLS+GET to
// any port on a public host. Tests pass allowAnyPort=true so httptest
// servers (ephemeral ports) still resolve.
const standardTLSPort = "443"

// isValidDomainOrHostPort reports whether s is a syntactically valid
// DNS name (optionally with a :port suffix). Guards the URL builder
// from query strings, fragments, whitespace, and other shenanigans
// that would otherwise survive into the request URL.
//
// In production (allowAnyPort=false) a :port suffix is only accepted
// when it is exactly 443; any other port is rejected. allowAnyPort is
// the test escape hatch for httptest servers on ephemeral ports.
//
// Tolerates IPv4 literals (for httptest) but not IPv6 bracket form
// — we never ingest IPv6 literals as home-domains in practice.
func isValidDomainOrHostPort(s string, allowAnyPort bool) bool { //nolint:gocognit,gocyclo // dispatch-heavy; splitting would reduce linearity
	// Split off optional :port.
	host, port, hasPort := strings.Cut(s, ":")
	if hasPort {
		if !allowAnyPort {
			// Production: only the SEP-1 HTTPS port is allowed.
			if port != standardTLSPort {
				return false
			}
		} else {
			// Test mode: port must be 1-5 digits, value 1-65535.
			if port == "" || len(port) > 5 {
				return false
			}
			for _, c := range port {
				if c < '0' || c > '9' {
					return false
				}
			}
		}
	}
	if host == "" || len(host) > 253 {
		return false
	}
	// Reserved TLDs (RFC 2606/6761: example/test/invalid/localhost) are
	// guaranteed to never resolve to a real destination, so an on-chain
	// home_domain claiming one is malformed input, not a legitimate
	// issuer — reject it at the syntax gate rather than relying solely
	// on the dial-layer guard to fail the lookup.
	if nettools.IsReservedTLD(host) {
		return false
	}
	// Hostname character set per RFC 952 + RFC 1123: letters,
	// digits, hyphens, and dots. No underscores, spaces, query
	// chars, slashes, etc.
	for _, c := range host {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '.' {
			continue
		}
		return false
	}
	// Labels (between dots) must not start or end with a hyphen,
	// and must not be empty. "-foo" and "foo-" are invalid; ".foo"
	// (leading dot) is invalid; "foo..bar" (double dot) is invalid.
	labels := strings.Split(host, ".")
	for _, lbl := range labels {
		if lbl == "" {
			return false
		}
		if lbl[0] == '-' || lbl[len(lbl)-1] == '-' {
			return false
		}
		if len(lbl) > 63 {
			return false
		}
	}
	return true
}

// isBlocked delegates to the canonical union blocklist in internal/nettools
// (CS-008 — the block ranges used to be duplicated here + in the two webhook
// guards with divergent coverage). Allow-override in tests via
// Options.AllowPrivateIPs.
func (d *ssrfDialer) isBlocked(ip net.IP) bool {
	if d.allowPrivateIPs {
		return false
	}
	return nettools.IsBlockedIP(ip)
}

// ─── partial parse ──────────────────────────────────────────────────

// recoverSEP1Sections reads a document that does not parse whole, one
// top-level table at a time, and returns what it could read plus the
// tables it could not.
//
// # Why this exists
//
// A stellar.toml is published by the issuer, not by us, and a syntax
// error in one table is not evidence about the others. WisdomTree's
// file (stellar.wisdomtree.com, measured 2026-09-16) ends its ACCOUNTS
// array with an unterminated string on line 20. Every one of its
// eighteen [[CURRENCIES]] tables is well-formed, thirteen of them
// declare an RWA anchor class, and those thirteen carry 7,023,543
// tokens across roughly 30,000 trustlines each. A whole-document parse
// threw all of it away over a missing quotation mark in an unrelated
// table.
//
// Discarding a whole document for a defect in one table is the same
// class of mistake as counting a missing field as a zero: it turns an
// issuer's typo into OUR silence about assets that demonstrably exist.
//
// # What this is NOT
//
// It is not a lenient parser. Nothing is repaired, guessed or
// re-punctuated. Each kept section is handed to the SAME toml.Unmarshal
// with the SAME strictness; a section that does not parse is dropped
// and named. The result can therefore only ever be a SUBSET of what a
// valid document would have produced — recovery can never admit a
// declaration that strict TOML would have rejected.
//
// An attacker gains nothing: they control their own file, so anything
// they could smuggle through here they could simply have written as
// valid TOML in the first place.
//
// # Why the multi-line-string gate
//
// Splitting on lines that start a table is only sound if such a line
// cannot be DATA. Without multi-line strings it cannot be: TOML has no
// other construct in which a bare [[NAME]] at column zero is a value.
// Inside a triple-quoted literal it could be, and then this function
// would read a table the real parser would have seen as text. So a
// document containing either delimiter is refused outright rather than
// split on a guess — which is why the gate is on the DOCUMENT and not
// on the section.
func recoverSEP1Sections(body []byte) (map[string]any, []SkippedSection, error) {
	text := string(body)
	if strings.Contains(text, `"""`) || strings.Contains(text, `'''`) {
		return nil, nil, errors.New("sep1: refusing section recovery: document uses multi-line strings, in which a table header cannot be told from text")
	}

	lines := strings.Split(text, "\n")
	var starts []int
	for i, ln := range lines {
		if isTOMLTableHeader(ln) {
			starts = append(starts, i)
		}
	}
	if len(starts) == 0 {
		return nil, nil, errors.New("sep1: refusing section recovery: no top-level table to read independently")
	}

	// The preamble — everything before the first table — carries the
	// bare top-level keys (VERSION, NETWORK_PASSPHRASE, ACCOUNTS). It is
	// a section like any other and is dropped the same way when it does
	// not parse, which is exactly what happens to WisdomTree's.
	sections := [][2]int{{0, starts[0]}}
	for n, st := range starts {
		end := len(lines)
		if n+1 < len(starts) {
			end = starts[n+1]
		}
		sections = append(sections, [2]int{st, end})
	}

	out := map[string]any{}
	var skipped []SkippedSection
	kept := 0
	for _, se := range sections {
		chunk := strings.Join(lines[se[0]:se[1]], "\n")
		if strings.TrimSpace(chunk) == "" {
			continue
		}
		part, err := parseTOMLSection(chunk)
		if err != nil {
			header := "(top-level keys)"
			if se[0] < len(lines) && isTOMLTableHeader(lines[se[0]]) {
				header = strings.TrimSpace(lines[se[0]])
			}
			skipped = append(skipped, SkippedSection{Header: header, Err: err.Error()})
			continue
		}
		mergeTOMLSection(out, part)
		kept++
	}

	if kept == 0 {
		return nil, nil, errors.New("sep1: section recovery read nothing")
	}
	return out, skipped, nil
}

// parseTOMLSection decodes one section split out by
// [recoverSEP1Sections]. The nesting check is repeated per section: a
// malformation earlier in the document can leave the whole-body scan
// reading this section as nested inside it, while the decoder sees it
// standalone.
func parseTOMLSection(chunk string) (map[string]any, error) {
	if err := checkTOMLNesting([]byte(chunk)); err != nil {
		return nil, err
	}
	part := map[string]any{}
	if err := toml.Unmarshal([]byte(chunk), &part); err != nil {
		return nil, err
	}
	return part, nil
}

// isTOMLTableHeader reports whether a line begins a top-level table or
// array-of-tables. Deliberately strict: column zero, no leading
// whitespace, a bare or quoted key name, and nothing after the closing
// bracket but whitespace or a comment. A line this refuses is treated
// as part of the section above it, which is the conservative direction
// — it can only cause a section to be dropped, never a declaration to
// be invented.
func isTOMLTableHeader(line string) bool {
	if len(line) == 0 || line[0] != '[' {
		return false
	}
	rest := line
	if strings.HasPrefix(rest, "[[") {
		rest = rest[2:]
		i := strings.Index(rest, "]]")
		if i < 0 {
			return false
		}
		return tomlTableName(rest[:i]) && tomlTrailerOK(rest[i+2:])
	}
	rest = rest[1:]
	i := strings.Index(rest, "]")
	if i < 0 {
		return false
	}
	return tomlTableName(rest[:i]) && tomlTrailerOK(rest[i+1:])
}

// tomlTableName accepts the characters TOML allows in a bare or dotted
// table name, plus the quotes a quoted key may carry.
func tomlTableName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.', r == '"', r == '\'', r == ' ':
		default:
			return false
		}
	}
	return true
}

// tomlTrailerOK accepts only whitespace or a comment after the header's
// closing bracket.
func tomlTrailerOK(s string) bool {
	s = strings.TrimSpace(s)
	return s == "" || strings.HasPrefix(s, "#")
}

// mergeTOMLSection folds one independently-parsed section into the
// accumulating document.
//
// Arrays of tables APPEND, because that is what they mean and because
// each [[CURRENCIES]] block arrives as its own section. Everything else
// keeps the FIRST occurrence: a key defined twice is a duplicate-key
// error in strict TOML, and letting a later section overwrite an
// earlier one would make recovery produce a document strict TOML would
// never have produced. First-wins is the reading that stays a subset.
func mergeTOMLSection(dst, src map[string]any) {
	for k, v := range src {
		prev, exists := dst[k]
		if !exists {
			dst[k] = v
			continue
		}
		pa, pok := prev.([]map[string]any)
		va, vok := v.([]map[string]any)
		if pok && vok {
			dst[k] = append(pa, va...)
			continue
		}
		pl, plok := prev.([]any)
		vl, vlok := v.([]any)
		if plok && vlok {
			dst[k] = append(pl, vl...)
		}
	}
}
