package v1

import "strings"

// maskEmail redacts a customer email address for application logging
// (audit PRV1 / OBS-04): keep the first local-part character + the full
// domain, hide the rest — "alice@example.com" -> "a***@example.com".
// Enough to correlate a log line to a domain / support ticket without
// persisting the full PII in logs that ship to a third-party sink and
// outlive the account.
//
// Every log call site in this package that has a customer email in
// hand MUST route it through here. The ones that matter today are the
// signup verification send-failure (signup.go) and the two Stripe
// webhook triage lines (stripe_webhook.go) — all three are error/warn
// paths, which is exactly where an operator debugging an incident
// would otherwise leave a plaintext address in the log store.
//
// This is a deliberate twin of dashboardauth's unexported maskEmail
// rather than a shared call: package v1 does NOT import
// dashboardauth (server.go documents the dependency as one-way —
// dashboardauth depends on internal/platform + internal/notify, never
// on this package's envelope), and inverting that edge for a 15-line
// pure string helper would be a much worse trade than duplicating it.
// The two implementations must stay behaviourally identical; the
// contract is pinned by a test in each package.
func maskEmail(email string) string {
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
