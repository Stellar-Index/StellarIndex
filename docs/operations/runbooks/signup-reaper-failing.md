---
title: Runbook — signup-reaper-failing
last_verified: 2026-10-21
status: living
severity: P3
---

# Runbook — `stellarindex_signup_reaper_failing`

## At a glance

P3. `stellarindex_signup_reaper_failing`; orphan accounts accumulate, not customer-facing. Typical MTTR 5-30 min.

P3 (ticket). Rule: `configs/prometheus/rules.r1/signup-reaper.yml` (r1), mirrored in `deploy/monitoring/rules/signup-reaper.yml`. The reaper (`internal/signupreaper`, hourly) cannot delete orphan `accounts` rows left by lost signup races (F-1255), so they accumulate. A slow table leak, not customer-facing. Typical MTTR 5-30 min.

Symptom: the summed 6h `error` rate of `stellarindex_signup_reaper_runs_total` exceeds the `ok` rate (with at least one run) for 30+ min; API log `signup-reaper: reap failed`.

## What the reaper runs

`ReapSuspendedOrphans` (`internal/platform/postgresstore/account_store.go`) runs this in bounded batches. An `error` outcome means a batch failed; batches already committed stay deleted and the rest retries next tick:

```sql
DELETE FROM accounts a
WHERE a.id IN (
  SELECT a2.id FROM accounts a2
   WHERE a2.status = 'suspended'
     AND a2.suspended_reason LIKE 'signup-race:%'
     AND a2.suspended_at IS NOT NULL
     AND a2.suspended_at < now() - interval '24 hours'   -- DefaultMinAge
     AND NOT EXISTS (SELECT 1 FROM users u    WHERE u.account_id = a2.id)
     AND NOT EXISTS (SELECT 1 FROM api_keys k WHERE k.account_id = a2.id)
   LIMIT 2000
);
```

## Check

```sh
# 1) Which outcome dominates (API serves /metrics on :3000).
curl -fs http://localhost:3000/metrics \
  | grep '^stellarindex_signup_reaper_'

# 2) Underlying error from API logs.
journalctl -u stellarindex-api -n 100 \
  | grep 'signup-reaper: reap failed'

# 3) Backlog size.
sudo -u postgres psql -X -d stellarindex -c \
  "SELECT count(*) FROM accounts WHERE status='suspended' AND suspended_reason LIKE 'signup-race:%';"
```

## Likely causes

| Underlying error | Cause | Fix |
| ---------------- | ----- | --- |
| connection refused / timeout | Postgres down / failover | Wait for recovery; see [`postgres.md#stellarindex_postgres_ping_failing`](postgres.md#stellarindex_postgres_ping_failing) |
| `permission denied for table accounts` (42501) | Migration applied as superuser | `ALTER TABLE accounts OWNER TO stellarindex` (migrations/README rule 7) |
| statement timeout | DB under load | Check DB load; resolves on its own |

## Fix

1. Confirm Postgres reachability from the API host.
2. Fix table ownership if the error is `42501`.
3. Verify the `ok` rate recovers above the `error` rate; the alert clears after 30 min sustained.
4. To turn the reaper off, set `[signup_reaper] enabled = false` and restart the API (orphans are harmless).
5. Optional: once Postgres is healthy, run the DELETE above by hand in `psql`, repeating until it deletes 0 rows, to clear the backlog.

False positives: a cold-start blip or one transient error at very low volume; `for: 30m` and the 6h window damp both. A large backlog with a healthy `/v1/auth/callback` also warrants checking whether the signup race fires abnormally often.

## Related

`internal/signupreaper/`, `internal/api/v1/dashboardauth/` (the `/v1/auth/callback` path that produces the orphans), [`postgres.md#stellarindex_postgres_ping_failing`](postgres.md#stellarindex_postgres_ping_failing).
