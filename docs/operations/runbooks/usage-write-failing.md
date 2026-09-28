---
title: Runbook — usage-write-failing
last_verified: 2026-09-28
status: draft
severity: P3
---

# Runbook — `stellarindex_usage_write_failing`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_usage_write_failing` (P3 / ticket) |
| Detected by | Prometheus rule in `deploy/monitoring/rules/api.yml` and the R1 single-host overlay `configs/prometheus/rules.r1/api.yml`. |
| Typical MTTR | 5–30 min: clears on its own once usage-counter writes start succeeding again. |
| Impact | **Revenue.** Every dropped billable unit is unmetered request traffic. Because the write failed rather than the read, `MonthToDate` sees a real (low) Redis value, not an error, so `MonthlyQuota` never fails open and there is no other signal that the ceiling has gone uncapped. |

## What this fires on

`internal/api/v1/middleware/usage.go`'s `UsageTracker` best-effort
increments the per-subject Redis usage counters on every metered
request. Before GH-1277 a failed `Increment`/`IncrementDetailBy` logged
at `Debug` — below the API's production log level — and was otherwise
invisible.

`stellarindex_usage_units_dropped_total{counter="billable"|"detail"}`
now increments by the request's unit count on each failed write. This
rule watches the `billable` label — the one that feeds `MonthlyQuota`
— and fires on `rate(...[5m]) > 0` sustained `for: 10m`, so a one-off
blip does not ticket but a genuinely sustained write failure does.

This is the **write-side twin** of
[monthly-quota-fail-open](monthly-quota-fail-open.md), not a duplicate
of it: that alert fires when the counter *read* fails and the request
is served without enforcing the cap; this one fires when the counter
*write* fails, which means the *next* read sees a falsely low value
instead of an error — the quota gate is not even aware anything is
wrong. The two are mutually exclusive per request and do not normally
move together.

## Quick diagnosis (≤ 5 min)

1. **Is Redis up?** Same first check as
   [monthly-quota-fail-open](monthly-quota-fail-open.md#quick-diagnosis--5-min):
   `redis-cli -a "$REDIS_PASSWORD" ping`, check `/readyz`.
2. **Firing ALONE (Redis healthy)?** Suspect the usage-counter key
   namespace, an ACL change scoped to `usage:*`/`usage:ep:*` keys, or a
   wedged `TxPipeline` — `journalctl -u stellarindex-api | grep -i
   'usage: increment failed\|usage: detail increment failed'` for the
   `err` field (Debug level; raise the unit's log level temporarily if
   needed).
3. **How much billing exposure?**
   `sum(rate(stellarindex_usage_units_dropped_total{counter="billable"}[5m]))`
   is the request-units/sec currently going unmetered.

## Remediation

- **Redis down / degraded** → follow the Redis recovery path; the
  tracker self-heals on the first successful write.
- **Namespace / ACL drift** → confirm the API's Redis credential still
  has write access to the `usage:` prefix; this is the same credential
  `monthly_quota.go`'s reads use, so if only writes fail suspect a
  permission asymmetry (e.g. a read-replica ACL applied to the wrong
  connection).
- **After recovery**, the durable `usage_daily` rollup (fed from the
  same counters, on a 5-minute cadence) is a lower bound on what was
  actually served during the window — reconcile from it, not from this
  alert's rate, to estimate unmetered exposure.

## Do NOT

- **Do not treat this as the same incident as
  `stellarindex_monthly_quota_fail_open`** just because both are usage-
  counter alerts — confirm which one is actually firing before paging;
  they point at opposite ends of the same read/write pair.
- Do not silence because "metering is best-effort" — that principle
  covers a single request's fate, not a sustained, silent revenue leak.

## Related

- [monthly-quota-fail-open](monthly-quota-fail-open.md) — the read-side
  twin.
- [ratelimit-fail-open](ratelimit-fail-open.md) — same backing store,
  different counter family.
