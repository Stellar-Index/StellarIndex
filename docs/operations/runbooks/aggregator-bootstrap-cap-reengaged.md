---
title: Runbook — aggregator-bootstrap-cap-reengaged
last_verified: 2026-09-30
status: draft
severity: P3
---

# Runbook — `stellarindex_aggregator_bootstrap_cap_reengaged`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_aggregator_bootstrap_cap_reengaged` |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/aggregator.yml` |
| Typical MTTR | 1 – 4 h to repair the gap; otherwise up to 30 days for it to roll out |
| Impact | Three or more pairs whose confidence had been released from the ADR-0019 bootstrap cap are pinned at 0.5 again. Prices still serve; `confidence` and the anomaly-freeze inputs that read it drop for those pairs, and `confidence_factors.bootstrap_capped` flips to `true` on `/v1/price`. |

## Background

The bootstrap cap releases when a pair's 30-day baseline holds at least
28.5 days-equivalent of 1-minute buckets (`BootstrapDensityDays` in
`internal/aggregate/confidence/score.go`; ADR-0019). The density is the
bucket count of the `prices_1m` continuous aggregate over the 30-day
window, divided by 1,440, recomputed by the baseline refresher about
hourly. Once released, a pair is capped again only below 27
days-equivalent (`BootstrapReengageDensityDays`), so the densest pairs,
near 29.8, absorb about 67 hours of missing minutes. That gate state is
held in the aggregator's memory: after a restart every pair must clear
28.5 again, so a restart that follows about 30 hours of gap re-caps them.

One pair re-capping alone is a thin pair, which is why this alert needs
three pairs to re-cap inside the same hour. It compares against each
pair's last reported state before that hour, looking back up to two
days, so a re-cap that arrives with a restart after an outage still
counts. Several pairs dropping together means their
windows lost the same minutes — an ingestion outage, an aggregator
outage, or trades that landed after `prices_1m`'s 5-minute refresh lag
and were never materialised.

## Symptoms

- `stellarindex_aggregator_bootstrap_capped{pair}` went 0 → 1 on several
  major pairs at once.
- `stellarindex_aggregator_baseline_density_days{pair}` for those pairs
  stepped below 27, or below 28.5 across an aggregator restart.
- `/v1/price` shows `confidence` of exactly 0.5 with
  `confidence_factors.bootstrap_capped: true` on pairs that read higher
  earlier.

## Quick diagnosis (≤ 5 min)

1. Size the drop: graph `stellarindex_aggregator_baseline_density_days`
   for the capped pairs over the last day. A step of N days-equivalent
   is N × 1,440 missing buckets.
2. Find the gap: count `prices_1m` buckets per day for one affected
   pair over the window. A day well short of 1,440 (for a pair that
   trades every minute) is the gap.

   ```sql
   SELECT date_trunc('day', bucket) AS day, count(*) AS buckets
   FROM prices_1m
   WHERE base_asset = '<base>' AND quote_asset = '<quote>'
     AND bucket > now() - INTERVAL '30 days'
   GROUP BY 1 ORDER BY 1;
   ```

3. Compare with `trades` for the same day. Trades present but buckets
   missing means the continuous aggregate never materialised that range;
   trades missing too means an ingestion gap.

## Mitigation

- **Buckets missing, trades present:** refresh the aggregate over the
  gap, e.g. `CALL refresh_continuous_aggregate('prices_1m', '<from>',
  '<to>');`. The next baseline refresh picks up the restored density and
  the cap releases on the following confidence compute.
- **Trades missing:** repair ingestion for the gap first (see
  `all-ingestion-down.md` or `source-stopped.md`), then refresh the
  aggregate as above.
- **Gap cannot be repaired:** the cap stays engaged until the gap rolls
  out of the 30-day window. Record the expected release date in the
  ticket; the served `bootstrap_capped` flag tells consumers why
  confidence reads 0.5.

## Known false-positive patterns

- **Several thin pairs at the gate.** If three pairs that each hover
  near the gate flip in the same hour, the alert fires without a shared
  gap. The per-day bucket counts from step 2 show no common short day.

## Related

- `anomaly-freeze-engaged.md` — the thin-baseline false-positive pattern
  this cap feeds.
- `docs/adr/0019-anomaly-response-and-confidence-scoring.md` — the
  28.5 days-equivalent density gate and its 27 re-engage edge.
- `docs/reference/metrics/README.md`
  § `stellarindex_aggregator_baseline_density_days`.
