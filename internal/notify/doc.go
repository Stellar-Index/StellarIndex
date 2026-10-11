// Package notify is the transactional-email abstraction.
//
// One Sender interface, one Resend implementation, one Noop implementation for tests.
// Templates render Go text/template, so the API binary needs no React-Email runtime.
//
// The abstraction exists because transactional email sits on the signup, invite-accept and
// paid-tier failure paths: a provider outage must be switchable to another driver without
// redeploying.
package notify
