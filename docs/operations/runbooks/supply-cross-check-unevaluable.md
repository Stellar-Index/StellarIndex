---
title: Runbook — supply-cross-check-unevaluable
last_verified: 2026-09-23
status: draft
severity: P3
---

# Runbook — `stellarindex_supply_cross_check_unevaluable`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_supply_cross_check_unevaluable` |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/supply.yml` + `configs/prometheus/rules.r1/supply.yml`; promtool-tested in `deploy/monitoring/rule-tests/supply_test.yml` |
| Typical MTTR | 30 min – 2 hours |
| Impact | None served directly. The classic ↔ SAC conservation check for at least one pair is not running, so [`stellarindex_supply_cross_check_divergence`](supply-cross-check-divergence.md) cannot fire for it. A real supply mis-sum on that asset would go unnoticed while this alert is open. |

## Why this exists

The aggregator's cross-check refresher reads the latest classic and SAC
snapshot for each configured pair every `aggregator_refresh_cadence`.
When it cannot evaluate a pair, it increments
`stellarindex_supply_cross_check_total{outcome}` and **deletes** that
pair's `stellarindex_supply_cross_check_divergence_stroops` series.
Before that, the gauge kept its last value, and Prometheus re-exported
it on every scrape. A pair whose classic refresher had stalled therefore
kept reading as a healthy `0` indefinitely, with no rule on the counter.
Now an absent series is expected for an unevaluable pair, and this alert
reports the gap.

## Symptoms

- `increase(stellarindex_supply_cross_check_total{outcome=~"missing_snapshot|read_error|misaligned"}[30m]) > 0`
  for more than an hour, per `outcome` × `wrap_class`.
- One or more `classic_key` series are missing from
  `stellarindex_supply_cross_check_divergence_stroops`.

| `outcome` | Means |
| --------- | ----- |
| `missing_snapshot` | One side of the pair has no `asset_supply_history` row. |
| `read_error` | Reading a snapshot or running the comparison keeps failing. |
| `misaligned` | Both snapshots exist but are more than 1000 ledgers (`supply.CrossCheckLedgerTolerance`) apart, so one side's refresher has stalled. |

## Quick diagnosis (≤ 10 min)

The counter has no `classic_key` label, so the pair comes from the
aggregator log. Every unevaluable outcome logs its pair: `read_error`
and `misaligned` log at WARN, `missing_snapshot` at DEBUG.

```sh
# 1) Which pair, and why. For missing_snapshot, compare the pairs
#    registered at startup with the series the gauge still exports.
journalctl -u stellarindex-aggregator --since='2 hours ago' --no-pager \
  | grep -E 'cross-check: (classic read failed|sac read failed|snapshots misaligned|compare failed)|cross-check pairs registered'
curl -fs http://localhost:9465/metrics \
  | grep '^stellarindex_supply_cross_check_divergence_stroops'

# 2) How old each side's latest snapshot is (substitute the pair's keys).
psql -d stellarindex -c \
  "SELECT asset_key, max(time) AS latest, max(ledger_sequence) AS ledger
     FROM asset_supply_history
    WHERE asset_key IN ('USDC:GA5Z...', 'CCW6...')
    GROUP BY asset_key;"
```

## Mitigation

- **`misaligned` or `missing_snapshot` on one side.** That side's supply
  refresher is not producing snapshots. Follow
  [`supply-refresh-stalled`](supply-refresh-stalled.md) or
  [`supply-refresh-error-dominant`](supply-refresh-error-dominant.md)
  for the stale asset. The cross-check recovers on the first tick after
  both sides are within 1000 ledgers of each other.
- **`missing_snapshot` on a newly added pair.** Wait one refresh cadence
  for both sides. If it persists, check that the classic asset is in
  `[supply].watched_classic_assets` and the SAC is in
  `watched_sep41_contracts`. The aggregator logs a WARN at startup for
  every `sac_wrappers` entry that is not cross-checked.
- **`read_error`.** Check storage first:
  [`pg-conns-saturated`](pg-conns-saturated.md) and
  [`timescale-primary-down`](timescale-primary-down.md). A `compare
  failed` line instead means a snapshot has a nil `total_supply`.
  Re-seed that asset's snapshot and do not change the tolerance.

When the cause is fixed, the next tick restores the divergence series.
If it then reads over 1, follow
[`supply-cross-check-divergence`](supply-cross-check-divergence.md).

## Related

- [`supply-cross-check-divergence`](supply-cross-check-divergence.md) —
  the alert this one keeps honest.
- [`docs/architecture/supply-pipeline.md`](../../architecture/supply-pipeline.md)
  — the outcome table for the cross-check counter.
- `internal/supply/crosscheck_refresher.go` — `Tick` and the outcome
  kinds. `cmd/stellarindex-aggregator/main.go::crossCheckPairs` builds
  the pair set. It rejects a wrapper that is not the classic asset's
  derived SAC at startup.
