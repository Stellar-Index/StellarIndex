---
adr: 0019
title: Anomaly response policy and confidence scoring — per-asset statistical baselines
status: Accepted
date: 2026-04-28
supersedes: []
superseded_by: null
---

# ADR-0019: Anomaly response policy and confidence scoring

## Context

Fresh, complete data can still be manipulated: a thin single-venue asset (USTRY) is pushed on one venue and consumers price collateral off it. Multi-source consensus does not help a single-venue asset, and a fixed global percentage threshold fits no asset class (stablecoin 0.05% and memecoin 50% per bucket are both routine), freezes real crashes, and can be dodged by slow drift.

## Decision

Anomaly response is a continuous confidence score plus a freeze policy on the closed-bucket surface, not a binary published/not-published decision on a fixed threshold. Formulas, factor shapes, thresholds and the freeze lifecycle are in [oracle-manipulation-defense.md](../architecture/oracle-manipulation-defense.md#freeze-layer-9).

1. **Per-asset baseline.** Per `(base, quote)` pair, rolling 30-day robust statistics: `return_median` and `return_mad` (MAD scaled by 1.4826, not sigma, because sigma inflates after the first attack and hides the next), plus typical source count and liquidity. `z_score = abs(return_pct - return_median) / return_mad`. `return_mad` is computed at 1d, 7d and 30d, and the anomaly fires on the LARGEST z across windows holding enough samples (frog-boiling defence).
2. **Confidence.** Every published price carries `confidence` in [0, 1], the normalised weighted geometric mean `prod(factor_i ^ weight_i) ^ (1 / sum(weights))` of seven factors: z-score, source count, source-class diversity, liquidity (log-saturating up to $1,000,000), cross-oracle agreement, triangulation agreement (weight 0.5, weight 0 when unchecked, never counted as a source) and baseline quality. The wire carries `confidence_factors`, the per-factor decomposition.
3. **Freeze.** The freeze fires only when all three hold:

```
confidence < 0.45 AND z_score > 5.0 AND source_count <= 1
```

   0.45, not the original 0.10: at 0.10 the trigger was z of about 15, so the control was decorative.

4. **Per-surface policy** (ADR-0018). `/v1/price` (closed bucket) honours the freeze and serves the last-known-good with `flags.frozen: true` and its original `observed_at`; `flags.divergence_warning` stays whatever the divergence service says, it is not forced. `/v1/price/tip` and `/v1/observations` ignore the freeze: the live value and the raw per-source data are their contract.
5. **Freeze lifecycle.** Initial hold 30 minutes, or 10 for a pair with no corroborating lens (a lens that produced a reading, agreeing or not). At each expiry, if the condition still holds, extend 30 minutes, up to 4 times; after that escalate to an operator (P1) and stay frozen until an operator ends it. The ladder is per (pair, window) for 5m, 1h and 24h; the marker's presence is the single pair-level `flags.frozen`.
6. **Auto-unfreeze.** From the end of the initial hold, two consecutive buckets with `confidence > 0.30` AND `z_score < 3.0` AND `release_corroborated` (a lens reading from this bucket agrees with the bucket's own fresh price within 5%; a composite lens releases within `release_band_pct`, see the detail page). A pair with no lens cannot auto-release. An escalated freeze never auto-releases.
7. **Operator override.** `stellarindex-ops freeze-unfreeze -reason ...`; it clears the marker and stamps `recovered_at` in the durable record. A bare Redis `DEL` is not an override.
8. **Bootstrap.** A pair with no usable baseline publishes no `confidence` and has no Phase 2 freeze eligibility; the Phase 1 per-class percentage threshold (`[anomaly.thresholds]`: `warn_pct` and `freeze_pct`, freezing only a single-source step) still applies to it. With a baseline, confidence is capped at 0.5 until baseline density reaches 28.5 days-equivalent, re-engaged below 27.
9. **Rollout.** Phase 1 per-class thresholds (transitional), Phase 2 statistical baselines, Phase 3 cross-oracle integration (`internal/divergence/`).

## Invariant

- The Phase 2 freeze must never fire on fewer than three signals: `confidence`, `z_score` and `source_count` are separate legs of one AND (the Phase 1 class-threshold rule and the triangulated-composite refusal are separate freeze paths). Enforced by `TestPhase2FreezeFires_MissingOneSignal` and `TestPhase2FreezeFires_ConfidenceConditionIsNotVacuous`.
- `confidence` is the normalised combiner: the exponent is `1 / sum(all seven weights)`, `w_tri` is 0 when triangulation is unchecked, and a composite never feeds `source_count`. Enforced by `internal/aggregate/confidence/adr_parity_test.go`.
- A freeze is never released by calm alone: auto-unfreeze needs a corroborating lens that agrees with the candidate price, and an escalated freeze is released only by an operator.
- The freeze record is durable and fail-closed: Redis loss never releases a live freeze, a window's release never ends a sibling window's ladder, and ambiguity about ownership resolves to the longer hold. Enforced by the freeze and orchestrator tests under `internal/aggregate`.
- Freeze applies to the closed-bucket surface only; tip and observations never freeze.

## Consequences

- No operator picks "the right percentage": the threshold is what 5 sigma of the asset's own MAD computes to, and one algorithm covers stablecoins to memecoins.
- Confidence is graded, so sophisticated consumers gate on it; the freeze is the safety net for consumers who do not read it. It does not defend against multi-source manipulation across several CEXes; that needs the Phase 3 cross-oracle reference.
- A freeze serves a stale last-known-good price, which is its own money bug (MNY-22); the short uncorroborated hold and the corroboration-gated release trade that against a false-freeze on thin books.
- A pair without a baseline publishes no `confidence`; its absence is the signal consumers gate on.
- Operators get alerts `stellarindex_anomaly_freeze_escalated` (P1), `stellarindex_anomaly_freeze_extension_rate` (P3) and `stellarindex_anomaly_freeze_active`, and the runbook [anomaly-freeze-engaged](../operations/runbooks/anomaly.md#stellarindex_anomaly_freeze_engaged).
- Alternatives rejected: permanent per-asset fixed percentages, a hard 503 on anomaly, Pyth-style range pricing as the default wire shape, always-publish-never-freeze, sigma instead of MAD, a single rolling window, and skipping Phase 1.

## Evidence

- `internal/aggregate/freeze` (lifecycle policy), `internal/aggregate/orchestrator` (freeze, composite reference, confidence), `internal/aggregate/baseline` (multi-window baseline), `internal/aggregate/confidence` (combiner).
- `TestPhase2FreezeFires_CalibratedToADRZBand` pins the freeze band; `internal/aggregate/confidence/adr_parity_test.go` pins the combiner and constants in the architecture page.
- Design context: [oracle-manipulation-defense.md](../architecture/oracle-manipulation-defense.md); related ADRs 0010, 0017, 0018; migrations 0119, 0163.
