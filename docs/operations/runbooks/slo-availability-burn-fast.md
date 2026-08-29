---
title: Runbook — slo-availability-burn-fast
last_verified: 2026-08-29
status: current
severity: P1
---

# Runbook — `stellarindex_slo_availability_burn_fast`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_slo_availability_burn_fast` |
| Severity | **P1** (page — rule label `severity: page`, routed to the Alertmanager `chat-page` receiver) |
| Detected by | `configs/prometheus/rules.r1/slo.yml` (r1 overlay, `job="stellarindex-api"`, loaded from `/etc/prometheus/rules.r1/*.yml`; multi-host template: `deploy/monitoring/rules/slo.yml`) |
| Typical MTTR | 15–30 min |
| Impact | Availability budget (99.99 % non-5xx over 30 d — the SLA target) burning at > 14.4× rate. At this rate 5 % of the monthly budget is consumed every hour; the whole 30-day budget is gone in ~2 days. Consumers see request failures now. |

## Symptoms

Multi-window detection: `stellarindex:api_error_ratio:5m` AND
`stellarindex:api_error_ratio:1h` (slo `api_availability_3_nines_9`) both
**> 14.4×** the budget (14.4 × 0.0001 = **0.144 %** 5xx), sustained `for: 2m`.

Note the alert's `runbook_url` annotation points at `api-5xx.md`, not this
file — a responder following the page link lands there first; this runbook is
the family-specific supplement.

Unlike the latency burn family, the availability burn rules have **no
min-traffic guard**, so this alert CAN fire on quiet r1 where a handful of
synthetic 5xx dominate the ratio — check the traffic floor first (see false
positives).

`stellarindex_api_error_rate_high` (P3, > 1 %) and possibly
`stellarindex_api_error_rate_critical` (P1, > 5 %) may fire alongside — but a
fast burn at 0.144 % can page well before either direct-threshold alert trips.

## Quick diagnosis (≤ 5 min)

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
- **`/v1/price` 5xx → upstream Postgres or Redis failed**; jump to `timescale-primary-down.md` or `redis-master-down.md`.
- **All routes 5xx → API process itself is sick**; check OOM (`dmesg | grep -i kill`) and the runtime gauges the API already exports — there is no pprof endpoint in the binary:
  `curl -s http://localhost:3000/metrics | grep -E '^go_goroutines|^go_memstats_heap_inuse'`.
- **Recent deploy → roll back via `gh workflow run deploy.yml -f region=r1 -f version=<previous-tag> -f binaries=stellarindex-api`**.

## Mitigation (≤ 15 min)

- [ ] Step 1 — if recent deploy correlates with the burn onset: roll back. Per `deploy-workflow.md`'s "automatic rollback on health-probe failure" semantics this should already have happened; if it didn't, investigate the deploy's health-probe path.
- [ ] Step 2 — if upstream resource saturated: jump to the appropriate runbook.
- [ ] Step 3 — if the API process needs a kick: `systemctl restart stellarindex-api` (the systemd unit has `Restart=on-failure`; manual restart is the same effect).
- [ ] Verification: `stellarindex:api_error_ratio:5m` back below the trip
  point — 14.4 × 0.0001 = **0.144 %** — and holding. The alert itself only
  resolves once the **1h window drains** below 0.144 % too, which lags the
  fix; watch the 5m window for confirmation the bleeding stopped.

## Root cause analysis

For postmortem:
- The full request log over the burn window, filtered to 5xx.
- `go_goroutines` / `go_memstats_*` trends from Prometheus over the burn window (no pprof exists in the binaries — the runtime gauges on `:3000/metrics` are the in-process signal).
- Kernel `dmesg` over the burn window (OOM-kill markers).

## Known false-positive patterns

- **Synthetic probes at low traffic** — `stellarindex-sla-probe.timer` and
  `stellarindex-smoke.timer` (`configs/healthchecks/`) plus cache prewarm hit
  the local API. With no min-traffic guard on the availability rules, at
  < ~5 req/s real traffic a few probe 5xx can exceed 0.144 %. Check the total
  request rate (Quick diagnosis, first block) before mitigating.
- **Brief upstream blips** — Cloudflare → R1 has periodic single-region network interruptions; if the burn was < 60 s and recovered without intervention, it's the network, not us. The `for: 2m` window catches most cases. (Caddy-generated 502/503 when the API is unreachable are not in `http_requests_total` and don't count against this SLO — `api-down.md` covers that case.)
- **Weekly k6 load test** — `k6-weekly.yml` is SCHEDULED (cron `0 2 * * 0`, 02:00 UTC every Sunday), and it targets **staging only** — it cannot trip this alert on r1. Don't attribute an r1 burn to it.

## Related

- `slo-availability-burn-medium.md` / `slo-availability-burn-slow.md` — same family, slower burn.
- `api-down.md` — when scrape `up{job="stellarindex-api"} == 0`.
- `api-5xx.md` (the alert's `runbook_url` target) / `api-latency.md` — adjacent route-level alerts.
- `wire-paging.md` — confirm the `chat-page` receiver actually reaches a human.
- ADR-0008 — HA topology + availability target (multi-region decision amended by ADR-0050 / `docs/architecture/multi-region-ha.md`).
- ADR-0009 — latency budget (separate from availability budget).

## Changelog

- 2026-08-29 — re-verified against HEAD: SLA is 99.99 % (not 99.9 %); budget
  arithmetic (5 %/hour, whole budget ≈ 2 days — not "gone in ~1 hour");
  rule path → r1 overlay primary; no-min-traffic-guard note + synthetic-probe
  false positive; `runbook_url` → api-5xx.md note; access log has no `.err`
  field — diagnosis pipelines rewritten (`-o cat`, `[.path,.request_id,.status]`);
  unit names (`postgresql@15-main`, `redis-server`, `caddy`); pprof commands
  dropped in both places (no pprof endpoint exists in any binary) → runtime
  gauges via `:3000/metrics`; verification threshold corrected to the real
  trip point (0.144 % on the 5m window; 1h window must also drain — not
  "< 0.5 %"); k6-weekly is scheduled (cron 02:00 UTC Sunday), staging-only.
- 2026-05-12 — initial draft (audit-2026-05-12 F-1237 closure).
