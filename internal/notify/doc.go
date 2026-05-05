// Package notify is the transactional-email abstraction.
//
// One Sender interface, one Resend implementation, one Noop
// implementation for tests + dev-without-credentials. Templates
// render Go text/template — no React-Email runtime in the API
// binary.
//
// Why an abstraction at all when we've locked the provider:
// transactional email is in the critical path of customer
// signup + invite-accept + paid-tier failure flows. If Resend
// has an outage we want a dial we can flip without redeploying
// — Postmark and Mailgun are interface-compatible and ship as
// fallback drivers (not in this package today; added when the
// first outage proves the dial is needed).
package notify
