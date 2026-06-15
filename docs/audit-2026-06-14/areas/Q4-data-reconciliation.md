# Q4 — External data reconciliation (served data vs independent ground truth)

**Date:** 2026-06-15 · **Method:** sampled r1's live served API (`localhost:3000`)
and diffed each value against an INDEPENDENT source — Horizon
(`horizon.stellar.org`, network ground truth for ledger fields) and CoinGecko
(`api.coingecko.com`, market ground truth for price/supply/market-cap). This is
the "is the data RIGHT" check the rest of the audit (which verified the code) did
not cover. Sample is small but spans the load-bearing surfaces: ledger fidelity,
price, and supply/market-cap.

## Results

| # | metric | r1 served | independent truth | source | Δ | verdict |
|---|---|---|---|---|---|---|
| 1 | ledger 63039000 `total_coins` | 105,443,902,087.3472865 XLM | 105,443,902,087.3472865 | Horizon | 0 | ✅ exact (to the stroop) |
| 2 | ledger 63039000 `fee_pool` | 9,968,273.0312869 XLM | 9,968,273.0312869 | Horizon | 0 | ✅ exact |
| 3 | XLM `price_usd` | $0.1904186531 | $0.189895 | CoinGecko | +0.27% | ✅ excellent |
| 4 | USDC `price_usd` | $0.999728 | ≈$1.00 (peg) | CoinGecko id usd-coin | — | ✅ peg holds |
| 5 | XLM `total_supply` | 50,001,806,812 XLM | 50,001,786,840 | CoinGecko | +0.00004% | ✅ exact (5 sig-fig) |
| 6 | **XLM `circulating_supply`** | **50,001,806,812 XLM** | **33,768,287,738** | CoinGecko | **+48.1%** | ❌ **overstated** |
| 7 | **XLM `market_cap_usd`** | **$9,510,507,806** | **$6,428,218,680** | CoinGecko | **+47.9%** | ❌ **overstated (follows #6)** |

## What reconciled (the reassuring part)

- **Ledger substrate is faithful to the chain.** `total_coins` + `fee_pool`
  match Horizon to the stroop — the lake → ClickHouse → explorer → API path
  reproduces the raw ledger exactly (and corrected my own prior assumption that
  mainnet total_coins was ~50B; it is genuinely 105.4B — the 2019 SDF reduction
  did not lower the on-chain `total_coins` field).
- **The pricing pipeline is accurate.** XLM at +0.27% vs CoinGecko and USDC
  holding its peg validate the VWAP/aggregation → `/v1/price` path against the
  real market. This is the flagship product working correctly.
- **Total supply is right.** r1's `xlm_sdf_reserve_exclusion` total (50.0B)
  matches CoinGecko's total to 5 significant figures.

## The finding — XLM circulating + market cap overstated ~48%

`circulating_supply == total_supply` for XLM (both 50.0B). The methodology
subtracts the SDF *reserve* to get from on-chain `total_coins` (105.4B) down to
`total_supply` (50.0B) ✅ — but applies **no further exclusion for the SDF's
non-circulating operational/locked holdings** (~16.2B XLM) that the market (and
SDF's own published circulating figure) excludes. So `circulating` is really
`total`, and `market_cap_usd` (the headline number on the pricing API + the
explorer asset page) is **~48% too high for XLM** ($9.5B served vs $6.4B real).

This **concretely quantifies the A07 supply-basis Medium** (the "exclusion label
asserts an exclusion that wasn't fully applied" class) on the single highest-
visibility asset. Severity: **Medium** — it's a headline-number accuracy gap,
not a correctness/data-loss bug, and `price` (the core product) is accurate.

**Fix path (methodology, not a hot-fix):** incorporate SDF's published
non-circulating account set into the XLM circulating derivation (the
`[supply].PerAsset` LockedSet is the existing knob — populate it with the SDF
mandate accounts), OR, until then, relabel the basis so consumers know the
served `circulating` for XLM is "total − SDF reserve," not market-circulating.
Per the repo's "don't read policy into artifacts" norm this is flagged for an
explicit methodology decision rather than silently redefined here.

## Not yet reconciled (scope honesty)

Single-sample-per-surface. A fuller pass would diff: per-Stellar-asset supply
for the top-N verified assets vs Stellar Expert; 24h trade counts/volume per
protocol vs Stellar Expert's DEX stats; and OHLC history points vs an exchange's
historical candles. Recommended as a recurring reconciliation job (the
`internal/divergence` package already does a live price-only cross-check vs
CoinGecko/Chainlink — extend it to supply + counts).
