---
adr: 0046
title: MAD-based outlier filtering for VWAP inputs
status: Proposed
date: 2026-07-08
supersedes: []
superseded_by: null
---

# ADR-0046 — MAD-based outlier filtering for VWAP inputs

- **Decides:** the replacement design for the Phase-1 σ-threshold
  outlier filter (`internal/aggregate/outliers.go`), per BACKLOG #44's
  "write the ADR now — the design needs no traffic, only the
  thresholds do".
- **Defers:** the numeric thresholds and the shadow→serve flip date,
  which require production trade distributions.

## Context

`FilterOutliers` drops trades whose per-trade price deviates from the
bucket **mean** by more than `sigma × stdev` (Phase-1 default 3.0–4.0).
Mean/σ statistics have a well-known failure mode that our own test
suite documents (`outliers_test.go`: "a single huge outlier inflates σ
enough that it can't remove itself"): both the mean and σ are computed
FROM the contaminated sample, so a large-enough outlier *masks itself*
— and two coordinated outliers mask each other even more effectively.
For a pricing API this is not a corner case, it is the adversarial
case: wash-trades or fat-finger fills on a thin pair are precisely the
inputs an outlier filter exists for, and precisely the inputs that
defeat a σ-filter.

The robust-statistics standard replacement is the **median absolute
deviation (MAD)**: median and MAD have a 50% breakdown point — up to
half the sample can be arbitrarily corrupted without moving the
estimate — versus 0% for mean/σ (one point suffices).

## Decision

### 1. Modified z-score on log-prices

Replace the σ-test with the Iglewicz–Hoaglin modified z-score,
computed on **log-prices**:

```
mᵢ = 0.6745 · |log pᵢ − median(log p)| / MAD(log p)
drop trade i when mᵢ > k
```

Log-space because price noise is multiplicative (a 2× and a ½×
deviation should be equally outlying); the σ-filter's linear space
treats the 2× as twice as deviant as the ½×.

Float64 projection remains acceptable (the filter is a heuristic
gate, not a value-serving computation — same rationale as today).

### 2. Degenerate-case policy

- `n < 5`: no-op (return input unchanged). Mirrors today's `n < 3`
  guard, raised because a median over 3–4 points is too coarse to
  reject on; a thin bucket's price signal is already degraded and the
  confidence machinery (not the outlier filter) is the honest channel
  for that.
- `MAD == 0` (≥ 50% of prices identical — common in thin buckets and
  in oracle-adjacent pairs): fall back to an exact tolerance band —
  drop only trades where `|log pᵢ − median| > log(1 + ε)` with ε
  generous (placeholder 25%). Never divide by zero, never drop the
  identical majority.
- All inputs zero-base-amount: dropped unconditionally (unchanged).

### 3. Threshold placeholders — tuned only against production traffic

- `k = 3.5` (the Iglewicz–Hoaglin canonical default) as the initial
  placeholder.
- `ε = 0.25` for the MAD==0 band.
- Both become config surface (`[aggregation]` block) with the
  placeholder defaults; the tuning exercise happens against ≥30 days
  of real per-pair trade distributions and is out of scope here.

### 4. Rollout: shadow first, serve second

Phase A (**shadow**): compute BOTH filters per bucket; serve the
σ-filter result unchanged; emit a per-pair disagreement metric
(`aggregator_outlier_filter_disagreement_total{pair,direction}`) when
the two filters drop different trade sets. Zero serving risk.

Phase B (**flip**): after the disagreement rate is understood and k/ε
are tuned, serve the MAD result and delete the σ path. The flip is a
config default change plus a CHANGELOG entry, not a code rewrite.

### 5. Explicitly not in scope

- **Volume-weighted median**: rejected for now — weighting the median
  by trade size re-opens a manipulation channel (one large wash trade
  drags the weighted median) that the unweighted median is immune to.
  Revisit only with evidence that small-trade noise dominates a pair.
- Per-source trust weighting (BACKLOG #44's weighted-VWAP sibling) —
  separate concern, separate decision.
- The freeze/anomaly machinery (`internal/aggregate/freeze`) — the
  outlier filter drops individual trades inside a bucket; freeze
  handles whole-pair anomalies. Unchanged interfaces.

## Consequences

- The masked-outlier class (self-hiding single spike, coordinated
  pair) is closed by construction: breakdown point goes 0% → 50%.
- Thin-bucket behaviour is explicitly specified (no-op under 5
  trades; identical-majority protected under MAD==0) instead of
  emergent.
- Shadow mode means the first production effect is a *metric*, not a
  served-price change — tuning happens with evidence, and the
  disagreement metric doubles as the tuning dataset.
- One more config knob; mitigated by shipping placeholders that match
  the literature defaults.

## Sign-off checklist (@ash)

- [ ] Log-space modified z-score as the mechanism
- [ ] Degenerate-case policy (§2)
- [ ] Shadow-first rollout (§4)
- [ ] Volume-weighted median rejected for v1 (§5)
