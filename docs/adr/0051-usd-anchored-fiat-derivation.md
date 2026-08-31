---
adr: 0051
title: USD-anchored derivation of local-currency prices
status: Accepted
date: 2026-08-31
supersedes: []
superseded_by: null
---

# ADR-0051: USD-anchored derivation of local-currency prices

## Context

A wallet showing a balance to a Brazilian user needs XLM in BRL. A
Japanese user needs JPY. This is the premise the price surface was
built for, and it is what most consumers of a price API actually
want — a number in the currency the person reading the screen thinks
in.

Until this ADR the API could not answer that. Measured on r1,
2026-08-31, `/v1/price?asset=native&quote=fiat:CCY`:

| quote | result |
|---|---|
| USD, EUR, GBP | 200 |
| BRL, JPY, CHF, CAD, AUD, MXN, ZAR, NGN, ARS, INR, CNY | **404** |

Three currencies out of the 133 in the ADR-0010 allow-list. The
reason is that those three are the only fiats anything we ingest is
quoted in. Every fiat pair with market data in the last 24 hours:

| quote | distinct bases | rows |
|---|---|---|
| `fiat:USD` | 20 | 26,920 |
| `fiat:EUR` | 3 | 3,769 |
| `fiat:GBP` | 3 | 2,790 |

EUR and GBP are there because Binance and Kraken quote XLM/EUR and
XLM/GBP directly — not because of anything FX-derived.

What makes this a gap rather than a limitation is that both halves of
the answer were already on the box and fresh. `fx_quotes` carried
BRL at 5.1837 from the `massive` feed, updated that day, alongside
~130 other currencies. `prices_1m` carried XLM/USD. The product of
the two is XLM/BRL. Nothing composed them.

The existing `tryFiatCrossRate` looked like it should cover this, and
does not: it opens by requiring **both** sides to be fiat, so it
serves `fiat:EUR/fiat:BRL` but refuses `native/fiat:BRL`. The
stablecoin proxy (ADR-0026) is also not it — that maps pegged
*assets* to their fiat (USDC→USD, EURC→EUR, MXNe→MXN) and depends on
a peg existing and having liquidity. MXN has a peg entry and still
404s, because no XLM/MXNe market carries data. Liquidity, not
mapping, is the binding constraint there.

No prior ADR covers deriving a crypto price in an arbitrary fiat.
ADR-0010 established `fiat` as an asset *type*; ADR-0026 established
stablecoin→fiat proxying as late-bound aggregator policy. The
composition rule was never written down, which is why it was never
built.

## Decision

**Price any non-fiat asset in any fiat we hold an FX rate for, by
composing the asset's USD price with the USD→CCY rate:**

```
price(asset, CCY) = price(asset, USD) × rate_usd[CCY]
```

USD is the anchor because it is the only currency with broad direct
coverage (20 distinct bases vs 3), so anchoring anywhere else would
derive more values, not fewer.

This lands as a fourth and final layer in `Server.priceFallback`,
after the Redis VWAP cache, the stablecoin proxy, and the fiat-fiat
cross. Ordering is the substantive part of the decision: **an
observed market always beats a derived value.** XLM/EUR is a real
CEX print and continues to be served as one; the derived path only
fires where there is no market at all.

Three properties are binding:

1. **Derived values are flagged.** `flags.triangulated = true`. A
   synthesised number must not be presentable as a market print.
2. **Provenance is preserved and extended.** `sources` carries the
   USD leg's own venues *plus* the FX feed. A customer auditing a BRL
   price can see both what set the USD price and what converted it.
3. **A withheld USD leg is never laundered through FX.** Every
   withholding decision — a directory-scam-flagged issuer, the
   decimals guard — is made on the USD leg. Re-reading that leg and
   multiplying by a rate would publish exactly the price policy
   declined to publish, via a route nobody had gated. The withheld
   verdict propagates and the derivation serves nothing.

Point 3 is not hypothetical. It is the MSP-02 / MSP-06 class this
repo has already been bitten by twice: `/v1/vwap` and `/v1/twap`
served a scam-flagged issuer at 200 while every reader-backed surface
withheld it, and a withheld verdict reached on the proxy leg was
swallowed and reported as not-found. A new fallback layer is exactly
where that class recurs, so the guard is a test, not a comment.

## Consequences

- **Positive:** ~130 currencies become available for every asset we
  can price in USD, from data already ingested. No new feed, no new
  storage, no ingest change. `/v1/price`, `/v1/price/batch` and
  `/v1/price/tip` all gain it — batch and tip matter most, since a
  wallet prices a whole portfolio in one round trip.
- **Negative:** A derived price inherits the FX feed's error and
  staleness on top of the market's. `fx_quotes` is a **daily** fix,
  so a BRL price can rest on a rate up to ~24h old while its USD leg
  is seconds old. That is acceptable for display — it is what every
  wallet already does — but it is not a settlement rate, and the
  methodology doc says so plainly.
- **Operational impact:** One extra price read plus a map lookup on
  the miss path only. Derived currencies are, by construction, ones
  with no market rows, so the read they trigger is the USD leg the
  caller could have asked for anyway. No new alert; a stale FX feed
  is already covered by the forex worker's own freshness signal.
- **Downstream design impact:** USD becomes load-bearing as the
  pivot. If USD coverage for an asset degrades, every derived
  currency for that asset degrades with it — a single point of
  failure that direct markets do not have. Accepted knowingly: the
  alternative is 3 currencies instead of 133.

## Alternatives considered

1. **Ingest direct fiat markets for more currencies** — rejected: the
   markets largely do not exist. There is no XLM/BRL book of any
   size anywhere we could ingest. This would produce nothing for most
   of the 133.
2. **Derive at ingest, storing XLM/BRL rows** — rejected for the same
   reason ADR-0026 rejected eager stablecoin normalisation: it bakes
   a conversion into stored data, so an FX correction cannot be
   applied retroactively and a stale rate becomes indistinguishable
   from an observation. Late binding keeps the derived value honest
   and cheap to fix.
3. **Anchor on EUR, or pick the best-covered anchor per asset** —
   rejected: USD has 20 distinct bases against EUR's 3, so any other
   anchor derives strictly fewer pairs, and a per-asset anchor makes
   the served number depend on coverage that changes under you.
4. **Return the USD price with a conversion rate alongside, and let
   the client multiply** — rejected: it moves money math into every
   client, guarantees they disagree with each other and with our own
   charts, and gives up the ability to flag the result as derived.
5. **Serve it only behind an explicit opt-in parameter** — rejected:
   the honest default is to answer the question asked. The value is
   flagged `triangulated`, which is the disclosure mechanism the wire
   format already has for exactly this.

## References

- Related ADRs: [0010](0010-off-chain-fiat-representation.md) (fiat as
  an asset type), [0026](0026-stablecoin-fiat-proxy-late-binding.md)
  (late-bound stablecoin→fiat policy — the same late-binding argument),
  [0003](0003-i128-no-truncation.md) (the derivation uses exact
  `big.Rat`, never float64, on a served price),
  [0015](0015-last-closed-bucket-rate-serving.md) (the USD leg is a
  closed-bucket value, so the derived one is too).
- Implementation: `Server.tryUSDAnchoredFiatCross` +
  `Server.resolveUSDLeg` in `internal/api/v1/price.go`; the parallel
  arm in `internal/api/v1/price_tip.go`.
- Guards: `internal/api/v1/usd_anchored_fiat_cross_test.go` — in
  particular `TestPriceWithheldUSDLegIsNotLaunderedThroughFX` (the
  side-door guard) and `TestPriceDerivedFiatDoesNotShadowARealMarket`
  (the ordering guard).
- Methodology: `docs/methodology/local-currency-pricing.md`.
