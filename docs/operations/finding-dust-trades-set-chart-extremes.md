---
title: Finding — dust trades set OHLC chart extremes
last_verified: 2026-07-22
status: diagnosed, fix designed, NOT yet implemented
---

# Dust trades set OHLC chart extremes

## Symptom

The home-page XLM/USD chart shows a drop to **$0.1333** at the 06:00 UTC bar on
2026-07-17, against a bar VWAP of $0.1832 — a ~27% dip that never happened.

Reported by the operator; this is the same *class* as the earlier S-012 finding
($0.56 highs) but a **different root cause**, and the existing outlier filter
cannot catch it.

## Root cause

`migrations/0002_create_price_aggregates.up.sql` builds the OHLC extremes with
no size filter at all:

```sql
max(quote_amount / base_amount) AS high_price,
min(quote_amount / base_amount) AS low_price
FROM trades
GROUP BY bucket, base_asset, quote_asset
```

Every trade contributes to high/low regardless of notional. The offending print:

| field | value |
|---|---|
| pair | `USDC-GA5ZSEJY…` / `native` (REVERSE direction) |
| base_amount | **2 stroops** |
| quote_amount | **15 stroops** |
| usd_volume | **$0.00000027** |
| price | 15/2 = **7.5000** XLM per USDC |

`OHLCSeries` (`internal/storage/timescale/aggregates.go:1240`) normalises
reverse-direction pairs by inverting them:

```sql
CASE WHEN base_asset = $1 THEN low_price ELSE 1.0 / NULLIF(high_price, 0) END AS lo
```

so that 7.5 high inverts to **1/7.5 = 0.1333333333**, which becomes the served
low for XLM/USD.

## Why the existing outlier filter does not catch it

`selectExtreme` (`internal/api/v1/ohlc_fiat_combine.go`, shipped v0.18.0) drops
candidates outside `combinedOutlierBandRatio` (2x) of the bar VWAP. Here:

- bar VWAP = 0.1832 ⟹ low band floor = 0.0916
- offending low = 0.1333 — **comfortably inside the band**

It also is not dust at the *constituent* level: that constituent had 321 trades
and $14,493 of volume. The bad print is a single dust fill **inside an otherwise
legitimate constituent**, and the serve-layer filter only ever compares
whole-constituent extremes — it can never see inside one.

Tightening the band is NOT a safe fix: catching 0.728x VWAP would clip genuine
intra-bar moves.

## Scale

Trades on 2026-07-17 (one day):

| bucket | count | share |
|---|---|---|
| usd_volume < $0.01 | **1,448,695** | **24%** |
| usd_volume < $1 | 1,674,790 | 28% |
| total | 6,018,245 | — |

A quarter of all trades are sub-cent dust. Any one of them can set an extreme on
any pair, in either direction, on every granularity.


## Why the dust exists: path-payment remainders

The offending fill was NOT a standalone order. The transaction
(`6231307e…`, ledger 63514245) contains exactly ONE operation —
`PathPaymentStrictSend` — and the trades table's `op_index` is the CLAIM-ATOM
index within that path payment, not an operation index. The full chain:

| hop | sold → bought | usd_volume | leg price |
|---|---|---|---|
| 0 | XLM → BTC | $20.22 | — |
| 1 | USDC → XLM | $0.09 | 5.458 |
| 2 | USDC → XLM | $19.99 | 5.459 ✓ market |
| 3 | USDC → XLM | **$0.00000027** | **7.500** ← the outlier |

Hops 1–2 filled at the true market rate. Hop 3 is the **remainder** — the
2-stroop crumb left after the earlier hops consumed the available depth, swept
against the next offer in the book at a worse price. At 2 stroops there is no
precision left: 15/2 = 7.5 exactly, so the "price" is an artifact of dividing two
tiny integers.

**This is the real modeling error.** We record every claim atom of a path payment
as an independent market trade. Economically this was ONE ~$20 conversion that
executed at ~5.458 — it was never a market quote of 7.5 XLM/USDC. Path-payment
remainders are structurally guaranteed to produce these crumbs, which is why 24%
of trades are sub-cent.

It also gives the notional floor a principled meaning: it is not "ignore small
trades", it is **ignore fills too small to carry price information**. $0.01
excludes this crumb by ~37,000x while leaving hops 1 ($0.09) and 2 ($19.99)
intact, so the genuine execution stays fully represented.

Worth considering alongside the notional floor: whether a path payment's
intermediate hops should contribute to price discovery at all, or whether only
the end-to-end rate is a real observation. That is a broader modelling decision
(it affects VWAP and volume too, not just extremes) and should be taken
deliberately.

## Fix (designed, not implemented)

The extremes must be computed over economically meaningful trades only. This has
to happen in the **continuous aggregate**, because the cagg stores only the
extremes — the serve layer cannot recover what was already collapsed.

```sql
COALESCE(
  max(quote_amount / base_amount) FILTER (WHERE usd_volume >= 0.01),
  max(quote_amount / base_amount)
) AS high_price
```

The `COALESCE` fallback matters: a bucket containing *only* dust still reports an
extreme rather than NULL.

Open questions before implementing:
1. **Threshold.** $0.01 excludes the observed dust with large margin. Needs a
   sweep over historical data to confirm it never removes a legitimate extreme.
2. **`usd_volume` NULLs.** Pairs with no USD pricing need a size-based fallback
   (a stroop floor), or they keep today's behaviour.
3. **Cagg support.** Verify TimescaleDB accepts `FILTER` + `COALESCE` in a
   continuous aggregate definition at these versions; if not, the filter moves
   into a view over the cagg or the trades-side write path.
4. **Re-materialisation cost.** Seven caggs (1m/15m/1h/4h/1d/1w/1mo) over full
   history. Heavy — must be scheduled off the D2 window.

## Blast radius

Every OHLC chart on every pair and every granularity, in both directions
(inverted pairs get dust highs turned into lows and vice versa). This is the
"money surface" — treat the change with the same care as a pricing migration and
verify against known-good bars before and after.
