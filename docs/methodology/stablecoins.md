---
title: Stablecoin supply methodology
last_verified: 2026-10-05
status: current
---

# How Stellar Index serves stablecoin supply

`GET /v1/stablecoins` serves the stablecoins on Stellar with their
circulating supply and USD value. Implementation:
`internal/api/v1/stablecoins.go`.

## What is in the set

The set is the verified-currency catalogue's `class: stablecoin` entries
that carry a Stellar issuer, keyed by `(code, issuer)`. The catalogue is
hand-vetted; the endpoint adds no issuer and derives nothing from an
aggregator. A same-code token from another issuer is not in the set.

## What is out

- Catalogue stablecoins with no Stellar issuer are listed under `excluded`.
  Today that is USDT: Tether has no native Stellar issuer, and the USDT seen
  on Stellar is third-party bridged.
- Stablecoins nobody has added to the catalogue. The headline is therefore
  always a lower bound.

## Valuation

Supply and price come from the same reads `/v1/assets` uses (supply
preference chain, substance gate, dust guard). The price is the real pair
price and is never coerced to the peg, so a depeg shows. `supply_usd` is
the asset's `/v1/assets` market cap. When that market cap is withheld
(the Stellar price failed the substance gate and a global-market or
declared-peg price fills in), the row is `withheld`, shows its
`price_basis`, and is named in `total.not_summed` as `market_cap_withheld`.
All amounts are decimal strings.

## The total

`total.supply_usd` sums the published per-asset strings of USD-pegged,
non-yield-bearing members. It is absent when none is valued.
`total.lower_bound` is true when a member is unvalued or not summed, a supply
is itself a floor, or `excluded` is non-empty.

- Non-USD pegs (EURC) are not summed into the USD total; `by_peg` keeps them
  apart.
- Yield-bearing wrappers (yUSDC) are listed with their own market price but
  not summed, since their price is not meant to sit at the peg.
- The peg of each ticker is a table in `stablecoins.go`; a ticker missing from
  it is served with peg `unknown` and never summed.
