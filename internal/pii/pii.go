// Package pii holds the redaction helpers that must behave IDENTICALLY
// everywhere personal data can reach a log line.
//
// It exists because duplicated copies drift. Package v1 must not import
// dashboardauth (the dependency is one-way), so two unexported
// `maskEmail` copies, one per package, would each need their own table
// test, and a copy without one could drift into leaking an address
// with nothing failing.
//
// A leaf package with no internal imports dissolves the problem instead
// of policing it. Both packages import this one, there is no import
// inversion, and Go guarantees there is exactly one implementation to
// test.
package pii

import "strings"

// MaskEmail renders an address safe to log: one leading character of the
// local part, then the domain. It is deliberately lossy — enough to
// correlate two log lines about the same user during an incident, not
// enough to recover the address.
//
// Anything it cannot parse as an address is hidden ENTIRELY rather than
// passed through, because the failure mode of a permissive redactor is
// the plaintext it was meant to prevent. The empty string stays empty so
// an absent value logs as absent rather than as "***".
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
