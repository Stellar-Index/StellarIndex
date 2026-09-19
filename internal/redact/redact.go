// Package redact renders values that carry a credential safe to print.
//
// It is the secrets counterpart to internal/pii, which does the same job
// for personal data, and it is a leaf package for the same reason: every
// caller — config validation, the migration tool, whatever comes next —
// shares one implementation and one set of tests, so a redaction cannot
// drift into a leak in the one copy nobody covered.
//
// Two functions, because there are two situations and using the wrong
// one either leaks or destroys the diagnostic:
//
//   - [ConnString] is for text WE compose, where the whole value is
//     suspect and the scheme is the only part worth keeping.
//   - [Credentials] is for text we did NOT compose — a third-party
//     library's error, which may embed a DSN verbatim — where the host,
//     the database and the reason are the operator's entire diagnostic
//     and only the secret may be cut out.
//
// Neither is a parser. Both run on strings that are, by definition,
// malformed or unknown at the point they are rendered, so they assume
// nothing about the input's shape.
package redact

import (
	"regexp"
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
