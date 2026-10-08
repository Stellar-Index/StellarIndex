// Package redact renders values that carry a credential safe to print; it is
// the secrets counterpart to internal/pii. Pick by what the caller knows:
// [ConnString] for text we compose, [Credentials] for text we did not (cut by
// pattern), [Known] when the process holds the connection strings (cut by
// value), [ParseFailure] for an unparseable DSN, which repeats none of it.
package redact

import (
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ConnString renders a connection string for a fatal boot error, keeping only
// the scheme: the input is malformed, and managed-Redis URIs carry an inline
// password just as Postgres DSNs do.
func ConnString(v string) string {
	if v == "" {
		return "(empty)"
	}
	if i := strings.Index(v, "://"); i > 0 {
		return v[:i] + "://<redacted>"
	}
	return "<redacted>"
}

// urlPasswordPattern matches the password in `scheme://user:SECRET@`. The user
// may not cross `/` (so a match cannot start in a path), but the password may
// hold `/`, `%` and `@` and runs to the LAST `@`: a raw-pasted generated
// password is what makes a DSN unparseable, and nothing tells its `@` from the
// delimiter. The cost is over-redacting a rare `host:port/path@x`.
var urlPasswordPattern = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^/@\s"']*?):[^\s"']*@`)

// quotedURLPasswordPattern is the same cut inside a Go %q string, as
// *url.Error renders it. There the closing `"` bounds the password exactly, so
// one holding a space or quote cannot escape the match as it does unquoted.
var quotedURLPasswordPattern = regexp.MustCompile(`("[a-zA-Z][a-zA-Z0-9+.\-]*://[^/@"\\]*?):(?:\\.|[^"\\])*@`)

// keywordPasswordPattern matches libpq keyword (`password=X`, `password='x y'`)
// and query (`?password=X`) spellings, case-insensitively. `password_env=NAME`
// does not match: it holds an env-var name, which is the diagnostic.
var keywordPasswordPattern = regexp.MustCompile(`(?i)\b(sslpassword|password)\s*=\s*(?:'(?:\\.|[^'])*'|[^\s&"';]*)`)

// Credentials strips inline secrets from text formatted elsewhere, such as a
// library error embedding its DSN. Apply it where the process writes, so new
// error paths inherit it; host, database and reason survive.
func Credentials(s string) string {
	// Quoted first: it knows where the URL ends, so it must see the text
	// before the token pattern has cut a first segment out of it.
	s = quotedURLPasswordPattern.ReplaceAllString(s, "${1}:<redacted>@")
	s = urlPasswordPattern.ReplaceAllString(s, "${1}:<redacted>@")
	return keywordPasswordPattern.ReplaceAllString(s, "${1}=<redacted>")
}

// redacted is what a cut secret is replaced with, and marker is the same
// with the userinfo delimiters around it, so every path below renders
// the same thing and a second pass finds nothing left to change.
const (
	redacted = "<redacted>"
	marker   = ":" + redacted + "@"
)

// Known is [Credentials] for a process that holds the connection strings its
// output might repeat, so it can cut each secret by value: the longest prefix
// of it after its own anchor (`scheme://user:`, `password=`), which catches a
// truncated echo, then the whole `:password@` span raw or %q-escaped. Both
// keep a delimiter so a DSN like postgres://app:app@localhost/app does not
// redact `database "app"`. Held strings are cut together: two sharing an
// anchor, cut one at a time, leak the second's tail.
//
// Not closed: a secret echoed without its anchor or span; a query password
// containing `&<recognised parameter>=` (see [queryValueEnd]); credentials not
// held as connection strings (PGPASSWORD, passfile). It is the second line of
// defence; not echoing operator text at all is the first.
func Known(text string, connStrings ...string) string {
	var (
		held  []heldSecret
		spans []string
	)
	for _, v := range connStrings {
		secrets := heldSecrets(v)
		for _, s := range secrets {
			s.escaped = goEscape(s.secret)
			held = append(held, s)
			if qa := goEscape(s.anchor); qa != s.anchor {
				held = append(held, heldSecret{anchor: qa, secret: s.secret, escaped: s.escaped})
			}
		}
		if span := passwordSpan(v, secrets); span != "" && span != marker {
			spans = append(spans, span)
		}
	}
	text = cutHeld(text, held)
	// Longest first, so a span that happens to sit inside another cannot
	// break the longer one up before it is matched.
	sort.SliceStable(spans, func(i, j int) bool { return len(spans[i]) > len(spans[j]) })
	for _, span := range spans {
		text = strings.ReplaceAll(text, span, marker)
		text = strings.ReplaceAll(text, goEscape(span), marker)
	}
	return Credentials(text)
}

// goEscape renders s as it appears inside a Go %q-quoted string. Quoting
// is per character, so the escaped form of a prefix is a prefix of the
// escaped form — which is what lets [cutHeld] match a truncated echo in
// either spelling.
func goEscape(s string) string {
	q := strconv.Quote(s)
	return q[1 : len(q)-1]
}

// passwordSpan returns the `:password@` stretch of a URL-form connection string
// (to the last `@`), or "" if it has none or it covers another held secret's
// anchor: then the `@` belongs to a query password, and [cutHeld] cuts it.
func passwordSpan(v string, held []heldSecret) string {
	_, r, _ := urlRest(v)
	if r < 0 {
		return ""
	}
	rest := v[r:]
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return ""
	}
	c := strings.Index(rest[:at], ":")
	if c < 0 || c+1 == at {
		return ""
	}
	span := rest[c : at+1]
	for _, s := range held {
		if s.anchor != "" && strings.Contains(span, s.anchor) {
			return ""
		}
	}
	return span
}

// heldSecret is one secret a held connection string carries, with the
// text that immediately precedes it. The anchor is never replaced; it is
// what stops the cut from firing on text that merely shares a word with
// the secret.
type heldSecret struct {
	anchor  string
	secret  string
	escaped string // the secret as %q renders it; filled in by [Known]
}

// heldSecrets lists every secret in a connection string: URL userinfo (anchor
// starts at the scheme, since v may be an argv element with dashes; with no `@`
// see [unterminatedColon]), URL query passwords, and libpq keyword values.
func heldSecrets(v string) []heldSecret {
	s, r, opaque := urlRest(v)
	if r < 0 {
		return keywordSecrets(v)
	}
	rest := v[r:]
	var out []heldSecret
	if c, end := userinfoPassword(rest); c >= 0 {
		out = append(out, heldSecret{anchor: v[s:r] + rest[:c+1], secret: rest[c+1 : end]})
	}
	out = append(out, querySecrets(rest)...)
	if opaque {
		// A keyword string can hold a `word:` that reads as a scheme; keep
		// its keyword reading too.
		out = append(out, keywordSecrets(v)...)
	}
	return out
}

// urlRest returns where a URL-form connection string's scheme starts and where
// the text after its separator starts, or r = -1. A bare `scheme:` with no `//`
// counts only when it opens v, after dashes or `-flag=`; libpq quotes it whole.
func urlRest(v string) (s, r int, opaque bool) {
	if i := strings.Index(v, "://"); i > 0 {
		return schemeStart(v, i), i + 3, false
	}
	i := strings.Index(v, ":")
	if i <= 0 {
		return 0, -1, false
	}
	s = schemeStart(v, i)
	if s == i || (s > 0 && v[s-1] != '-' && v[s-1] != '=') {
		return 0, -1, false
	}
	r = i + 1
	if strings.HasPrefix(v[r:], "/") {
		r++
	}
	return s, r, true
}

// schemeStart walks back from the `://` at i over the characters a
// scheme may hold, then forward past any that may not open one — the
// dashes of a DSN typed where a flag goes.
func schemeStart(v string, i int) int {
	s := i
	for s > 0 && isSchemeByte(v[s-1]) {
		s--
	}
	for s < i && !isLetter(v[s]) {
		s++
	}
	return s
}

func isLetter(b byte) bool { return b|0x20 >= 'a' && b|0x20 <= 'z' }

func isSchemeByte(b byte) bool {
	return isLetter(b) || (b >= '0' && b <= '9') || b == '+' || b == '.' || b == '-'
}

// userinfoPassword locates the password in the part of a URL after its
// `://`: the index of the colon before it and the index just past it,
// or -1 if there is none.
func userinfoPassword(rest string) (colon, end int) {
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return unterminatedColon(rest), len(rest)
	}
	c := strings.Index(rest[:at], ":")
	if c < 0 || c+1 == at {
		return -1, 0
	}
	return c, at
}

// queryPasswordKey finds the libpq password parameters in a URL's query.
// Group 1 is the anchor: the key as the operator spelled it, without the
// `?` or `&`, so text that re-renders the query still matches.
var queryPasswordKey = regexp.MustCompile(`(?i)[?&]((?:sslpassword|password)=)`)

// querySecrets extracts each `?password=` value from a string known to
// be ONE URL, which is what lets the value hold a space, a quote or a
// `;` — each of which ends it for keywordPasswordPattern, written for
// free text where they are the only boundary on offer.
func querySecrets(rest string) []heldSecret {
	var out []heldSecret
	for _, m := range queryPasswordKey.FindAllStringSubmatchIndex(rest, -1) {
		val := rest[m[3]:]
		out = append(out, heldSecret{anchor: rest[m[2]:m[3]], secret: val[:queryValueEnd(val)]})
	}
	return out
}

// queryValueEnd ends a query password at the `&` of a RECOGNISED parameter, not
// the first `&`, which a raw password may hold. An unknown parameter is
// swallowed: that costs a diagnostic, not a credential.
func queryValueEnd(val string) int {
	for from := 0; ; {
		amp := strings.Index(val[from:], "&")
		if amp < 0 {
			return len(val)
		}
		amp += from
		if name, _, ok := strings.Cut(val[amp+1:], "="); ok && isConnParam(name) {
			return amp
		}
		from = amp + 1
	}
}

// isConnParam reports whether name is a connection parameter that may
// follow a password in a Postgres URL: libpq's, pgx's pool settings, the
// run-time parameters commonly set this way, and golang-migrate's `x-`
// family.
func isConnParam(name string) bool {
	name = strings.ToLower(name)
	return strings.HasPrefix(name, "x-") || connParams[name]
}

var connParams = map[string]bool{
	"host": true, "hostaddr": true, "port": true, "dbname": true, "user": true,
	"password": true, "passfile": true, "require_auth": true, "channel_binding": true,
	"connect_timeout": true, "client_encoding": true, "options": true,
	"application_name": true, "fallback_application_name": true,
	"keepalives": true, "keepalives_idle": true, "keepalives_interval": true,
	"keepalives_count": true, "tcp_user_timeout": true, "replication": true,
	"gssencmode": true, "sslmode": true, "requiressl": true, "sslnegotiation": true,
	"sslcompression": true, "sslcert": true, "sslkey": true, "sslpassword": true,
	"sslcertmode": true, "sslrootcert": true, "sslcrl": true, "sslcrldir": true,
	"sslsni": true, "requirepeer": true, "ssl_min_protocol_version": true,
	"ssl_max_protocol_version": true, "krbsrvname": true, "gsslib": true,
	"gssdelegation": true, "service": true, "target_session_attrs": true,
	"load_balance_hosts": true, "search_path": true, "timezone": true,
	"statement_timeout": true, "lock_timeout": true,
	"idle_in_transaction_session_timeout": true, "default_query_exec_mode": true,
	"statement_cache_capacity": true, "description_cache_capacity": true,
	"pool_max_conns": true, "pool_min_conns": true, "pool_max_conn_lifetime": true,
	"pool_max_conn_lifetime_jitter": true, "pool_max_conn_idle_time": true,
	"pool_health_check_period": true,
}

// keywordPasswordKey and keywordValue read a libpq keyword password as libpq
// does: quoted runs to its closing `'` (or end), unquoted to unescaped space,
// which the free-text pattern cannot follow once %q has doubled a backslash.
var (
	keywordPasswordKey = regexp.MustCompile(`(?i)\b(?:sslpassword|password)\s*=\s*`)
	keywordValue       = regexp.MustCompile(`(?s)^(?:'(?:\\.|[^'\\])*'?|(?:\\.|[^\s\\])*)\\?`)
)

func keywordSecrets(v string) []heldSecret {
	var out []heldSecret
	for _, m := range keywordPasswordKey.FindAllStringIndex(v, -1) {
		out = append(out, heldSecret{anchor: v[m[0]:m[1]], secret: keywordValue.FindString(v[m[1]:])})
	}
	return out
}

// cutHeld replaces, wherever an anchor ends, the longest prefix there of any
// secret held behind it, across spellings and secrets; a shorter cut would print
// the rest. No skip for text already reading `<redacted>`: a password starting
// with it would leak its remainder.
func cutHeld(text string, held []heldSecret) string {
	if len(held) == 0 {
		return text
	}
	var b strings.Builder
	for i := 0; i < len(text); {
		n := extendPastNested(text, i, heldPrefixLen(text, i, held), held)
		if n == 0 {
			b.WriteByte(text[i])
			i++
			continue
		}
		b.WriteString(redacted)
		i += n
	}
	return b.String()
}

// extendPastNested grows a cut at i of n bytes past any second secret whose
// anchor it swallows: in `postgres://host:5432/db?password=HEAD@TAIL` the
// userinfo reading eats `password=` and would otherwise leave `TAIL`.
func extendPastNested(text string, i, n int, held []heldSecret) int {
	for grew := n > 0; grew; {
		grew = false
		for j := i + 1; j <= i+n && j < len(text); j++ {
			if m := heldPrefixLen(text, j, held); j+m > i+n {
				n = j + m - i
				grew = true
			}
		}
	}
	return n
}

// heldPrefixLen is how much of text, from i, repeats the start of a
// secret whose anchor ends at i.
func heldPrefixLen(text string, i int, held []heldSecret) int {
	n := 0
	for _, s := range held {
		if s.anchor != "" && strings.HasSuffix(text[:i], s.anchor) {
			n = max(n, commonPrefixLen(text[i:], s.secret), commonPrefixLen(text[i:], s.escaped))
		}
	}
	return n
}

func commonPrefixLen(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// ParseFailure says why a held connection string could not be used without
// repeating it: net/url's reason quotes fragments of the input (a password with
// `/`, `?` or `#` reads as `invalid port ":<password>"`). The string is
// described only when it parses; otherwise only the kind is returned.
func ParseFailure(connString string, err error) string {
	kind := parseErrorKind(err)
	u, perr := url.Parse(connString)
	if perr != nil {
		return kind
	}
	return kind + " in " + safeURL(u)
}

// parseErrorKind names what is wrong with a connection string using only
// this package's own words. The error's own text is READ — to tell an
// escape from a port from a userinfo — and never passed through, because
// net/url quotes the input fragment it choked on into every one of them.
func parseErrorKind(err error) string {
	var (
		ue *url.Error
		ee url.EscapeError
		he url.InvalidHostError
	)
	if errors.As(err, &ue) {
		err = ue.Err
	}
	switch msg := err.Error(); {
	case errors.As(err, &ee):
		return "invalid URL escape (a literal % must be written %25)"
	case errors.As(err, &he):
		return "an invalid character in the host"
	case strings.Contains(msg, "invalid port"):
		return "invalid port after the host (a reserved character in the password reads as one)"
	case strings.Contains(msg, "invalid userinfo"):
		return "an invalid character in the user:password part"
	case strings.Contains(msg, "missing protocol scheme"):
		return `no scheme before the "://"`
	case strings.Contains(msg, "first path segment in URL cannot contain colon"):
		return "a colon in the first path segment (the scheme is missing)"
	case strings.Contains(msg, "control character"):
		return "a control character"
	default:
		return "a syntax error"
	}
}

// safeURL renders a URL that PARSED, keeping everything an operator
// needs — scheme, user, host, database, options — and dropping the
// password. It is structural, not textual: every part comes from the
// parser rather than from an index into the string, so there is no
// boundary left to guess wrong.
func safeURL(u *url.URL) string {
	out := *u
	if out.User != nil {
		out.User = url.User(out.User.Username())
	}
	if out.RawQuery != "" {
		q := out.Query()
		for key := range q {
			if k := strings.ToLower(key); k == "password" || k == "sslpassword" {
				q.Set(key, redacted)
			}
		}
		// Re-encoded from the parsed values, not copied from the text, so
		// a parameter the parser could not read is dropped rather than
		// echoed. Encode() escapes the marker's angle brackets; undo that
		// one substitution so the message reads.
		out.RawQuery = strings.ReplaceAll(q.Encode(), url.QueryEscape(redacted), redacted)
	}
	return out.String()
}

// unterminatedColon returns the colon a secret starts at in a URL with no `@`,
// or -1. A numeric port is believed; anything else (`user:secret#host/db`) is a
// secret to the end of the string, because a guessed boundary leaks.
func unterminatedColon(rest string) int {
	from := 0
	if strings.HasPrefix(rest, "[") {
		// A bracketed IPv6 host is all colons and none of them is this one.
		from = strings.Index(rest, "]") + 1
	}
	c := strings.Index(rest[from:], ":")
	if c < 0 {
		return -1
	}
	c += from
	end := c + 1
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == len(rest) || rest[end] == '/' || rest[end] == '?' {
		return -1
	}
	return c
}
