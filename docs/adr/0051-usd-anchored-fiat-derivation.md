---
adr: 0051
title: USD-anchored derivation of local-currency prices
status: Superseded
date: 2026-08-31
supersedes: []
superseded_by: [0053]
---

# ADR-0051: USD-anchored derivation of local-currency prices

## Context

Venues quote XLM only in USD, EUR and GBP, so `/v1/price?quote=fiat:BRL` and most of the ADR-0010 fiat allow-list had no direct market. Both halves of an answer existed: the asset's USD price and a USD-to-currency FX rate. ADR-0053 now owns the ordering between a direct book and this derivation.

## Decision

1. A non-fiat asset is priced in any fiat that has an FX rate by composing `price(asset, USD) x rate_usd[CCY]`. USD is the anchor because it has the broadest direct coverage.
2. It is the last layer of `priceFallback`, after the Redis VWAP cache, the stablecoin proxy and the fiat-fiat cross, so it fires where no direct book exists. A direct book is weighed against it by ADR-0053.
3. A derived value is flagged `triangulated`; `sources` carries the USD leg's venues plus the FX feed.
4. A withheld USD leg is never laundered through FX: every withholding verdict (scam-flagged issuer, decimals guard) is made on the USD leg and propagates, so the derivation serves nothing and the caller reports `price-withheld`.
5. Composition is exact `big.Rat`, never float64. Closed surfaces (`/v1/price`, `/v1/price/batch`, `/v1/oracle/x_last_price`) bind the FX leg to an hourly fixing at the USD bucket's end (`closedUSDAnchoredFiatCross`). `/v1/price/tip` uses the forex snapshot rate and refuses one older than `fx_cross_max_age_hours` (`tryUSDAnchoredFiatCross`).
6. The derivation is late-bound and never stored at ingest, so an FX correction applies retroactively (same argument as ADR-0026).

## Invariant

- A withheld USD leg serves no derived price: `TestPriceWithheldUSDLegIsNotLaunderedThroughFX` in `internal/api/v1/usd_anchored_fiat_cross_test.go`.
- A derived price is never float64 arithmetic: `convertAtFixing` and `tryUSDAnchoredFiatCross` in `internal/api/v1/price.go`, by review under AGENTS.md money invariant 1.
- A tip derivation rejects a stale FX rate: `TestPriceUSDAnchoredFiatCrossRejectsStaleTickerEvenWithFreshSnapshot` in `internal/api/v1/usd_anchored_fiat_cross_stale_test.go`.
- A currency with no FX rate, or an asset with no USD price, still misses: `TestPriceUnknownFiatStillMisses`, `TestPriceNoUSDLegStillMisses` in `usd_anchored_fiat_cross_test.go`.

## Consequences

- Every asset priced in USD is priced in every fiat with an FX rate, from data already ingested; no new feed or storage.
- A derived price inherits the FX leg's error and lag; it is a display rate, not a settlement rate (`docs/methodology/local-currency-pricing.md`).
- USD is the single pivot: degraded USD coverage degrades every derived currency for that asset.

## Evidence

`internal/api/v1/price.go` (`closedUSDAnchoredFiatCross`, `tryUSDAnchoredFiatCross`, `resolveUSDLeg`, `convertAtFixing`), `internal/api/v1/price_tip.go`, `internal/api/v1/usd_anchored_fiat_cross_test.go`. Ordering superseded by [ADR-0053](0053-fiat-price-basis-rule.md).
