---
title: VWAP & price-aggregation methodology
last_verified: 2026-07-10
status: current
---

# How Stellar Index computes a price

This is the public methodology for the aggregated prices served by
`/v1/price`, `/v1/price/tip`, and `/v1/vwap`. It documents exactly what
goes into a number and — where it matters — what deliberately does **not**.
For TWAP and OHLC specifically, see [twap-ohlc.md](twap-ohlc.md); for the
freshness contract, [freshness — the two surfaces](#freshness--two-honest-contracts)
below.

Everything here is exact-precision integer/rational arithmetic
(`*big.Int` / `*big.Rat`), never float — token amounts from Soroban are
128-bit and would lose precision above 2^53 in an IEEE-754 double
(ADR-0003). The internal reference is
`docs/architecture/aggregation-plan.md`; the code is
`internal/aggregate/`.

## VWAP — the core number

Volume-Weighted Average Price is the summary of "what did this asset
trade at" over a window. Weighting each trade's price by the base volume
it moved:

```
VWAP = Σ(price_i × volume_i) / Σ(volume_i)
```

With `price_i = quote_i / base_i` and `volume_i = base_i`, this collapses
to the exact single-division form we actually compute:

```
VWAP = Σ(quote_i) / Σ(base_i)      — total quote moved ÷ total base moved
```

Trades with non-positive base or quote are skipped (they carry no
meaningful weight). A window with zero contributing base volume returns
"price unavailable" rather than a fabricated number.

## The policy chain — raw trade → served price

The **filtered** price flows through one ordered path
(`internal/aggregate/orchestrator`). The order matters:

| Step | Default | What it does |
|---|---|---|
| 1. Stablecoin expansion | OFF (operator opt-in) | Expand a fiat-quote target (`XLM/fiat:USD`) to its direct pair + stablecoin-backed pairs, rewriting the quote side |
| 2. Source-class filter | ON | Drop every trade whose source is not a VWAP-eligible exchange (see below) |
| 3. Outlier filter | ON (σ = 4.0) | Drop trades whose price is > N standard deviations from the window mean |
| 4. Min-USD-volume gate + freeze | ON | Suppress a window that clears too little USD volume; serve last-known-good instead of a freshly-computed value on an anomaly freeze |
| 5. VWAP | — | `Σquote / Σbase` over what survives |

The class filter runs **before** the outlier filter so outlier statistics
are computed only over eligible exchange trades, not contaminated by
oracle/aggregator rows.

### Two serving paths — the direct read and its guard

Be precise about which price a given `/v1/price` request returns, because
there are **two** paths:

1. **Filtered (Redis).** For the aggregator's configured pairs — the
   headline fiat pairs, triangulated cross-pairs, and stablecoin-proxy
   rewrites — the value above is written to Redis and served from cache.
   This is the primary product.
2. **Direct (`prices_1m` CAGG).** `/v1/price` also reads the most-recent
   CLOSED `prices_1m` continuous-aggregate bucket for the requested pair.
   That CAGG is a bare `Σ(quote)/Σ(base)` per bucket — it does **not** run
   through the class / outlier / min-volume / freeze chain above. A pair
   with **no** `prices_1m` rows — a pure-synthetic fiat pair like
   `native/fiat:USD` (SDEX native trades are quoted in issuer-stablecoins,
   never `fiat:USD`) — misses this read and falls through to the filtered
   Redis value. But any pair with real `prices_1m` rows is served straight
   off the raw CAGG bucket, and that includes both **directly-quoted**
   DEX/CEX pairs (a Soroban token priced in `USDC-GA5Z…`,
   `crypto:BTC/crypto:USDT`, …) **and** headline pairs that have a real
   fiat CEX market (`crypto:XLM/fiat:USD` via Kraken/Coinbase XLM/USD
   books).

To keep the direct path honest, that read is wrapped in a
**serving-sanity guard** (`internal/aggregate.GuardServedVWAP`): the
candidate bucket's VWAP is checked against a robust bound — the union of a
wide **ratio band** and a **MAD band** — over the pair's recent trailing
closed buckets. A candidate that is grossly off (an order-of-magnitude
fat-finger / manipulation print the CAGG would otherwise serve with
`stale=false`) is rejected, and the newest clean trailing bucket
(last-known-good) is served instead. The guard is deliberately
**conservative**: on a healthy bucket it is a pure pass-through (a liquid
pair like `crypto:XLM/fiat:USD` sits tightly clustered and always passes,
so its served value is byte-identical to pre-guard behaviour); it catches
only gross deviation and never a legitimately volatile-but-real move — a
stablecoin depeg is *served*, not hidden — and a pair with too little
history to form a baseline fails **open** (serves the
candidate). All of it is exact `*big.Rat` (ADR-0003).

## Source-class policy — who gets a vote

Not every price we ingest votes in the average. Each source carries a
class in `internal/sources/external/registry.go`, and **only sources that
are `Class = Exchange` AND `IncludeInVWAP = true` contribute to VWAP.**

| Class | Examples | In VWAP? |
|---|---|---|
| Exchange | Soroswap, Aquarius, Phoenix, Comet, SDEX (on-chain DEX); Binance, Kraken, Coinbase, Bitstamp (CEX); FX feeds | **Yes** — genuine executed trades |
| Oracle | Reflector, RedStone, Band, Chainlink | No — **reported alongside**, not counted |
| Aggregator | CoinGecko, CoinMarketCap, CryptoCompare | No — divergence cross-check only |
| Lending | Blend | No — auction/stress prices reported as a secondary signal |
| Router / Bridge | Soroswap-Router, DeFindex, CCTP, Rozo | No — they emit no independent trades (they invoke other contracts, which do) |
| Authority-sanity | ECB daily FX | No — sanity check only |

Why exclude oracles and aggregators: they publish **already-aggregated,
derived** prices under their own governance and methodology. Folding them
into our VWAP would either double-count the upstream markets they
themselves summarise, or impose their aggregation policy on our output. We
surface them on `/v1/sources` and use them for divergence detection — an
independent cross-check, not an input. (An operator can opt a single
oracle into aggregation per-source, but the default is excluded.)

**Fail-closed:** an unregistered / mis-typed source falls back to
`Exchange, IncludeInVWAP=false` — visible on `/v1/sources` but silent in
VWAP until an operator explicitly registers it. A typo can never quietly
inject unauthorised data into the average.

**Trust in the trades themselves:** every on-chain source is attributed
by contract identity (ADR-0035) so a look-alike contract can't inject
fabricated trades under a protocol's name — with **one known exception,
Comet**, which still matches on topic bytes alone (CS-026). See the
[per-protocol verification pages](../protocols/README.md).

## Stablecoin → fiat proxy (late binding)

Most Stellar liquidity is quoted against stablecoins (`XLM/USDT`,
`XLM/USDC`), but customers ask for `XLM/USD`. We bridge that at
**aggregate time, never at ingest time.** The decoder stores the real
pair; the aggregator optionally rewrites the quote side through this map
when computing a fiat-denominated VWAP:

| Stablecoin | Proxied to | | Stablecoin | Proxied to |
|---|---|---|---|---|
| USDT, USDC, DAI, PYUSD, USDP | USD | | EURC, EUROC, EUROB | EUR |
| MXNe | MXN | | | |

Only the **quote** side is rewritten (`XLM/USDT → XLM/USD` preserves the
"price of XLM in USD" axis). Why late binding: **pegs break.** USDT
trading at \$0.968 during a stress event *is* the news; folding
`USDT → USD` unconditionally at decode time would hide a depeg from every
downstream consumer. Storing the raw pair and binding late keeps the
trade feed honest while still answering the fiat question. The map is
`internal/aggregate/stablecoin.go`; classic-issued stablecoins
(`USDC-GA5Z…`) are intentionally *not* auto-proxied (they carry full
issuer identity — substitution there is an explicit per-deployment
choice).

## Outlier filtering — honest about the current form

The outlier filter drops any trade whose price is more than **σ = 4.0
standard deviations** (configurable) from the window's unweighted mean.
It is a **standard-deviation (σ) filter, not a MAD filter** today
(`internal/aggregate/outliers.go`):

- σ ≤ 0 disables it; with fewer than 3 valid prices it is a no-op (a
  degraded signal isn't compounded by dropping half the data).
- Statistics use the float64 price projection — acceptable because
  outlier detection is a heuristic gate, not the value-serving
  computation (the VWAP itself stays exact-rational).

**Known limitation:** a σ-mean filter is brittle on small windows with
fat tails — a single extreme print can inflate σ enough to hide the next
one. A MAD-based (median-absolute-deviation) filter is the planned
replacement behind the same `outlier_sigma_threshold` flag; it has not
shipped. We document the σ form here rather than claim a robustness
property we don't yet have. See
`docs/operations/runbooks/aggregator-outlier-storm.md`.

## Triangulation — implied cross-pairs

When no direct market exists for a requested pair, we can derive it from a
chain of direct prices:

```
price(A→C) = price(A→B) × price(B→C)          (e.g. XLM/USD × USD/EUR = XLM/EUR)
```

`internal/aggregate/triangulate.go` multiplies exact rationals along an
arbitrary-length chain; any zero/negative leg fails closed
(`ErrTriangulateZero`). A triangulated response carries
`flags.triangulated: true` so the derivation is never hidden. For
chained-fiat legs a forex snapshot (`FXQuoteAtOrBefore`) supplies the FX
rate at-or-before the trade time from the active FX feed's `fx_quotes`
table.

## Closed-bucket serving (cross-region determinism)

Every region must serve the **same** answer for `/v1/price` — a client
hammering DNS-rotated regions can't be shown a flickering price. So
`/v1/price` (and `/v1/vwap`, `/v1/twap`, `/v1/ohlc`) serve **only the
most-recent fully-closed aggregate bucket**, never the in-progress one
(ADR-0015). A closed window has a fixed identity: once computed, its value
is immutable and replicates deterministically to every replica. The
response carries the bucket's `[from_ts, to_ts]` as `as_of` so the client
sees exactly which window the rate covers.

### Continuous-aggregate column caveats (`prices_1m`/`prices_*`)

The direct-read path serves the `prices_*` continuous aggregates
(`migrations/0002…`). Two honest notes on how those columns are computed:

- **The `vwap` column is `sum((quote/base)·base) / sum(base)`.** That is
  algebraically identical to the exact `Σquote / Σbase` documented above,
  but it is written as a per-row divide-then-multiply in NUMERIC, so the
  intermediate `quote/base` is rounded to NUMERIC's division scale before
  being re-multiplied — a negligible-but-nonzero difference from the exact
  single-division form that `internal/aggregate/vwap.go` (the Redis/tip
  path) computes in `*big.Int`. Editing the applied migration 0002 in place
  is not possible, and recreating seven CAGGs over a decade of history to
  save that rounding is not worth the re-materialisation risk, so the form
  stands and is documented here instead.
- **The `twap` column is `avg(quote/base)` — an unweighted per-trade
  mean, NOT a time-weighted average.** Despite the name it carries no time
  weighting. The genuinely time-weighted TWAP lives in the dedicated
  `twap_1h` / `twap_1d` aggregates (`migrations/0081…`), which
  `/v1/twap` and `/v1/history` read; nothing reads `prices_*.twap`. Treat
  that column as a legacy equal-weight mean.
- **Every column above assumes both legs are 7-decimal.** `quote_amount`
  and `base_amount` are smallest-unit integers; the ratio only equals the
  true price when both assets share a decimals scale. As of 2026-07-10
  EVERY serving path — the query-time computations (`/v1/vwap`,
  `/v1/twap`, `/v1/history`, `/v1/ohlc` single-bar mode, `/v1/price/tip`,
  the aggregator's published VWAP behind `/v1/price/stream`) AND every
  CAGG-reading path (`/v1/price`'s closed-1m bucket, `/v1/ohlc?interval=`
  series, `/v1/chart`, the `last_price` on `/v1/markets` / `/v1/pools` /
  `/v1/pairs`, and the SEP-40 oracle passthroughs) — corrects for a
  confirmed non-7-decimals leg via a read-time `10^(dec_base−dec_quote)`
  scalar (`internal/aggregate.AdjustPrice`) applied to the finished
  ratio at serve time; nothing declines anymore — see
  `docs/operations/runbooks/dex-nonstandard-decimals.md`. The stored
  `prices_*` CAGG columns documented above remain RAW — a consumer
  querying them directly via SQL must apply the same factor by hand.
  Volume columns are NOT price-corrected: OHLC base/quote volumes are
  raw smallest-unit sums in each asset's own declared decimals (the
  documented wire contract), and `volume_usd` is already USD-anchored.
  As of this writing 5 confirmed non-7-decimals Soroban tokens exist
  (one 6dp, one 9dp, three 18dp), all trading via aquarius.

## Freshness — two honest contracts

Our freshness SLA target is **≤ 30s price staleness**, and we serve
**two** freshness contracts on purpose (ADR-0015 / ADR-0018). We state
both plainly rather than imply a single 30s number for every surface:

| Endpoint | Contract | Typical `observed_at` age |
|---|---|---|
| **`/v1/price/tip`** (+ SSE stream) | rolling-window VWAP over the freshest trades, recomputed per request/tick | **≤ 5s** — the ≤30s SLA surface |
| `/v1/price` | last-closed bucket (cross-region-deterministic, cacheable) | **30–150s by design** (1-minute bucket close + aggregation cycle) |

The `/v1/price` **30–150s** figure is a structural property of closing a
1-minute bucket and running the aggregation cycle on top — it is **not**
a sub-30s number, and we do not advertise it as one. Integrators pick per
use case: `/v1/price/tip` for a live wallet asset page, `/v1/price` for
anything that must agree across replicas, audits, or CDN caches. Full
detail: `docs/architecture/freshness-definition.md`.

## What this methodology deliberately does NOT do

- **No per-venue weighting.** Every VWAP-eligible source weights at 100
  today; a weighted-venue tier is deferred (see the aggregation plan).
- **No LKG (last-known-good) fallback on window aggregates.** A TWAP/OHLC
  over a window with no trades is *undefined*, not "stale" — those
  endpoints return `404 no-trades` rather than inventing a value from a
  different window. Only the tip endpoint carries an LKG (`flags.stale`).
  See [twap-ohlc.md](twap-ohlc.md).
- **No eager stablecoin normalisation, no oracle-into-VWAP folding** —
  both covered above.

## Cross-reference

- [twap-ohlc.md](twap-ohlc.md) — TWAP/OHLC compute + the no-trades contract
- [Per-protocol verification pages](../protocols/README.md) — what feeds the exchange class + the contract-identity gating that makes each trade trustworthy
- `docs/architecture/aggregation-plan.md` — the internal binding spec (config surface, metrics, alerts)
- `docs/architecture/freshness-definition.md` — the two-contract freshness design
- ADR-0003 (i128 precision), ADR-0015 (closed-bucket serving), ADR-0035 (contract-identity gating)
