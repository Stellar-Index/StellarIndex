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

// marker is what a cut secret is replaced with, delimiters included, so
// every path below renders the same thing and a second pass finds
// nothing left to change.
const marker = ":<redacted>@"

// Known is [Credentials] for a process that HOLDS the connection strings
// its output might repeat — the migration tool, which is handed the DSN
// in argv or the environment — and can therefore do what no pattern
// can: find the secret by value instead of by shape.
//
// A pattern has to guess where a password ends, and unquoted text gives
// it only whitespace to go on: `flag provided but not defined:
// -postgres://u:pass word@host` cuts `pass` and prints `word`. A
// process that knows the string it was given does not have to guess.
// For each value, the `:password@` span — the first `:` of the userinfo
// to the LAST `@` — is replaced wherever it appears, raw or as Go's %q
// would escape it, and whatever remains goes through [Credentials].
//
// The span keeps its delimiters ON PURPOSE. Replacing the bare password
// would arm a destructive rewrite on the commonest development DSN
// there is, postgres://app:app@localhost/app, and turn `database "app"
// does not exist` into `database "<redacted>"…`. `:app@` occurs nowhere
// but in the echo of the DSN itself.
//
// Pass anything that might be a connection string; a value that is not
// one contributes nothing.
func Known(text string, connStrings ...string) string {
	for _, v := range connStrings {
		span := passwordSpan(v)
		if span == "" || span == marker {
			continue
		}
		text = strings.ReplaceAll(text, span, marker)
		if q := strconv.Quote(span); q[1:len(q)-1] != span {
			text = strings.ReplaceAll(text, q[1:len(q)-1], marker)
		}
	}
	return Credentials(text)
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

// goQuotedPattern matches a Go %q-quoted fragment, escapes included.
var goQuotedPattern = regexp.MustCompile(`"(?:\\.|[^"\\])*"`)

// queryPasswordPattern is the libpq query parameter when the WHOLE
// string is known to be one URL: the value runs to the next `&` and may
// hold a space, which keywordPasswordPattern — written for free text,
// where a space ends the value — would stop at.
var queryPasswordPattern = regexp.MustCompile(`(?i)([?&](?:sslpassword|password)=)[^&]*`)

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
// keeping everything but the secret. Knowing the bounds is what lets it
// handle a password holding whitespace or quotes, which [Credentials]
// can only do inside a quoted string.
func knownConnString(v string) string {
	i := strings.Index(v, "://")
	if i <= 0 {
		// Keyword/value form, or nothing recognisable.
		return Credentials(v)
	}
	head, rest := v[:i+3], v[i+3:]
	if span := passwordSpan(v); span != "" {
		rest = strings.Replace(rest, span, marker, 1)
	} else if !strings.Contains(rest, "@") {
		rest = cutUnterminatedPassword(rest)
	}
	return head + queryPasswordPattern.ReplaceAllString(rest, "${1}<redacted>")
}

// cutUnterminatedPassword handles a URL with no `@` at all. Usually that
// is a DSN with no credentials (`host:5432/db`), and it is returned as
// is. But `user:secret#host/db` — a `#` typed where the `@` goes, or a
// password pasted without its tail — has the same shape, and there the
// secret starts at the colon and nothing marks where it ends. A numeric
// port is believed; anything else after the first colon is dropped to
// the end of the string, host included, because a guessed boundary is
// a partial leak.
func cutUnterminatedPassword(rest string) string {
	from := 0
	if strings.HasPrefix(rest, "[") {
		// A bracketed IPv6 host is all colons and none of them is this one.
		from = strings.Index(rest, "]") + 1
	}
	c := strings.Index(rest[from:], ":")
	if c < 0 {
		return rest
	}
	c += from
	end := c + 1
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == len(rest) || rest[end] == '/' || rest[end] == '?' {
		return rest
	}
	return rest[:c] + ":<redacted>"
}
