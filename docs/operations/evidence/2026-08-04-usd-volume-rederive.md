---
title: Evidence — 2026-08 usd_volume re-derive + CAGG rebuild (tier-3b poisoning remediation)
last_verified: 2026-08-04
status: executed
---

# 2026-08-04: the tier-3b re-derive, executed

Runbook: [usd-volume-rederive-2026-08.md](../usd-volume-rederive-2026-08.md).
This is the execution record + acceptance evidence.

## What ran

- **v0.25.0** released + deployed to r1 (indexer, aggregator, api, ops,
  migrate) at 2026-08-04 ~17:30 CEST. Post-deploy battery: edge smoke
  13/13, route sweep clean after cache warmup (sole residue:
  `/accounts/{g}/trades` deep-history — the pre-existing product
  decision, unchanged), supply reconcile 8/8 in tolerance, migration
  0134 applied (194,084/194,084 unique slugs).
- **Range pinned**: on-chain ledgers **63,587,177 → 63,797,298**
  (`ts >= 2026-07-22`, right edge at deploy). 12.36M on-chain rows
  (sdex 12.26M, aquarius 93k, soroswap/phoenix/comet/defindex/blend
  the rest); 2.58M base-native.
- **Chunk prep**: `_hyper_1_31953_chunk` (07-16→07-23, 1.8 GB
  compressed / 31 GB raw) decompressed first; compression policy 1000
  paused for the run, re-enabled after (backlog self-drains).
  Calibration: 2k ledgers took 52+ min INTO the compressed chunk vs
  **36 s** after decompression (~100×).
- **Re-derive**: 5 `ch-rebuild -sdex -write` windows (≤50k ledgers)
  under `run-heavy-job.sh`, oldest→newest, `prices_1m` refreshed per
  window (tier-3b circularity break). Wall: **1h44m**
  (17:00:47Z → 18:44:44Z), zero errors, generation stamped per window.
- **CAGG rebuild**: prices_1m/15m/1h/4h/1d/1w, twap_1h/1d,
  dex_volume_by_pair_1d, source_volume_1h, pools_per_source_1h over
  [2026-07-21, 2026-08-06]; prices_1mo re-run over
  [2026-06-01, 2026-10-01] (its 2-bucket minimum window — the padded
  16-day span was too small for a monthly CAGG).

## Acceptance gates

**Gate 1 — verify-usd-volume, 14 days (07-21 → 08-03): 0 violations.**
Exact tiers AND the new XLM-BASE BOUND, every day clean. (The bound's
FIRST run flagged 39 "violations" — all kraken/bitstamp XLM/EUR+GBP at
ratio ≈ 0.100: the CHECK had hardcoded the on-chain stroop scale for a
1e8-scaled CEX base leg. Fixed to per-connector subclass dispatch —
the CS-040 lesson, again — and re-run clean. On-chain groups never
violated under either scaling.)

**Gate 2 — fleet-wide XLM-leg magnitude (trailing 24h, base-native
on-chain, usd_volume non-null):**

| | sum(usd_volume) | sum(XLM leg × XLM/USD) | >10× over | >10× under |
|---|---|---|---|---|
| before | **$21,786,924.90** | $631,433.96 | 198 | 10 |
| after | **$601,674.15** | $601,337.05 | **0** | **0** |

Agreement to **0.06%**. ~$21.2M/day of fabricated volume removed.

**Gate 3 — determinism**: window 5 (63,787,177–63,797,298, 488,426
rows) fingerprinted (md5 over ordered
source|ledger|tx|op|usd_volume|amounts) across three runs. Run 1→2
DIFFERED (fingerprint `1a488a12…` → `97439717…`, same row count) —
expected, not a defect: run 1 executed before the full CAGG rebuild
and the bridge tier reads `prices_1m.volume_usd`, which the rebuild
corrected in between; the environment changed BY DESIGN. Run 2→3, in
the frozen environment, was **byte-identical** (`97439717…` both
times). Lesson folded forward: byte-determinism claims only hold with
frozen inputs — fingerprint AFTER the final CAGG state, not across it.

**Convergence pass**: because windows 1–4's bridge-tier rows were
derived against progressively-corrected `prices_1m`, the full 5-window
orchestrator ran a SECOND time against the stable post-rebuild CAGGs
(19:25→21:22 UTC, zero errors), followed by a final 12-refresh CAGG
sweep. Post-convergence magnitude (fresh trailing-24h window):
$626,758.60 reported vs $626,339.60 XLM-leg — **0.07% agreement,
0 outliers** — the corrected state is stable under re-derivation.

**Gate 4 — served surfaces**: every `XRP-G…` look-alike serves
`market_cap_usd: null` + `unverified_ticker_collision: true` (two of
three also substance-withheld their price); `USDT-GCQTGZQQ…` carries
the restored reference-only warning ("NO verified issuance on
Stellar"); dust pairs 404 `errors/price-withheld`; XLM serves fresh
closed-bucket VWAP;
`stellarindex_price_serve_substance_withheld_total` counting per
surface.

## Left open (unchanged by this run)

- `/accounts/{g}/trades` deep-history windowing (product decision).
- Pre-07-23 pegged-row restamp tool (launch-plan queued buildable).
- Compression backlog: chunks touched by the run recompress via policy
  1000 over the following hours — check `compression-lag` if still
  uncompressed after a day.
- CI infra on main: `web/explorer` types-drift false positive +
  vitest `markAsUncloneable` environment error + `web/status`,
  `integration tests (Docker)` — all failing identically BEFORE this
  work; need their own fix.
