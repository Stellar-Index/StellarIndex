---
title: Finding — dust trades set OHLC chart extremes
last_verified: 2026-07-24
status: FIXED — migration 0115 deployed, 2× VWAP band removed
---

# Dust trades set OHLC chart extremes

Audit B11-F1. The rationale of record for migration 0115, the removal of
`combinedOutlierBandRatio`, and the dust floors in `usd_fx_resolver.go`.

## Symptom and root cause

The XLM/USD chart showed a low of **$0.1333** against a bar VWAP of $0.1832.
`migrations/0002_create_price_aggregates.up.sql` built OHLC extremes with no
size filter (`max/min(quote_amount / base_amount)`), so every trade set
high/low regardless of notional. The print: pair `USDC-GA5ZSEJY…` / `native`
(reverse direction), base **2 stroops**, quote **15 stroops**, usd_volume
**$0.00000027**, price 15/2 = 7.5. `OHLCSeries` inverts reverse-direction
pairs (`1.0 / NULLIF(high_price, 0)`), so the 7.5 high became a 0.1333 low.

The serve-layer 2× VWAP band (`selectExtreme`) could not catch it: 0.1333 sits
inside the 0.0916 floor, and the band only ever compared whole-constituent
extremes (this constituent: 321 trades, $14,493). Tightening the band would
clip genuine intra-bar moves.

On one day (6,018,245 trades) 24% had `usd_volume < $0.01`.

## Why the dust exists: path-payment remainders

Tx `6231307e…` (ledger 63514245) is one `PathPaymentStrictSend`; the trades
table's `op_index` is the claim-atom index within it.

| hop | sold → bought | usd_volume | leg price |
|---|---|---|---|
| 0 | XLM → BTC | $20.22 | — |
| 1 | USDC → XLM | $0.09 | 5.458 |
| 2 | USDC → XLM | $19.99 | 5.459 ✓ market |
| 3 | USDC → XLM | **$0.00000027** | **7.500** ← the outlier |

Hop 3 is the remainder swept against the next offer. Every claim atom is
recorded as an independent market trade, so path payments structurally produce
these crumbs.

**Open modelling decision:** whether a path payment's intermediate hops should
contribute to price discovery at all, or only the end-to-end rate. It affects
VWAP and volume too; take it deliberately.

## Decision (operator)

**Filter on trade SIZE, never on price divergence.** A real $100k fat-finger
must still show. `combinedOutlierBandRatio` is removed, not retuned: every case
it existed for was dust.

| case | amounts | price | usd_volume | caught by notional floor? |
|---|---|---|---|---|
| $0.56 high wick (S-012 class) | 1 ↔ 1 stroop | 1.000000 | $0.0000001 | ✅ |
| $0.1333 low (this finding) | 2 ↔ 15 stroops | 7.5000 | $0.00000027 | ✅ |
| absurd high | 128 ↔ 4.9e9 stroops | 38,252,788 | $0.0000129 | ✅ |
| hypothetical $100k fat-finger | large | far off market | $100,000 | ❌ — correctly SHOWN |

Amounts are integer stroops, so price carries a quantisation error of about
`1/base + 1/quote`; at 1↔1 or 2↔15 stroops that is 50–100%. A size floor removes
fills below the ledger's own measurement resolution.

| filter (same day) | excluded | share |
|---|---|---|
| `least(base,quote) < 100` stroops | 310,539 | 5.2% |
| `least(base,quote) < 1,000` | 563,764 | 9.4% |
| `least(base,quote) < 10,000` | 1,226,222 | 20.4% |
| `usd_volume < $0.01` | 1,448,695 | 24.1% |

## Fix

- `migrations/0115_ohlc_extremes_notional_floor.up.sql`: open/high/low/close of
  all seven `prices_*` CAGGs take
  `COALESCE(agg(...) FILTER (WHERE usd_volume >= 0.01), agg(...))`; the
  threshold is defined once as `ohlc_extreme_min_usd_volume`. `twap_1h`/`twap_1d`
  are recreated unchanged as dependents of `prices_1m`. The migration is
  `WITH NO DATA`: applying it empties the views (~1.1 TB) until re-materialised
  per the migration header.
- `internal/api/v1/ohlc_fiat_combine.go`: the 2× band removed.
- `test/integration/ohlc_dust_floor_test.go`: the 2↔15-stroop crumb serves
  `0.1333333333` before and `0.1822` after, on every grain.

NULL `usd_volume` never satisfies the filter, so an entirely unpriced bucket
keeps the unfiltered extreme; a mixed bucket takes extremes from priced trades
only. **Open:** a stroop-based floor for unpriced pairs is a separate decision.

Blast radius: every OHLC chart, every pair and grain, both directions. Treat
changes here as a pricing migration and verify against known-good bars.
