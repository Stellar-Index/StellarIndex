---
title: Listing-priced valuation
last_verified: 2026-09-15
status: current
---

# Listing-priced valuation

A verified-catalogue asset whose **market capitalisation this index
declines to publish** can still carry a valuation from an independent
listing platform — under its own name, its own provenance, and never
inside `market_cap_usd`.

This page says what that figure is, what has to be true before it is
served, and the two things it must never be read as.

## The hole it fills

USDT0 launched on Stellar on 2026-09-02. On this network it trades about
**$106 a day**. The dust-liquidity guard therefore suppresses its market
cap — a price scraped off a $106/day market, multiplied by two and a half
million tokens, is a headline nobody should publish — and the row serves
`market_cap_low_liquidity: true` with no figure.

That refusal is correct and is unchanged by anything on this page. What
it does not establish is that the price is unknowable. An independent
listing platform publishes a USD price for that exact token, derived from
venues this index does not observe and has no opinion about. Supply times
**that** price states something true and separately checkable. Folding it
into market cap would state something false.

## What has to be true

Five conditions, all of them, in this order.

1. **The asset is in the verified catalogue.** A `(code, issuer)` pair
   written into `internal/currency/data/seed.yaml` — a code change and a
   redeploy. The listing corroborates; the catalogue attests. Neither
   alone publishes anything.

2. **The listing names the address exactly.** Two routes count:

   | Route | What the platform published |
   |---|---|
   | `classic` | the asset's own `CODE-GISSUER` id |
   | `sac` | the Stellar Asset Contract address `canonical.Asset.SacContractID()` derives from that exact `(code, issuer)` and the network passphrase |

   Both are needed: measured against the live upstream on 2026-09-15,
   EURC, AQUA, SHX, VELO, BLND and yUSDC are named by their classic ids,
   while USDC, PYUSD, USDT0 and XLM are named by their SAC addresses and
   not by their classic ids at all.

   The second route is safe for a structural reason rather than a
   probabilistic one. SAC derivation is a pure function of the asset and
   the network, so the published address is reachable from one
   `(code, issuer)` pair and no other; an impersonator minting the same
   code from a different `G`-account derives a different address, and the
   listing's row does not name it.

   **Matching by code is never done.** This network carries impersonating
   issuers of PYUSD, USDT, USDC and XLM — one holding a 920-billion fake
   balance — and a code match would hand each of them the real
   instrument's price.

3. **There is a hole, and it is a price hole.** A row that publishes a
   market cap is left alone entirely (`status: market_cap_published`). So
   is a row carrying an observed market price that cleared the substance
   gate with no dust suppression (`status: market_price_observed`): its
   missing cap is a missing *supply* reading, and filling it from a third
   party would hide a gap in this index's own data behind somebody else's
   number. A declared-peg or transitive price is a conversion basis, not
   an observation, and does **not** stand the arm down.

4. **The supply comes from the lake, not from a trustline sum.** The
   figure multiplies the larger of the row's own reading and
   Σmint − Σburn − Σclawback over the asset's SAC, and `supply_basis`
   says which was used. USDT0 is why: its trustline-visible supply is
   **6,469 tokens against 2,581,052 by mint−burn**, because almost all of
   its float sits in balances a trustline query is blind to by
   construction. Valuing the wrong one publishes a figure 400× too small
   and looks entirely plausible doing it.

5. **The snapshot is fresh.** The directory read fails closed: an
   unreadable or empty snapshot publishes nothing and says
   `listing_unavailable`, which means *nobody looked* and never *nobody
   lists it*. The price's own age is bounded on the **platform's**
   publication clock, by the same two constants the RWA reference arm
   uses — 72 hours labels it `stale`, 7 days withholds it entirely.

## What it is not

It is **not** a market capitalisation, and the two are never folded
together:

- `market_cap_usd` is adversarially verified. A price reaches it only
  after the thin-market substance gate, the dust-liquidity guard and the
  scam-issuer suppression have each declined to withhold it, and behind
  that price is a trade somebody settled on a venue this index observed.
- `listing_valuation.value_usd` is supply times a figure a third party
  published about venues this index observed none of and applied none of
  its gates to. It is a second opinion, published because withholding a
  figure that can be correctly sourced and correctly labelled is its own
  kind of dishonesty — not because it is equivalent.

A consumer that adds the two has to say so. The `/rwa` page's stablecoin
tile does: the moment its total contains a listing-priced row, the tile's
copy reads *"This total mixes two bases"*, names how many rows came from
which, and the combined tile beside it stops claiming there are only two.

## On the wire

```json
"listing_reference": {
  "price_usd": "0.999357",
  "source": "coingecko",
  "listing_id": "usdt0",
  "quote": "fiat:USD",
  "address": "CBSJZEIO5C7KC2SF3MKSNXXJSW5G3VTNBX4ATMKUI3B2MR4JKM4R26YF",
  "address_form": "sac",
  "as_of": "2026-09-15T16:38:00Z",
  "provenance": "listing_platform_price"
},
"listing_valuation": {
  "status": "published",
  "value_usd": "2579393.28",
  "circulating_supply": "25810528958550",
  "supply_basis": "lake_flows"
}
```

`provenance` shares its vocabulary with `RWAReference.provenance`, which
is where the same claim is already published for contract-issued
real-world assets — see [rwa-definition.md](rwa-definition.md).

Both blocks are omitted entirely on any row outside the population above,
which is almost every row. `listing_valuation` appearing alone, carrying
only a `status`, is the arm saying which of its conditions failed.

## Where it surfaces

- `GET /v1/assets/{asset_id}`
- `GET /v1/assets?asset_class=stablecoin|crypto` (the catalogue listing)
- the `/rwa` page's stablecoin and combined tiles

It is deliberately **not** added to `GET /v1/rwa/assets`: that surface
already publishes this exact claim through its own `reference` block, at
`provenance: listing_platform_price`, with a summary basis that names the
mixture in prose.
