---
title: Runbook — asset-volume-rollup-failing
last_verified: 2026-07-06
status: draft
severity: P3
---

# Runbook — `stellarindex_asset_volume_rollup_failing`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_asset_volume_rollup_failing` (informational) |
| Detected by | Prometheus rules in `deploy/monitoring/rules/aggregator.yml` + `configs/prometheus/rules.r1/aggregator.yml` |
| Typical MTTR | 5–15 min (almost always Postgres reachability, shared with louder alerts) |
| Impact | The `/v1/assets` listing's `volume_24h_usd` column stops advancing — it holds its last-good value (stale, NOT zero) and the volume-desc sort order freezes. NO customer pricing impact. |

## Symptoms

- `stellarindex_asset_volume_rollup_sweeps_total{outcome="refresh_error"}`
  increasing; `outcome="ok"` flat.
- `journalctl -u stellarindex-aggregator | grep "asset-volume rollup refresh failed"`
  shows the wrapped Postgres error every ~2 min.
- The explorer's assets list shows a frozen 24h-volume column / stale
  volume-ranked order.

## Quick diagnosis (≤ 5 min)

```sh
# Is it failing, and how often?
curl -s localhost:9464/metrics | grep stellarindex_asset_volume_rollup_sweeps_total

# The worker's own log line carries the wrapped error:
journalctl -u stellarindex-aggregator --since -30min | grep -i "asset-volume rollup"

# refresh_error is almost always Postgres. Check the table exists
# (migration 0087):
sudo -u postgres psql stellarindex -c '\d asset_volume_24h'

# Spot-check the rollup has fresh rows (computed_at should be within a
# few minutes):
sudo -u postgres psql stellarindex -c \
  'SELECT count(*), max(computed_at) FROM asset_volume_24h;'
```

## Mitigation (≤ 15 min)

- Missing table → migration 0087 didn't apply on this deployment. Run
  the migrator (deploy.yml auto-applies; manual:
  `stellarindex-migrate -dir /usr/local/share/stellarindex/migrations up`).
- Postgres down / lock contention → follow the storage runbook; this
  alert clears itself on the next successful sweep (the worker retries
  forever, and the upsert-then-prune is idempotent).
- No operator "catch-up" step exists or is needed: the next sweep
  recomputes the full trailing-24h volume from scratch.

## Root cause analysis

The worker runs one query — the trailing-24h base-OR-quote SUM over the
`prices_1m` continuous aggregate — plus an upsert + prune, on a 2-minute
cadence. It is the heavier of the two #43 rollups (it scans ~24h of
`prices_1m` across all pairs). A failure here with healthy `/v1/price`
traffic almost always means the aggregator host lost Postgres
reachability, or the served tier is under lock/IO pressure — check what
else fired in the same window. If `ok`-outcome sweep *latency*
(`stellarindex_asset_volume_rollup_sweep_duration_seconds`) is climbing
toward the cadence, the served-tier volume scan is getting heavier and
the cadence may need widening.

## Known false-positive patterns

None known yet. The alert requires 30 min of continuous failures at a
2-min sweep cadence, so single transient Postgres blips do not fire it.

## Related

- Metric reference: [`stellarindex_asset_volume_rollup_sweeps_total`](../../reference/metrics/README.md#stellarindex_asset_volume_rollup_sweeps_total)
  + [`stellarindex_asset_volume_rollup_sweep_duration_seconds`](../../reference/metrics/README.md#stellarindex_asset_volume_rollup_sweep_duration_seconds)
- Worker: `internal/aggregate/assetvolrollup/worker.go` (wired in `cmd/stellarindex-aggregator/main.go`)
- SUM + upsert SQL: `internal/storage/timescale/coins.go`
- Table: `migrations/0087_create_asset_volume_24h_rollup.up.sql`
- Endpoint served from the rollup: `/v1/assets` (`volume_24h_usd`)
- Catalogue row: [alerts-catalog.md](../alerts-catalog.md)

## Changelog

- 2026-07-06 — created with the asset-volume rollup (#43).
