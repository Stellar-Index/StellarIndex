---
adr: 0046
title: MAD-based outlier filtering for VWAP inputs
status: Accepted
date: 2026-07-08
supersedes: []
superseded_by: null
---

# ADR-0046: MAD-based outlier filtering for VWAP inputs

## Context

A mean/stdev filter computes both from the contaminated sample, so one large outlier, or two coordinated ones, hides itself; wash trades on thin pairs are exactly that case.
Median and MAD have a 50% breakdown point.

## Decision

1. Outliers are found with an exact `*big.Rat` median and 1.4826 times the MAD of prices (`internal/aggregate/robust.go`). A price is dropped when its deviation exceeds sigma times that scale. The deviation is measured in ratio space (a price below the centre is mirrored to `centre²/p`), so a half-size print is exactly as outlying as a double-size one. The scale is a price-space MAD, not a log-space one.
2. Degenerate cases. Fewer than 3 usable prices, or sigma at or below 0, is a no-op. Zero-base or zero-quote trades are dropped first. When MAD is 0 the scale falls back to 0.5% of the centre (a ±2% band at sigma 4), so an identical majority is never dropped and honest dispersion is not collapsed.
3. The published VWAP uses the time-local layer (`FilterOutliersLocal`, `internal/aggregate/outliers_local.go`, wired in `internal/aggregate/orchestrator/orchestrator.go`), which scores each print against the window band and its time neighbourhood so an agreed regime shift survives. `aggregate.outlier_sigma_threshold` (default 4) configures only that layer.
4. The whole-window `FilterOutliers` has no config key. `/v1/vwap` applies it only when the request passes `outlier_sigma`, which defaults to 0. `/v1/ohlc` and `/v1/twap` default to the constant `ohlcDefaultOutlierSigma` (4.0), which matches the config default by convention only.
5. No volume-weighted median, since one large wash trade would drag it. Instead a trim whose survivors hold less base volume than the prints it dropped returns an empty slice and the window is withheld (`keepIfVolumeMajority`).

## Invariant

- The filter is robust to self-masking outliers and symmetric in ratio space: `internal/aggregate/outliers_test.go`, `internal/aggregate/band_symmetry_test.go`.
- A volume minority never sets the price: `internal/aggregate/outliers_volume_test.go`.
- Money-path statistics are exact rationals, never `float64` (AGENTS.md money rule); only sigma is a float, converted before it touches a price.

## Consequences

- The self-masking class is closed by construction.
- Thin buckets are specified, not emergent.
- Two outlier surfaces exist (published local layer, per-request whole-window); `docs/methodology/vwap-aggregation.md` describes both.
- Per-source trust weighting and the freeze/anomaly machinery stay separate decisions.

## Evidence

`internal/aggregate/outliers.go`, `internal/aggregate/robust.go`, `internal/aggregate/outliers_local.go`, `internal/api/v1/ohlc.go`.
