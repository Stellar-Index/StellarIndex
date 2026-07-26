---
title: Runbook — monthly-quota-fail-open
last_verified: 2026-07-26
status: draft
severity: P3
---

# Runbook — `stellarindex_monthly_quota_fail_open`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_monthly_quota_fail_open` (P3 / ticket) |
| Detected by | Prometheus rule in `deploy/monitoring/rules/api.yml` and the R1 single-host overlay `configs/prometheus/rules.r1/api.yml`. |
| Typical MTTR | 5–30 min: clears on its own the moment the usage counter is readable again; the fix is whatever made it unreadable. |
| Impact | **Revenue.** Every metered key is uncapped for the duration. Usage still meters (the customer is still billed) but the agreed monthly ceiling is not enforced, and overage served in this window cannot be reclaimed — the responses went out. |

## What this fires on

`internal/api/v1/middleware/monthly_quota.go` deliberately **fails open**:
if the month-to-date counter read errors, the request is served rather
than 500'd. That is the right trade — the cap is a billing-fairness
mechanism, not a security boundary, and a Redis blip must not 429 paying
customers — but it means the spend ceiling silently switches **itself**
off.

Before C3-082 (audit-2026-07-23) the only trace was a `logger.Debug`
line, which is below the API's production log level: in practice, no
trace at all. The counter `stellarindex_monthly_quota_fail_open_total`
now increments once per bypassed request, and this rule is its consumer.

The rule fires on `rate(...[5m]) > 0` sustained `for: 10m`, so:

- a one-off blip during a Redis failover does **not** ticket;
- "the cap has been unenforced continuously for ten minutes" always does.

This is the exact twin of
[ratelimit-fail-open](ratelimit-fail-open.md), and the two normally move
together because they share a backing store. The difference is what the
open window costs: the rate limiter's is throughput/abuse headroom, this
one's is money.

## Quick diagnosis (≤ 5 min)

1. **Is Redis up?** `systemctl status redis` on r1, then
   `redis-cli -a "$REDIS_PASSWORD" ping`. Check the API's `/readyz` —
   the redis checker should already be red if the store is unreachable.
2. **Is `stellarindex_ratelimit_fail_open` firing too?** If yes, this is
   a plain Redis availability incident; treat that runbook as primary and
   this alert as a consequence.
3. **Firing ALONE (Redis healthy)?** Then the failure is specific to the
   usage-counter read path, not to Redis availability. In order of
   likelihood:
   - **Key namespace / eviction**: the month-to-date counters live under
     the `UsageKeyForSubject` namespace. A `maxmemory-policy` of
     `allkeys-lru` evicting them mid-month produces read errors on this
     path while the limiter's own keys survive.
   - **AUTH drift**: password rotated on one side only —
     `journalctl -u stellarindex-api | grep -i 'monthly-quota'` shows the
     `NOAUTH` / `WRONGPASS` cause in the WARN line's `err` field.
   - **Connection-pool exhaustion** under peak load
     (`context deadline exceeded` in the same line).
4. **How much money is exposed?**
   `sum(rate(stellarindex_monthly_quota_fail_open_total[5m]))` is the
   requests/sec currently bypassing the cap. Only keys with
   `monthly_quota > 0` reach this code at all, so this is already scoped
   to metered customers.

## Remediation

- **Redis down / degraded** → follow the Redis recovery path. The
  middleware self-heals on the first successful read and the alert clears
  within ~10 min of the last bypass.
- **AUTH drift** → re-sync the password into `/etc/stellarindex.toml`
  (`storage.redis_url`) and `systemctl restart stellarindex-api`.
- **Eviction** → confirm `maxmemory-policy`; the usage counters must not
  be in an evictable class. This is a config fix on Redis, not on the API.
- **After recovery**, decide whether any customer materially exceeded
  their cap during the window. The metered usage rows are intact (metering
  is a separate write path), so the overage is *measurable* even though it
  was not *prevented* — reconcile from the usage rollup, not from this
  counter.

## Do NOT

- **Do not "fix" this by failing closed.** Returning 429 on a counter
  read error converts a Redis blip into a hard denial for every metered
  customer, including ones nowhere near their cap. The fail-open is
  deliberate; this alert exists so the window is *observed*, not so the
  behaviour is removed.
- Do not silence while Redis is down "because we know" — the 10-minute
  `for:` already absorbs failovers, so a firing instance means a genuinely
  sustained uncapped window.

## Related

- [ratelimit-fail-open](ratelimit-fail-open.md) — the abuse-side twin,
  same backing store, same fail-open shape.
- [admin-audit-write-failing](admin-audit-write-failing.md) — the other
  best-effort path whose silence the same audit wave closed.
- [metrics-registry-absent](metrics-registry-absent.md) — the sibling
  class: a metric whose *producer* is missing rather than its consumer.
