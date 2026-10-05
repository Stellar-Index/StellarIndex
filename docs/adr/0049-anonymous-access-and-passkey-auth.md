---
adr: 0049
title: Anonymous access, open self-service registration, and passkey auth (no payment surface)
status: Accepted
date: 2026-08-14
supersedes: []
superseded_by: null
---

# ADR-0049: Anonymous access, open self-service registration, and passkey auth (no payment surface)

## Context

The v1 product is a public data API, and the auth model was pivoted to self-service with no payments.
The model shipped without a record, so open registration and the missing billing coupling could be mistaken for accidents.

## Decision

1. **Anonymous reads.** Read routes work without a key under the anonymous rate-limit class, counted per client IP; an authenticated caller is counted in its owner account's bucket (`middleware.RateLimitBySubject`).
2. **Open registration.** `POST /v1/register` mints an account and a first API key with no prior authentication (`internal/api/v1/register.go`). It is create-only, requires `Content-Type: application/json` (`internal/api/v1/csrf.go`, which forces a CORS preflight on cross-site browser POSTs; the Origin-based `RequireSameSiteWrite` is deliberately not mounted because API callers send no Origin), and passes the shared signup IP throttle (`internal/auth/signup_ip_throttle.go`).
3. **Throttle failure mode.** The IP throttle fails open only for the first `DefaultSignupThrottleDwellTime` (30 s) of continuous Redis errors; after that `CheckIP` returns `ErrThrottleUnavailable` and signup returns 503.
4. **Metering is by quota, not charge.** Accounts carry a free-tier `RegisterLimits` block, enforced as API-key `MonthlyQuota` on both the Postgres and Redis validators.
5. **Orphan cleanup.** A registration whose validator mirror write failed is suspended with a `signup-race:` reason and reclaimed by `internal/signupreaper`. Expired `magic_link_tokens` are deleted 48 h past expiry by `internal/magiclinkreaper`.
6. **Dashboard credentials are WebAuthn passkeys plus passwordless magic-link login** (`internal/api/v1/dashboardauth/`, `internal/platform/webauthncredential.go`). The `PasskeyCeremonyGuard` interface and its in-process guard live in `dashboardauth/passkey_ceremony_guard.go`; `internal/auth/passkey_ceremony_guard.go` is its Redis adapter.
7. **No payment or billing surface in v1.** A paid tier is a new ADR that supersedes this one.

## Invariant

- `POST /v1/register` rejects a non-JSON content type: `TestRegister_RequiresContentType` in `internal/api/v1/register_test.go`.
- Signup fails closed with 503 once the throttle's Redis dwell is exceeded: `TestRegister_ThrottleUnavailableFailsClosedAndCounts` in `internal/api/v1/register_test.go`.
- A registration whose mirror write fails leaves an orphan the reaper can reclaim, never a 200: `TestRegister_MirrorFailureSuspendsOrphanForReaper` in `internal/api/v1/register_test.go`.
- The signup reaper is bound to the Postgres account store, not the dashboard bundle: `TestRun_SignupReaperBindsToThePostgresAccountStoreNotTheDashboardBundle` in `cmd/stellarindex-api/signup_reaper_wiring_test.go`.

## Consequences

- Onboarding has no friction, no PCI scope and no password store.
- Open key minting is an abuse surface, bounded by the content-type gate, the IP throttle and free-tier quota; response is rate limits and key revocation, not chargebacks.
- Accounts, keys, sessions and passkeys in Postgres cannot be re-derived from the chain, so their durability rests on the Postgres backup of ADR-0043.
- Invite-gated registration, a dormant payment integration and passwords were rejected for v1; revisit gating if abuse exceeds the rate-limit envelope.

## Evidence

`internal/api/v1/register.go`, `internal/api/v1/csrf.go`, `internal/auth/signup_ip_throttle.go`, `internal/signupreaper`, `internal/magiclinkreaper`, wiring in `cmd/stellarindex-api/main.go`.
