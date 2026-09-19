---
title: Runbook — slo-latency-burn-fast
last_verified: 2026-09-19
status: current
severity: P1
---

# Runbook — `stellarindex_slo_latency_burn_fast`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_slo_latency_burn_fast` |
| Severity | **P1** (page — rule label `severity: page`, routed to the Alertmanager `chat-page` receiver) |
| Detected by | `configs/prometheus/rules.r1/slo.yml` (r1 overlay, `job="stellarindex-api"`, loaded from `/etc/prometheus/rules.r1/*.yml`; multi-host template: `deploy/monitoring/rules/slo.yml`) |
| Typical MTTR | 15–30 min |
| Impact | Latency budget (99.9 % of `/v1/price`, `/v1/price/batch` + SEP-40 oracle requests under 200 ms over 30 d, per ADR-0009) burning at > 14.4× rate. At this rate 5 % of the monthly budget is consumed every hour; the whole 30-day budget is gone in ~2 days. Customer-visible pricing-surface latency is degraded now. |

## Symptoms

Multi-window burn-rate detection: the slow-request fraction
(`1 - stellarindex:api_slow_request_ratio:{5m,1h}`, slo
`api_latency_p95_under_200ms`) both **> 14.4×** the budget (14.4 × 0.001 =
1.44 % of requests slower than 200 ms), sustained `for: 2m`.

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

The underlying `stellarindex_api_latency_p95_high` alert may fire alongside,
but note its shape: it is **all-routes** p95 > 500 ms with `for: 10m` and
`severity: ticket` — a route-scoped fast burn can page long before (or
without) it firing.

## Quick diagnosis (≤ 5 min)

```sh
# Identify the slow route(s). The histogram has a `route` label, not `path`.
curl -s 'http://127.0.0.1:9090/api/v1/query' \
  --data-urlencode 'query=histogram_quantile(0.95, sum by (le,route) (rate(http_request_duration_seconds_bucket{job="stellarindex-api"}[5m])))' \
  | jq '.data.result[] | {route: .metric.route, p95: .value[1]}'

# Top-N slow requests in the last 5 min from the API access log
journalctl -u stellarindex-api --since '5 min ago' --no-pager -o cat \
  | jq -r 'select(.latency_ms > 200) | [.path, .latency_ms] | @tsv' | sort -k2 -n -r | head -20

# Is it a database query slowing us down?
runuser -u postgres -- psql -d stellarindex -c "SELECT query, calls, mean_exec_time, max_exec_time FROM pg_stat_statements ORDER BY max_exec_time DESC LIMIT 10;"
```

Key signals:
- **Single slow route** → probably a code-path regression; check recent deploys (release tags in `git log`).
- **All routes slow** → upstream resource saturation (CPU / memory / postgres connections / Redis latency).
- **Specific user behaviour** → check rate-limit headers on the slow requests; a paid tier may be hammering one endpoint.

## Mitigation (≤ 15 min)

- [ ] Step 1 — if the slowness coincides with a recent deploy: roll back via `gh workflow run deploy.yml -f region=r1 -f version=<previous-tag> -f binaries=stellarindex-api` (per `deploy-workflow.md`).
- [ ] Step 2 — if no recent deploy and a single route is dominant: attribute
  it with the route-level Prometheus quantile above plus a
  `pg_stat_statements` snapshot for that route's queries. (There is no pprof
  endpoint in any binary — profiling is not an available step; the runtime
  gauges on `:3000/metrics` — `go_goroutines`, `go_memstats_*` — are the
  in-process signal.)
- [ ] Step 3 — if all routes slow + postgres connections saturated → jump to `pg-conns-saturated.md`.
- [ ] Step 4 — if all routes slow + Redis latency high → check Redis health (RDB BGSAVE blocked? memory saturated?); jump to `redis-master-down.md` family.
- [ ] Verification: the 5m slow fraction back under the trip point (1.44 %) —
  equivalently `histogram_quantile(0.95, ...)` on the SLO routes back
  < 0.20 s — sustained ≥ 5 min. The alert itself resolves only once the 1h
  window also drains.

## Root cause analysis

Capture for postmortem:
- The exact 5-min window where the burn started + the dominant slow route's `pg_stat_statements` snapshot.
- Recent deploy timestamps relative to the burn onset.
- Per-route p95/p99 trend graph for the prior 24 h.

## Known false-positive patterns

- **First 5 min after deploy** — connection pool warm-up + cache cold-start can push p95 transiently. The `for: 2m` window catches this; if it persists past 5 min it's real.
- **Weekly k6 load test** — `k6-weekly.yml` lost its `schedule:` trigger on
  2026-09-15 and is dispatch-only, and it targets **staging only** even then,
  so it cannot trip this alert on r1. To cross-reference a firing time anyway,
  check the workflow's run timestamps (`gh run list --workflow k6-weekly.yml`)
  — there is no `k6_weekly_running` heartbeat metric. The Sunday 02:00 UTC slot
  is now `sla-proof-weekly.yml`, which reads Prometheus and drives no load;
  what it CAN coincide with is a burn it did not cause but will report.
- **The SLA probe itself** — `stellarindex-sla-probe.timer` fires every 15
  minutes and drives ~30 s of requests at concurrency 1 against
  `localhost:3000`. At low real traffic it is a measurable share of the
  request mix, which is a reason to check the total request rate before
  attributing a p95 move to a regression.

## Related

- `slo-latency-burn-medium.md` — same family at the slower burn rate (also `severity: page`).
- `slo-latency-burn-slow.md` — same family at the slowest burn rate.
- `api-latency.md` (the alert's `runbook_url` target) — the route-level p95/p99 alerts.
- `pg-conns-saturated.md`, `cache-miss-rate-high.md` — common upstream causes.
- `wire-paging.md` — confirm the `chat-page` receiver actually reaches a human.
- ADR-0009 — API latency budget allocation.
- Note: r1 currently runs at p95 = 246 ms structurally; multi-region cutover is the long-term fix.

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
- 2026-08-29 — re-verified against HEAD: rule path → r1 overlay primary;
  budget arithmetic (5 %/hour, whole budget ≈ 2 days — not "gone in ~1
  hour"); min-traffic guard (`> 5` req/s — deliberately cannot fire on quiet
  r1) documented; `runbook_url` → api-latency.md note; the "underlying p95
  alert fires alongside" claim corrected (that alert is all-routes, `for:
  10m`, `severity: ticket`); PromQL `sum by (le,path)` → `sum by (le,route)`
  (the histogram has no `path` label) + r1 Prometheus curl shape; psql →
  runuser shape; pprof profile step dropped (no pprof endpoint exists) →
  route-level Prometheus + pg_stat_statements; `k6_weekly_running` heartbeat
  replaced with the workflow's run timestamps (scheduled cron 02:00 UTC
  Sunday, staging-only); `-o cat` on journalctl pipelines.
- 2026-05-12 — initial draft (audit-2026-05-12 F-1237 closure).
