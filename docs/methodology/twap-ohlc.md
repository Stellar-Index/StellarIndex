---
title: TWAP / OHLC methodology + freshness semantics
last_verified: 2026-09-02
status: ratified
---

# TWAP / OHLC methodology

This doc explains how `/v1/twap` and `/v1/ohlc` compute their
output and — importantly — how they behave when the requested
window has no trade data. Customers comparing our wire shape to
`/v1/price` may notice a deliberate asymmetry: `/v1/price` will
serve a stale-marked LKG (last known good) value during cache-
cold windows while `/v1/twap` and `/v1/ohlc` return HTTP 404
`errors/no-trades`. That asymmetry is intentional; this page
documents why.

## Wire shape

| Endpoint | Returns | Window-shape | LKG fallback |
| --- | --- | --- | --- |
| `/v1/price` | one price scalar | the last **closed** 1-minute bucket (ADR-0015 — cross-region deterministic) | yes — flags.stale + LKG age |
| `/v1/price/tip` | one price scalar | tip-anchored ("price as of N seconds ago", ADR-0018) | yes — flags.stale + LKG age |
| `/v1/twap` | one TWAP scalar over `[from, to)` | client-specified | **no** |
| `/v1/ohlc` (single-bar) | one OHLCV row over `[from, to)` | client-specified | **no** |
| `/v1/ohlc?interval=…` (series) | N OHLCV rows | client-specified | **no** |

## Why `/v1/price` has LKG but `/v1/twap`/`/v1/ohlc` don't

A TWAP or OHLC over a window with zero trades is not "the TWAP
with old data" — it's **undefined**. Specifically:

- **TWAP** weights each trade's price by the duration until the
  next trade. A window with no trades has no weighting to compute
  against. The only sane TWAP value over `[t, t+w)` when zero
  trades exist is "no value."
- **OHLC** is open / high / low / close of trades in `[t, t+w)`.
  Zero trades means no open, no high, no low, no close. There is
  no "stale OHLC" — only the OHLC of a *different* window, which
  answers a different question.

In contrast, `/v1/price` answers "what was the last **closed**
bucket's price?" (the sub-minute tip surface is `/v1/price/tip`).
Answering with the most recent closed value we have, even if it's 30
minutes old, with `flags.stale: true`, is honest about the staleness
while still being useful. The customer asked "what's the price?", not
"what's the price of [t, t+30s)?"

If a TWAP/OHLC handler invented an answer by stretching the
window backwards to find data, it would silently change the
semantics of the response. A customer requesting a 24h TWAP
ending now would unknowingly receive a TWAP of yesterday's
24h ending yesterday — a different number representing a
different time window. Returning 404 is the correct contract:
"there are no trades in the window you asked about; if you
want a longer lookback, ask for one."

## What customers should do under "no trades in window"

1. **Widen the window.** The closed-bucket clamp (ADR-0015)
   means `to` defaults to the previous 30-s boundary; explicit
   `to` is respected verbatim. Widening `from` backwards by an
   hour, a day, or a week is a fast retry pattern.
2. **Fall back to `/v1/price` (or `/v1/price/tip`).** For a "give me
   the latest price even if it's old" semantic, the scalar-price
   surfaces are the right ones — both carry `flags.stale` and the LKG
   age in the envelope's `as_of`. Use `/v1/price` when you want the
   cross-region-deterministic closed bucket, `/v1/price/tip` when you
   want sub-minute freshness.
3. **Use `/v1/history`.** For "show me every trade in this
   window" semantics, the history surface returns raw trades
   without aggregation, which is what TWAP/OHLC compute against.

## Stablecoin-proxy fallback IS present

The TWAP/OHLC endpoints DO carry the X/USD → X/USDC|EURC|… proxy
that `/v1/price` has. If you request `?base=native&quote=fiat:USD`
the handler **combines every constituent** — the direct fiat pair plus
every `[trades].usd_pegged_classic_assets` operator-declared peg pair
(Circle USDC, EURC, …) — into one trade set before computing, and the
response carries `flags.triangulated: true` when a peg constituent
contributed. It is not a first-non-empty retry: taking the first
non-empty peg made the same `?quote=fiat:USD` question answer
differently depending on whether you asked for a point or a series
(C1-024), and the combined set is exactly the one the live aggregator
computes its VWAP over. This is
`internal/api/v1/ohlc_fiat_combine.go::ohlcSeriesFiatCombined` (series)
and `::fiatCombinedTrades`, reached from the shared point-path fetch
`vwap.go::tradesInRangeWithStablecoinFallback` used by `/v1/vwap`,
`/v1/twap` and single-bar `/v1/ohlc`.

This proxy fallback is *not* an LKG fallback — it answers the
same question ("what was the price over this window?") against
a near-equivalent pair, not against a different time window.

## Cascade-window behaviour

Under a Redis MISCONF cascade, the `/v1/price` LKG path remained
available because LKG values
live in Postgres (the cache layer being down doesn't lose them).
`/v1/twap` and `/v1/ohlc` correctly returned 404 for windows
where the cascade had blocked the underlying trade ingest — that
is the right answer for "what's the TWAP of a window where we
didn't observe any trades."

Operators investigating the asymmetry should:

1. Confirm the cascade-affected window is a real ingest gap, not
   a cache-fronted view of stale data.
2. Use `/v1/price` for "is anything live for this pair right now"
   diagnostics.
3. Use `/v1/diagnostics/cursors` for the authoritative per-source
   ingest state.

One observation — "SSE+streaming clients on /twap or /ohlc
see hard 404s during cache-cold storms while /price stays
nominally up" — is true but reflects the right user-facing behaviour:
streaming TWAP clients SHOULD see 404 when no trades land in
the streaming window, because the next valid TWAP doesn't exist
yet. Quietly emitting yesterday's TWAP into a today-anchored
stream would be a correctness regression, not a robustness win.

## The `twap` column in the `prices_*` CAGGs is NOT the served TWAP

The continuous aggregates created by migration 0002 materialize a
column named `twap` computed as `avg(quote_amount / base_amount)` —
an **equal-weight mean of per-trade prices**, not a time-weighted
average. It is read by exactly one consumer: the `twap_1h` /
`twap_1d` aggregates (migration 0081) are `avg(prices_1m.twap)`, and
they back `/v1/chart?price_type=twap` — a minute-resolution
approximation of a time-weighted mean, documented in 0081's own
header. `/v1/twap` never reads it: that TWAP is always computed on
demand from raw trades by `internal/aggregate/twap.go` (accumulated in
10^40 fixed-point `big.Int` for linear cost and converted to an exact
`big.Rat` once at the end; genuinely time-weighted). Do not add new
readers of the raw CAGG column; it survives only because dropping a
column from an indefinite continuous aggregate requires a full
rematerialization.

Relatedly: the CAGG `vwap` uses the per-row form
`sum((quote/base)*base)/sum(base)` rather than the exact
`sum(quote)/sum(base)`. Measured on r1 (2026-07-02, 40,565 1h-bucket
comparisons) the divergence from exact is ≤ 1.0e-16 relative — below
the 12-decimal wire truncation, so the served historical VWAP and
the live exact-rational VWAP agree at wire precision. New aggregates
must use the exact single-division form (migrations/README.md rule 8).

## Cross-reference

- `internal/api/v1/twap.go::handleTWAP` — TWAP path.
- `internal/api/v1/ohlc.go::handleOHLC` — OHLC path.
- `internal/api/v1/vwap.go::tradesInRangeWithStablecoinFallback` —
  the shared point-path fetch (fiat quotes combine every constituent).
- `internal/api/v1/ohlc_fiat_combine.go::ohlcSeriesFiatCombined` — the
  series-path fiat combine.
- `internal/api/v1/price.go::handlePrice` — LKG-bearing closed-bucket path.
- `internal/api/v1/price_tip.go::handlePriceTip` — LKG-bearing tip path.
- ADR-0015 — last-closed-bucket contract; ADR-0018 — URL discipline
  (`/v1/price` vs `/v1/price/tip`).

## Changelog

- 2026-05-28 — initial draft.
- 2026-07-02 — documented the dead CAGG `twap` column + the verified-
  immaterial (≤1e-16) CAGG `vwap` numeric-form divergence.
- 2026-09-02 — corrected four stale claims against the code (#359):
  `/v1/price` is the CLOSED-bucket surface (the tip is `/v1/price/tip`);
  the fiat proxy COMBINES every constituent rather than taking the first
  non-empty peg (`ohlcTradesWithStablecoinFallback` was deleted in
  C1-024); the CAGG `twap` column is not dead — `twap_1h`/`twap_1d` are
  `avg(prices_1m.twap)` and back `/v1/chart?price_type=twap`; and
  `aggregate.TWAP` accumulates in 10^40 fixed-point `big.Int`, exacting
  to `big.Rat` only at the end.
