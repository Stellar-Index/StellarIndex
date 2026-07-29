---
title: Evidence — verify-usd-volume first 30-day production run
last_verified: 2026-07-29
status: filed — calibration input + one coherent methodology finding
---

# verify-usd-volume -days 30 — 2026-07-29 ~08:50Z (2026-06-29 → 2026-07-28)

**Command:** `stellarindex-ops verify-usd-volume -days 30` on r1
(report-only; full log `/var/log/verify-usd-volume-30d.log` on r1).

## Result: 8,397 exact-tier violations over 30 days — ONE coherent class

Every violation is `[base_pegged]` on `sdex` with **USDC as the BASE
asset**: the verifier expects `usd_volume == base_amount` exactly (the
$1-peg assumption, slack=0), while stored values track the trade's
market-rate valuation. Deltas are typically **<1% relative** (stored
mostly slightly HIGH); worst single-group daily |Δ| observed $432–$1,858
(the USDC/native group). This is a **methodology divergence between
insert-time valuation and the verifier's peg assumption**, not data
corruption — consistent with the standing policy that stablecoin≈fiat
binding happens at the aggregator layer, late, while the verifier
hard-codes the peg.

**Disposition (next-release queue, not launch-blocking):** decide the
canonical convention — either the verifier gains a peg-wobble slack
(~1%), or base_pegged insert-time valuation adopts the peg. Until then
exact-tier violations of this shape are EXPECTED report noise.

## Calibration data (the run's actual purpose — C4-055/066 alert)

Per-day tier magnitudes (30-day shape, representative days):

| tier | groups/day | sum(usd_volume)/day |
|---|---|---|
| quote_pegged | ~850–870 | **~$3.5–3.9 B** |
| base_pegged | ~670–790 | ~$1–2.4 M |
| estimated | ~28.7–29.2 k | ~$0.4–0.9 M |

The estimated (tier-3/4) mass is **~4 orders of magnitude below** the
exact quote_pegged mass, and its day-to-day swing is ~2×. Alert
recommendation: threshold the estimated-tier daily sum at ~10× its
observed ceiling (≈$10 M/day) as a manipulation/overrun tripwire — far
above organic variance, far below where it could distort headline
volume. (Per the tool's own warning, do NOT land an uncalibrated bound —
this table IS the calibration basis.)

**What this does NOT prove:** correctness of estimated-tier values
(unpriceable by construction), CEX usd_volume (separate coverage
alerts), or anything before 2026-06-29.
