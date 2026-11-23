---
title: Stablecoin supply methodology
last_verified: 2026-11-21
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

`total.supply_usd` sums the published per-asset strings of USD-pegged
members, leaving out a yield-bearing wrapper whose underlying is in the set.
It is absent when none is valued. `total.lower_bound` is true when a member
that should be summed is unvalued, a non-USD or unmapped peg is left out, a
supply is itself a floor, or `excluded` is non-empty. Every member left out,
valued or not, is named in `total.not_summed`.

- Non-USD pegs (EURC) are not summed into the USD total; `by_peg` keeps them
  apart. Each `by_peg` row carries `assets_valued` and its own `lower_bound`,
  true when one of its members is unvalued, a supply is a floor, or the
  member list was truncated.
- Yield-bearing wrappers (yUSDC) name their underlying by asset id in
  `stablecoins.go`. While that underlying is in the set, the wrapper is listed
  with its own market price but not summed, since its backing is already
  counted in the underlying's supply: it is in its `by_peg` row's `assets` but
  not in `assets_valued` or `supply_usd`, it is named in `total.not_summed`,
  and, valued or not, it makes neither the row nor the total a floor. A
  wrapper whose underlying is not in the set is an ordinary member.
- A `by_peg` row's `lower_bound` ignores `excluded` (USDT has no Stellar
  issuer) and `not_summed`, so it can read false where `total.lower_bound` is
  true for the same members. Only `total.lower_bound` carries those reasons.
- The peg of each ticker is a table in `stablecoins.go`; a ticker missing from
  it is served with peg `unknown` and never summed.
