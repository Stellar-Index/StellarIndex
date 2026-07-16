# Recon: money / pricing / aggregation / supply (HEAD f84e2d0b) — the diligence-bar surface

## Value representations (the canonical spine is exact; boundaries are strings)
- `canonical.Amount` wraps *big.Int → NUMERIC (decimal string via Valuer) → JSON STRING always (512-char DoS cap). trades base/quote_amount = smallest-unit ints, NO decimals column (scale is per-source convention). oracle Price=Amount + Decimals uint8. Supply *big.Int. CAGGs read back as string (vwap::text). Orchestrator VWAP = exact big.Rat → 12dp truncate-toward-zero string. CH supply_flows Int128, sums in Int256.
- i128/u128/u256 → Amount via hi/lo, never int64. SDEX int64 sign-extends into i128 (correct).

## float64 sites that LAND in stored/served money (FLAG list for finders)
- `fx_quotes.RateUSD/InverseUSD float64` → NUMERIC (worker InverseUSD=1.0/RateUSD). BUT pricing-critical read parses rate_usd::text into exact big.Rat.
- `/v1/price` fiat cross-rate fallback: `cross := rateQuote/rateAsset` float64 → served, labelled "vwap" (price.go:883-912).
- Fiat market-cap chart all-float64 (chart.go:852-888) vs CRYPTO mcap exact big.Rat (chart.go:1046-1065) — two precision regimes, same field.
- Fiat headline mcap uses big.Float-128 not big.Rat (assets_global.go:327).
- **`/v1/changes` serves ALL money as JSON float NUMBERS** (changes.go:33-55) — the one API violating strings-only.
- Coinbase backfill (IncludeInVWAP:true) candle amounts float64→FormatFloat(8)→big.Int — the only VWAP-contributing trade-amount path transiting float (lossless <2^53).
- Aggregator/authority oracle prices (coingecko/cmc/cryptocompare/ecb) float64→scale (IncludeInVWAP:false, feed divergence/global tiers only).
- price_source_contributions VolumeUSD/Weight float64 → NUMERIC.

## Aggregation math
- **VWAP** Go = exact Σq/Σb big.Rat, normalized by decimals immediately (AdjustPrice, K=10^(baseDec−quoteDec)). **SQL CAGG** = sum((q/b)*b)/sum(b) — algebraically Σq/Σb but per-row NUMERIC division re-multiplied injects per-row rounding; NO decimals adjust, NO outlier/volume/freeze filter in CAGG → GuardServedVWAP patches at serve time.
- **TWAP** Go = time-weighted Σ(price·Δt)/ΣΔt (needs pre-sorted, doesn't sort). **CAGG `twap` column = avg(q/b) = EQUAL-WEIGHT mean, NOT time-weighted** (migrations 0002:43). Real time-weighted TWAP only at 1h/1d (0081).
- OHLC first/last/max/min of exact ratios (sorted-input contract).
- **Direction-combining** (every CAGG read, SDEX stores both orientations): flipped rows contribute 1/vwap weighted by trade_count. Trade-count-weighted mean of {vwap, 1/vwap_flipped} ≠ exact union VWAP (approximation, bounded by spread).
- Stablecoin→fiat (stablecoin.go:24-37): USDT/USDC/DAI/PYUSD/USDP→USD, EURC/EUROC/EUROB→EUR, MXNe→MXN. Quote-side rewrite at AGGREGATION time only (depegs stay visible in raw trades). Sync test with canonical.StablecoinCodes.
- **Outlier filter = σ/z-score float64 (masking-vulnerable), NOT MAD** (outliers.go). MAD is only: (a) GuardServedVWAP serve-time guard at 3 call sites (/v1/price closed, /v1/assets tier-1, price alerts); (b) anomaly z-score baselines. Orchestrator's PUBLISHED VWAP relies on σ-filter + freeze, not MAD.
- Confidence = weighted geometric mean of 6 factors (log-sum), bootstrap cap 0.5 <30d. approxUSDVolume divides Σquote by 1e7 for USD pairs → CEX (1e8) overstate liquidity 10× (self-ack heuristic).
- Freeze (ADR-0019): Phase1 per-class thresholds, freeze only when dev≥pct AND sources≤1; Phase2 3-signal AND (conf<0.10 AND z>5 AND sources≤1). Freeze skips Redis write so LKG serves; TTL re-arm choreography.
- Triangulation: chain product of positive big.Rat; fiat legs use FX snap (rate_usd::text→big.Rat, 7-day lookback).
- Global fallback: Tier1 VWAP(count≥5) → Tier2 mean of aggregator prices → Tier3 triangulated.

## INVARIANT TIERS (money; weakest writer sets tier)
| # | Invariant | Tier | Weakest link |
|---|---|---|---|
| I-1 | i128 never truncates to int64/float | **CI+test** (on-chain) / **convention** (off-chain magnitudes) | TestI128TruncationGuard go/types walk (strong) + lint-i128 grep; but doesn't catch float round-trips of decoded JSON numbers (coinbase, pollers) |
| I-2 | Money columns NUMERIC | **DB** / **convention** for fx_quotes + price_source_contributions | lint checks COLUMN type not Go writer type; those 2 tables are float64-in |
| I-3 | Trade amounts >0 | **DB CHECK** | InsertTrade validates; BatchInsertTrades does NOT (relies on sink); lowercase-tx_hash etc. convention on batch path |
| I-4 | Oracle price>0, decimals≤38 | **DB** | single writer, NaN-confidence rejected |
| I-5 | Supply never negative; Alg-1/2/3 | **DB CHECK + runtime** | Alg-1 const 50,001,806,812 XLM (CS-010 basis honesty); Alg-2 clamp; Alg-3 mint−burn−clawback w/ missing-baseline vs pages sentinels; freshness gate w/ F-1320 dormancy exception (stall at same ledger 2 ticks reads "dormant" + publishes) |
| I-6 | Stablecoin peg correctness | **convention + test + watcher** | NO runtime peg check; map hardcoded; trusted at insert for usd_volume (depeg doesn't retro-correct); watcher=divergence + anomaly |
| I-7 | Serve only CLOSED buckets (ADR-0015) | **runtime** | sargable bucket<=now()-interval everywhere; no failure-case test found |
| I-8 | Derived money values re-derivable | **NONE (trap)** | trades.usd_volume + asset_supply_history both ON CONFLICT DO NOTHING → bad-at-insert value PERMANENT |
| I-9 | Only Exchange-class IncludeInVWAP votes | **runtime** | fail-closed registry default; toggled by DisableClassFilter |
| I-10 | Non-7dp never leaks unscaled price | **watcher+DB+runtime / convention** at gaps | AdjustPrice at most endpoints; NOT /v1/price/at, /v1/price/changes prices, market-cap 10^7 hardcodes, CAGG raw ratios |

## TOP TRAPS / FINDING CANDIDATES
1. **CS-040 REGRESSION (GATED high):** orchestrator.go:1268-1271 hardcodes decimals=8 for ALL fiat:USD trades; FX registry stamps AmountDecimals=6 (registry.go:128-138 cites CS-040); corrector `windowUSDVolume` (orchestrator.go:1410-1442) is DEAD CODE (0 callers). Latent (connector FX disabled; massive writes fx_quotes not trades) but re-enabling polygon-forex/exchangeratesapi (both IncludeInVWAP:true) → 100× volume-gate understatement + mixed 1e6/1e8 VWAP weighting. Doc comment (orchestrator.go:225 "sum/1e8 exact") asserts opposite of registry.
2. **ON CONFLICT DO NOTHING money traps:** trades.usd_volume (trades.go:440,638) + asset_supply_history (supply.go:63). Every downstream sum(volume_usd) (CAGGs, /v1/markets, contribution donut, Volume24hUSDForAsset) inherits bad values permanently.
3. **/v1/price fallback tiers unevenly guarded:** tier1 CAGG passes GuardServedVWAP; Redis-VWAP, stablecoin-peg-literal "1.000000000000", and float64 fiat cross-rate fallbacks DON'T — and cross-rate stamped PriceType:"vwap". Only stale=true signals consumer.
4. CAGG `twap` column not a TWAP (equal-weight mean).
5. Direction-combining approximation ≠ exact union VWAP.
6. Trade-level outlier = σ not MAD (masking); MAD guard only 3 serve sites, not published VWAP.
7. Non-7dp gaps: /v1/price/at (raw CAGG to wire), /v1/price/changes prices, marketCapDecimals=7 hardcode, populateMarketCap default 7 → non-7dp token mcap/FDV mis-scaled. Verify SEP-1 display_decimals firewall (F-1321) holds.
8. Fiat precision split (float64 vs big.Rat vs big.Float); /v1/changes money as JSON floats.
9. averageAggregatorPrices comment/code mismatch — silent Quo truncation >14dp (latent).
10. Aggregator Tier-2 mean, no outlier rejection: one bad print among 2 fresh sources moves global price 50%.
11. Redis TTL: VWAPTTL(window)=window → 24h key serves 24h-old via fallback; freeze TTL re-arm off-by-one risk.
12. **CoinGecko divergence reference has NO upstream-staleness gate** (coingecko.go:190) unlike Chainlink/oracle → frozen CG price suppresses/manufactures divergence warnings (echoes prior CS-087/088).
13. Chainlink divergence float64(answer.Int64()) unguarded on decimals==0 (unreachable currently, heuristic).
14. Band decoder u64@E9 (not i128); E18 pair-rate shape in oracle.go:87 has NO decoder (doc/code drift).
15. XLM alias (native vs crypto:XLM) duplicated 3 places → drift.
16. Newest (2026-07): SAC full-history seed, SEP-41 rollup genesis gating, subset-bound cross-check, usdQuoteDecimals unification, USDPeggedSorobanAssets gate.

## Red herrings (cite to save finder time)
Kraken timestamp float (ns only, amounts exact strings); Amount.Scan int64 arm (benign).
