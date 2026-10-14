---
title: Runbook — SLO burn-rate alerts
last_verified: 2026-10-06
status: living
severity: P1
---

# Runbook — SLO burn-rate alerts

Six alerts, two SLOs, three burn speeds. Rules: `configs/prometheus/rules.r1/slo.yml` (`job="stellarindex-api"`; multi-host template `deploy/monitoring/rules/slo.yml`). Fast and medium are `severity: page` (P1, `chat-page`), slow is `ticket` (P3). Each alert's `runbook_url` points at `api.md#stellarindex_api_error_rate_critical` (availability) or `api.md#stellarindex_api_latency_p95_high` (latency); this file is the family supplement.

| Alert | Windows (both exceed) | Trip point | `for` |
| ----- | --------------------- | ---------- | ----- |
| `..._availability_burn_fast` / `_latency_burn_fast` | 5m + 1h, > 14.4x | 1.44 % 5xx / slower than 200 ms | 2m |
| `..._burn_medium` | 30m + 6h, > 6x | 0.6 % | 5m |
| `..._burn_slow` | 6h + 24h, > 1x | 0.1 % | 30m |

Availability SLO (99.9 % non-5xx over 30 d, the published SLA target), recording-rule slo `api_availability_3_nines`, ratio `stellarindex:api_error_ratio:<window>`. Latency SLO: 99.9 % of `/v1/price`, `/v1/price/batch` and SEP-40 oracle requests under 200 ms (ADR-0009), slo `api_latency_p95_under_200ms`, slow fraction `1 - stellarindex:api_slow_request_ratio:<window>`.

Guards: availability rules have NO min-traffic guard, so on quiet r1 a few synthetic 5xx can dominate; check the traffic floor first. Latency rules require `stellarindex:api_slow_request_count:1h > 5` (absolute count of bad requests in the last hour).

Shared false positives: synthetic probes at low traffic (`stellarindex-sla-probe.timer`, `stellarindex-smoke.timer`, cache prewarm; below ~5 req/s a few probe 5xx can exceed the trip point). The weekly k6 load test cannot trip these on r1 (staging only, dispatch-only); `sla-proof-weekly.yml` only reads Prometheus.

## At a glance

SLO burn-rate alerts (`stellarindex_slo_*`); page severity per section, see [sev-playbook](../sev-playbook.md).

## stellarindex_slo_availability_burn_fast

**Symptom.** `stellarindex:api_error_ratio:5m` AND `:1h` (slo `api_availability_3_nines`) both exceed 14.4× the budget (14.4 × 0.001 = **1.44 %** 5xx) for 2m (page). Consumers see failures now. `stellarindex_api_error_rate_high` (> 1 %) and `_critical` (> 5 %) may fire alongside.

**Check.**

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

**Likely causes.**

- `/v1/price` 5xx: Postgres or Redis failed (`postgres.md#stellarindex_timescale_primary_down`, `cache.md`).
- All routes 5xx: API process sick; `dmesg | grep -i kill` and `curl -s http://localhost:3000/metrics | grep -E '^go_goroutines|^go_memstats_heap_inuse'` (no pprof endpoint exists).
- A recent deploy.
- Brief Cloudflare to R1 network blips under 60 s. Caddy-generated 502/503 are not in `http_requests_total`; see `api.md#stellarindex_api_down`.

**Fix.**

1. Deploy correlates with onset: `gh workflow run deploy.yml -f region=r1 -f version=<previous-tag> -f binaries=stellarindex-api` (auto-rollback should already have run; see `deploy-workflow.md`).
2. Upstream saturated: go to that runbook.
3. Process wedged: `systemctl restart stellarindex-api`.
4. Verify the 5m ratio holds under 1.44 %; the alert resolves only after the 1h window drains.

**Escalate:** SEV-1 per [sev-playbook](../sev-playbook.md).

## stellarindex_slo_availability_burn_medium

**Symptom.** `stellarindex:api_error_ratio:30m` AND `:6h` (slo `api_availability_3_nines`) both exceed 6× the budget (6 × 0.001 = 0.6 % 5xx) for 5m (page). Same triage as the fast burn with more time.

**Check.**

```sh
# Current error ratio and total request rate (on r1, Prometheus is local)
curl -s 'http://127.0.0.1:9090/api/v1/query?query=stellarindex:api_error_ratio:30m' | jq .data.result
curl -s 'http://127.0.0.1:9090/api/v1/query?query=sum(rate(http_requests_total{job="stellarindex-api"}[30m]))' | jq .data.result
# ≲ 5 req/s total → the ratio is synthetic-dominated; see false positives.

curl -s 'http://127.0.0.1:9090/api/v1/query?query=sum by (route,status)(rate(http_requests_total{job="stellarindex-api",status=~"5.."}[30m]))' | jq .data.result

journalctl -u stellarindex-api --since '30 min ago' --no-pager -o cat \
  | jq -r 'select(.status >= 500) | [.status, .path, .request_id, .user_agent] | @tsv' \
  | sort | uniq -c | sort -rn | head -20
```

**Likely causes.** As the fast burn; also a misbehaving authenticated client hitting a validation bug (check `X-RateLimit-Limit/Remaining/Reset`, `Retry-After`).

**Fix.**

1. Check the traffic floor first.
2. Attribute 5xx to routes with the queries above.
3. Coordinate in Discord `#stellarindex-pages`; if no page arrived, follow [wire-paging](wire-paging.md).
4. Forward-fix or roll back a recent deploy.

**Escalate:** if the 5m/1h windows also cross 14.4x, [burn_fast](#stellarindex_slo_availability_burn_fast) fires.

## stellarindex_slo_availability_burn_slow

**Symptom.** `stellarindex:api_error_ratio:6h` AND `:24h` (slo `api_availability_3_nines`) both exceed 1× the budget (1 × 0.001 = 0.1 % 5xx) for 30m (ticket). No acute impact; a structural regression is in flight. Treat as a planning ticket.

**Check.**

```sh
curl -s 'http://127.0.0.1:9090/api/v1/query?query=stellarindex:api_error_ratio:24h' | jq .data.result
curl -s 'http://127.0.0.1:9090/api/v1/query?query=sum(rate(http_requests_total{job="stellarindex-api"}[24h]))' | jq .data.result

curl -s 'http://127.0.0.1:9090/api/v1/query?query=sum by (route,status)(rate(http_requests_total{job="stellarindex-api",status=~"5.."}[24h]))' | jq .data.result

journalctl -u stellarindex-api --since '24 hours ago' --no-pager -o cat \
  | jq -r 'select(.status >= 500) | [.status, .path] | @tsv' | sort | uniq -c | sort -rn | head -20
```

**Likely causes.** Synthetic probes at near-zero traffic; a single-endpoint regression; failures of an aggregator-feeding surface such as `/v1/sources`.

**Fix.**

1. Spread across routes (systemic) or concentrated (one endpoint)?
2. Find the inflection: `git log --grep 'release:' --since '14 days ago'`.
3. File a planning ticket with trend graphs, dominant 5xx route and suspected commit range.

**Escalate:** none (ticket).

## stellarindex_slo_latency_burn_fast

**Symptom.** `1 - stellarindex:api_slow_request_ratio:{5m,1h}` above 1.44 % in both windows for 2m, behind the min-signal guard (page). Pricing-surface latency is degraded now. `stellarindex_api_latency_p95_high` (all-routes p95 > 500 ms, `for: 10m`) may lag it.

**Check.**

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

**Likely causes.**

- One slow route: code-path regression after a deploy.
- All routes slow: CPU, memory, Postgres connections or Redis latency.
- First 5 min after a deploy (pool warm-up); the SLA probe (`stellarindex-sla-probe.timer`, 15 min, ~30 s at concurrency 1) at low traffic.
- r1 runs at p95 = 246 ms structurally (F-1267); multi-region cutover is the long-term fix.

**Fix.**

1. Deploy correlates: roll back with `gh workflow run deploy.yml -f region=r1 -f version=<previous-tag> -f binaries=stellarindex-api`.
2. One route dominant: attribute with the quantile above plus a `pg_stat_statements` snapshot (no pprof; `go_goroutines`, `go_memstats_*` on `:3000/metrics`).
3. Postgres connections saturated: `postgres.md#stellarindex_timescale_connections_saturated`.
4. Redis slow (BGSAVE blocked, memory): the `cache.md` family.
5. Verify the 5m slow fraction under 1.44 % for 5 min; the alert resolves after the 1h window drains.

**Escalate:** SEV per [sev-playbook](../sev-playbook.md).

## stellarindex_slo_latency_burn_medium

**Symptom.** `1 - stellarindex:api_slow_request_ratio:{30m,6h}` above 0.6 % for 5m, behind the min-signal guard (page). Same tree as the fast burn with more time.

**Fix.**

1. Attribute first: route-level quantile (fast-burn curl) plus `pg_stat_statements` (`runuser -u postgres -- psql -d stellarindex`).
2. Coordinate in Discord `#stellarindex-pages` before rolling back.
3. Forward-fix if a recent deploy is the cause.

**Escalate:** if the fast alert fires, escalate immediately.

## stellarindex_slo_latency_burn_slow

**Symptom.** `1 - stellarindex:api_slow_request_ratio:{6h,24h}` above 0.1 % for 30m, behind the min-signal guard (ticket). Planning ticket, not an incident; any further latency incident this month overspends the SLO.

**Fix.**

1. Linear (scale) or stepped (recent change)?
2. Sample slow paths from `pg_stat_statements` over 7 days (`runuser -u postgres -- psql -d stellarindex`).
3. Find the inflection: `git log --grep 'release:' --since '14 days ago'`.
4. File a ticket with trend graphs, dominant slow path and suspected commit range. Steady traffic growth (correlated with `rate(http_requests_total[7d])`) means scale, not a code fix.

**Escalate:** none (ticket).

## Related

- `api.md#stellarindex_api_error_rate_critical`, `api.md#stellarindex_api_latency_p95_high`, `api.md#stellarindex_api_down`; latency upstream causes: `api.md#stellarindex_api_cache_miss_rate_high`, `postgres.md#stellarindex_timescale_connections_saturated`.
- [wire-paging](wire-paging.md): confirm `chat-page` reaches a human.
- ADR-0008 / `docs/architecture/ha-plan.md` (HA and availability target); ADR-0009 (latency budget).
