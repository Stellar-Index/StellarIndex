---
title: verify-usd-volume — first 30-day production report (calibration input)
captured: 2026-07-30 ~00:20Z on r1 (v0.21.4 ops binary)
command: stellarindex-ops verify-usd-volume -config /etc/stellarindex.toml -days 30
verdict: ✅ FULLY CLEAN — current pipeline exact AND the historical era re-stamped 2026-07-30 (13.3M rows): all 66 formerly-dirty days re-verify at 0 violations
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
- ~~The pre-07-23 window is flagged, not yet fixed.~~ FIXED — see the follow-up below.

## 2026-07-30 follow-up: historical re-stamp EXECUTED — 0 violations remain

The recommended re-stamp ran the same night. Full-history day-sweep first
mapped the true dirty era: **[2026-05-12, 2026-07-22], 66 dirty days,
24,396 violating group-days** (not just the 30-day window above; 2026-05-01
and earlier are clean). Every violation was one homogeneous class —
`[base_pegged] sdex` with base `USDC-GA5Z…` — so tier 2b's identity
(`usd_volume = base_amount / 10^7`, DEX classic decimals) applied as a
scoped day-chunked SQL UPDATE is exactly the verifier's own `expected`,
with no waterfall reimplementation (the single-entry peg list makes
quote-side precedence unreachable for these rows).

Result: **13,308,993 rows re-stamped across 72 days; acceptance sweep of
all 66 formerly-dirty days = 0 exact-tier violations** (`/root/restamp-sql.log`).

Route not taken, for the record: `ch-rebuild -sources sdex -sdex` is the
general-purpose correction (derive_generation upsert) and was tried first,
but measured ~47 h for this span (620 rows/s upserting into the
compressed-chunk era) — and taught three operational lessons: the sdex op
read OOMs the 10 GiB client pin above ~50k-ledger windows (tx-set then
join); failed `run-heavy-job.sh` runs can leave stale locks that silently
SKIP a relaunched window (use unique job names per attempt); and DML on
compressed Timescale chunks needs
`SET timescaledb.max_tuples_decompressed_per_dml_transaction = 0`
per-session (default cap 100k tuples, a single day needed 265k).
