---
title: Runbook — signup-reaper-failing
last_verified: 2026-07-05
status: living
severity: P3
---

# Runbook — `stellarindex_signup_reaper_failing`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_signup_reaper_failing` |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/signup-reaper.yml` |
| Typical MTTR | 5–30 min (usually Postgres reachability recovery) |
| Impact | The API binary's F-1255 speculative-account reaper can't delete orphan `accounts` rows, so lost-signup-race orphans accumulate unbounded in the `accounts` table. This is a slow table leak — NOT customer-facing. The public pricing surface, auth, and the dashboard are all unaffected; only the count of never-recovered `Suspended` orphan rows grows. |

## Symptoms

- `rate(stellarindex_signup_reaper_runs_total{outcome="error"}[6h]) > rate(stellarindex_signup_reaper_runs_total{outcome="ok"}[6h])` sustained 30+ min.
- API log lines repeat `signup-reaper: reap failed` with the underlying error (Postgres unreachable, query timeout, permission denied on `accounts`).
- A slow climb in `SELECT count(*) FROM accounts WHERE status='suspended' AND suspended_reason LIKE 'signup-race:%'`.

## Background — why this fires

F-1255: two concurrent `/v1/auth/callback` provisions for the same
just-verified email can both create an `accounts` row, but only one
`CreateUser` wins on `users_email_idx`. The loser's account is marked
`Suspended` with a `signup-race:` reason and never gets a user. The
reaper (`internal/signupreaper`, default hourly) deletes those orphans:

```sql
DELETE FROM accounts a
WHERE a.status = 'suspended'
  AND a.suspended_reason LIKE 'signup-race:%'
  AND a.suspended_at < now() - <min_age>       -- default 24h safety window
  AND NOT EXISTS (SELECT 1 FROM users u    WHERE u.account_id = a.id)
  AND NOT EXISTS (SELECT 1 FROM api_keys k WHERE k.account_id = a.id);
```

`error` means that DELETE failed — the whole sweep is a no-op and is
retried next tick. Common causes:

1. **Postgres unreachable** from the API host (network / restart /
   failover). Self-recovers once the connection is back.
2. **`accounts` permission denied (42501)** — the table was created by
   the `postgres` superuser instead of the `stellarindex` app role
   (migrations/README rule 7). Fix: `ALTER TABLE accounts OWNER TO
   stellarindex`.
3. **Query timeout** under heavy DB load. Usually transient.

## Quick diagnosis (≤ 5 min)

```sh
# 1) Confirm which outcome dominates (API serves /metrics on :3000).
curl -fs http://localhost:3000/metrics \
  | grep '^stellarindex_signup_reaper_'

# 2) Underlying error from API logs.
journalctl -u stellarindex-api -n 100 \
  | grep 'signup-reaper: reap failed'

# 3) Confirm the table is writable by the app role + see the backlog.
psql "$STELLARINDEX_POSTGRES_DSN" -c \
  "SELECT count(*) FROM accounts WHERE status='suspended' AND suspended_reason LIKE 'signup-race:%';"
```

## Decision tree

| Underlying error | Likely cause | Mitigation |
| ---------------- | ------------ | ---------- |
| connection refused / timeout | Postgres down / failover | Wait for recovery; check `postgres-ping-failing` |
| `permission denied for table accounts` | Migration applied as superuser | `ALTER TABLE accounts OWNER TO stellarindex` (migrations/README rule 7) |
| statement timeout | DB under load | Check DB load; alert auto-resolves once queries complete |

## Mitigation (≤ 30 min)

- [ ] **Confirm Postgres reachability** from the API host.
- [ ] **Fix table ownership** if the error is `42501` (see decision tree).
- [ ] **Verify** `rate(stellarindex_signup_reaper_runs_total{outcome="ok"}[6h])`
      recovers above the `error` rate; the alert auto-resolves after 30
      min sustained.
- [ ] If the reaper should be off, set `[signup_reaper] enabled = false`
      and restart the API — the series stops and the alert clears.
      (Orphans are harmless; they just accumulate.)
- [ ] Optional one-shot backlog clear once Postgres is healthy — the SQL
      in "Background" run by hand under `psql` is exactly what the reaper
      would do.

## Root cause analysis

Capture for the postmortem: the underlying error class, whether the
`accounts` table ownership was wrong, the orphan backlog size at
firing time, and the outage duration (alert FIRING → RESOLVED). A large
backlog + a healthy `/v1/auth/callback` path also warrants checking
whether the signup-race is firing abnormally often (a hot inbox being
hammered), separate from the reaper's own health.

## Known false-positive patterns

- **Cold start**: API boots, first sweep fires before Postgres is
  ready. The `for: 30m` clause masks this; a single restart blip is not
  this alert.
- **Very low volume**: with hourly sweeps and near-zero orphan
  production, one transient error can briefly tip the 6h ratio. The
  `for: 30m` + 6h window damp this; a genuine, sustained failure holds
  across multiple sweeps.

## Related

- [`docs/architecture/platform-spec.md`](../../architecture/platform-spec.md)
  — the accounts/users platform schema (migration 0027) the reaper
  operates on.
- `internal/signupreaper/` — the reaper package.
- `internal/api/v1/dashboardauth/` — the `/v1/auth/callback`
  provisioning path that produces the signup-race orphans (F-1255).
- Sibling alert: `postgres-ping-failing` (the broader "API can't reach
  Postgres" signal).

## Changelog

- 2026-07-05 — initial draft alongside the F-1255 speculative-account
  reaper.
