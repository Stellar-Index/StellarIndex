---
title: What counts as a tokenized real-world asset
last_verified: 2026-09-11
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

> **A candidate has to be in `issuers` before any of this is evaluated.**
> The membership build's only input is `BoundSep1Currencies`, which reads
> `issuers WHERE sep1_payload IS NOT NULL`, and until 2026-09-10 an
> `issuers` row could only be created by the classic-asset registry
> writer, which only ever ran on a TRADE. So an issuer whose assets are
> HELD but never traded on the SDEX was not refused by R1–R4 — it was
> never a candidate, and does not appear in the `refused` counts either.
> Measured 2026-09-10: 512,496 classic assets have a trustline against
> 199,793 registry rows, 61% absent, Franklin Templeton's BENJI among
> them. The registry now has a holdings source
> (`stellarindex-ops asset-registry-backfill`, migration 0158), so the
> candidate pool is the held population rather than the traded one — but
> the chain still runs `registry → issuer-enrich (home_domain) →
> sep1-refresh (payload) → candidacy`, and a newly-registered issuer
> enters the set only after those two jobs have reached it. A figure from
> this endpoint that looks low against a public dashboard is worth
> checking against that chain before it is read as a definition
> disagreement: the $3.88M-against-$4.03B gap was diagnosed as one, and
> was not.

### R1 — Identity

A classic Stellar asset with a code **and** an issuer G-address. The
native asset is not an RWA.

A contract-issued token qualifies through the **separate arm** below,
under requirements C1–C4, on its contract address. The two arms never
overlap: this one runs over SEP-1 attestations keyed by
`(code, issuer)`, that one over curated directory entries keyed by
contract address.

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

## The contract arm

Everything above describes an asset with an issuer account. The entities
that actually hold real-world assets on Stellar mostly do not have one.

Measured 2026-09-10, of the sixteen entities the public Stellar RWA
dashboard attributes $4.03B to, the ones we could check issue **nothing**
in `classic_assets`: Franklin Templeton (both G-addresses), Spiko,
Mercado Bitcoin, Cometum and fourteen of WisdomTree's eighteen addresses
all return zero rows. Their Stellar presence is contract-issued.

The exclusion ran deeper than R1. The `issuers` table is written from
exactly one call site — `registerIssuerSeen`, on classic-asset
registration — so an entity issuing only contract tokens never gets a
row, never gets a SEP-1 fetch, and never becomes a candidate. It was not
refused by R1 so much as never collected. Widening that table is not the
fix either: its key is a G-account, a contract address is not one, and a
SEP-1 fetch for a bare contract has nothing to bind to.

So the contract arm draws its population from `account_directory`, the
only table we hold that ties a real-world entity to a Stellar address
without passing through classic issuance. Its `CHECK` has always
accepted both strkey forms.

### C1 — Identity

A contract address, CRC-checked. It is the whole identity — never a
symbol, never a name. Two contracts declaring `BENJI` are two different
assets, exactly as two issuers of a classic `BENJI` are.

### C2 — Independent naming of that exact address

The curated third-party directory holds an entry for **that exact
contract address**. Not the entity. Not a domain. Not a G-account that
might have deployed it. The address whose supply this surface is about
to multiply by a price.

This is what replaces R2, and it is **not** a relaxation — because of
what R2 was actually worth.

R2 proves a claim came from a domain the issuer controls, and the cost
of satisfying it is one domain registration. Measured, **19 distinct
issuers publish a token called `BENJI` in this lake and every one is an
impersonator**: `franklintempleton.co.com`,
`franklintempleton.hqlumens.com`, `benji.qlumen.co`,
`stellar.dtcc.network`, `treasury.dtcc.company`. Each serves a valid,
correctly-bound `stellar.toml`. Not one is `franklintempleton.com`. R2
admitted all of them. R3 is what kept them out, and R3 is what carries
this arm.

**What an attacker would have to control.** To get a fake contract
admitted, an attacker needs a curated directory entry naming their own
C-address with an issuing-class tag and no scam tag — that is, landing a
reviewed change in a third party's published set under the name of the
entity being impersonated.

The classic equivalent for R2 is registering a lookalike domain and
serving a file: no review, no third party, roughly the price of a
domain, done 19 times over for one ticker. And R3 on the classic arm
vouches for an **account** — one recognition covers every token that
account will ever issue, including ones issued afterwards. The contract
arm has no such carry-over: recognition is per address, and a contract
address is one token.

The contract arm is therefore **strictly harder to defeat** than the
classic one. What it gives up is the issuer's own voice, and the
issuer's own voice is the part an impersonator supplies for ten dollars.

### C3 — Recognition, not flagged

The entry carries at least one tag from a vocabulary deliberately
narrower than the account one: `issuer`, `anchor`, `custodian` — and
**not** `defi`, `exchange` or `sdf`.

On an account those three describe an entity that might issue a
real-world instrument. On a contract address they describe
infrastructure that issues nothing: the curated set carries the Aquarius
AMM pool contracts under exactly those tags, and admitting them would
put liquidity-pool shares on a page asserting real-world backing.

The scam vocabulary is the same single list every other consumer reads,
and a scam tag beats every recognition tag on the same address.

### C4 — Real-world instrument

A directory entry says who an address belongs to. It does not say the
token is a real-world asset — the curated set names stablecoin
contracts, pool shares and protocol infrastructure under the same tags,
and admitting every recognised contract would publish USDC as a
tokenized real-world asset. So C4 keeps R4's job, by one of two bases:

**`curated_contract_instrument`** — an in-repo curated entry binds this
exact contract address to a named instrument and its class. The
[ADR-0040](../adr/0040-completing-contract-gating.md) curated-set
mechanism, the same one the oracle bindings use.

It ships **empty**, and that is a refusal rather than an oversight:
populating it needs contract addresses from a primary source, and an
address inferred from a dashboard is a fabricated identity for a
financial instrument. An empty curated set refuses everything, which is
correct for a set with no verified members.

**`contract_oracle_rwa_feed`** — the token's on-chain SEP-41 `symbol` is
an ADR-0028 allow-listed RWA code. The symbol is contract-authored,
exactly as a classic asset code is issuer-authored, and this arm is
admissible for exactly the same reason its classic twin is: the
independent recognition of the address has **already** happened. It
answers *which* instrument an address someone vouched for holds, never
*whether* the address is vouched for. A row admitted this way carries no
`anchor_class`, for the same reason the classic oracle arm does not.

### What was considered and rejected

- **Contract-instance provenance** — admitting a contract because a
  recognised G-account deployed it. The right shape, and we hold no
  deployer edge to read: the `contractid` registry is factory-anchored
  per ADR-0035 and covers protocol children, not token issuance.
- **SEP-41 metadata as the admission** — admitting a contract whose
  `name()` says treasury. That is the token talking about itself at no
  cost at all, cheaper than a domain, and it would admit every
  impersonator in the lake.
- **The issuer's SEP-1 naming the contract** — the current SEP-1 draft
  carries a `contract` field on `[[CURRENCIES]]`. This is the strongest
  available strengthener and is worth having: the curator supplies the
  domain, so an attacker cannot choose who has to corroborate them, and
  defeating the pair means a directory merge **and** control of the real
  entity's domain. Not built yet for a measured reason — our parser does
  not read the field, and 14 of the 16 entities have no fetched payload
  at all. See the
  [coverage reconciliation](rwa-coverage-reconciliation.md).

### Valuing a contract asset

Both existing reads of a Soroban asset — the `/v1/assets` listing spine
and `GET /v1/assets/{id}` — gate the discovered-contract arm on a 24h
volume rollup, which admits 60 of ~117k contracts. That gate is right
for a listing and wrong here: a tokenized fund is held, not traded, and
can carry a nine-figure supply while never appearing in a volume
rollup. The RWA surface reads its **membership-bounded** set without it.

Supply is the certified lake's `Σmint − Σburn − Σclawback` — the same
figure `/v1/assets/{id}/supply` serves, from the same reader. An
incomplete reading (a negative net, meaning the flows are incompletely
seeded rather than that supply is negative) is treated as unavailable
and never clamped to zero, which would read as a fully-burned token.

Decimals come from the token's own on-chain metadata, not the hardcoded
7 a catalogue row carries. On this surface that is not a display detail:
market cap divides supply by 10^decimals, so a 6-decimal token valued at
7 publishes a tenth of its real capitalisation.

Everything else is the `/v1/assets` pipeline unchanged, including the
substance gate — which explicitly covers Soroban assets — and the
dust-liquidity guard. One gate is newly applied rather than reused: a
contract asset has never been subject to the directory scam suppression
on any surface, because the existing fill keys on the issuer G-address
and skips every row without one. On a page that admits a contract on the
strength of a directory entry, declining to re-read that entry when it
turns hostile would be indefensible, so the contract arm re-reads it at
valuation time.

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
| `assets_served` | assets | The classic rows in `assets[]`. |

Every stage carries an `arm`. The table above is the `classic` arm; the
`contract` arm narrows a different population from a different root and
they meet only at the served set, so the arithmetic reconciles **within**
an arm and never across the boundary:

| Stage | Unit | What it counts |
| --- | --- | --- |
| `curated_directory_entries` | directory addresses | Every row in the curated third-party directory. |
| `directory_contract_addresses` | contracts | Those whose address is a contract. An account address names an entity; a contract address names one token. |
| `directory_recognised_contracts` | contracts | Named with an issuing-class tag and no scam tag — C2 and C3 satisfied. |
| `contract_candidates_evaluated` | assets | Those put to the full C1→C4 evaluation. |
| `contract_assets_admitted` | assets | Those the definition admitted. |
| `contract_assets_served` | assets | The contract rows in `assets[]`. |
| `directory_recognised_issuing_accounts` | directory addresses | **Terminal census, not part of the narrowing.** |

That last stage is the one that matters most. It counts recognised,
unflagged issuing entities this index holds **no token for** — and
`unreached_entities[]` names a bounded sample of them. They are not
refused by any requirement; there is nothing to refuse, because no token
of theirs was ever collected. Measured 2026-09-10, Franklin Templeton
and Spiko are both in this position: in the directory, tagged `issuer`,
correct domains, and absent from the `issuers` table entirely.

It takes no part in the stage arithmetic, deliberately: it counts rows
the first stage already dropped, and folding a coverage census into a
narrowing would make the funnel close by adding a number that measures
something else.

When no curated-directory contract reader is wired, the contract arm is
**absent** from `stages[]` and `basis` says it was not measured — rather
than a run of zeros, which would read as a network with no
contract-issued real-world assets in it.

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

## Valuation — the market basis

Every figure in this section comes from the existing `/v1/assets` read
path, unchanged: the same catalogue query, the same thin-market
substance gate, the same supply-derived market cap, the same
dust-liquidity guard, the same scam-issuer payload suppression. This
surface adds no market-price path of its own, so it cannot publish a
figure `/v1/assets` would have withheld.

A second, separately-labelled valuation sits beside it —
[the reference basis](#the-second-valuation-what-the-backing-is-claimed-to-be-worth),
which values the same float at an oracle's price for the underlying
instrument. It is never summed into anything below, and nothing below
changed when it was added.

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

## The second valuation: what the backing is claimed to be worth

A real-world asset is bought and held, not traded. Measured on the
production deployment, four of the six classic members carry an
independent oracle valuation of their instrument and **two** have ever
produced a market price this platform will publish. Valuing the set at
observed market prices alone therefore reports nothing at all for half
of it — while an oracle prices the underlying instrument daily.

So each row carries a second figure, `reference_valuation`, and the set
carries its own total in `summary.reference_valuation`:

```
reference_valuation.value_usd = circulating_supply ÷ 10^decimals × reference.price_usd
```

`decimals` is served on the row — 7 for a classic asset, and whatever a
SEP-41 contract declares for a contract-issued one — so the figure can
be re-derived by hand from the published supply and the published
reference price. Exact rational arithmetic throughout, with a single
rounding to 2 dp at the end (ADR-0003), which is what makes the summary
total the exact sum of the per-row strings.

### It is a claim, not an observation

This is the distinction the whole section exists to hold, and it is the
reason the second figure is not called a market cap anywhere:

| | `valuation.market_cap_usd` | `reference_valuation.value_usd` |
| --- | --- | --- |
| What the price is | What buyers were observed paying | What an oracle says the instrument is worth |
| Evidence | A settled trade | A published feed, plus the issuer's declaration that one token is one unit of the instrument |
| Gates | Thin-market substance gate, dust-liquidity guard, scam-issuer suppression — each can withhold it | None of them apply. There is no market in it for them to measure |
| Adversarial standing | Somebody paid it | Nobody was observed paying it |
| An asset that never trades | Publishes nothing | Publishes the full figure |

Both are published because neither alone is honest here. A market-cap
total is silent about assets that plainly exist and are plainly held;
a reference total is an assertion by the issuer and its oracle that
nobody has tested by paying it.

**They are never folded together.** `market_cap_usd` means exactly what
it meant before this figure existed, over exactly the same rows: a
reference valuation never substitutes for a withheld market cap, never
enters `summary.market_cap_usd`, and never moves a row from unvalued to
valued on the market basis. A test asserts this directly, by serving the
same set twice — once with the oracle stream wired and once with no
oracle at all — and comparing every market-basis field, and every
membership funnel stage, across the two.

### The same discipline, on the new total

- **Absent, never zero.** `summary.reference_valuation.value_usd` is
  omitted when no member is reference-valued. A `"0.00"` there asserts
  that the backing behind every tokenized instrument in the set is worth
  nothing.
- **Counted separately.** `assets_valued` and `assets_unvalued` inside
  `reference_valuation` are that basis's own counts, independent of the
  market-basis pair beside them, because the two bases admit different
  rows.
- **`lower_bound`** is true whenever any member carries no reference
  valuation.
- **No fallback is ever invented.** `XAU` and `AUMTL` are unvalued on
  this basis and stay unvalued. An oracle does publish a feed called
  `XAU`, but it prices a troy ounce of spot metal: multiplying a token
  supply by an ounce price is not a valuation of anything, so no binding
  may target it and the row says so. Nothing prices an instrument called
  `AUMTL` at all.
- **Provenance travels with the figure.** Every reference-valued row
  carries the `reference` block that produced it — publisher, canonical
  feed id, denominator and vintage — and
  `summary.reference_valuation.sources` names the oracles behind the
  total. A dollar figure that cannot be traced to a publisher is worse
  than an absent one on this surface: it can be neither checked nor
  challenged.

### One refusal, two fields

Every rule that refuses a reference refuses both figures that depend on
it, so `reference_valuation.status` and `premium.status` carry the
**same string** on those rows. They are assigned together, from one
constant, at one line — which is what stops them becoming two accounts
of one event. They differ only where the reasons genuinely do:

| Situation | `premium.status` | `reference_valuation.status` |
| --- | --- | --- |
| A reference, and no market price | `no_market_price` | `published` |
| A reference, and a declared-peg price | `market_price_not_observed` | `published` |
| A reference, and no supply reading | `published` or a market refusal | `supply_unavailable` |
| No reference at all | the rule that refused it | the same string |

`supply_unavailable` is the valuation's own: a premium compares two
prices and needs no supply. Everything else is documented under
[the other four rules](#the-other-four-rules) and in the section below.

### Contract-issued members are refused by name

A contract member reaches this surface with everything the arithmetic
needs — a supply from the certified lake and its own declared decimals —
and is still **not** reference-valued. Nothing binds a contract
**address** to an oracle feed.

The available join is refused deliberately. A contract admitted on
`contract_oracle_rwa_feed` got in because its on-chain SEP-41 symbol is
an ADR-0028 code — but a symbol is metadata the contract itself authors,
so pricing a token by it is the code-keyed join
[this whole section refuses](#the-binding-is-on-the-pair-never-on-the-code),
with a *weaker* key than the classic one. Recognition of the address
establishes who deployed it; it does not establish that one of its
tokens is one unit of the instrument an oracle prices under that name.
The curated contract set records instrument and class rather than a
feed, and [ships empty](#c4--real-world-instrument) for want of primary
sources.

So the row carries `reference_contract_not_bound` — its own status,
not the `reference_not_bound` that names a `(code, issuer)` pair a
contract does not have. A silently absent figure would make a
deliberate refusal indistinguishable from an oversight.

### Comparing the two totals

The two totals are sums over **different rows**, so subtracting one from
the other measures nothing. `summary.both_bases` gives both figures
restricted to the members carrying both, which is the only subset on
which the comparison means anything; its difference is the aggregate gap
between what the market pays for that overlap and what the reference
says it is worth. Per asset, the same gap in percentage terms is
`premium.pct` — identical to the ratio between the two dollar figures,
since both are the same float multiplied by the two prices.

### Where the uncovered rows are accounted for

The funnel's `valuation` arm continues past the served set and counts
every row carrying no reference figure under the reason that refused it,
with the same `actor` vocabulary the membership arms use. Its drop
reasons are the strings the rows themselves carry, read back off the
rows rather than recomputed, so the two can never disagree. It admits
and refuses nothing — membership is decided before either valuation.

`operator` is used where somebody here can act: no oracle publishes the
instrument, the read did not answer or has gone stale, no supply reading
covers the asset, or the curated contract binding does not exist yet.
`definition` is used where the rule is working and nobody should act: a
flagged issuer, an unbound pair, a feed that prices an ounce, a value
denominated in a reserve asset, a non-positive value. None is the
`issuer`'s — an issuer cannot make an oracle price its instrument, and
saying so would send a reader to the one party who cannot fix it.

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

`circulating_supply` is served on every row, including rows with no
served price — it is a chain fact rather than a price claim, and the
reference basis needs it precisely where the market basis has nothing to
say. On the classic arm the market-cap fill reads a supply only when it
is about to multiply it by a price, so this surface fills the remainder
separately from the same two readers; on the contract arm the lake
supply is read for every row before any price is consulted. The
per-asset `decimals` beside it is the scale that turns it into a
whole-token float.

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
  `internal/api/v1/rwa_reference.go` (the oracle reference, the
  reference-priced valuation and the premium),
  `internal/api/v1/rwa_funnel.go` (the accounting, including the
  valuation arm).
- [ADR-0028](../adr/0028-rwa-asset-representation.md) — the `rwa:`
  reference-asset namespace and the oracle feed allow-list R4 reads.
- [dex-tvl.md](dex-tvl.md) — the same posture applied to a different
  aggregate: exclusions named, lower bounds labelled, and silence
  preferred to a plausible-looking wrong figure.
