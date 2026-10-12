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

// SEP1 is the parsed subset of a stellar.toml we use; the full content stays in [SEP1.Raw] for uncommon fields.
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

	// RecoveredSections names each top-level table that could NOT be read when the document was
	// parsed section by section (empty if it parsed whole). An absent field is not "the issuer
	// declared nothing" when the section carrying it is listed here.
	RecoveredSections []SkippedSection
}

// SkippedSection is one top-level table a recovered parse could not read: Header is the table line verbatim, Err the parser's message.
type SkippedSection struct {
	Header string
	Err    string
}

// Currency is one [[CURRENCIES]] entry (subset of SEP-1); numeric values such as `max_supply` and `fixed_number` stay strings to avoid precision loss (ADR-0003).
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
	// IsAssetAnchored and Regulated are nil when the entry omits the key,
	// so an absent declaration never reads as an explicit false.
	IsAssetAnchored        *bool
	AttestationOfReserve   string
	RedemptionInstructions string
	Regulated              *bool
	ApprovalServer         string
	ApprovalCriteria       string
}

// Resolver fetches and parses stellar.toml for a home-domain; safe for concurrent use, no cache.
type Resolver struct {
	client *http.Client

	// allowPrivateIPs mirrors [Options.AllowPrivateIPs], the single test-mode switch. It also
	// relaxes the home_domain port restriction (production requires 443) for httptest servers.
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

// WithClient replaces the Resolver's HTTP client, test-only (production uses NewResolver's SSRF-guarded client); it keeps our CheckRedirect policy (cross-host and downgrade rejection) on the incoming client so tests cannot hide redirect regressions.
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
		// SECURITY: Proxy stays nil. Honouring HTTP(S)_PROXY would dial the proxy (which passes
		// the guard) and hand it the issuer-controlled host, so the SSRF guard in DialContext
		// would never see the real target and a home-domain could reach internal hosts.
		Proxy: nil,
		DialContext: (&ssrfDialer{
			inner:           dialer,
			allowPrivateIPs: opts.AllowPrivateIPs,
		}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		// Bounds the response HEADERS only; the body cap is enforced by the caller's io.LimitReader.
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

// checkRedirect is the SEP-1 redirect policy: at most 5 hops, no https-to-http downgrade, and no
// cross-origin hop. Host AND port must match because trust is origin-scoped, and another port
// would bypass the 443-only rule [isValidDomainOrHostPort] applies.
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

// redirectOrigin returns the case-folded host:port a URL dials, filling the default port so
// "EXAMPLE.com:443" and "example.com" compare equal.
func redirectOrigin(u *url.URL) string {
	port := u.Port()
	if port == "" && u.Scheme == "https" {
		port = standardTLSPort
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// ErrSSRFBlocked is returned when a SEP-1 URL resolves to a private or loopback IP, or (in production) targets a port other than 443.
var ErrSSRFBlocked = errors.New("sep1: target IP is in a private or reserved range")

// ErrTOMLTooLarge is returned when the response body exceeds [maxBodyBytes].
var ErrTOMLTooLarge = errors.New("sep1: TOML body exceeds 100 KiB limit")

// maxBodyBytes caps the stellar.toml body at SEP-1's own limit, which the reference client
// (go-stellar-sdk StellarTomlMaxSize) also enforces.
const maxBodyBytes = 100 << 10

// ErrTOMLTooDeep is returned when nesting exceeds [maxTOMLNestingDepth]; checked BEFORE the decoder because [maxBodyBytes] bounds input, not the superlinear decode work (16 KB of nested tables allocate 1.81 GiB against MemoryMax=2G), and an over-deep document fails like any unparseable one.
var ErrTOMLTooDeep = errors.New("sep1: TOML nests tables deeper than the parse budget allows")

// Nesting bounds enforced by [checkTOMLNesting]. maxTOMLNestingDepth is the real limit (from a string- and comment-aware scan; 32 is far
// past SEP-1's two levels) and also bounds dotted key paths. maxRawTOMLNestingDepth is a context-free backstop, not a guarantee
// (TestSkipTOMLStringMatchesDecoder pins the guarantee); it also refuses 256 unclosed brackets or dots inside one string.
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

// rawKeyPathDepth over-estimates the longest key path in body, in segments, without knowing where strings or comments are (every dot
// counts), bounding a path the string-aware [tomlKeyPathDepth] cannot see. Like [rawBracketDepth] it is a backstop, not the bound.
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

// tomlKeyPathDepth returns the longest key path, in segments, the decoder would build, skipping strings and comments as
// [tomlNestingDepth] does. A dot counts only while every byte since the last separator could belong to a key, which keeps floats and
// dates in values out of the count.
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

// rawBracketDepth returns the greatest excess of opening over closing brackets in body, counting
// every byte including strings and comments. It is context-free, so a closing bracket in data
// lowers it as an opening one raises it; that is why it backs up [tomlNestingDepth].
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

// tomlNestingDepth returns the deepest structural bracket nesting in body, skipping comments and
// every TOML string form so brackets in DATA are not counted. It stops counting down at zero: a
// stray closing bracket must not let a later opening run start below zero and hide its depth.
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

// skipTOMLString returns the index just past the string literal opening at body[i], handling all four TOML string forms (escapes apply to
// basic strings only). An unterminated string ends the scan at the newline (single-line) or EOF (multi-line), matching the lexer, since
// the decoder rejects the document anyway.
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

// endOfTOMLStringClose returns the index just past the closing delimiter at body[j]. A multi-line string may end with up to two content
// quotes before its closing three (`"""x"""""` is `x""`); after an escaped backslash the lexer takes three (toml v1.6.0).
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

// Per-field length caps: one field can be ~100 KiB under the body cap and values are served verbatim (/v1/issuers, the explorer), so each
// copied string is capped at parse time, in RUNES on a rune boundary ([truncateRunes]): maxShortFieldRunes for short fields,
// maxLongFieldRunes for free text.
const (
	maxShortFieldRunes = 256
	maxLongFieldRunes  = 4096
)

// maxDocumentationFields and maxCurrencies bound FIELD/ENTRY COUNT, not bytes: the byte caps do not stop many short keys or entries from
// growing the struct, and each is served verbatim. SEP-1 declares under a dozen documentation fields; real anchors list at most a few
// hundred currencies.
const (
	maxDocumentationFields = 64
	maxCurrencies          = 1000
)

// truncateRunes returns s limited to maxRunes runes, cutting on a UTF-8 rune boundary so the
// result is never invalid UTF-8.
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

// Resolve fetches and parses the stellar.toml for domain, a bare lowercased home-domain (e.g. "circle.com"), NOT a URL; it builds `https://<domain>/.well-known/stellar.toml`.
func (r *Resolver) Resolve(ctx context.Context, domain string) (*SEP1, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return nil, errors.New("sep1: empty domain")
	}
	// A full URL is a common mistake, so it gets a friendlier error than generic validation.
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

// parseSEP1 decodes TOML bytes into a SEP1, separate from the HTTP path so tests can drive it.
func parseSEP1(body []byte) (*SEP1, error) {
	// Refuse over-nested input BEFORE the decoder: the byte cap bounds input, not decode work.
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

// applySEP1Documentation copies the [DOCUMENTATION] table onto sep, capping each field's runes,
// each key's length and the field count ([maxDocumentationFields]). Keys are sorted first so the
// fields kept under the cap do not depend on Go's randomised map order.
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

// appendSEP1Currencies reads the [[CURRENCIES]] array onto sep, capped at [maxCurrencies].
// The decoder yields either []map[string]any or []any of maps; reading only one would silently
// drop every currency in a document that decoded as the other.
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
	// getString caps each field at maxRunes so a hostile issuer cannot smuggle a ~100 KiB field
	// past the whole-body cap.
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
	getOptBool := func(k string) *bool {
		if v, ok := m[k].(bool); ok {
			return &v
		}
		return nil
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
	c.IsAssetAnchored = getOptBool("is_asset_anchored")
	c.AttestationOfReserve = getString("attestation_of_reserve", maxShortFieldRunes)
	c.RedemptionInstructions = getString("redemption_instructions", maxLongFieldRunes)
	c.Regulated = getOptBool("regulated")
	c.ApprovalServer = getString("approval_server", maxShortFieldRunes)
	c.ApprovalCriteria = getString("approval_criteria", maxLongFieldRunes)
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

// ssrfDialer blocks dials to private, loopback, link-local and multicast addresses and, outside
// test mode, any port but [standardTLSPort]. The check runs AFTER DNS resolution and BEFORE the
// TCP connect, so it catches rebinding hostnames that resolve differently per call.
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

// standardTLSPort is the only port a production SEP-1 fetch dials: another port in a home_domain is rejected up front and by ssrfDialer
// on every hop, so neither it nor a redirect can open a blind TLS+GET to any port on a public host. Tests pass allowAnyPort=true for
// httptest's ephemeral ports.
const standardTLSPort = "443"

// isValidDomainOrHostPort reports whether s is a valid DNS name with an optional :port, keeping query strings, fragments and whitespace
// out of the request URL. Production accepts only port 443; allowAnyPort is the httptest escape hatch. IPv4 literals pass; IPv6 bracket
// form does not.
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
	// Reserved TLDs (RFC 2606/6761) never resolve to a real destination, so a home_domain
	// claiming one is malformed; reject it here rather than rely on the dial-layer guard.
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

// isBlocked delegates to the canonical union blocklist in internal/nettools,
// shared with the webhook guards so coverage cannot diverge. Allow-override in tests via
// Options.AllowPrivateIPs.
func (d *ssrfDialer) isBlocked(ip net.IP) bool {
	if d.allowPrivateIPs {
		return false
	}
	return nettools.IsBlockedIP(ip)
}

// ─── partial parse ──────────────────────────────────────────────────

// recoverSEP1Sections reads a document that does not parse whole, one top-level table at a time, returning what it read plus the tables
// it could not. Not a lenient parser: each kept section goes through the same strict toml.Unmarshal, so the result is only a subset of
// valid TOML. A document with a multi-line delimiter is refused outright, since a table-start line could be string data.
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

// parseTOMLSection decodes one section split out by [recoverSEP1Sections]. The nesting check is
// repeated per section because an earlier malformation can make the whole-body scan read this
// section as nested while the decoder sees it standalone.
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

// isTOMLTableHeader reports whether a line begins a top-level table or array-of-tables. Deliberately strict (column zero, bare or quoted
// name, only whitespace or a comment after the bracket): a refused line joins the section above, which can only drop a section, never
// invent a declaration.
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

// mergeTOMLSection folds one independently-parsed section into the accumulating document. Arrays of tables APPEND; everything else keeps
// the FIRST occurrence, since a duplicate key is a strict-TOML error and a later overwrite would build a document strict TOML never
// would.
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
