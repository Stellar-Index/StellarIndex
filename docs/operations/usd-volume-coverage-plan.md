---
title: USD volume coverage — 100% CEX / 99.5% SDEX
last_verified: 2026-07-22
status: diagnosed; plan agreed; implementation pending
---

# USD volume coverage

## The bar (operator, 2026-07-22)

- **100%** of external-exchange (CEX/FX) trades priced in USD.
- **99.5%+** of SDEX trades priced in USD.
- Unpriced is acceptable ONLY for genuinely illiquid Stellar tokens, and must be
  a visible, measured exception — never a silent NULL.

## Why this matters more than it looks

`usd_volume` is the universal denominator for every volume surface we publish:
DEX volumes, asset volumes, venue rankings, market share, transaction and
transfer volumes. A NULL does not merely omit a row — it **silently deflates
every aggregate built on it**.

Measured on 2026-07-17 (one day): **424,825 of 6,018,245 trades (7.06%) have NULL
`usd_volume`**, and they are overwhelmingly NOT dust:

| source | NULL | of which non-dust |
|---|---|---|
| bitstamp | 10,916 | 10,764 (99%) |
| binance | 35,211 | 34,233 (97%) |
| aquarius | 6,172 | 5,991 (97%) |
| kraken | 46,151 | 43,299 (94%) |
| coinbase | 51,965 | 47,109 (91%) |
| sdex | 274,175 | 112,841 (41%) |

Unpriced volume that day, valued at rates we already hold:

| pair | quote volume | ≈ USD |
|---|---|---|
| BTC/EUR | €513,307,246 | ~$585.5M |
| ETH/EUR | €220,853,052 | ~$251.9M |
| BTC/GBP | £47,871,447 | ~$64.0M |
| ETH/GBP | £24,933,618 | ~$33.3M |
| XLM/EUR | €3,438,241 | ~$3.9M |
| XLM/GBP | £237,157 | ~$0.3M |
| **total** | | **~$939M** |

against **$3.167B counted** ⟹ we understate total volume by **~23%**.

Worse, the gap is **uneven** (1.4% of binance trades vs 21.9% kraken vs 65.5%
aquarius), so venue comparisons and market-share figures are biased toward
venues that happen to quote in USD.

## Root cause

`tradeUSDVolume` (`internal/storage/timescale/trades.go`) already implements a
three-tier waterfall:

1. `usdVolumeDecimals` — quote is USD / USD-pegged
2. `tradeUSDVolumeViaFX` — `VWAPUSDFXResolver.USDPriceAt(quote, ts)`
3. `tradeUSDVolumeViaXLMBaseAnchor` — XLM-base anchor

The tiers are sound; **tier 2 queries the wrong table**. `VWAPUSDFXResolver`
looks up `<asset>/<peg>` in `prices_1m`, but `prices_1m` contains only
crypto/fiat pairs — there is no `fiat:EUR/fiat:USD` row and never will be.
Verified: zero rows.

The rate is sitting in **`fx_quotes`** (`ticker`, `rate_usd`, `inverse_usd`,
`observed_at`), refreshed daily, with **6,462 observations per ticker back to
2026-05-10**. `Store.FXQuoteAtOrBefore` already reads it — the trade-insert path
simply never consults it for a fiat quote.

So BTC/EUR fails all three tiers: not USD (1), no `fiat:EUR/fiat:USD` in
prices_1m (2), base is BTC not XLM (3) ⟹ NULL.

## Plan

### Tier 2a — fiat quotes via `fx_quotes` (fixes ~100% of the CEX gap)
When the quote is `fiat:*`, resolve `inverse_usd` at-or-before the trade
timestamp via `FXQuoteAtOrBefore` and multiply. This alone should close BTC/EUR,
ETH/EUR, BTC/GBP, ETH/GBP, XLM/EUR, XLM/GBP — i.e. essentially the entire
external-exchange shortfall.

### Tier 2b — base-side pegs (fixes `USDC/YxT`-class)
The waterfall only ever inspects the QUOTE. When the **base** is USD/USD-pegged,
`usd_volume = base_amount` directly. Cheap, exact, and catches 6,193 trades/day
on one pair alone.

### Tier 3b — XLM bridge for SDEX token/token (the path to 99.5%)
Most Stellar tokens have an XLM market. For a token/token trade where neither
side is priceable directly, value via `TOKEN/XLM × XLM/USD` at the trade
timestamp. Prefer the side with the deeper XLM market; require the bridge
quote to be non-dust (see the dust finding) so a crumb can't set a valuation.

### Tier 4 — direct USD price for either side
If either asset has any USD price at that timestamp (CEX, oracle, or SDEX
USD-peg market), value the trade with it.

### Measurement + enforcement (non-negotiable)
- Emit `stellarindex_trade_usd_volume_unpriced_total{source,subclass}` and a
  coverage ratio per source.
- Alert when CEX coverage < 100% or SDEX coverage < 99.5% over a window.
- Add the unpriced ratio to the completeness/verdict surface so it is part of the
  go-live gate, not a side metric.

### Backfill
Forward-fix alone leaves history wrong. `fx_quotes` reaches back to 2026-05-10,
so historical trades can be revalued at point-in-time rates. Follow the existing
`scripts/ops/recompute-usd-volume-soroban.sql` precedent. Trades older than the
FX history need a documented policy (nearest-available rate, or leave unpriced
and exclude from historical volume claims).

## Correctness constraints

- **Point-in-time rates, always.** Value each trade at the rate at its ledger
  close time, never spot — otherwise historical volumes silently rewrite
  themselves as rates drift.
- **Do not retroactively change a depeg.** Existing behaviour trusts the peg at
  insert time and records the observed quote amount; keep that.
- **Bridge quotes must be non-dust**, or a 2-stroop crumb could set the
  valuation rate for a real trade (see
  `finding-dust-trades-set-chart-extremes.md`).
