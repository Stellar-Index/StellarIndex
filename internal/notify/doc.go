// Package notify is the transactional-email abstraction: one Sender interface, a Resend driver and
// a Noop for tests. Signup, invite-accept and paid-tier failure paths send mail, so a provider
// outage must be switchable to another driver without redeploying.
package notify
