// Package pii holds the redaction helpers that must behave IDENTICALLY everywhere personal
// data can reach a log line.
//
// Package v1 must not import dashboardauth, so per-package copies of maskEmail could drift into
// leaking an address with nothing failing. This leaf package, with no internal imports, gives
// both packages exactly one implementation to test.
package pii

import "strings"

// MaskEmail renders an address safe to log: one leading character of the local part, then the domain, lossy by design (enough to correlate log lines, not to recover the address). Anything it cannot parse as an address is hidden ENTIRELY, because a permissive redactor fails by leaking plaintext; the empty string stays empty.
func MaskEmail(email string) string {
	at := strings.LastIndex(email, "@")
	if at <= 0 {
		if email == "" {
			return ""
		}
		return "***" // malformed / no domain — hide entirely
	}
	local, domain := email[:at], email[at:]
	if len(local) <= 1 {
		return "***" + domain
	}
	return local[:1] + "***" + domain
}
