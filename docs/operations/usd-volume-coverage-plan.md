---
title: USD volume coverage — 100% CEX / 99.5% SDEX
last_verified: 2026-07-22
status: IMPLEMENTED + verified live (v0.19.0); verdict-surface gate outstanding (INV-0975)
---

# USD volume coverage

## The bar (operator)

- **100%** of external-exchange (CEX/FX) trades priced in USD.
- **99.5%+** of SDEX trades priced in USD.
- Unpriced is acceptable ONLY for genuinely illiquid Stellar tokens, as a
  visible, measured exception, never a silent NULL.

## Outcome (measured live, v0.19.0)

Coverage of trades in the first hours after deploy vs the 2026-07-17 baseline:

| source | before | after | bar |
|---|---|---|---|
| binance | 98.626% | **100.000%** | 100% |
| coinbase | 96.319% | **100.000%** | 100% |
| kraken | 78.118% | **100.000%** | 100% |
| bitstamp | 91.273% | **100.000%** | 100% |
| sdex | 83.848% | **99.734%** | 99.5% |

The remaining unpriced tail is intended: obscure tokenised gold/equity wrappers
(`USGOLD/GOLDRESERVE`, `HD/IBM`, `RUGOLDMINE/GOLDRESERVE`, `ATME/FiaT`) with no
XLM market or priceable counterparty, plus pure SEP-41/SEP-41 pairs, excluded
**deliberately**: `decimals()` is per-contract and not plumbed through the
trade-insert path, so assuming 1e7 would be a silent money bug, worse than NULL.

## Why it matters

`usd_volume` is the denominator of every volume surface (DEX, asset, venue
rankings, market share, transaction/transfer volumes). A NULL **silently
deflates every aggregate built on it**.

2026-07-17, one day: **424,825 of 6,018,245 trades (7.06%) had NULL
`usd_volume`**, mostly not dust:

| source | NULL | of which non-dust |
|---|---|---|
| bitstamp | 10,916 | 10,764 (99%) |
| binance | 35,211 | 34,233 (97%) |
| aquarius | 6,172 | 5,991 (97%) |
| kraken | 46,151 | 43,299 (94%) |
| coinbase | 51,965 | 47,109 (91%) |
| sdex | 274,175 | 112,841 (41%) |

Unpriced volume valued at held rates: BTC/EUR €513,307,246 (~$585.5M), ETH/EUR
€220,853,052 (~$251.9M), BTC/GBP £47,871,447 (~$64.0M), ETH/GBP £24,933,618
(~$33.3M), XLM/EUR €3,438,241 (~$3.9M), XLM/GBP £237,157 (~$0.3M); total
**~$939M** against **$3.167B counted**, so total volume was understated by
**~23%**. The gap was uneven (1.4% of binance trades vs 21.9% kraken vs 65.5%
aquarius), biasing venue comparisons toward USD-quoting venues.

## Root cause

`tradeUSDVolume` (`internal/storage/timescale/trades.go`) had a three-tier
waterfall: (1) `usdVolumeDecimals`, quote is USD-pegged; (2)
`tradeUSDVolumeViaFX`, `VWAPUSDFXResolver.USDPriceAt(quote, ts)`; (3)
`usdVolumeViaXLMBaseAnchor`. Tier 2 queried the wrong table: it looks up
`<asset>/<peg>` in `prices_1m`, which holds only crypto/fiat pairs (no
`fiat:EUR/fiat:USD` row, zero rows verified). The rate lives in **`fx_quotes`**
(`ticker`, `rate_usd`, `inverse_usd`, `observed_at`; refreshed daily; 6,462
weekday buckets across 132 tickers back to **2001-05-11**), which
`Store.FXQuoteAtOrBefore` reads but the insert path never consulted for a fiat
quote. BTC/EUR therefore failed all three tiers.

## Plan (all shipped v0.19.0 unless marked)

- **Tier 2a, fiat quotes via `fx_quotes`:** for a `fiat:*` quote, multiply by
  `inverse_usd` at-or-before the trade timestamp (`FXQuoteAtOrBefore`). 100% of
  the 2026-07-17 external-exchange NULLs were fiat-quoted (`fiat:EUR` 121,409 +
  `fiat:GBP` 22,834), so this alone took all four CEX venues to 100%.
- **Tier 2b, base-side pegs:** when the BASE is USD-pegged,
  `usd_volume = base_amount` (43,277 on-chain trades on 2026-07-17).
- **Tier 3b/4b, XLM bridge, both legs (path to 99.5%):** value via
  `TOKEN/XLM × XLM/USD` at trade time; prefer the side with the deeper XLM
  market; the bridge quote must be non-dust. Quote leg alone reached 87.5% of
  the remaining unpriced; adding the BASE leg (old XLM-only base anchor widened
  to any classic asset) reached **99.2%**. The bridge has a 24h freshness
  window, not the direct market's 1h (69.9% vs 89.5% at 24h): the tokens it
  prices trade $8-$220 per day, and sub-cent valuations make a stale rate a
  rounding error while a NULL drops the row.
- **Tier 4, direct USD price for either side:** any USD price at that timestamp
  (CEX, oracle, SDEX USD-peg market).

### Measurement + enforcement

`stellarindex_trade_inserts_total{source,usd_volume_populated}` is emitted in
`InsertTrade`/`BatchInsertTrades`, the choke point every trade passes exactly
once (it had been emitted from the sink's `persistTrade`, blind to the
dispatcher batch path and all external connectors). Alerts
`stellarindex_cex_usd_volume_coverage_low` and
`stellarindex_onchain_usd_volume_coverage_low` enforce the two bars
(`configs/prometheus/rules.r1/usd-volume-coverage.yml`).

**Outstanding (INV-0975):** fold the unpriced ratio into the completeness/verdict
surface so it gates go-live.

### Backfill

Rows older than the v0.19.0 deploy keep their NULLs until re-derived with
`stellarindex-ops usd-volume-restamp` at point-in-time rates (procedure:
[usd-volume-rederive-2026-08.md](usd-volume-rederive-2026-08.md); INV-0974 done).
`fx_quotes` reaches 2001, so FX history does not limit it.

## Correctness constraints

- **Point-in-time rates, always.** Value each trade at its ledger close time,
  never spot, or historical volumes rewrite as rates drift.
- **Do not retroactively change a depeg.** Insert-time behaviour trusts the peg
  and records the observed quote amount; keep that.
- **Bridge quotes must be non-dust** so a 2-stroop crumb can't set a valuation
  rate (see `finding-dust-trades-set-chart-extremes.md`).
