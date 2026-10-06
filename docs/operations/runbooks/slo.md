---
title: Runbook — SLO burn-rate alerts
last_verified: 2026-10-06
status: living
severity: P1
---

# Runbook — SLO burn-rate alerts

Six alerts, two SLOs, three burn speeds each. Rules: `configs/prometheus/rules.r1/slo.yml` (r1 overlay, `job="stellarindex-api"`, loaded from `/etc/prometheus/rules.r1/*.yml`; multi-host template: `deploy/monitoring/rules/slo.yml`). Fast and medium carry `severity: page` (P1, Alertmanager `chat-page` receiver); slow carries `severity: ticket` (P3). Multi-window burn-rate detection per the Google SRE workbook ch. 5.

Each alert's `runbook_url` points at the route-level alert section in `api.md`, not at this file: the availability family at `api.md#stellarindex_api_error_rate_critical`, the latency family at `api.md#stellarindex_api_latency_p95_high`. A responder following the page link lands there first; this runbook is the family-specific supplement.

Budget arithmetic (both SLOs, 0.001 budget): at 14.4×, 5 % of the monthly budget goes every hour (whole budget gone in ~2 days); at 6×, 5 % every 6 h (~5 days); at 1× the budget lasts exactly 30 days, zero slack, so any further incident that month overspends it.

## At a glance

| Alert | Severity | Windows (both must exceed) | Trip point | `for` | MTTR |
| ----- | -------- | -------------------------- | ---------- | ----- | ---- |
| [`stellarindex_slo_availability_burn_fast`](#stellarindex_slo_availability_burn_fast) | P1 page | 5m + 1h, > 14.4× | 1.44 % 5xx | 2m | 15-30 min |
| [`stellarindex_slo_availability_burn_medium`](#stellarindex_slo_availability_burn_medium) | P1 page | 30m + 6h, > 6× | 0.6 % 5xx | 5m | 30-90 min |
| [`stellarindex_slo_availability_burn_slow`](#stellarindex_slo_availability_burn_slow) | P3 ticket | 6h + 24h, > 1× | 0.1 % 5xx | 30m | days |
| [`stellarindex_slo_latency_burn_fast`](#stellarindex_slo_latency_burn_fast) | P1 page | 5m + 1h, > 14.4× | 1.44 % slower than 200 ms | 2m | 15-30 min |
| [`stellarindex_slo_latency_burn_medium`](#stellarindex_slo_latency_burn_medium) | P1 page | 30m + 6h, > 6× | 0.6 % slower than 200 ms | 5m | 30-90 min |
| [`stellarindex_slo_latency_burn_slow`](#stellarindex_slo_latency_burn_slow) | P3 ticket | 6h + 24h, > 1× | 0.1 % slower than 200 ms | 30m | days |

Availability SLO (99.9 % non-5xx over 30 d, the published SLA target), recording-rule slo `api_availability_3_nines`, ratio `stellarindex:api_error_ratio:<window>`. Latency SLO: 99.9 % of `/v1/price`, `/v1/price/batch` + SEP-40 oracle requests under 200 ms over 30 d (ADR-0009), slo `api_latency_p95_under_200ms`, slow fraction `1 - stellarindex:api_slow_request_ratio:<window>`.

Guards differ: the availability rules have **no min-traffic guard**, so on quiet r1 a handful of synthetic 5xx can dominate the ratio; check the traffic floor first. The latency rules carry a **min-signal guard**, `stellarindex:api_slow_request_count:1h > 5`: an absolute count of bad (slow-or-error) requests over the SLO-scoped routes in the trailing hour, not a floor on total traffic. (RLT-329, #739) The guard used to compare TOTAL request rate against a 5 req/s floor sized off a ~2.4 req/s synthetic-monitoring baseline; once the smoke/prewarm/probe User-Agent filter excluded that traffic from the histogram, real pre-launch traffic never cleared 5 req/s and every latency burn alert sat permanently disarmed regardless of how badly the SLO was burning. The count-based guard fires on a handful of genuinely bad real requests no matter how few total requests came with them, while a single cold-cache outlier in a near-empty window still can't trip it.

Shared false positives (availability family; each alert section says which apply):

- **Synthetic probes at low traffic**: `stellarindex-sla-probe.timer` and `stellarindex-smoke.timer` (`configs/healthchecks/`) plus cache prewarm hit the local API. With no min-traffic guard, at < ~5 req/s real traffic a few probe 5xx can exceed the trip point. Check the total request rate before mitigating.
- **Weekly k6 load test**: not a candidate. `k6-weekly.yml` lost its `schedule:` trigger on 2026-09-15 (dispatch-only) and targets **staging only** even on dispatch, so it cannot trip any of these alerts on r1. The Sunday 02:00 UTC slot is now `sla-proof-weekly.yml`, which only READS Prometheus (over an ssh port-forward) and drives no load against anything. Don't attribute an r1 burn to k6.

## stellarindex_slo_availability_burn_fast

P1 page. Trips when `stellarindex:api_error_ratio:5m` AND `stellarindex:api_error_ratio:1h` (slo `api_availability_3_nines`) both exceed 14.4× the budget (14.4 × 0.001 = **1.44 %** 5xx), sustained `for: 2m`. Impact: consumers see request failures now.

No min-traffic guard (see above), so it CAN fire on quiet r1. `stellarindex_api_error_rate_high` (P3, > 1 %) will normally have ticketed just before this fires, and `stellarindex_api_error_rate_critical` (P1, > 5 %) may fire alongside; a fast burn trips at 1.44 %, between the two direct-threshold lines.

Quick diagnosis (<= 5 min):

```sh
# Traffic floor + current ratio (on r1, Prometheus is local). At ≲ 5 req/s
# total, the ratio is synthetic-dominated — see false positives.
curl -s 'http://127.0.0.1:9090/api/v1/query?query=stellarindex:api_error_ratio:5m' | jq .data.result
curl -s 'http://127.0.0.1:9090/api/v1/query?query=sum(rate(http_requests_total{job="stellarindex-api"}[5m]))' | jq .data.result

# Which routes are 5xx-ing
journalctl -u stellarindex-api --since '5 min ago' --no-pager -o cat \
  | jq -r 'select(.status >= 500) | .path' | sort | uniq -c | sort -rn | head -10

# Sample some errors. The access log has NO .err field (attrs:
# method,path,status,bytes,latency_ms,request_id,remote_ip,user_agent);
# ERROR-level lines with the underlying error are separate log lines.
journalctl -u stellarindex-api --since '5 min ago' --no-pager -o cat \
  | jq -r 'select(.status >= 500) | [.path, .request_id, .status] | @tsv' | head -10

# Is upstream the issue?
systemctl status stellarindex-aggregator stellarindex-indexer postgresql@15-main redis-server caddy --no-pager | head -40
```

Key signals:

- **`/v1/price` 5xx**: upstream Postgres or Redis failed; jump to `postgres.md#stellarindex_timescale_primary_down` or `cache.md`.
- **All routes 5xx**: the API process itself is sick; check OOM (`dmesg | grep -i kill`) and the runtime gauges the API already exports (there is no pprof endpoint in the binary): `curl -s http://localhost:3000/metrics | grep -E '^go_goroutines|^go_memstats_heap_inuse'`.
- **Recent deploy**: roll back via `gh workflow run deploy.yml -f region=r1 -f version=<previous-tag> -f binaries=stellarindex-api`.

Mitigation (<= 15 min):

- [ ] Step 1: if a recent deploy correlates with the burn onset, roll back. Per `deploy-workflow.md`'s "automatic rollback on health-probe failure" semantics this should already have happened; if it didn't, investigate the deploy's health-probe path.
- [ ] Step 2: if an upstream resource is saturated, jump to the appropriate runbook.
- [ ] Step 3: if the API process needs a kick: `systemctl restart stellarindex-api` (the unit has `Restart=on-failure`; manual restart is the same effect).
- [ ] Verification: `stellarindex:api_error_ratio:5m` back below 1.44 % and holding. The alert itself only resolves once the **1h window drains** below 1.44 % too, which lags the fix; watch the 5m window for confirmation the bleeding stopped.

Root cause (for postmortem): the full request log over the burn window filtered to 5xx; `go_goroutines` / `go_memstats_*` trends from Prometheus over the window (no pprof exists in the binaries; the runtime gauges on `:3000/metrics` are the in-process signal); kernel `dmesg` over the window (OOM-kill markers).

False positives: synthetic probes at low traffic (above; check the total request rate from the first block before mitigating). **Brief upstream blips**: Cloudflare -> R1 has periodic single-region network interruptions; if the burn was < 60 s and recovered without intervention, it's the network, not us. The `for: 2m` window catches most cases. Caddy-generated 502/503 when the API is unreachable are not in `http_requests_total` and don't count against this SLO; `api.md#stellarindex_api_down` covers that case. k6 is not a candidate (above).

## stellarindex_slo_availability_burn_medium

P1 page. Trips when `stellarindex:api_error_ratio:30m` AND `stellarindex:api_error_ratio:6h` (slo `api_availability_3_nines`) both exceed 6× the budget (6 × 0.001 = 0.6 % 5xx), sustained `for: 5m`. Impact: some customers see 5xx; act before the fast-burn alert fires. No min-traffic guard.

Same triage tree as the fast burn (above); the difference is urgency. At medium burn rate you have time to:

1. **Check the traffic floor first.** At near-zero real traffic a handful of synthetic 5xx can exceed 0.6 %:

   ```sh
   # Current error ratio and total request rate (on r1, Prometheus is local)
   curl -s 'http://127.0.0.1:9090/api/v1/query?query=stellarindex:api_error_ratio:30m' | jq .data.result
   curl -s 'http://127.0.0.1:9090/api/v1/query?query=sum(rate(http_requests_total{job="stellarindex-api"}[30m]))' | jq .data.result
   # ≲ 5 req/s total → the ratio is synthetic-dominated; see false positives.
   ```

2. **Attribute the 5xx to routes**, from Prometheus (route pattern, not raw path):

   ```sh
   curl -s 'http://127.0.0.1:9090/api/v1/query?query=sum by (route,status)(rate(http_requests_total{job="stellarindex-api",status=~"5.."}[30m]))' | jq .data.result
   ```

   and from the API access log. The log is JSON (`log_format` defaults to `json`); the access-log attrs are `method,path,status,bytes,latency_ms,request_id,remote_ip,user_agent` (there is no `err` field; 429s are not logged at all):

   ```sh
   journalctl -u stellarindex-api --since '30 min ago' --no-pager -o cat \
     | jq -r 'select(.status >= 500) | [.status, .path, .request_id, .user_agent] | @tsv' \
     | sort | uniq -c | sort -rn | head -20
   ```

3. Coordinate in Discord `#stellarindex-pages` (the Alertmanager `chat-page` receiver, `configs/alertmanager/alertmanager.r1.yml`). Paging was documented as NOT wired on 2026-07-27 (`sev-playbook.md` section 3); if no page arrived, follow `wire-paging.md`.
4. Apply a forward-fix if a recent deploy is the obvious cause.

If the 5m/1h windows ALSO cross 14.4×, `stellarindex_slo_availability_burn_fast` fires (`for: 2m`, also `severity: page`); switch to the fast-burn section. Caveat: 502/503 generated by Caddy when the API is unreachable are not in `http_requests_total` and are not counted by this SLO; `api.md#stellarindex_api_down` covers that case.

False positives: synthetic probes at low traffic (above; the 0.6 % trip point; step 1 is the check). **Customer behaviour**: a single misbehaving authenticated client retrying a 4xx-able malformed request can occasionally shift to 5xx if the route's input validation has bugs; check the rate-limit headers (`X-RateLimit-Limit/Remaining/Reset`, `Retry-After`) on the failing requests. k6 cannot trip this on r1 (above).

## stellarindex_slo_availability_burn_slow

P3 ticket. Trips when `stellarindex:api_error_ratio:6h` AND `stellarindex:api_error_ratio:24h` (slo `api_availability_3_nines`) both exceed 1× the budget (1 × 0.001 = 0.1 % 5xx), sustained `for: 30m`. Impact: no acute customer impact, but a structural regression is in flight. The earliest signal in the availability family; treat it as a planning ticket, not an incident.

No min-traffic guard: on quiet r1 a trickle of synthetic 5xx can hold the ratio above 0.1 % indefinitely. Check the traffic floor before treating the trend as a real regression:

```sh
curl -s 'http://127.0.0.1:9090/api/v1/query?query=stellarindex:api_error_ratio:24h' | jq .data.result
curl -s 'http://127.0.0.1:9090/api/v1/query?query=sum(rate(http_requests_total{job="stellarindex-api"}[24h]))' | jq .data.result
```

Investigation:

1. Sample the 5xx pattern across the last 7 days: spread across all routes (systemic) or concentrated (single endpoint regression)?

   ```sh
   curl -s 'http://127.0.0.1:9090/api/v1/query?query=sum by (route,status)(rate(http_requests_total{job="stellarindex-api",status=~"5.."}[24h]))' | jq .data.result

   journalctl -u stellarindex-api --since '24 hours ago' --no-pager -o cat \
     | jq -r 'select(.status >= 500) | [.status, .path] | @tsv' | sort | uniq -c | sort -rn | head -20
   ```

2. Cross-reference with deploy history (`git log --grep 'release:' --since '14 days ago'`) to find the inflection point.
3. File a planning ticket with the trend graphs + dominant 5xx route + suspected commit range.

False positives: synthetic probes at low traffic (above; exceed 0.1 % at near-zero real traffic). **Long-tail external dependency failures**: if a small fraction of requests fail because an external poller (CoinGecko, ECB) is having structural availability issues, those don't necessarily count as our 5xx; check whether the failing path is `/v1/sources` or another aggregator-feeding surface.

## stellarindex_slo_latency_burn_fast

P1 page. Trips when the slow-request fraction (`1 - stellarindex:api_slow_request_ratio:{5m,1h}`, slo `api_latency_p95_under_200ms`) is above 14.4× the budget in both windows (14.4 × 0.001 = 1.44 % of requests slower than 200 ms), sustained `for: 2m`, behind the min-signal guard (above). Impact: customer-visible pricing-surface latency is degraded now.

The underlying `stellarindex_api_latency_p95_high` alert may fire alongside, but it is **all-routes** p95 > 500 ms with `for: 10m` and `severity: ticket`; a route-scoped fast burn can page long before (or without) it firing.

Quick diagnosis (<= 5 min):

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

- **Single slow route**: probably a code-path regression; check recent deploys (release tags in `git log`).
- **All routes slow**: upstream resource saturation (CPU / memory / postgres connections / Redis latency).
- **Specific user behaviour**: check rate-limit headers on the slow requests; a paid tier may be hammering one endpoint.

Mitigation (<= 15 min):

- [ ] Step 1: if the slowness coincides with a recent deploy, roll back via `gh workflow run deploy.yml -f region=r1 -f version=<previous-tag> -f binaries=stellarindex-api` (per `deploy-workflow.md`).
- [ ] Step 2: if no recent deploy and a single route is dominant, attribute it with the route-level Prometheus quantile above plus a `pg_stat_statements` snapshot for that route's queries. There is no pprof endpoint in any binary, so profiling is not an available step; the runtime gauges on `:3000/metrics` (`go_goroutines`, `go_memstats_*`) are the in-process signal.
- [ ] Step 3: if all routes slow and postgres connections are saturated, jump to `postgres.md#stellarindex_timescale_connections_saturated`.
- [ ] Step 4: if all routes slow and Redis latency is high, check Redis health (RDB BGSAVE blocked? memory saturated?); jump to the `cache.md` family.
- [ ] Verification: the 5m slow fraction back under 1.44 % (equivalently `histogram_quantile(0.95, ...)` on the SLO routes back < 0.20 s), sustained >= 5 min. The alert itself resolves only once the 1h window also drains.

Root cause (capture for postmortem): the exact 5-min window where the burn started + the dominant slow route's `pg_stat_statements` snapshot; recent deploy timestamps relative to the burn onset; per-route p95/p99 trend graph for the prior 24 h.

False positives:

- **First 5 min after deploy**: connection pool warm-up + cache cold-start can push p95 transiently. The `for: 2m` window catches this; if it persists past 5 min it's real.
- **Weekly k6 load test**: cannot trip this on r1 (above). To cross-reference a firing time anyway, check the workflow's run timestamps (`gh run list --workflow k6-weekly.yml`); there is no `k6_weekly_running` heartbeat metric. What `sla-proof-weekly.yml` CAN coincide with is a burn it did not cause but will report.
- **The SLA probe itself**: `stellarindex-sla-probe.timer` fires every 15 minutes and drives ~30 s of requests at concurrency 1 against `localhost:3000`. At low real traffic it is a measurable share of the request mix, which is a reason to check the total request rate before attributing a p95 move to a regression.

r1 currently runs at p95 = 246 ms structurally (F-1267, audit-2026-05-12); multi-region cutover is the long-term fix.

## stellarindex_slo_latency_burn_medium

P1 page (`severity: page` in both rule trees). Trips when the slow-request fraction (`1 - stellarindex:api_slow_request_ratio:{30m,6h}`, slo `api_latency_p95_under_200ms`) is above 6× the budget in both windows (6 × 0.001 = 0.6 % of requests slower than 200 ms), sustained `for: 5m`, behind the min-signal guard (above). Impact: customer-visible latency degraded but not catastrophic; act before this escalates to the fast-burn alert.

Same investigation tree as the latency fast burn; the difference is urgency, not cause. At medium burn rate you have time to:

1. Attribute the slowness before mitigating: the route-level Prometheus quantile (`sum by (le,route)`; see the fast-burn curl above) plus a `pg_stat_statements` snapshot (`runuser -u postgres -- psql -d stellarindex`). There is no pprof endpoint in any binary; the runtime gauges on `:3000/metrics` (`go_goroutines`, `go_memstats_*`) are the in-process signal.
2. Coordinate in Discord `#stellarindex-pages` (the Alertmanager `chat-page` receiver, `configs/alertmanager/alertmanager.r1.yml`) before rolling back.
3. Apply a forward-fix if a recent deploy is the obvious cause.

If the burn rate accelerates and the **fast** alert (5m + 1h at 14.4×, `for: 2m`) fires, escalate immediately. Root cause: as the fast burn (p95 trend graphs, recent deploy timestamps, `pg_stat_statements` snapshots). False positives: k6 cannot trip this on r1 (above); `sla-proof-weekly.yml` drives no load.

## stellarindex_slo_latency_burn_slow

P3 ticket. Trips when the slow-request fraction (`1 - stellarindex:api_slow_request_ratio:{6h,24h}`, slo `api_latency_p95_under_200ms`) is above 1× the budget in both windows (1 × 0.001 = 0.1 % of requests slower than 200 ms), sustained `for: 30m`, behind the min-signal guard (above). Impact: no acute customer impact, but a structural regression is in flight; any additional latency incident this month overspends the SLO.

The earliest signal in the burn-rate family. Treat as a planning ticket, not an incident:

1. Identify the trend: linear (gradual scale problem) or stepped (recent change)?
2. Sample the slow paths from `pg_stat_statements` over the prior 7 days (`runuser -u postgres -- psql -d stellarindex`).
3. Cross-reference with the deploy history (`git log --grep 'release:' --since '14 days ago'`) to find the inflection point.
4. File a planning ticket with the trend graphs + dominant slow path + suspected commit range.

Mitigation is usually code-side: refactor the slow path or add a cache layer. Coordinate with the team before any deploy.

False positive: **steady traffic growth**. As customer adoption grows, baseline p95 drifts up linearly. If the trend is correlated with `rate(http_requests_total[7d])` growth, the response is "scale" (more replicas, R2/R3 cutover, paid-tier carve-out), not "fix this code path". r1 currently runs at p95 = 246 ms structurally (F-1267, audit-2026-05-12).

## Related

- `api.md#stellarindex_api_error_rate_critical` / `api.md#stellarindex_api_latency_p95_high` (the `runbook_url` targets) and `api.md#stellarindex_api_down`; for the latency family also `api.md#stellarindex_api_cache_miss_rate_high` and `postgres.md#stellarindex_timescale_connections_saturated` as common upstream causes. `api.md#stellarindex_api_error_rate_critical` still uses HA hostnames (`api-01`, `api-XX`); on r1 run its commands locally.
- Escalation chain for both families: [burn_slow](#stellarindex_slo_availability_burn_slow) (ticket) -> [burn_medium](#stellarindex_slo_availability_burn_medium) (30m + 6h at 6x, page) -> [burn_fast](#stellarindex_slo_availability_burn_fast) (P1); latency: [slow](#stellarindex_slo_latency_burn_slow) -> [medium](#stellarindex_slo_latency_burn_medium) -> [fast](#stellarindex_slo_latency_burn_fast).
- `wire-paging.md`: confirm the `chat-page` receiver actually reaches a human.
- ADR-0008: HA topology + availability target (multi-region decision amended by ADR-0050 / `docs/architecture/ha-plan.md`). ADR-0009: API latency budget allocation (separate from the availability budget).
