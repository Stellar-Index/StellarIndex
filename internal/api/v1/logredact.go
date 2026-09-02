package v1

import "github.com/Stellar-Index/StellarIndex/internal/pii"

// maskEmail redacts a customer email address for application logging
// (audit PRV1 / OBS-04): keep the first local-part character + the full
// domain, hide the rest — "alice@example.com" -> "a***@example.com".
// Enough to correlate a log line to a domain / support ticket without
// persisting the full PII in logs that ship to a third-party sink and
// outlive the account.
//
// Every log call site in this package that has a customer email in
// hand MUST route it through here. The one that matters today is the
// signup verification send-failure (signup.go) — an error/warn path,
// which is exactly where an operator debugging an incident would
// otherwise leave a plaintext address in the log store.
//
// The implementation lives in internal/pii so there is exactly ONE of
// it. It used to be a deliberate twin of dashboardauth's copy — package
// v1 must not import dashboardauth, and inverting that edge for a
// 15-line helper would have been a worse trade — but the comment also
// claimed "the contract is pinned by a test in each package", and that
// was false: only dashboardauth had the table test. A leaf package with
// no internal imports dissolves the import problem instead of policing
// it (#346 F8).
func maskEmail(email string) string { return pii.MaskEmail(email) }
