---
title: Runbook — slo-latency-burn-medium
last_verified: 2026-09-19
status: current
severity: P1
---

# Runbook — `stellarindex_slo_latency_burn_medium`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_slo_latency_burn_medium` |
| Severity | **P1** (page — rule label `severity: page` in both rule trees, routed to the Alertmanager `chat-page` receiver) |
| Detected by | `configs/prometheus/rules.r1/slo.yml` (r1 overlay, `job="stellarindex-api"`, loaded from `/etc/prometheus/rules.r1/*.yml`; multi-host template: `deploy/monitoring/rules/slo.yml`) |
| Typical MTTR | 30–90 min |
| Impact | Latency budget (99.9 % of `/v1/price`, `/v1/price/batch` + SEP-40 oracle requests under 200 ms over 30 d, per ADR-0009) burning at > 6× rate. At this rate 5 % of the monthly budget is consumed every 6 h; the whole 30-day budget is gone in ~5 days. Customer-visible latency is degraded but not catastrophic; act before this escalates to the fast-burn alert. |

## Symptoms

Multi-window detection: the slow-request fraction
(`1 - stellarindex:api_slow_request_ratio:{30m,6h}`, slo
`api_latency_p95_under_200ms`) both **> 6×** the budget (6 × 0.001 = 0.6 %
of requests slower than 200 ms), sustained `for: 5m`.

Note the alert's `runbook_url` annotation points at `api-latency.md`, not
this file — a responder following the page link lands there first; this
runbook is the family-specific supplement.

The latency burn rules carry a **min-signal guard**:
`stellarindex:api_slow_request_count:1h > 5` — an absolute count of bad
(slow-or-error) requests over the SLO-scoped routes in the trailing hour,
not a floor on total traffic. (RLT-329, #739): the guard used to compare
TOTAL request rate against a 5 req/s floor sized off a ~2.4 req/s
synthetic-monitoring baseline; once the smoke/prewarm/probe User-Agent
filter excluded that traffic from the histogram, real pre-launch traffic
never cleared 5 req/s and every latency burn alert sat permanently
disarmed regardless of how badly the SLO was burning. The count-based
guard fires on a handful of genuinely bad real requests no matter how few
total requests came with them, while a single cold-cache outlier in a
near-empty window still can't trip it.

## Quick diagnosis + Mitigation

Same investigation tree as `slo-latency-burn-fast.md` — the difference is urgency, not cause. At medium burn rate you have time to:

1. Attribute the slowness before mitigating: the route-level Prometheus
   quantile (`sum by (le,route)` — see the fast-burn runbook for the exact
   r1 curl) plus a `pg_stat_statements` snapshot
   (`runuser -u postgres -- psql -d stellarindex`). There is no pprof
   endpoint in any binary — the runtime gauges on `:3000/metrics`
   (`go_goroutines`, `go_memstats_*`) are the in-process signal.
2. Coordinate in Discord `#stellarindex-pages` (the Alertmanager `chat-page`
   receiver, `configs/alertmanager/alertmanager.r1.yml`) before rolling back.
3. Apply a forward-fix if a recent deploy is the obvious cause.

If the burn rate accelerates and the **fast** alert (5m + 1h windows at
14.4×, `for: 2m`) fires, escalate immediately.

## Root cause analysis

Same as fast-burn — capture p95 trend graphs, recent deploy timestamps, and `pg_stat_statements` snapshots.

## Known false-positive patterns

- **Weekly k6 load test** — `k6-weekly.yml` has no `schedule:` trigger since
  2026-09-15 and targets **staging only** even on dispatch, so it cannot trip
  this alert on r1. The Sunday 02:00 UTC slot now belongs to
  `sla-proof-weekly.yml`, which reads Prometheus over an ssh port-forward and
  drives no load against anything.

## Related

- `slo-latency-burn-fast.md` — escalation when the 5-min/1-hour windows also cross 14.4×.
- `slo-latency-burn-slow.md` — earliest signal, longer windows.
- `api-latency.md` (the alert's `runbook_url` target).
- `wire-paging.md` — confirm the `chat-page` receiver actually reaches a human.
- ADR-0009 — API latency budget allocation.

## Changelog

- 2026-09-19 — re-armed the min-signal guard (RLT-329, #739). The
  `> 5 req/s` TOTAL-traffic floor from 2026-07-02 was sized off a
  ~2.4 req/s synthetic-monitoring baseline; once the smoke/prewarm/probe
  User-Agent filter (2026-09-16) excluded that traffic from
  `http_request_duration_seconds` entirely, real pre-launch traffic never
  cleared 5 req/s and this alert was disarmed regardless of burn severity.
  Replaced with `stellarindex:api_slow_request_count:1h > 5`, an absolute
  count of bad (slow-or-error) requests, independent of total traffic
  volume. Dropped the old "if it fired, real traffic exceeds the floor"
  triage note — it no longer applies to a count-based guard.
- 2026-08-29 — re-verified against HEAD: **severity corrected P2/ticket →
  P1** (the rule label is `severity: page` in both rule trees, `for: 5m`);
  budget arithmetic (5 % per 6 h, whole budget ≈ 5 days — not "budget in
  ~6 hours"); rule path → r1 overlay primary; min-traffic guard
  (deliberately cannot fire on quiet r1) + `runbook_url` → api-latency.md
  notes; CPU-profile step replaced with route-level Prometheus +
  pg_stat_statements (no pprof endpoint exists); k6-weekly false positive
  corrected (scheduled, staging-only — not in-band r1 traffic).
- 2026-05-12 — initial draft (audit-2026-05-12 F-1237 closure).
