package middleware

import (
	"net/http"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// maxUnescapes bounds how many percent-decoding layers redactEscaped looks through.
const maxUnescapes = 3

// RedactedPath is the request path for a log line or a response body, with any plaintext API key replaced. It is
// the decoded path, so a percent-encoded key is caught too.
func RedactedPath(r *http.Request) string {
	return redactEscaped(r.URL.Path, false)
}

// RequestInstance is the path and query for a problem body's `instance`, with any plaintext API key replaced. Each
// part keeps its escaped form unless decoding it reveals a key: `sip%5F…` spells one the redactor would not see.
func RequestInstance(r *http.Request) string {
	s := redactEscaped(r.URL.EscapedPath(), false)
	if r.URL.RawQuery != "" {
		s += "?" + redactEscaped(r.URL.RawQuery, true)
	}
	return s
}

// redactEscaped returns s byte for byte unless its deepest decoded form (up to maxUnescapes layers) holds a plaintext
// key; then it returns that form, redacted. Decoding is lenient so one stray `%` cannot hide a key elsewhere in s.
func redactEscaped(s string, query bool) string {
	cur := s
	for range maxUnescapes {
		next := lenientUnescape(cur, query)
		if next == cur {
			break
		}
		cur = next
	}
	if red := auth.RedactKeySecrets(cur); red != cur {
		return red
	}
	return s
}

// lenientUnescape decodes every valid %XX in s (and `+` as a space in a query), leaving an invalid escape as it is.
func lenientUnescape(s string, query bool) string {
	if !strings.ContainsAny(s, "%+") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]):
			b.WriteByte(unhex(s[i+1])<<4 | unhex(s[i+2]))
			i += 2
		case c == '+' && query:
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c <= '9':
		return c - '0'
	case c <= 'F':
		return c - 'A' + 10
	default:
		return c - 'a' + 10
	}
}
