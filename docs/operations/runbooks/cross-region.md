---
title: Runbook — cross-region monitor alerts
last_verified: 2026-10-05
status: draft
---
# Runbook — cross-region monitor

## At a glance

Cross-region consistency check alerts: stale, divergence, fetch errors.

Rules: `configs/prometheus/rules.r1/cross-region.yml` (mirror `deploy/monitoring/rules/cross-region.yml`). All P3 ticket. Emitted by `internal/ops/archive/cross_region_monitor.go`, run as `stellarindex-ops cross-region-monitor -regions name=URL,name=URL,... [-pairs ...] [-metric vwap|twap|ohlc] [-window DUR] [-samples N] [-interval DUR] [-listen :PORT]` (default `-listen :9479`, `-interval` 60s). Design: [ADR-0050](../../adr/0050-multi-region-ha-architecture.md).

## Diagnosis

```sh
curl -fs http://localhost:9479/healthz
curl -fs http://localhost:9479/metrics | grep -E \
  'stellarindex_cross_region_(checks|divergences|fetch_errors|check_errors)_total|last_run|last_reached|in_flight'
```

- `last_run` far behind wall-clock, `in_flight` stuck at 1: loop wedged; restart the `cross-region-monitor` process.
- `last_reached` far behind `last_run`: every fetch is failing; check network/DNS to the `-regions` targets.
- `divergences_total` climbing on a `(pair, metric)`: see the divergence section.

## stellarindex_cross_region_divergence

`sum by (pair, metric) (rate(stellarindex_cross_region_divergences_total[15m])) > 0`, `for: 5m`. Two regions serve DIFFERENT values for the same closed bucket: a pipeline correctness fault. `stellarindex_cross_region_checks_total{outcome="divergence"}` per region shows which side moved.

Fix: compare both against the certified raw ledger lake, not each other, then re-run the wrong region's ingest/projection for the range ([projector-replay](projector.md#stellarindex_projector_replay_stalled)).

## stellarindex_cross_region_fetch_errors

`sum by (region, pair, metric) (rate(stellarindex_cross_region_fetch_errors_total[15m])) > 0`, `for: 10m`. A region fetch fails (timeout, 5xx, parse error) and that region is not compared; precursor to `_check_stale`. Fix connectivity or restart the monitor; not a data fault by itself.

## stellarindex_cross_region_check_stale

`time() - stellarindex_cross_region_last_run_timestamp_seconds > 300`, `for: 5m`. The loop stopped completing sweeps, so a real divergence would go unseen; `/healthz` is already 503. `stellarindex_cross_region_check_in_flight` stuck at 1 means a stuck sweep. Restart the monitor; read its logs.

## Related

[Alerts catalogue](../alerts-catalog.md)
