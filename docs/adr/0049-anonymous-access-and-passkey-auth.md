---
adr: 0049
title: Anonymous access, open self-service registration, and passkey auth (no payment surface)
status: Proposed
date: 2026-08-14
supersedes: []
superseded_by: null
---

# ADR-0049: Anonymous access, open self-service registration, and passkey auth (no payment surface)

> **Retroactive record (audit-2026-08-14 D-L1).** The auth-model pivot below
> already SHIPPED (open `/v1/register`, WebAuthn passkeys, no billing/Stripe
> surface) but never had an ADR, so the decision-record surface stopped
> describing auth. This ADR reconstructs the decision and its threat model
> from the shipped code so future contributors have the "why". It is marked
> `Proposed` rather than `Accepted` because the RATIONALE and the accepted
> risk envelope are reconstructed, not authored by the decision-maker —
> the operator should ratify (or correct) the threat model below and then
> move it to `Accepted`. The code, not this doc, is authoritative on what
> ships today.

## Context

The v1 product serves a public data API (prices, supply, explorer). The
account/auth surface was pivoted to a self-service, no-payments model:

- **No payment surface.** There is no Stripe/billing integration in the
  tree; accounts carry a free-tier limits block (`RegisterLimits`,
  `internal/api/v1/register.go`), not a paid plan. Metering is by API-key
  `MonthlyQuota` (enforced on both the Postgres and the default Redis
  validator — see `middleware.MonthlyQuota` wiring in
  `cmd/stellarindex-api/main.go`), not by charge.
- **Open self-service registration.** `POST /v1/register` mints an account
  + first API key anonymously (`internal/api/v1/register.go`); it is a
  create-only surface (`RegisterAccountCreator`) guarded by a same-site
  write check (`internal/api/v1/csrf.go`) and the shared signup IP throttle
  (`internal/auth/signup_ip_throttle.go`), not by prior authentication.
- **Passkeys as the account credential.** WebAuthn credentials
  (`internal/platform/webauthncredential.go`,
  `internal/api/v1/dashboardauth/passkey.go`) with a ceremony guard
  (`internal/auth/passkey_ceremony_guard.go`) back dashboard sign-in, plus
  passwordless magic-link login (`dashboardauth/handlers.go`).

Recording this now matters because, absent a decision record, a future
contributor cannot tell whether open registration and the removed payment
coupling are intentional invariants or accidents — and might re-introduce
billing coupling or silently tighten/loosen registration against unstated
intent (the exact drift D-L1 flags).

## Decision

The v1 auth model is: **anonymous-by-default read access; open, unauthenticated
self-service `/v1/register` that mints a free-tier account + API key; API-key
metering (not payment) for quota; and WebAuthn passkeys + magic-link for the
dashboard account surface. There is no payment/billing surface in v1.**

## Consequences

- **Positive:** Zero-friction onboarding for a public data product; no PCI /
  payment-processor surface to secure; passkeys avoid a password store.
- **Negative / accepted risk envelope (to be ratified):** an open
  key-minting endpoint is an abuse surface. It is bounded today by the
  same-site write check + signup IP throttle + free-tier `MonthlyQuota`, and
  register-path orphans (PG account/key written, validator mirror failed)
  are marked `signup-race:` for the `signupreaper` to reclaim
  (`internal/api/v1/register.go`, `internal/signupreaper`). Known open edges
  the operator should weigh when ratifying: the throttle's local fallback
  fails open per-instance during a Redis outage, and magic-link tokens have
  their own retention/reaper considerations (tracked separately in the
  2026-08-14 audit). Loss of the Postgres auth data (accounts, keys,
  sessions, passkeys) is non-re-derivable — see ADR-0043 / the backup-DR
  findings.
- **Operational impact:** no payment ops; abuse response leans on rate
  limits + key revocation + the reaper rather than chargebacks.
- **Downstream design impact:** any future paid tier is a NEW decision that
  supersedes this ADR; do not re-add billing coupling without one.

## Alternatives considered

1. **Authenticated-only / invite-gated registration** — rejected for v1: it
   defeats the zero-friction goal for a public data API; revisit if abuse
   exceeds the rate-limit envelope.
2. **Retain a payment/billing surface** — rejected for v1: no paid plan
   shipped, and carrying a dormant payment integration is unnecessary attack
   surface and compliance load.
3. **Passwords instead of passkeys** — rejected: passwords add a credential
   store to protect; passkeys + magic-link avoid it.

## References

- Related ADRs: ADR-0042 (v1 wire shape), ADR-0043 (backup/restore — auth
  data durability), ADR-0018 (adjacent API-surface decision).
- Code: `internal/api/v1/register.go`, `internal/api/v1/csrf.go`,
  `internal/auth/signup_ip_throttle.go`,
  `internal/api/v1/dashboardauth/passkey.go`,
  `internal/platform/webauthncredential.go`, `internal/signupreaper`.
- Origin: audit-2026-08-14 finding D-L1 (missing auth-pivot ADR).
