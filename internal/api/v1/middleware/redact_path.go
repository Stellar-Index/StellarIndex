package middleware

import (
	"net/http"
	"net/url"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// maxUnescapes bounds how many percent-decoding layers redactEscaped looks through.
const maxUnescapes = 3

// RedactedPath is the request path for a log line or a response body, with any plaintext API key replaced. It is
// the decoded path, so a percent-encoded key is caught too.
func RedactedPath(r *http.Request) string {
	return redactEscaped(r.URL.Path, url.PathUnescape)
}

// RequestInstance is the path and query for a problem body's `instance`, with any plaintext API key replaced. Each
// part keeps its escaped form unless decoding it reveals a key: `sip%5F…` spells one the redactor would not see.
func RequestInstance(r *http.Request) string {
	s := redactEscaped(r.URL.EscapedPath(), url.PathUnescape)
	if r.URL.RawQuery != "" {
		s += "?" + redactEscaped(r.URL.RawQuery, url.QueryUnescape)
	}
	return s
}

// redactEscaped returns s unchanged unless s, or s with up to maxUnescapes layers of percent-encoding removed, holds
// a plaintext key; then it returns the first form that shows the key, redacted.
func redactEscaped(s string, unescape func(string) (string, error)) string {
	cur := s
	for range maxUnescapes + 1 {
		if red := auth.RedactKeySecrets(cur); red != cur {
			return red
		}
		next, err := unescape(cur)
		if err != nil || next == cur {
			return s
		}
		cur = next
	}
	return s
}
