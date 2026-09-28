---
title: Runbook — after-response-tasks-dropped
last_verified: 2026-09-28
status: draft
severity: P3
---

# Runbook — `stellarindex_after_response_tasks_dropping`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_after_response_tasks_dropping` (P3 / ticket) |
| Detected by | Prometheus rule in `deploy/monitoring/rules/api.yml` and the R1 single-host overlay `configs/prometheus/rules.r1/api.yml`. |
| Typical MTTR | 5–30 min: clears once the shared after-response pool stops saturating. |
| Impact | Lost usage-counter increments and/or `TouchUsage` last-seen updates for the dropped requests. Metering and last-seen are both best-effort — the client response is never affected — but a sustained rate is silent revenue/observability leak. |

## What this fires on

`internal/api/v1/middleware/after_response.go`'s `AfterResponse` (GH-627)
hands `UsageTracker`'s counter writes and `TouchUsage`'s debounced touch
to a shared, bounded worker pool instead of running them inline on the
request goroutine — that fix is what took the two seams' Redis/Postgres
bookkeeping off the client's critical path. If the pool's queue is full
when a task is submitted, the task is dropped (never blocks the request)
and `stellarindex_after_response_tasks_dropped_total` increments.

## Quick diagnosis (≤ 5 min)

1. **Is the drop rate correlated with a Redis/Postgres slowdown?**
   Check `stellarindex_usage_units_dropped_total` and Redis/Postgres
   latency — a wedged or slow store means pool workers stay busy longer
   per task, so the fixed-size pool backs up under ordinary load.
2. **Is request volume elevated?** A traffic spike alone can saturate
   the pool even with a healthy store.

## Remediation

- **Downstream store slow/wedged** → follow the Redis/Postgres recovery
  path; the pool self-heals once tasks drain faster than they arrive.
- **Sustained high volume** → the pool size
  (`afterResponseWorkers`/`afterResponseQueueSize` in `after_response.go`)
  may need raising; this is a capacity decision, not an incident fix.

## Do NOT

- Do not treat a brief spike as an incident — `for: 10m` already filters
  transient blips.

## Related

- [usage-write-failing](usage-write-failing.md) — the store-write-failure
  signal for the same counters.
