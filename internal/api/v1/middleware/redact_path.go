package middleware

import (
	"net/http"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// RedactedPath is the request path for a log line or a response body, with any plaintext API key replaced. It is
// the decoded path, so a percent-encoded key is caught too.
func RedactedPath(r *http.Request) string {
	return auth.RedactKeySecrets(r.URL.Path)
}

// RequestInstance is the path and query for a problem body's `instance`, with any plaintext API key replaced. The
// escaped form is kept unless the decoded path holds a key: `sip%5F…` spells one the redactor would not see.
func RequestInstance(r *http.Request) string {
	if !strings.Contains(r.URL.Path, "sip_") {
		return auth.RedactKeySecrets(r.URL.RequestURI())
	}
	s := r.URL.Path
	if r.URL.RawQuery != "" {
		s += "?" + r.URL.RawQuery
	}
	return auth.RedactKeySecrets(s)
}
