---
title: What counts as a tokenized real-world asset
last_verified: 2026-09-10
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
`issuer` names the account that served the file.

The comparison is on the **account**, not on the bytes the toml carried:
surrounding whitespace and the case an issuer typed its own key in do
not change which account it is, and a strkey is base32, so two spellings
differing only in case decode to the same 32 bytes. That is a
canonicalisation and not a relaxation — a value that is not shaped like
a G-account strkey binds to nothing, and the entry is always carried
under the canonical spelling rather than the toml's. A declaration with
no `issuer` field at all is refused: the file was fetched from a domain
the account *chose*, and many accounts can choose the same domain, so an
unbound entry would let any of them inherit the claim.

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

## Coverage: where the rest of the population went

`refused[]` alone cannot answer "is that all?". It reports the
requirements that were **evaluated**, and the stages that remove most of
the population run before any candidate reaches the definition. Measured
on production 2026-09-10, the endpoint served 6 assets and a refusal
tally of 3 — over a table holding 59,303 issuer accounts, 44,376 with a
`home_domain` and 14,635 with a fetched SEP-1 payload. Nothing in the
response distinguished a network that holds six real-world assets from a
pipeline discarding fourteen thousand candidates in silence.

`funnel[]` is the complete accounting. It runs from every issuer account
that could carry an attestation down to the rows served, with a counted
reason on every drop:

| Stage | Unit | What it counts |
| --- | --- | --- |
| `issuers_with_home_domain` | issuer accounts | Every account a SEP-1 attestation could exist for. |
| `issuers_with_sep1_attestation` | issuer accounts | Those whose `stellar.toml` has been fetched and parsed at least once. |
| `issuers_declaring_currencies` | issuer accounts | Those whose payload carries at least one `[[CURRENCIES]]` entry. |
| `sep1_currency_entries` | declarations | Every `[[CURRENCIES]]` entry across those payloads. |
| `issuer_bound_entries` | declarations | Those naming the account that served the file (R2). |
| `candidate_assets_evaluated` | assets | The bound declarations put to the full R1→R4 evaluation. |
| `assets_admitted` | assets | Those the definition admitted. |
| `assets_served` | assets | The rows in `assets[]`. |

Each stage's drops account exactly for the difference to the next stage
of the same unit, and `funnel.balanced` states whether that
reconciliation held. When membership cannot be established the funnel is
**empty and unbalanced** rather than a column of zeros, for the same
reason `market_cap_usd` is absent rather than `"0.00"`.

Every drop carries an `actor` naming who can move it — `operator`,
`issuer`, or `definition` for a drop that is the rule working as
intended. That distinction is the point. Two numbers that look identical
as bare counts are opposite findings:

- `entry_declares_another_issuer` is R2 refusing an impersonator. It is
  expected to be the largest bucket on the whole surface and it needs no
  action: a `stellar.toml` describes only the account that served it.
- `sep1_attestation_never_fetched` is an issuer that publishes a domain
  whose file nobody has fetched yet. It is not a property of the network
  at all — it is a refresh cron that has not reached that account — and
  it is the largest coverage lever an operator holds.

The second class is where this surface's coverage actually comes from.
Widening it means moving one of those numbers — fetching the
attestations, or extending the curated directory so an issuer that
publishes a correctly-bound real-world declaration can be recognised.
It never means loosening R1–R4 until a bucket empties: a longer
dashboard bought that way is a directory of impersonators with dollar
figures attached, which is strictly worse than under-reporting.

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

## Premium and discount to the instrument's value

A tokenized treasury has two prices: what an independent oracle says the
instrument is worth, and what the Stellar market will pay for the token.
The gap between them is the number a holder actually needs, and it is the
one figure here that neither the chain nor an oracle produces alone.

Each row carries `reference` — an oracle's valuation of the real-world
instrument, with its publisher, the canonical feed id, its denominator
and its vintage — and `premium`, the market price measured against it:

```
premium.pct = (market − reference) ÷ reference × 100
```

Positive means the token trades **above** the instrument's independent
valuation, negative **below**. Exact rational arithmetic, served as a
decimal string.

### The binding is on the pair, never on the code

This is the same identity rule the definition itself turns on, applied to
the figure. Asset codes are not unique on Stellar: any account may issue
a token called `USTRY`, `BENJI` or `XAU`, and the network holds many
that do. A reference joined on the code alone answers every one of them
with the real instrument's net asset value, so an unrelated token trading
at $0.20 is published at an 81% discount to a security it has nothing to
do with. That is a false financial claim about a real instrument.

So the join runs through an explicit, curated table of
`(code, issuer) → feed` bindings, served in full as
`definition.bound_instruments`. It is matched **exactly** on both halves —
a case variant is a different token — and it fails closed: a pair absent
from the table gets `reference_not_bound` and no figure, whatever the
token is called.

This is the curated-set mechanism [ADR-0040](../adr/0040-completing-contract-gating.md)
sanctions for gates whose membership cannot be derived on chain: an
enumerated allow-list, review-gated by living in code, where an unlisted
candidate is refused and the refusal is *reported* rather than absorbed.
Each entry records its evidence in `internal/rwa/oracle_reference.go`,
and an entry needs all three of:

1. the issuer's domain-bound SEP-1 entry for that exact pair, naming the
   instrument (requirement R2);
2. the curated directory attributing that account to a named entity.
   Where ADR-0028 also attributes the feed to an entity the two must
   *agree* — but ADR-0028 attributes only some feeds that way (USDY,
   USST, XAUm, deJAAA, deJTRSY) and lists CETES, USTRY, TESOURO, GILTS,
   KTB and SPXU by ticker alone. For a ticker-only feed this cannot be
   met by matching attributions and requirement 3 carries the binding;
3. something tying the feed to that issuer specifically rather than to an
   instrument of that name. Price agreement between the token's Stellar
   market price and the feed is the strongest form; a documented
   product-line correspondence is the weaker one and is marked as such.

Entries carrying only the weaker form are the ones to challenge first in
review.

### The other four rules

Each removes a way of publishing a number that means something other than
what it says. A row failing any of them carries no figure and states
which rule refused it in `premium.status`.

| Rule | Refusal |
| --- | --- |
| The oracle value must be **dollars**. A bare `_FUNDAMENTAL` feed publishes net asset value in the token's *reserve* asset, so its value is a ratio, not a price. The denominator is read off the stored row and must be `fiat:USD`; it is never converted here. | `reference_not_usd_denominated` |
| The feed must price **one token**. `rwa:XAU` is spot gold per troy ounce and `rwa:SPXU` is one share of an exchange-traded fund. Neither is one token of anything, so no binding may target one — a test enforces it — and a token of such a code is refused with that as the stated reason. | `reference_not_instrument_scoped` |
| The publisher must be an **oracle**. Aggregators write into the same table for divergence comparison; a premium against an aggregator's read of the market compares the market with itself. Their rows are dropped when the snapshot is built, so a bound pair with no oracle row left is reported as having no feed. | `no_reference_feed` |
| The market price must be **observed**. A price carried on `price_basis` is a declared peg or a transitive derivation, and a premium against either reports the issuer's own claim back as a market finding. | `market_price_not_observed` |

A scam-flagged issuer gets no valuation of any kind, including a third
party's (`withheld_issuer_flagged`). Handing an impersonator the real
instrument's net asset value would publish a *larger* claim than the one
the flag suppressed — a dollar figure on the impersonator, sourced from
an oracle that never named it.

### Absence, outage and expiry are three different statements

Each `premium.status` means one thing and only one thing, because a
reader cannot act on a status that collapses them:

- `reference_not_bound` — the pair is not in the curated table. Usually a
  code collision.
- `no_reference_feed` — the pair *is* bound, but no oracle is publishing
  its feed.
- `reference_unavailable` — the oracle read did not answer. Nothing is
  known either way. A failed read has not learned what the oracles carry
  and is not entitled to report an absence; on the wire it must not look
  like one.
- `reference_expired` — the bound feed's most recent observation is older
  than the seven-day window. Reachable only when a snapshot is carried
  across a sustained read failure.

### What the comparison rests on

That one unit of the token is one unit of the named instrument. The
evidence is the issuer's own domain-bound SEP-1 declaration (R2) plus the
independent recognition of the issuing account (R3) — the same evidence
that admitted the asset — and **not** a separate measurement of
denomination. The surface states this beside the figure rather than
letting a percentage imply a certainty nobody established.

An unavailable comparison is **absent**, never `0`. Zero would read as
"trades at par", which is a finding; a comparison that was never made is
not that finding. A reference older than 72 hours — the longest ordinary
gap between two strikes of a real-world instrument's value — is labelled
`stale` rather than withheld, because a value struck last Friday is still
the last one published.

The outer bound is absolute and is enforced on the **observation**, not
on the snapshot: an oracle reading older than seven days is not served,
however it reached the request. A live read cannot return one — the
stream query defines an active stream by that window — but a snapshot is
carried forward across a failed read, and under a sustained outage the
carried rows would otherwise age past the bound this page claims.

## Coverage

`summary.earliest_first_seen_ledger` is the lowest ledger any member was
first observed at, read from an index complete since genesis. It is a
true first appearance, not the start of a sampling window — the same
distinction that separates a complete index from a query over a
retention window.

`summary.assets_with_reference` and `summary.assets_compared` state the
coverage of the instrument comparison: how many members carry an
independent valuation, and how many of those also have a market price to
measure it against. Both are served because the difference between them
is exactly the set of blank premium cells, and a reader who saw only the
premiums would take the blanks for zeros.

The membership set is rebuilt at most once per ten minutes, off the
request path. Its inputs move on daily cadences (the SEP-1 refresh cron
and the directory sync), so the window is well inside the rate at which
the answer can change. On a rebuild failure the previous set is served
rather than an empty one.

The oracle reference snapshot is cached separately and for 30 seconds
only. It is a price: reusing a ten-minute-old net asset value against a
live market price would report a premium neither figure supports. One
stream read serves the whole set however many members it has, and a read
that does not answer leaves the previous snapshot in place. Until the
first successful read there is no snapshot to carry, and rows then say
`reference_unavailable` — not that no oracle publishes their instrument,
which is a finding a failed read has not earned.

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

- Implementation: `internal/rwa` (the definition and the
  comparable-instrument classification),
  `internal/storage/timescale/sep1_bound_currencies.go` (the provenance
  rule), `internal/api/v1/rwa.go` (the read path and wire shape),
  `internal/api/v1/rwa_reference.go` (the oracle reference and the
  premium).
- [ADR-0028](../adr/0028-rwa-asset-representation.md) — the `rwa:`
  reference-asset namespace and the oracle feed allow-list R4 reads.
- [dex-tvl.md](dex-tvl.md) — the same posture applied to a different
  aggregate: exclusions named, lower bounds labelled, and silence
  preferred to a plausible-looking wrong figure.
