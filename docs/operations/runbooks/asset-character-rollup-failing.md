---
title: Runbook — asset-character-rollup-failing
last_verified: 2026-10-03
status: draft
severity: P3
---

# Runbook — `stellarindex_asset_character_rollup_failing`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_asset_character_rollup_failing` (informational) |
| Detected by | Prometheus rules in `deploy/monitoring/rules/aggregator.yml` + `configs/prometheus/rules.r1/aggregator.yml` |
| Typical MTTR | 5–15 min to fix the cause; recovery lands on the next 6h sweep |
| Impact | `volume_character` label + signals on `/v1/assets` and `/v1/assets/{id}` hold their last-good value (stale, NOT blank). NO pricing impact. |

## Why the window is 13h

The worker sweeps every 6h, so a short `rate()` window with a `for:` gate
never fires. The alert needs a `refresh_error` in the last 13h and no `ok`
sweep in that same window: two consecutive sweeps failed.

## Quick diagnosis (≤ 5 min)

```sh
curl -s localhost:9464/metrics | grep stellarindex_asset_character_rollup_sweeps_total
journalctl -u stellarindex-aggregator --since -12h | grep -i "asset-character rollup"
sudo -u postgres psql stellarindex -c '\d asset_volume_character'
sudo -u postgres psql stellarindex -c \
  'SELECT count(*), max(computed_at) FROM asset_volume_character;'
```

## Mitigation

- Missing table → migration 0149 did not apply; run the migrator
  (deploy.yml auto-applies).
- Postgres down / lock contention / statement timeout → follow the storage
  runbook. The worker retries every 6h; there is no manual catch-up step.
  A restart re-runs the first sweep after the 30 min startup delay.

## Related

- Metric reference: [`stellarindex_asset_character_rollup_sweeps_total`](../../reference/metrics/README.md#stellarindex_asset_character_rollup_sweeps_total)
- Worker: `internal/aggregate/assetcharacterrollup/worker.go`
- Table: `migrations/0149_create_asset_volume_character_rollup.up.sql`
- Catalogue row: [alerts-catalog.md](../alerts-catalog.md)
