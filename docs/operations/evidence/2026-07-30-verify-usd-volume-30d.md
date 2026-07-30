---
title: verify-usd-volume — first 30-day production report (calibration input)
captured: 2026-07-30 ~00:20Z on r1 (v0.21.4 ops binary)
command: stellarindex-ops verify-usd-volume -config /etc/stellarindex.toml -days 30
verdict: 🔵 current pipeline EXACT (7 consecutive clean days); 7,973 violations all bounded to the pre-2026-07-23 stamping era — historical re-stamp recommended, not an active bug
---

# verify-usd-volume — 30-day production report

The prove-it battery's last unfiled row (three generations of plans
called for this run; this is the first).

## Exact tiers (quote_pegged / base_pegged): identity holds NOW

Per-day exact-tier violations over [2026-06-30, 2026-07-29]:

- **2026-06-30 → 2026-07-22: violations EVERY day** (226–587/day,
  7,973 total). All are `base_pegged` SDEX dust groups
  (`USDC/<long-tail-token>`), stored ≈ expected × ~1.007 — a
  consistent ~+0.7% drift.
- **2026-07-23 → 2026-07-29: ZERO violations, seven consecutive
  days.** The cutoff is sharp and aligns with the current valuation
  path deploying.

Disposition: rows stamped before 2026-07-23 were valued by the
pre-peg-identity path (resolver-priced USDC instead of the $1 peg
identity `usd_volume = pegged_leg / 10^decimals`); the honest verifier
flags them retroactively. Magnitude is bounded and small — worst
single-group daily |Δ| = 1,858 USD (USDC/native), typical deltas are
sub-cent on dust groups; base_pegged daily sums are ~0.3% of
quote_pegged's. **Recommendation**: a windowed historical re-stamp of
pegged-tier rows before 2026-07-23 (needs a small ops tool that reuses
`USDVolumeQuoteSpec` — do NOT hand-write SQL that re-implements the
peg waterfall; that is the reimplementation trap the verifier itself
documents). Until then, pre-07-23 pegged usd_volume carries ≤~0.7%
overstatement on affected groups.

## Estimated tiers (FX / XLM-anchor): calibration numbers

30-day daily-sum spread (USD):

| tier | min | mean | max |
|---|---|---|---|
| quote_pegged | 1.39B | 2.96B | 4.34B |
| base_pegged | 255k | 10.1M | 56.0M |
| estimated | 167k | 23.7M | 117M |

The estimated tier is 0.006%–8% of the day's pegged volume (median
~0.8%) — small but with a 700× daily range, so an absolute-sum alert
would be noise. **Calibration recommendation for C4-055/066**: keep the
existing coverage-ratio alerts as the primary signal and do NOT add an
estimated-tier sum threshold — the measured spread shows no stable
band to bound. Re-visit after 90 days of data if the estimated share
trends up.

## What this does NOT prove

- Tier-3/4 (estimated) VALUES are measured, not judged — the tool is
  explicit that no threshold exists for them yet.
- Coverage (rows with NULL usd_volume) is the standing
  `stellarindex_{cex,onchain}_usd_volume_coverage_low` alerts' job,
  not this check's.
- The pre-07-23 window is flagged, not yet fixed.
