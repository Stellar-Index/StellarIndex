---
title: Runbook — cross-region monitor alerts
last_verified: 2026-10-05
status: draft
---
# Runbook — cross-region monitor

Rules: `configs/prometheus/rules.r1/cross-region.yml` (mirror `deploy/monitoring/rules/cross-region.yml`, identical). All three are severity ticket (P3). Typical MTTR 15-60 min.

Emitted by `internal/ops/archive/cross_region_monitor.go`, run as `stellarindex-ops cross-region-monitor -regions name=URL,name=URL,... [-pairs ...] [-metric vwap|twap|ohlc] [-window DUR] [-samples N] [-interval DUR] [-listen :PORT]` (default `-listen :9479`, default `-interval` 60s). Design: [ADR-0050](../../adr/0050-multi-region-ha-architecture.md).

A real divergence means two regions serve DIFFERENT values for the same closed bucket: a correctness fault in one region's pipeline. A fetch-error or stale alert means the monitor is blind to that fault, not that it is absent.

## Diagnosis

```sh
curl -fs http://localhost:9479/healthz
curl -fs http://localhost:9479/metrics | grep -E \
  'stellarindex_cross_region_(checks|divergences|fetch_errors|check_errors)_total|last_run|last_reached|in_flight'
```

- `last_run` far behind wall-clock, `in_flight` stuck at 1: the loop is wedged; restart the `cross-region-monitor` process.
- `last_reached` far behind `last_run`: every region fetch is failing; check network/DNS to the configured `-regions` targets before suspecting the data.
- `divergences_total` climbing on a `(pair, metric)`: pull both regions' served values for the bucket and compare against the raw ledger lake to find which region is wrong.

## At a glance

- [`stellarindex_cross_region_divergence`](#stellarindex_cross_region_divergence)
- [`stellarindex_cross_region_fetch_errors`](#stellarindex_cross_region_fetch_errors)
- [`stellarindex_cross_region_check_stale`](#stellarindex_cross_region_check_stale)

## stellarindex_cross_region_divergence

Trips: `sum by (pair, metric) (rate(stellarindex_cross_region_divergences_total[15m])) > 0`, `for: 5m`.

Two regions disagreed on a closed bucket's served value. `stellarindex_cross_region_checks_total{outcome="divergence"}` per region shows which side moved.

Fix: identify which region's pipeline produced the wrong value, comparing against the certified raw ledger lake, not the other region (both could be behind a since-fixed defect), and re-run its ingest/projection for the affected range ([projector-replay](projector.md#stellarindex_projector_replay_stalled)).

## stellarindex_cross_region_fetch_errors

Trips: `sum by (region, pair, metric) (rate(stellarindex_cross_region_fetch_errors_total[15m])) > 0`, `for: 10m`.

A region fetch is failing (timeout, 5xx or parse error); that region is not compared while this persists. Precursor to `_check_stale`. Fix connectivity or restart the monitor; this alone is not a data fault.

## stellarindex_cross_region_check_stale

Trips: `time() - stellarindex_cross_region_last_run_timestamp_seconds > 300`, `for: 5m`.

The check loop stopped completing sweeps (stuck or process down); no comparison runs for any pair, so a real divergence would go unseen. The 5 min threshold is five default intervals, past the monitor's own `/healthz` stale bound (3*interval + timeout), so `/healthz` is already 503 when this fires. `stellarindex_cross_region_check_in_flight` persistently 1 means a stuck sweep. Restart the monitor; read its logs.

## Related

- [Alerts catalogue](../alerts-catalog.md)
