---
title: Runbook — cross-region-divergence
last_verified: 2026-09-25
status: living
severity: P3
---

# Runbook — `stellarindex_cross_region_divergence`

Covers three related alerts from the same monitor:

- `stellarindex_cross_region_divergence` — two regions disagreed on a
  closed bucket's served value.
- `stellarindex_cross_region_fetch_errors` — a region fetch is failing,
  so that region isn't being compared.
- `stellarindex_cross_region_check_stale` — the check loop itself has
  stopped completing sweeps; every comparison is blind.

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | see above |
| Severity | P3 (ticket) |
| Detected by | `configs/prometheus/rules.r1/cross-region.yml` (mirror: `deploy/monitoring/rules/cross-region.yml`) |
| Emitted by | `internal/ops/archive/cross_region_monitor.go` (`cross-region-check` binary), one process per monitored pair of regions |
| Typical MTTR | 15–60 min |
| Impact | A real divergence means two regions are serving DIFFERENT values for the same closed bucket — a correctness fault in one region's pipeline. A fetch-error or stale sweep means the monitor is blind to that fault, not that it is absent. |

## Diagnosis

```sh
# Sweep health
curl -fs http://localhost:9479/healthz
curl -fs http://localhost:9479/metrics | grep -E \
  'stellarindex_cross_region_(checks|divergences|fetch_errors|check_errors)_total|last_run|last_reached|in_flight'
```

- `last_run` far behind wall-clock, `in_flight` stuck at 1 → the loop is
  wedged; restart the `cross-region-check` process.
- `last_reached` far behind `last_run` → every region fetch is failing;
  check network/DNS to the configured `-regions` targets before
  suspecting the data itself.
- `divergences_total` climbing on a specific `(pair, metric)` → pull
  both regions' served values for the bucket directly and compare
  against the raw ledger lake to determine which region is wrong.

## Resolution

1. Fetch errors / stale sweep: fix connectivity or restart the monitor
   process; this alone does not indicate a data fault.
2. Confirmed divergence: identify which region's pipeline produced the
   wrong value (compare against the certified raw ledger lake, not the
   other region — both could in principle be behind a since-fixed
   defect) and re-run its ingest/projection for the affected range.
