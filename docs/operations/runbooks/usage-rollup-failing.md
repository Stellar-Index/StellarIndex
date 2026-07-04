---
title: Runbook — usage-rollup-failing
last_verified: 2026-07-04
status: draft
severity: P3
---

# Runbook — `stellarindex_usage_rollup_failing`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_usage_rollup_failing` (informational) |
| Detected by | Prometheus rules in `deploy/monitoring/rules/api.yml` + `configs/prometheus/rules.r1/api.yml` |
| Typical MTTR | 5–15 min (it's almost always Redis or Postgres reachability, shared with louder alerts) |
| Impact | Dashboard per-endpoint usage analytics stop advancing; `/v1/account/usage` degrades to endpoint-less legacy per-day rows. NO customer pricing impact. Counters keep accumulating in Redis (35-day TTL) — nothing is lost unless the outage exceeds that. |

## Symptoms

- `stellarindex_usage_rollup_sweeps_total{outcome=~"scan_error|sink_error"}`
  increasing; `outcome="ok"` flat.
- `journalctl -u stellarindex-api | grep "usage rollup sweep failed"`
  shows the underlying Redis / Postgres error every ~5 min.
- Dashboard `/dashboard/usage` per-endpoint table frozen at the
  last successful sweep; daily totals may still move (they come
  from the Redis fallback path).

## Quick diagnosis (≤ 5 min)

```sh
# Which half is failing?
curl -s localhost:3000/metrics | grep stellarindex_usage_rollup_sweeps_total

# scan_error → Redis. Check reachability + the counter keys:
redis-cli -n 0 --scan --pattern 'usage:ep:*' | head

# sink_error → Postgres. Check the table exists (migration 0071):
sudo -u postgres psql stellarindex -c '\d usage_daily'

# The worker's own log line carries the wrapped error:
journalctl -u stellarindex-api --since -30min | grep -i "usage rollup"
```

## Mitigation (≤ 15 min)

- `sink_error` + missing table → migration 0071 didn't apply on
  this deployment. Run the migrator (deploy.yml auto-applies;
  manual: `stellarindex-migrate -dir /usr/local/share/stellarindex/migrations up`).
- Redis / Postgres down → follow the respective infra runbook;
  this alert clears itself on the next successful sweep (the
  worker retries forever, sweeping today + yesterday, and the
  upsert is GREATEST-merged so replays are safe).
- No operator "catch-up" step exists or is needed inside the
  35-day Redis TTL. Beyond that window, unswept days are gone —
  note it in the incident log; there is no re-derive path.

## Root cause analysis

The worker is deliberately dependency-thin: one SCAN + HGETALLs in
Redis, one batched upsert in Postgres. A failure here with healthy
`/v1/price` traffic almost always means the API host lost ONE of
its two backends — check what else fired in the same window.

## Known false-positive patterns

None known yet. The alert requires 30 min of continuous failures at
a 5-min sweep cadence (≥ 6 consecutive), so single transient
Redis/Postgres blips do not fire it.

## Related

- Metric reference: [`stellarindex_usage_rollup_sweeps_total`](../../reference/metrics/README.md#stellarindex_usage_rollup_sweeps_total)
  + [`stellarindex_usage_rollup_sweep_duration_seconds`](../../reference/metrics/README.md#stellarindex_usage_rollup_sweep_duration_seconds)
- Worker: `internal/usage/rollup.go` (wired in `cmd/stellarindex-api/main.go`)
- Table: `migrations/0071_create_usage_daily.up.sql`
- Endpoint served from the rollups: `/v1/account/usage`
- Catalogue row: [alerts-catalog.md](../alerts-catalog.md)

## Changelog

- 2026-07-04 — created with the usage-rollup pipeline (#32/#37b).
