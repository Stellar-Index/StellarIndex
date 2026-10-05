---
title: Anomaly freeze and confidence scoring — formulas, thresholds and lifecycle
last_verified: 2026-10-05
status: current — detail behind ADR-0019; the code is authoritative where they differ
---

# Anomaly freeze and confidence scoring

The policy is [ADR-0019](../adr/0019-anomaly-response-and-confidence-scoring.md). This page holds
the formulas, thresholds and lifecycle rules it relies on. Attack catalogue:
[oracle-manipulation-defense.md](oracle-manipulation-defense.md).

## Per-asset baseline

For each `(base, quote)` pair, over a rolling 30-day window of 1-minute buckets:

- `return_median`: median bucket-to-bucket % change in VWAP.
- `return_mad`: median absolute deviation of those returns, scaled by 1.4826 (sigma-equivalent for normal data).
- `source_count_p50`, `liquidity_p50_usd`: typical source count and bucket liquidity.

MAD, not sigma: sigma inflates after the first manipulation in the training window and hides the next; medians do not.

```
z_score = abs(return_pct - return_median) / return_mad
```

| Asset class | typical return_mad | 5-sigma trigger |
|---|---|---|
| Stablecoin, treasury token | ~0.05% | ~0.25% |
| Major crypto (XLM, BTC, ETH) | ~2% | ~10% |
| Governance token | ~10% | ~50% |
| Memecoin / new listing | ~50% | ~250% |

**Multi-window safeguard.** `return_mad` is computed at 1d, 7d and 30d. `MultiBaseline.MaxZScore`
(`internal/aggregate/baseline/multi.go`) takes the LARGEST z across windows holding at least
`MinZScoreSamples` returns, so one window over threshold fires. It scores single-bucket spikes only and
is blind to slow drift; the frog-boiling defence is the separate `MultiBaseline.MaxDriftZScore`, which
latches about 30 days and cannot self-clear, so it is kept out of the fire, extend and release decisions
and reaches release only through `confidence`.

## Confidence score

`confidence in [0, 1]` is a normalised weighted geometric mean, `prod(factor_i ^ weight_i) ^ (1 / sum(weights))`
(`internal/aggregate/confidence.Compute`):

```
confidence = (
  z_score_factor(z_score)                    ^ w_z       *
  source_count_factor(n_sources)             ^ w_src     *
  diversity_factor(class_count)              ^ w_div     *
  liquidity_factor(bucket_volume)            ^ w_liq     *
  cross_oracle_factor(divergence_pct)        ^ w_xoracle *
  triangulation_agreement_factor(divergence) ^ w_tri     *
  baseline_quality_factor(days_history)      ^ w_qual
) ^ (1 / (w_z + w_src + w_div + w_liq + w_xoracle + w_tri + w_qual))
```

The exponent is what makes weights relative, makes the 0.7 cross-oracle neutral value neutral rather than
a cap, and lets the bootstrap cap sit above ordinary scores. Weights are tunable in `[anomaly.weights]`,
default 1.0, except `w_tri` (below).

Factor shapes:

- `z_score_factor`: 1.0 at z=0, sigmoid decay to ~0 at z=10.
- `source_count_factor`: logistic, `sourceCountInflectionN = 3.0` (`factors.go`); `SourceCountFactor(1)` is
  about 0.119 (not the 0.3 the original text intended), near 1.0 from n >= 6.
- `diversity_factor`: 0.5 for one source class, 1.0 for two or more.
- `liquidity_factor`: log-saturating between $1K and a **$1,000,000** ceiling (was $100K: BTC/USD's 5m
  bucket p50 is $123,678, so the median bucket of the deepest pair saturated it). `LiquidityFactor(123_678)`
  reads 0.697; the 0.5 point is `sqrt(1e3 * 1e6)`, about $31,623. Pairs whose volume cannot be valued in USD
  pass the `LiquidityUnmeasured` sentinel and read `LiquidityUnmeasuredFactor` = 0.5, deliberately not
  tracked down to the curve's 0.333 floor.
- `cross_oracle_factor`: 1.0 within 1% of the cross-oracle median, decaying; 0.7 neutral when no data.
- `triangulation_agreement_factor`: same shape as cross-oracle against the composite a configured chain
  implies (XLM/EUR direct vs XLM/USD x USD/EUR); 1.0 within 2%, halving every 4 points beyond, 0.7 no-data.
  **Default weight 0.5** (a composite reuses our own leg VWAPs and venues, so it is half-strength evidence).
  **Weight 0 when unchecked**, so an un-triangulated pair scores bit-for-bit what it scored before the
  factor existed; any constant neutral value would re-score the whole index under a normalised combiner. It
  never feeds `source_count`: counting a composite as a venue would disarm the `source_count <= 1` freeze
  leg on exactly the thin pairs chains are deployed for. See `orchestrator/triangulate_corroborate.go`.
- `baseline_quality_factor`: 0.5 with no baseline, ramping to 1.0 over 30 days.

Scale of the freeze leg: with all weights 1.0, `confidence < 0.10` means the raw six-factor product is
below 1e-6. For the single-source corner (source 0.119, one class 0.5, mature baseline 1.0, no cross-oracle
0.7, $12K volume 0.540) the non-z factors multiply to 0.0225, so `confidence < 0.10` needs z of about 15.

Wire: `confidence` plus `confidence_factors` (`ConfidenceFactors`, `internal/api/v1/price.go`): the seven
factor values in [0, 1] (`z_score`, `source_count`, `diversity`, `liquidity`, `cross_oracle`,
`baseline_quality`, `triangulation_agreement`), the evidence fields `cross_oracle_checked`,
`cross_oracle_agreement` (integer count), `liquidity_measured`, `triangulation_checked`, and
`baseline_age_days` and `bootstrap_capped`. No raw source count, USD liquidity or divergence percentage
is served.

Chained pairs (AQUA -> USDC -> USD -> COP): chained confidence is the weakest leg's confidence (the minimum, `RouteConfidence`).

## Freeze condition and calibration

```
freeze_condition = confidence < 0.45 AND z_score > 5.0 AND source_count <= 1
```

`confidence` decays gently in z, so the freeze's real trigger is emergent from threshold, combiner and
factor set. Measured on the shipped combiner for a single-source bucket at the $12K publish floor:

| z | confidence (mature) | confidence (sparse baseline) |
|---|---|---|
| 0 | 0.5308 | 0.4804 |
| 5 | 0.4734 | 0.4285 |
| 6 | 0.4269 | 0.3864 |
| 8 | 0.3197 | 0.2894 |

At 0.10 the freeze needed z of about 15 (a ~30% one-minute move for XLM), so the control was decorative;
0.45 puts the trigger at z of 5.5 to 6. After the $1M liquidity ceiling the confidence leg crosses at
z of about 4.79 ($12K volume) or 5.85 ($100K); the `z > 5.0` leg is independent, so nothing freezes below
z = 5, and below about $15.6K volume the AND leans on z and `source_count`. Too-lax and too-eager are both
hazards (a stale last-known-good is its own money bug, MNY-22); `TestPhase2FreezeFires_CalibratedToADRZBand`
pins the band both ways. `confidence_max_freeze` is `[anomaly.phase2]`-tunable.

Earlier false fires came from non-USD-quoted pairs whose `confidence` was pinned to 0 (`approxUSDVolume`
returned 0 for pairs it could not value), which collapsed the AND to two signals (COR-14).

## Bootstrap (warmup) gate

A pair with no usable baseline (no baseline row, or `MaxZScore` reports `!valid`) publishes **no
`confidence`** and is not Phase 2 eligible (`Orchestrator.computeConfidence`); it can still be frozen by
the Phase 1 class-threshold rule (`evaluateAndMaybeFreeze`), which needs a previous price and a single-source step reaching the class `freeze_pct`.

For a pair with a baseline, confidence is capped at 0.5 until baseline density clears the gate.
`confidence.Inputs.BaselineAgeDays` is the count of 1-minute buckets behind the 30-day baseline divided
by 1440 (at most 30.0, negative when no baseline). The cap releases at `BootstrapDensityDays` = 28.5 and
re-engages only below `BootstrapReengageDensityDays` = 27, so a shared ingestion gap of about three days
does not re-cap a dense pair. A calendar-mature pair that trades sparsely stays capped. Gate state is per
pair in aggregator memory, so a restart re-applies the 28.5 gate. Gauges:
`stellarindex_aggregator_bootstrap_capped`, `stellarindex_aggregator_baseline_density_days`.

## Phase 1 class thresholds

Operator-set per-class percentage thresholds (`[anomaly.thresholds]`, `warn_pct` / `freeze_pct`; a freeze needs a single-source step reaching `freeze_pct`):
stablecoin and treasury 1 / 3, crypto 20 / 50, governance 50 / 100, default 30 / 75. Binary
warn/freeze/clear.

## What a freeze serves

- `/v1/price` (closed bucket): last-known-good with `flags.frozen: true` (read with `flags.frozen_checked`)
  and `flags.single_source: true`, and the original `observed_at`. `flags.divergence_warning` is NOT forced:
  it comes only from the cross-reference divergence service, read with `divergence_checked`, because a
  freeze runs no cross-reference and forcing it would publish the uninterpretable `true` + unchecked state
  (CS-087).
- `/v1/price/tip`: ignores freeze, returns the observed value with low confidence and flags.
- `/v1/observations`: ignores freeze, raw per-source data.

## Freeze lifecycle (`internal/aggregate/freeze.Policy`)

A pure function of (previous state, this bucket's signal), state carried in the durable freeze marker.

- **Initial hold:** 30 minutes, or **10 minutes for a pair with no corroborating lens**. "Corroborated" means a
  second lens produced a READING this bucket (`triangulation_checked` OR `cross_oracle_checked`), not that
  it agreed: a disagreeing lens is the strongest evidence the freeze is true. 10 rather than 5 so it outlasts
  the longest default window carrying the spike (5m) and several 30s ticks. Extensions are not scaled.
- **Extension:** at each expiry, if the condition still holds, extend 30 minutes, up to 4 times.
- **Escalation:** after the ladder (2 hours) escalate to operator review (P1); an escalated freeze does NOT
  auto-unfreeze, only an operator ends it.
- **Auto-unfreeze:** a continuous trigger, evaluated every bucket from the end of the initial hold (the
  initial hold is a hard minimum). Needs two consecutive buckets with `confidence > 0.30` AND `z < 3.0` AND
  `release_corroborated`: a lens reading from this bucket that agrees within 5% with the bucket's own fresh
  price (cross-oracle median against the candidate directly). Calm alone cannot tell "repriced" from
  "manipulation parked", because mid-freeze buckets score per-tick returns against `frozenPrevVWAPs`
  (shadow comparator set to each refused bucket's fresh VWAP; `prevVWAPs` itself does not advance on a refused bucket; `Orchestrator.decideBucket`). A pair with no
  lens cannot auto-release and rides the ladder to a human.
- **Composite reference** (`[aggregate.composite_reference]`, default on for crypto:XLM/fiat:GBP and
  XLM/EUR): the composite is rebuilt on the SAME bucket from this tick's leg publish and a fresh FX
  snapshot. Agreement within `tolerance_bps` (75) suppresses the fire (`corroboration_basis=composite`);
  disagreement or unavailability freezes as normal. It is also a release lens when the composite agrees
  within `release_band_pct` (2%, tighter than the 5% cross-oracle band). The FX cross never counts as a
  second source.
- **Tunables:** every duration lives under `[anomaly.phase2]`. Alerts: `stellarindex_anomaly_freeze_escalated`
  (P1), `stellarindex_anomaly_freeze_extension_rate` (P3), `stellarindex_anomaly_freeze_active` (info).
  Release metric: `stellarindex_anomaly_freeze_released_total{mode}`.

### Durability and the operator override

Redis is a cache and may be flushed, so the marker is not the record. Migration 0119 puts `hold_until`,
`extensions_used`, `escalated`, `corroborated` on `freeze_events`, written on every transition and read
back whenever the marker is missing, bounded by `hold_until` + the marker grace (5 minutes) so a long-dead
aggregator cannot resurrect a stale freeze. Marker missing is disambiguated by `recovered_at`: OPEN row =
Redis lost it (rehydrate), CLOSED row = the freeze genuinely ended. The marker TTL is remaining hold plus
grace, a liveness backstop. The recovery worker applies the same `hold_until + grace` bound, and
`Writer.Clear` retires the durable ladder in the same call that deletes the marker.

**The override is `stellarindex-ops freeze-unfreeze -reason ...`**, which clears the marker AND stamps
`recovered_at`. A bare `redis-cli DEL` is not an override: the next tick rehydrates, and against an
escalated freeze it is permanently inert.

### Per-window ladders (migration 0163)

The aggregator runs one independent ladder per (pair, window) for 5m, 1h and 24h.

- Pair-scoped: the marker's presence (the single `flags.frozen` served; the operator override is `freeze-unfreeze`, which retires it for every window).
- Window-scoped: the marker carries one ladder per window; `freeze_events.window_ladders` (jsonb, keyed by
  window seconds) carries one per window on the pair's open row. The 0119 columns remain the fail-closed
  summary (furthest hold, highest rung, escalated if any window is) read by the recovery worker and
  `freeze-unfreeze -list`; `hold_until IS NOT NULL` is the switch the record is honoured on.
- Unknowable ownership resolves conservatively: a marker or row written before the window dimension holds
  one unowned ladder that answers for every window until its own `hold_until + grace`. A stale
  `window_ladders` map (a rolled-back binary kept advancing only the columns) is raised entry by entry to the
  fail-closed fold of itself and the pair-level ladder, carried as the unowned one.
- Rule 1, a release asks the record, not only the process: the last window to release deletes the marker,
  and "last" is decided from the marker AND the durable ladders (not the in-memory map, which a window
  under `min_usd_volume`, or any window after a restart, has not entered). The release keeps the freeze
  while either place records another window's live ladder, retires only its own entry, and leaves a marker
  or record it cannot read alone. The operator override retires the whole durable record.
- Rule 2, the lifecycle-free writer (triangulated-composite refusal, flat TTL, no ladder) sets the serving
  flag and nothing else: it leaves an owned marker as found and carries the durable ladders into one it must
  re-create.
- Rule 3, one key has one TTL and several owners: no write shortens the marker's TTL below the longest
  `remaining hold + grace` of any live ladder in it.
- "Frozen" for triangulation is not "frozen this tick": the laundering guard (MNY-22) also honours a live
  in-memory ladder bounded by `hold_until + grace`, so an empty or thin window cannot publish a frozen
  leg's last-known-good as a fresh derived price.
