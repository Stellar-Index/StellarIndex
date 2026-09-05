---
title: What counts as a tokenized real-world asset
last_verified: 2026-09-05
status: current
---

# What counts as a tokenized real-world asset

The page at `/rwa` and the endpoint at `GET /v1/rwa/assets` publish a
set of Stellar assets described as representing real-world value, each
with a valuation. This document is the definition of that set: what
qualifies, what does not, and what the evidence behind each requirement
actually proves.

The definition is the whole product. A permissive rule does not make a
longer dashboard — it makes a directory of impersonators with dollar
figures attached.

## The problem the definition exists to solve

Asset codes are not unique on Stellar. Any account can issue a token
called `USTRY`, `BENJI` or `XAU`, and many do. Identity is therefore
always the pair `(code, issuer)` — a code alone identifies nothing.

SEP-1 gives an issuer a place to describe its own assets: a
`stellar.toml` at the `home_domain` the issuer account sets on chain,
with a `[[CURRENCIES]]` entry per asset carrying, among other fields, an
`anchor_asset_type`. That field is the obvious basis for an RWA set, and
on its own it is worthless. Measured on the production deployment
2026-09-05:

| Reading | Count |
| --- | --- |
| `[[CURRENCIES]]` entries declaring `anchor_asset_type` | 1,182,000+ |
| of which bound to the account that served the file | 23,900 |
| bound entries declaring a real-world instrument class | 4,166 |
| distinct issuers behind those | 130 |
| **of those 130 issuers, tagged `malicious` by the curated directory** | **128** |

The domains publishing them are the tell: `nasdaq.com.co`,
`cboe.com.co`, `tase.com.co`, `spglobal.com.co`, `asx.com.co`,
`six-group.com.co`, `euronext.co.com`, `jpx.co.com`,
`berkshirehathaway.co`, `blackrock.co.com`. Each serves a valid,
correctly-bound SEP-1 file declaring hundreds of `stock` tokens. The
binding proves the file came from a domain the issuer controls; it
proves nothing whatsoever about whether that domain is the exchange it
is named after.

The same holds for the instruments an RWA price oracle covers. `BENJI`
exists under two different issuers, one of them directory-flagged.
`USDY` exists under `ondo.finance` and under `blackrock.co.com`. `XAU`
exists under `xau.cl` and under a dozen `swisscustody`-shaped lookalikes,
`stellarmetals.gold`, `blakrock.claims` and `lobstr.cfd`.

## The definition

An asset is in the set when **all four** requirements hold. They are
implemented in `internal/rwa` and evaluated in this order; the endpoint
reports the first one a candidate failed.

### R1 — Identity

A classic Stellar asset with a code **and** an issuer G-address.

The native asset is not an RWA. Soroban-only contracts are out of scope:
SEP-1 `[[CURRENCIES]]` binds a declaration to a `(code, issuer)` pair,
and a bare contract address has no such binding, so admitting one would
mean admitting an unbound claim.

### R2 — Issuer-bound self-declaration

The issuer account serves a SEP-1 `stellar.toml`, fetched over HTTPS
from the `home_domain` that account set **on chain**, containing a
`[[CURRENCIES]]` entry whose `code` matches the asset and whose declared
`issuer` is equal to the account that served the file.

This is the same provenance rule the SEP-1 logo overlay enforces, added
after a token was able to take over another issuer's served logo by
naming that issuer in its own toml. A toml describes only the account
that served it.

What R2 proves: the claim came from a domain the issuer controls, bound
to this exact strkey. Nothing more. That is a low bar, which is what R3
is for.

### R3 — Independent recognition

The issuer G-address is named in the curated third-party account
directory (`account_directory`, migration 0136, synced from the
MIT-licensed stellar-expert public directory) with at least one
**recognition tag** — `issuer`, `anchor`, `custodian`, `exchange`,
`defi`, `sdf` — and **no** scam-class tag.

The scam-class vocabulary is not restated for this surface. It is read
from `timescale.DirectoryScamFlagTags`, the single list the
price-withholding gate and the `/v1/assets` rank expression also read, so
an issuer whose price the platform withholds can never be admitted here.

The recognition set is deliberately narrower than "has any tag":
`personal`, `wallet`, `memo-required`, `airdrop`, `application` and
`infra` describe an account without vouching for it as the issuer of a
real-world instrument.

R3 is the requirement that a party **other than the issuer** vouched for
that specific account. It is why the lookalike-exchange population is
absent rather than merely ranked low, and it is the requirement that
fails **closed**: if the directory cannot be read, no set is published.
Every other read on this surface degrades open, because every other read
only omits detail.

### R4 — Real-world instrument

The asset is a real-world instrument rather than one of the issuer's
other tokens, by one of two bases. The basis is served on every row so a
consumer can filter to the strength of evidence it needs.

**`sep1_anchor_declaration`** — the R2 entry declares an
`anchor_asset_type` in the closed vocabulary `stock`, `bond`,
`commodity`, `realestate`.

That vocabulary is SEP-1's own enumeration minus the terms that name no
real-world instrument. `anchor_asset_type` is free text on the wire —
the production set holds `equity`, `etf`, `metal`, `rwa`, `real_estate`,
`sovereign` and dozens of other invented spellings — and folding
synonyms in is how a closed set stops being closed. Three exclusions are
deliberate:

- **`fiat`** — a fiat-anchored token is a stablecoin. Different
  instrument, different risk story, its own surface. Folding it in would
  silently multiply the headline figure.
- **`crypto`** and **`nft`** — neither is a real-world asset.
- **`other`** — it classifies nothing, and 6,309 bound entries carry it.

**`oracle_rwa_feed`** — the asset's code is an ADR-0028 allow-listed RWA
code, meaning an independent price oracle publishes a net-asset-value
feed for an instrument of that name. This admits the real issuers that
publish a bound `[[CURRENCIES]]` entry without filling in
`anchor_asset_type`, which several do.

This arm matches on the **code**, which is exactly the identity the rest
of the definition refuses — so it is admissible only **after** R3 has
bound the issuer to a recognised, unflagged account. It is never
load-bearing on its own. A row admitted this way carries **no**
`anchor_class`: an oracle feed names an instrument, not its class, and
inventing one would publish a classification nothing declared.

## What an asset that fails gets

Nothing on this surface. It is absent — not hidden behind a filter, not
ranked last. The set is a claim about real-world backing, and an asset
that cannot meet the bar has no partial place in it. Its own
`/v1/assets/{id}` page continues to serve it under the gates that
already apply there, including any scam warning.

The refusals are counted and published: `refused[]` on the response
reports how many candidates each requirement turned away, so the served
set is never mistaken for the whole population of assets that *claim* to
be real-world assets.

## Valuation

Every figure comes from the existing `/v1/assets` read path, unchanged:
the same catalogue query, the same thin-market substance gate, the same
supply-derived market cap, the same dust-liquidity guard, the same
scam-issuer payload suppression. This surface adds no price path of its
own, so it cannot publish a figure `/v1/assets` would have withheld.

Membership is decided **before** valuation, from identity and
attestation only. No number moves an asset in or out: a withheld price
cannot silently shrink the set, and a large market cap cannot buy a
place in it.

Each row carries a `valuation.status`, and only `published` carries
money:

| Status | Meaning |
| --- | --- |
| `published` | A price and a supply were both available and no gate withheld them. |
| `withheld_issuer_flagged` | The issuer acquired a scam-class directory tag after the membership set was built. The row stays; its valuation does not. |
| `unpriced` | No USD price — the market produced none, or the substance gate withheld it as too thin to aggregate. |
| `withheld_low_liquidity` | A price exists but the dust-liquidity guard refused to turn it into a market cap. |
| `supply_unavailable` | A price exists but no circulating-supply reading does. |

When a price is served but is not a direct market observation — an
operator-declared fiat peg, or a value derived through one substance-gated
intermediate hop — the row carries `valuation.price_basis` naming which,
exactly as `/v1/assets` does. A valuation surface that showed the figure
and hid how it was derived would be the same claim with the caveat
removed.

A withheld or unavailable valuation is **absent**, never zero and never a
stale figure. `circulating_supply` is served regardless: it is a raw
chain fact, not a price claim.

`summary.market_cap_usd` is the exact sum of the rows' own published
market caps — add up what you can see and you land on that number. It is
**absent**, not `"0.00"`, when no asset in the set publishes one, because
a zero there reads as a real total of zero dollars. `summary.lower_bound`
is true whenever any member is unvalued, so the total is less than the
value of the set.

## Coverage

`summary.earliest_first_seen_ledger` is the lowest ledger any member was
first observed at, read from an index complete since genesis. It is a
true first appearance, not the start of a sampling window — the same
distinction that separates a complete index from a query over a
retention window.

The membership set is rebuilt at most once per ten minutes, off the
request path. Its inputs move on daily cadences (the SEP-1 refresh cron
and the directory sync), so the window is well inside the rate at which
the answer can change. On a rebuild failure the previous set is served
rather than an empty one.

## Known limits

- **The set is small.** Under this definition it is a handful of assets
  from a handful of issuers. That is the honest answer to "which Stellar
  assets are real-world assets whose backing an independent party has
  vouched for, per `(code, issuer)`" — and it is a different question
  from "which companies say they tokenise real-world assets on Stellar",
  which a hand-curated company list answers with a bigger number and no
  per-asset identity.
- **R3 delegates recognition to one third-party directory.** A real
  issuer the directory has not yet labelled is refused, and the refusal
  is counted rather than silently absorbed. A second recognition source
  would widen the set without weakening the rule; the
  `account_directory` schema is already scoped by `source` for exactly
  that.
- **No historical series.** Market cap over time needs a per-asset daily
  supply-and-price rollup that does not exist yet. Nothing on this
  surface is back-projected from current state, and no monthly series is
  published from a figure that was only ever measured today.
- **Soroban-issued RWAs are out of scope** until there is a binding for
  a contract address equivalent to what `[[CURRENCIES]]` gives a
  `(code, issuer)` pair.

## References

- Implementation: `internal/rwa` (the definition),
  `internal/storage/timescale/sep1_bound_currencies.go` (the provenance
  rule), `internal/api/v1/rwa.go` (the read path and wire shape).
- [ADR-0028](../adr/0028-rwa-asset-representation.md) — the `rwa:`
  reference-asset namespace and the oracle feed allow-list R4 reads.
- [dex-tvl.md](dex-tvl.md) — the same posture applied to a different
  aggregate: exclusions named, lower bounds labelled, and silence
  preferred to a plausible-looking wrong figure.
