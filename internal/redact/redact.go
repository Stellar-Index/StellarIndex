// Package redact renders values that carry a credential safe to print.
//
// It is the secrets counterpart to internal/pii, which does the same job
// for personal data, and it is a leaf package for the same reason: every
// caller — config validation, the migration tool, whatever comes next —
// shares one implementation and one set of tests, so a redaction cannot
// drift into a leak in the one copy nobody covered.
//
// Which function depends on what the caller knows, and using the wrong
// one either leaks or destroys the diagnostic:
//
//   - [ConnString] is for text WE compose, where the whole value is
//     suspect and the scheme is the only part worth keeping.
//   - [Credentials] is for text we did NOT compose — a third-party
//     library's error, which may embed a DSN verbatim — where the host,
//     the database and the reason are the operator's entire diagnostic
//     and only the secret may be cut out. It works by pattern, so it
//     can only be as good as the boundary the text offers.
//   - [Known] is [Credentials] for a process that holds the connection
//     strings its output might repeat, and so can cut the secret by
//     value, whatever characters it contains.
//   - [ParseFailure] renders the error from parsing a connection string
//     the caller holds, whose reason repeats fragments of the input
//     that no pattern can recognise.
//
// None is a parser. All run on strings that are, by definition,
// malformed or unknown at the point they are rendered, so they assume
// nothing about the input's shape.
package redact

import (
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ConnString renders a connection string for a fatal boot error without
// disclosing it. A Postgres DSN carries its password inline
// (postgres://user:pw@host/db) and so does a managed-Redis URI — every
// hosted Redis vendor issues rediss://default:<password>@host:port
// rather than the bare host:port a storage.redis_addr wants, which is
// the single likeliest way a real password reaches this code path. The
// branches that call this fire on MALFORMED input, so nothing about the
// string can be assumed: keep the scheme, which is the part that shows
// the operator what they got wrong, and drop the rest.
func ConnString(v string) string {
	if v == "" {
		return "(empty)"
	}
	if i := strings.Index(v, "://"); i > 0 {
		return v[:i] + "://<redacted>"
	}
	return "<redacted>"
}

// urlPasswordPattern matches the password half of a URL's userinfo:
// `scheme://user:SECRET@`. The username is kept — it is a useful
// diagnostic and it is not the secret — and a userinfo with no colon
// (scheme://user@host) is left alone for the same reason. A URL with no
// userinfo at all (scheme://host:5432/db) has no `@` and does not match.
//
// The two halves have DIFFERENT character classes, and the asymmetry is
// the point. The user part may not cross `/`, so the match cannot start
// somewhere inside a path. The password part may contain anything but
// `@` and whitespace, INCLUDING `/` and `%`, because a generated
// password routinely contains both and an operator pasting one raw is
// exactly what produces the unparseable DSN that lands in an error in
// the first place — excluding `/` here would let the commonest real
// secret through. The cost is that `scheme://host:port/path@x`, a
// URL with a port and a later `@`, is redacted as though the port were
// a password; over-redacting a rare path beats printing a password.
//
// The password runs to the LAST `@` of the token, not the first. It used
// to exclude `@`, which ended the match at the first one: a password
// pasted raw with an `@` in it — the very character that makes the DSN
// unparseable and sends it into an error — had its first segment cut
// and the rest printed (`u:<redacted>@rest-of-password@host`). Nothing
// in the text distinguishes the `@` that ends the userinfo from one
// inside the secret, so the only reading that cannot leak is the
// greedy one. Its cost is the same kind as above and as rare: a URL
// whose query holds an `@` (`…@host/db?application_name=me@corp`) loses
// its host to the redaction.
var urlPasswordPattern = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^/@\s"']*?):[^\s"']*@`)

// quotedURLPasswordPattern is the same cut for a URL that opens a Go
// %q-quoted string, which is how net/url's *url.Error — the one producer
// known to embed a whole DSN — renders it: `parse "<url>": <reason>`.
//
// It exists because the token pattern above ends a password at
// whitespace or a quote, which is the only boundary unquoted text
// offers. A password containing a space, a `'` or a `"` therefore did
// not match at all and printed WHOLE. Inside a quoted string the
// boundary is known exactly — the closing unescaped `"` — so the
// password may hold anything, a `\"` included, and still ends at the
// last `@` before that quote. It cannot run on into a second quoted URL
// in the same message, because it cannot cross the quote between them.
var quotedURLPasswordPattern = regexp.MustCompile(`("[a-zA-Z][a-zA-Z0-9+.\-]*://[^/@"\\]*?):(?:\\.|[^"\\])*@`)

// keywordPasswordPattern matches the other spellings Postgres accepts:
// the libpq keyword/value DSN (`host=h password=SECRET`), its quoted
// form (`password='se cret'`, which is how a password with a space is
// written and which a naive value class would truncate INTO the log),
// and the query parameter (`postgres://h/db?password=SECRET`).
// Case-insensitive because libpq keywords are. `password_env=NAME` does
// not match — the keyword has to be followed by the `=`, and there it is
// followed by `_env` — which matters, because that setting holds an
// env-var NAME and the whole diagnostic when it is wrong is the name.
var keywordPasswordPattern = regexp.MustCompile(`(?i)\b(sslpassword|password)\s*=\s*(?:'(?:\\.|[^'])*'|[^\s&"';]*)`)

// Credentials strips inline secrets out of text that was formatted
// somewhere else — typically a wrapped error from a library that
// embedded the connection string it was handed.
//
// This is the one that has to exist. Our own format strings can be
// audited; a dependency's cannot, and it only takes one that renders
// the raw URL for a fatal message to carry a production password into
// journald, Loki and any CI log that captured the deploy. Applying this
// at the point the process WRITES rather than at each call site is
// deliberate: a future error path inherits the redaction instead of
// having to remember it.
//
// Everything that is not a secret survives, because an operator staring
// at a failed migration needs the host, the database and the reason.
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

// Known is [Credentials] for a process that HOLDS the connection strings
// its output might repeat — the migration tool, which is handed the DSN
// in argv or the environment — and can therefore do what no pattern
// can: find the secret by value instead of by shape.
//
// A pattern has to guess where a password ends, and unquoted text gives
// it only whitespace to go on: `flag provided but not defined:
// -postgres://u:pass word@host` cuts `pass` and prints `word`. A
// process that knows the string it was given does not have to guess.
// Two by-value cuts run, and whatever remains goes through
// [Credentials]:
//
//   - every secret the held strings carry (see [heldSecrets]) is cut
//     where it follows its own anchor — `scheme://user:` or `password=` —
//     and the cut takes the longest PREFIX of the secret found there, not
//     only the whole of it. That is what an echo that stops short needs:
//     the flag package prints a rejected "flag name" up to its first `=`,
//     so `-postgres://u:abc==@host` comes back as `-postgres://u:abc`,
//     with no `@` for a span or a pattern to key on;
//   - the `:password@` span — the first `:` of the userinfo to the LAST
//     `@` — is then replaced wherever it still appears, raw or as Go's %q
//     would escape it, whatever precedes it. That is for text which
//     repeats the secret behind a different scheme or user than the one
//     it was held with.
//
// Both keep a delimiter ON PURPOSE. Replacing the bare password would
// arm a destructive rewrite on the commonest development DSN there is,
// postgres://app:app@localhost/app, and turn `database "app" does not
// exist` into `database "<redacted>"…`. `:app@` and `postgres://app:app`
// occur nowhere but in the echo of the DSN itself.
//
// The held strings are taken TOGETHER, not one at a time. This process
// holds two that usually share an anchor — the environment's DSN and the
// one in argv, both `postgres://stellarindex:` — and cutting for one and
// then the other let the first take only the prefix the two passwords
// share, which left the second nothing to match and printed its tail.
//
// WHAT THIS DOES NOT CLOSE, so that nobody reads "by value" as "every
// occurrence":
//
//   - a secret repeated on its own, or from its middle, with neither its
//     anchor before it nor the whole `:password@` span around it. That is
//     the price of the delimiter above. Nothing the migration tool runs
//     is known to print one; the pattern pass is all that stands behind
//     it.
//   - a query password that itself contains `&<recognised parameter>=`
//     (see [queryValueEnd]); what follows that `&` is read as the next
//     parameter.
//   - a credential the process was not handed as a connection string —
//     PGPASSWORD, a passfile, a service file. It is not held, so it is
//     not cut.
//
// Pass anything that might be a connection string; a value that is not
// one contributes nothing.
func Known(text string, connStrings ...string) string {
	var (
		held  []heldSecret
		spans []string
	)
	for _, v := range connStrings {
		for _, s := range heldSecrets(v) {
			s.escaped = goEscape(s.secret)
			held = append(held, s)
			if qa := goEscape(s.anchor); qa != s.anchor {
				held = append(held, heldSecret{anchor: qa, secret: s.secret, escaped: s.escaped})
			}
		}
		if span := passwordSpan(v); span != "" && span != marker {
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

// passwordSpan returns the `:password@` stretch of a URL-form connection
// string, or "" if it has none. The last `@` rather than the first, for
// the reason given on urlPasswordPattern; here it is exact rather than
// a guess whenever the string is one connection string, because the
// host part cannot legally contain an `@`.
func passwordSpan(v string) string {
	i := strings.Index(v, "://")
	if i <= 0 {
		return ""
	}
	rest := v[i+3:]
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return ""
	}
	c := strings.Index(rest[:at], ":")
	if c < 0 || c+1 == at {
		return ""
	}
	return rest[c : at+1]
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

// heldSecrets lists every secret in a connection string, in whichever
// spelling it was written:
//
//   - URL userinfo, `scheme://user:SECRET@`. The anchor starts at the
//     scheme, not at the start of v, because v may be a whole argv
//     element (`--postgres://…`, `-dsn=postgres://…`) and the echo does
//     not repeat the dashes it arrived with. With no `@` at all the
//     secret has no end marker and runs to the end of the string, on the
//     same terms as [unterminatedColon].
//   - URL query, `?password=SECRET` and `sslpassword`.
//   - libpq keyword/value, `password=SECRET` or `password='SE CRET'`.
func heldSecrets(v string) []heldSecret {
	i := strings.Index(v, "://")
	if i <= 0 {
		return keywordSecrets(v)
	}
	rest := v[i+3:]
	var out []heldSecret
	if c, end := userinfoPassword(rest); c >= 0 {
		out = append(out, heldSecret{anchor: v[schemeStart(v, i):i+3] + rest[:c+1], secret: rest[c+1 : end]})
	}
	return append(out, querySecrets(rest)...)
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

// queryValueEnd is where a query password ends: at the `&` that opens a
// parameter we RECOGNISE, or at the end of the string. Not at the first
// `&`, for the reason the userinfo cut runs to the last `@`: a password
// pasted raw may hold one, and `?password=abc&def&sslmode=disable` read
// by the URL grammar prints `def`. A parameter this list does not know
// is swallowed with the password, which costs a diagnostic and not a
// credential. What it cannot close is a password that itself contains
// `&<recognised name>=`; nothing in the string tells that from the real
// boundary, and the alternative is to drop every parameter after the
// password from every message.
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

// keywordPasswordKey and keywordValue read the password out of a libpq
// keyword/value string the way libpq does: a quoted value runs to its
// closing unescaped `'` — to the end of the string if there is none —
// and an unquoted one to the first unescaped whitespace. The free-text
// pattern cannot follow a backslash escape once %q has doubled it
// (`password=ab\ cd` echoes as `ab\\ cd`), so it stopped at the space.
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

// cutHeld replaces, wherever an anchor ends, the longest prefix found
// there of any secret held behind that anchor. The LONGEST match wins,
// across spellings and across secrets: the raw form matches a %q echo
// only up to its first escaped character, and one held password matches
// the echo of another only as far as the two agree, and a cut that
// stopped at either point would print the rest.
//
// A partial match is cut on purpose — that is the truncated echo — and
// it is safe for the reason the anchor exists: text that follows
// `scheme://user:` or `password=` is a credential or the start of one,
// and the pattern pass would cut it anyway wherever it can see its end.
func cutHeld(text string, held []heldSecret) string {
	if len(held) == 0 {
		return text
	}
	var b strings.Builder
	for i := 0; i < len(text); {
		n := 0
		// Already cut: a second pass must change nothing.
		if !strings.HasPrefix(text[i:], redacted) {
			n = heldPrefixLen(text, i, held)
		}
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

// goQuotedPattern matches a Go %q-quoted fragment, escapes included.
var goQuotedPattern = regexp.MustCompile(`"(?:\\.|[^"\\])*"`)

// ParseFailure renders the error from parsing a connection string the
// caller holds, for a fatal message, without repeating any of it.
//
// It exists because scrubbing the URL out of net/url's error is not
// enough: the REASON quotes the piece the parser choked on, and that
// piece is cut out of the input. A password with a `/`, `?` or `#` in it
// makes everything before that character look like a port, and the
// error reads `invalid port ":<most of the password>" after host` — a
// fragment no pattern can tell from an honest bad port. With a `#` the
// URL the error echoes is truncated before the `@`, so there is no
// userinfo left for a pattern to key on either.
//
// So nothing of the library's rendering of the input is passed through.
// The reason keeps its words and loses its quoted fragments; the
// connection string is re-rendered from the value itself, keeping the
// user, host, database and options. The fragment withheld is sometimes
// innocent (`invalid URL escape "%ZZ"` names three characters, which
// may or may not sit inside the password) and it is withheld anyway:
// the operator is told which string is wrong and what kind of wrong,
// and can see the rest in the file they just edited.
func ParseFailure(connString string, err error) string {
	reason := err.Error()
	var ue *url.Error
	if errors.As(err, &ue) {
		reason = ue.Err.Error()
	}
	reason = goQuotedPattern.ReplaceAllString(reason, `"<withheld>"`)
	return Known(reason, connString) + " in " + knownConnString(connString)
}

// knownConnString renders a string known to be ONE connection string,
// keeping everything but the secret. It is [Known] applied to the value
// itself, so the rendering of a DSN and the scrubbing of an echo of it
// cannot come to disagree about where a secret ends.
func knownConnString(v string) string { return Known(v, v) }

// unterminatedColon handles a URL with no `@` at all, returning the
// index of the colon a secret starts at, or -1. Usually such a URL is a
// DSN with no credentials (`host:5432/db`), and there is no secret. But
// `user:secret#host/db` — a `#` typed where the `@` goes, or a password
// pasted without its tail — has the same shape, and there the secret
// starts at the colon and nothing marks where it ends. A numeric port is
// believed; anything else after the first colon is a secret to the end
// of the string, host included, because a guessed boundary is a partial
// leak.
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
