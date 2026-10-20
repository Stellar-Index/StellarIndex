---
title: What counts as a tokenized real-world asset
last_verified: 2026-09-28
status: current
---

# What counts as a tokenized real-world asset

The page at `/rwa` and the endpoint `GET /v1/rwa/assets` publish a set of
Stellar assets that represent real-world value, each with a valuation. This
document defines that set and what the evidence behind each requirement
proves.

The definition is the whole product. A permissive rule makes a directory of
impersonators with dollar figures attached.

## The problem the definition exists to solve

Asset codes are not unique on Stellar. Any account can issue a token called
`USTRY`, `BENJI` or `XAU`. Identity is always the pair `(code, issuer)`.

SEP-1 lets an issuer describe its own assets in a `stellar.toml` at the
`home_domain` the issuer account sets on chain, with a `[[CURRENCIES]]` entry
per asset carrying, among other fields, an `anchor_asset_type`. On its own that
field is worthless. Measured on production 2026-09-05:

| Reading | Count |
| --- | --- |
| `[[CURRENCIES]]` entries declaring `anchor_asset_type` | 1,182,000+ |
| of which bound to the account that served the file | 23,900 |
| bound entries declaring a real-world instrument class | 4,166 |
| distinct issuers behind those | 130 |
| **of those 130 issuers, tagged `malicious` by the curated directory** | **128** |

The publishing domains are lookalikes such as `nasdaq.com.co`,
`spglobal.com.co` and `blackrock.co.com`. Each serves a valid, correctly-bound
SEP-1 file declaring hundreds of `stock` tokens. The binding proves the file
came from a domain the issuer controls, not that the domain is the exchange it
is named after. The same holds for instruments an RWA price oracle covers:
`BENJI` exists under two issuers (one directory-flagged), `USDY` under
`ondo.finance` and `blackrock.co.com`, `XAU` under `xau.cl` and a dozen
lookalikes.

## The definition

An asset is in the set when **all four** requirements hold. They are
implemented in `internal/rwa` and evaluated in this order; the endpoint
reports the first one a candidate failed.

> **A candidate has to be in `issuers` before any of this is evaluated.**
> The membership build's only input is `BoundSep1Currencies`, which reads
> `issuers WHERE sep1_payload IS NOT NULL`. An issuer whose assets are held
> but never traded on the SDEX was never a candidate and does not appear in
> the `refused` counts (measured 2026-09-10: 512,496 classic assets have a
> trustline against 199,793 registry rows; Franklin Templeton's BENJI was
> among the absent). The registry now has a holdings source
> (`stellarindex-ops asset-registry-backfill`, migration 0158). The chain runs `registry → issuer-enrich
> (home_domain) → sep1-refresh (payload) → candidacy`, and a new issuer enters
> the set only after those two jobs reach it. Check that chain before reading
> a low figure as a definition disagreement.

### R1 — Identity

A classic Stellar asset with a code **and** an issuer G-address. The native
asset is not an RWA.

A contract-issued token qualifies through the **separate arm** below, under
requirements C1–C4, on its contract address. The arms never overlap: this one
runs over SEP-1 attestations keyed by `(code, issuer)`, that one over curated
directory entries keyed by contract address.

### R2 — Issuer-bound self-declaration

The issuer account serves a SEP-1 `stellar.toml`, fetched over HTTPS from the
`home_domain` that account set **on chain**, containing a `[[CURRENCIES]]`
entry whose `code` matches the asset and whose declared `issuer` names the
account that served the file.

The comparison is on the **account**, not the toml's bytes: surrounding
whitespace and key case do not change which account it is, and the entry is
carried under the canonical spelling. A value not shaped like a G-account
strkey binds to nothing. A declaration with no `issuer` field is refused: many
accounts can choose the same domain, so an unbound entry would let any of them
inherit the claim. The SEP-1 logo overlay enforces the same rule.

R2 proves the claim came from a domain the issuer controls, bound to this
exact strkey. Nothing more. That is a low bar, which is what R3 is for.

### R3 — Independent recognition

The issuer G-address is named in the curated third-party account directory
(`account_directory`, migration 0136, synced from the MIT-licensed
stellar-expert public directory) with at least one **recognition tag** —
`issuer`, `anchor`, `custodian`, `exchange`, `defi`, `sdf` — and **no**
scam-class tag.

The scam-class vocabulary is `timescale.DirectoryScamFlagTags`, the single
list the price-withholding gate and the `/v1/assets` rank expression also
read. An issuer whose price is withheld can never be admitted here.

The recognition set is narrower than "has any tag": `personal`, `wallet`,
`memo-required`, `airdrop`, `application` and `infra` do not vouch for an
account as the issuer of a real-world instrument.

R3 requires that a party **other than the issuer** vouched for that specific
account, which is why the lookalike-exchange population is absent rather than
ranked low. It fails **closed**: if the directory cannot be read, no set is
published. Every other read on this surface degrades open.

#### The sibling route, and the assumption it rests on

An issuer account the directory has **not** listed is also recognised when the
directory lists another account, with a recognition tag and no scam flag, that
the **same** issuer-bound SEP-1 binds on the same home domain. The row reads
`recognition` = `curated_account_directory_via_domain_sibling`. A scam flag on the account itself still refuses
it, and the route supplies no instrument claim: R4 must still admit the asset
on its own declaration. It exists for Franklin Templeton's Luxembourg and
Singapore share classes (gBENJI, grBENJI, sgBENJI), ISIN-declared in
franklintempleton.com's SEP-1 beside the directory-listed BENJI issuer.

The route **assumes one entity per domain**. That holds for a fund manager
publishing its own share classes, not for a hosting domain whose file lists
several tenants: one recognised tenant would recognise all the others, and the
host rather than the directory would decide who passes R3. No such domain has a
recognised tenant today, so the assumption is stated rather than enforced.

### R4 — Real-world instrument

The asset is a real-world instrument rather than one of the issuer's other
tokens, by one of four bases. The basis is served on every row. The arms are
tried in the order below and the first to admit decides: a declared class
wins over an oracle feed, which wins over an ISIN.

**`sep1_anchor_declaration`** — the R2 entry declares an `anchor_asset_type`
in the closed vocabulary `stock`, `bond`, `commodity`, `realestate`.

That is SEP-1's own enumeration minus the terms that name no real-world
instrument. The field is free text on the wire (`equity`, `etf`, `metal`,
`rwa`, `real_estate`, `sovereign` and dozens more appear), and folding
synonyms in is how a closed set stops being closed. Three exclusions are
deliberate:

- **`fiat`** — a fiat-anchored token is a stablecoin, a different instrument
  and surface. Folding it in would silently multiply the headline figure.
- **`crypto`** and **`nft`** — neither is a real-world asset.
- **`other`** — it classifies nothing, and 6,309 bound entries carry it.

**`oracle_rwa_feed`** — the asset's code is an ADR-0028 allow-listed RWA code,
meaning an independent price oracle publishes a net-asset-value feed for an
instrument of that name. This admits real issuers that publish a bound entry
without filling in `anchor_asset_type`.

This arm matches on the **code**, the identity the rest of the definition
refuses, so it is admissible only **after** R3 has bound the issuer to a
recognised, unflagged account. A row admitted this way carries **no**
`anchor_class`: an oracle feed names an instrument, not its class.

**`sep1_isin_declaration`** — the R2 entry's `anchor_asset` is a well-formed
ISIN: twelve characters, a two-letter prefix, an alphanumeric body and a final
Luhn check digit. Form is all that is validated and all that is claimed.

An ISIN is assigned by a national numbering agency to a registered security
and is externally resolvable, so it is at least as strong as a self-chosen
class string. It is **not** evidence that *this* issuer is entitled to that
ISIN. Nothing checks that, and nothing needs to: an impersonator reaches this
arm only after defeating R2 and R3, and a test holds that the arm cannot be
lifted above them.

It admits **without** a class. It exists for Franklin Templeton's
share classes (gBENJI, grBENJI, sgBENJI), which declare
`anchor_asset_type: other` beside ISINs LU2900381208, LU3258450587 and
SGXZ71843866. Prose anchors such as `FOBXX`, `AU` or `US Treasury Notes` stay
refused. The arm admitted exactly those three assets.

**`sep1_isin_vetted`** — the same evidence, transcribed by hand. Some
issuer-bound entries name their ISIN only in `desc`, which the served tier
does not keep, and declare neither `anchor_asset_type` nor `anchor_asset`;
Bitbond's BB1 (ISIN DE000A2TR7Q2, a EUR bond) is the case. A maintainer reads
the issuer's own SEP-1 and binds the ISIN to exactly that `(code, issuer)` in
`rwa.VettedISINBindings`, with the page it was read from and the date. The row
serves all of it as `vetted_isin` with a review date one quarter out; past that
date it carries `review_due: true`, labelled and still served. The arm is
tried last, so an issuer that later declares `anchor_asset` wins on its own
declaration. It admits without a class, keys on the exact pair, and sits
behind R2 and R3 like every arm. It is membership only: a vetted ISIN never
prices anything.

A sibling-recognised issuer (see the
[sibling route](#the-sibling-route-and-the-assumption-it-rests-on)) still has
to pass one of these four arms on its own declaration. Every R4 arm is a
*self-declared* value, safe only because R2 and R3 stand in front of it.

#### The pre-filter contract

A pre-filter, `rwa.CouldQualify`, drops bound entries that could not satisfy
R4 under any issuer (tens of thousands declare an NFT, a crypto token or
nothing) before the full evaluation. The funnel counts what it dropped, and
`candidate_assets_evaluated` is what got through.

The pre-filter reads **every** asset-side input an R4 arm reads (the declared
class, the code and the declared anchor asset) and answers true when *any* arm
would, independent of the issuer. It is never a decision: R2 and R3 still have
to hold. A pre-filter narrower than the rule it precedes is a silent
membership change, so a test pins it to the arms over all three inputs.

## The contract arm

Everything above describes an asset with an issuer account. The entities that
hold real-world assets on Stellar mostly do not have one.

Measured 2026-09-10, of the sixteen entities the public Stellar RWA dashboard
attributes $4.03B to, the ones we could check (Franklin Templeton, Spiko,
Mercado Bitcoin, Cometum and fourteen of WisdomTree's eighteen addresses)
issue **nothing** in `classic_assets`. Their Stellar presence is
contract-issued.

The `issuers` table is written only by the classic-asset registry, keyed on a
classic asset's issuer, so an entity issuing only contract tokens never becomes
a candidate. Widening that table is not the fix: its key is a G-account, and a
SEP-1 fetch for a bare contract has nothing to bind to.

So the contract arm draws its population from `account_directory`, the only
table that ties a real-world entity to a Stellar address without classic
issuance. Its `CHECK` accepts both strkey forms. The population is the **union** of the contract addresses the curated directory
recognises and every address the in-repo curated binding set names, so no
address is dropped without a tally. Enumerating a binding is **not**
admitting one: each still has to satisfy
[C2](#c2--independent-naming-of-that-exact-address), and the ones that cannot
are reported under a reason.

### C1 — Identity

A contract address, CRC-checked. It is the whole identity, never a symbol or a
name. Two contracts declaring `BENJI` are two different assets.

### C2 — Independent naming of that exact address

An independent party names **that exact contract address**. Not the entity,
not a domain, not a G-account that might have deployed it.

This replaces R2, and it is **not** a relaxation. R2 costs one domain
registration: **19 distinct issuers publish a token called `BENJI` in this lake
and every one is an impersonator** (for example `franklintempleton.co.com` and
`stellar.dtcc.network`), each with a valid, correctly-bound `stellar.toml`. R2
admitted all of them; R3 kept them out, and R3 carries this arm.

**What an attacker would have to control.** A fake contract needs a curated
directory entry naming the attacker's C-address with an issuing-class tag and
no scam tag: a reviewed change in a third party's published set, under the
name of the entity being impersonated. The classic equivalent is a lookalike
domain with no review. Classic R3 vouches for an **account**, covering every
token it ever issues; contract recognition is per address. The contract arm is **strictly harder to defeat**.
It gives up only the issuer's own voice, which an impersonator supplies for
ten dollars.

#### Two ways to satisfy it, and why they are not the same shape

Measured 2026-09-15: the curated directory names **387** contract addresses. A
public listing platform's platform-to-address map names **17** on Stellar.
**Four** are in both. Thirteen are named by the listing and by nobody in the
directory, among them tokenized Treasury funds for which this repository holds
verified [curated bindings](#c4--real-world-instrument).

So C2 is satisfiable two ways:

1. **The curated account directory names the address**, with an issuing-class
   tag. This admits **on its own**.
2. **An independent listing directory names the address _and_ an in-repo
   curated binding names the same address.** This requires **two** sources
   that do not read each other, and admits on **neither** alone.

The curated directory is an
**adversarial identity system**: Stellar-specific, address-level, review-gated,
and carrying the scam vocabulary. Being named in it **attests**. A listing map
is a **convenience built for price aggregation**, with no flags and no review
of Stellar addresses. Being named in it asserts that a market exists. It
**corroborates**, and may not carry an attesting source's authority. It can
*agree*: two parties that do not read each other arriving at the same
56-character address for the same instrument is what C2 asks for. The in-repo
binding answers C4 (*which* instrument) and never C2 (*who else says so*),
because this index vouching for itself is not independence. It is the
**second** source in arm 2 and never the first.

Two consequences are published:

- An address the listing names that **no** curated binding names is
  **refused** as `contract_listed_without_curated_binding`. That is most of
  the map: stablecoins, wrapped bitcoin and the native asset's own Stellar
  Asset Contract. The refused set is counted on the funnel as
  `listing_contracts_without_curated_binding`, so "a listing admits nothing
  alone" is auditable.
- A curated binding **no** independent party names is refused as
  `contract_curated_binding_without_independent_listing`. Its address is
  verified and its instrument named; what is missing is somebody other than
  this repository saying so.

#### Why the listing map is not the curated directory re-published

Explorers read the same curated directories we do, so they are not
independent. The listing map is: it names 13 contract addresses
the directory does not, and the directory names 383 the listing does not (four
in common out of 400, a **1% overlap**). They disagree about what an address
*is* (`CBI7UCH5…` is tagged `defi` in the directory and carried as a priced
asset in the listing), and they have different keys and fields, so neither
could be produced from the other.

#### Failing closed

The listing is read from a cache filled by an hourly sync. If that read does
not answer, or its most recent answer is older than the 48-hour recognition
bound, **arm 2 stops admitting**: an old *recognition* admits an asset on a
fact nobody checked.

The set **shrinks**, and every affected binding is dropped under
`independent_listing_unavailable`, apart from the finding that nobody names it.
A deployment with no listing reader at all reports the arm as **not measured**
and makes no refusals. A binding whose curated tags could not be read has not
had C3 evaluated, and closes under `curated_tag_lookup_unavailable`, never
under the listing's name: the two reads are unrelated systems.

`funnel.listing_directory` publishes what the listing directory held and when
this index read it. `entries` counts fresh rows only, and `stale` counts rows
past the recognition bound: zero of both is a directory nobody has synced, zero
entries with stale rows is a sync that died. A sync that completed *after*
`observed_at` means the set in hand predates the rows; nothing is broken.

### C3 — Recognition, not flagged

The entry carries at least one tag from a vocabulary narrower than the account
one: `issuer`, `anchor`, `custodian`, and **not** `defi`, `exchange` or `sdf`.
On a contract address those three describe infrastructure that issues nothing:
the curated set carries the Aquarius AMM pool contracts under them.

The scam vocabulary is the same single list every other consumer reads, and a
scam tag beats every recognition tag on the same address, **on both C2 arms**.
It is applied before either arm is consulted, because the listing map carries
no flags and an address the curated directory named *only* to flag as malicious
could otherwise walk in through arm 2. A lookup that cannot answer closes the
arm rather than assuming the address is unflagged.

### C4 — Real-world instrument

A directory entry says who an address belongs to, not that the token is a
real-world asset: admitting every recognised contract would publish USDC as a
tokenized real-world asset. So C4 keeps R4's job, by one of two bases:

**`curated_contract_instrument`** — an in-repo curated entry binds this exact
contract address to a named instrument and its class. This is the
[ADR-0040](../adr/0040-completing-contract-gating.md) curated-set mechanism,
the one the oracle bindings use. Addresses must come from a primary source, never a dashboard.

The set holds twelve bindings from three issuers, in three classes, each bound
to its exact mainnet contract address. Nine are Spiko funds. Five are
tokenized T-Bill money-market funds (EUTBL, USTBL, UKTBL and the two EUR share
classes), classed `bond`. Four are the share classes of a tokenized overnight
swap fund (eurSAFO, SAFO, gbpSAFO, chfSAFO), classed `fund`. The tenth is
Matrixdock Gold (XAUm), one token per troy ounce of vaulted gold, classed
`commodity`. The last two are Centrifuge's deRWA tokens deJTRSY and deJAAA,
wrappers of the Janus Henderson Anemoy Treasury and AAA CLO funds, both
classed `bond`.

Every address comes from a primary source: Spiko's own deployment manifest at
`spiko.io` (each contract's `name()`, `symbol()` and `decimals()` byte-match it;
metadata may answer *which* instrument, never *whether*), Matrixdock's product
page at `matrixdock.com` (the on-chain WASM hash reproduces from its published
repository), and Centrifuge's SEP-1 file at `centrifuge.io`. The underlying
JTRSY and JAAA contracts that file also names are not bound, because binding
both risks counting one holding twice.

#### The class follows the instrument, not the issuer

The T-Bill funds hold short-dated sovereign debt, so `bond` is defensible.
The overnight swap fund is not a bond. It holds 152 listed equities at 119% of
net assets, one cash line, and seven total return swaps that hand all the
equity return to a bank counterparty in exchange for the overnight index rate;
it is a UCITS, not a money-market fund. `bond` would be false twice over, and
`stock` would be the opposite of what the token gives you. So the fund was held
**unbound**, worth over a billion dollars, until the vocabulary had a true
word.

That word is `fund`: a share in a pooled vehicle, whose exposure is the
vehicle's stated objective. It is served on
`definition.contract_anchor_classes`, the classic vocabulary plus this term;
the two lists differ on purpose. The classic arm READS an issuer's free-text
`anchor_asset_type`, so it can only accept what SEP-1 defines. A binding's
class is this index's own statement, made in code from a primary source and
reviewed as a change. An issuer cannot declare its way to `fund`; only a
reviewed binding can. Widening what a binding may SAY does not widen what is
ADMITTED: C1, C2 and C3 are untouched, and a class is never read until the
address has been named by two independent parties or by the curated directory.

XAUm is `commodity`: a bearer claim on a specific quantity of a specific
metal, redeemable for the metal. It is the first commodity row with an
independent price; the two classic commodity rows carry supply and no feed.

Spiko's cash-and-carry fund is deliberately **not** bound: a digital-asset
basis-trade fund holds crypto and futures, which the definition excludes.

A binding is **not** an admission. It is only the *second* of the two sources
[arm 2](#two-ways-to-satisfy-it-and-why-they-are-not-the-same-shape)
requires. The curated directory names none of these addresses, so each waits
on an independent listing naming it too. XAUm clears that bar; bindings that
do not are refused as `contract_curated_binding_without_independent_listing`.
Every binding, admitted or not, is published on
`definition.bound_contract_instruments`, so a refused one is visible as a
verified identity the surface still declines to value.

**`contract_oracle_rwa_feed`** — the token's on-chain SEP-41 `symbol` is an
ADR-0028 allow-listed RWA code. It is admissible for the reason its classic
twin is: the independent recognition of the address has **already** happened.
It answers *which* instrument an address someone vouched for holds, never
*whether* the address is vouched for. A row admitted this way carries no
`anchor_class`.

### What was considered and rejected

- **Contract-instance provenance** — admitting a contract because a
  recognised G-account deployed it. We hold no deployer edge: the `contractid`
  registry is factory-anchored per ADR-0035 and covers protocol children, not
  token issuance.
- **SEP-41 metadata as the admission** — the token talking about itself at no
  cost; it would admit every impersonator in the lake.
- **The issuer's SEP-1 naming the contract** — the current SEP-1 draft carries
  a `contract` field on `[[CURRENCIES]]`. It is the strongest strengthener, but
  our parser does not read the field and 14 of the 16 entities have no fetched
  payload. See the [coverage reconciliation](rwa-coverage-reconciliation.md).

### Valuing a contract asset

The `/v1/assets` listing spine and `GET /v1/assets/{id}` gate the
discovered-contract arm on a 24h volume rollup, which admits 60 of ~117k
contracts. The RWA surface reads its **membership-bounded** set without the gate, since a
tokenized fund is held, not traded.

Supply is the certified lake's `Σmint − Σburn − Σclawback`, as
`/v1/assets/{id}/supply` serves. An incomplete reading (a negative net) is
unavailable and never clamped to zero, which would read as a fully-burned
token.

Decimals come from the token's own on-chain metadata, not the hardcoded 7 a
catalogue row carries: a 6-decimal token valued at 7 publishes a tenth of its
real capitalisation. The scale is read from the contract instance's storage.
The map is `METADATA` (the soroban-token-sdk convention) or `Config` (where
some hand-written tokens keep it), and the field inside it is `decimal` in the
SDK and `decimals` in hand-written tokens. Every combination is read; of the
17 Soroban addresses the listing platform names, seven use the first map and
**ten** the second. A contract declaring both keys with different values, or
whose two maps disagree, is refused rather than resolved by preference:
choosing between contradictory self-declarations would invent an exponent for a
money figure.
[Contract-storage supply §6](contract-storage-supply.md#6-decimals-are-read-never-assumed)
records how the `Config` axis was found.

Everything else is the `/v1/assets` pipeline unchanged, including the
substance gate and the dust-liquidity guard. One gate is new: directory scam suppression keys on
the issuer G-address and never applied to a contract asset, so the contract arm
re-reads the directory entry at valuation time.

## What an asset that fails gets

Nothing on this surface. It is absent, not hidden behind a filter or ranked
last. Its own `/v1/assets/{id}` page keeps serving it, including any scam
warning.

The refusals are counted and published: `refused[]` on the response reports
how many candidates each requirement turned away, so the served set is never
mistaken for the whole population of assets that *claim* to be real-world
assets.

## Coverage: where the rest of the population went

`refused[]` reports only the requirements that were **evaluated**, and the
stages that remove most of the population run before any candidate reaches the
definition. Measured on production 2026-09-10, the endpoint served 6 assets and
a refusal tally of 3, over 59,303 issuer accounts, 44,376 with a `home_domain`
and 14,635 with a fetched SEP-1 payload. `funnel[]` is the complete accounting,
from every issuer account that could carry an attestation down to the rows
served, with a counted reason on every drop:

| Stage | Unit | What it counts |
| --- | --- | --- |
| `issuers_with_home_domain` | issuer accounts | Accounts a SEP-1 attestation could exist for. |
| `issuers_with_sep1_attestation` | issuer accounts | Those whose `stellar.toml` has been fetched and parsed. |
| `issuers_declaring_currencies` | issuer accounts | Those with at least one `[[CURRENCIES]]` entry. |
| `sep1_currency_entries` | declarations | Every `[[CURRENCIES]]` entry. |
| `issuer_bound_entries` | declarations | Those naming the account that served the file (R2). |
| `candidate_assets_evaluated` | assets | Bound declarations put to the full R1→R4 evaluation. |
| `assets_admitted` | assets | Those admitted. |
| `assets_served` | assets | The classic rows in `assets[]`. |

Every stage carries an `arm`. The table above is the `classic` arm. The
`contract` arm narrows a different population from a different root, so the
arithmetic reconciles **within** an arm and never across the boundary:

| Stage | Unit | What it counts |
| --- | --- | --- |
| `curated_directory_entries` | directory addresses | Every row in the curated directory. |
| `directory_contract_addresses` | contracts | Those whose address is a contract (an account address names an entity; a contract address names one token). |
| `directory_recognised_contracts` | contracts | Named with an issuing-class tag and no scam tag (C2, C3). |
| `contract_candidates_evaluated` | assets | Those put to the full C1→C4 evaluation. |
| `contract_assets_admitted` | assets | Those admitted. |
| `contract_assets_served` | assets | The contract rows in `assets[]`. |
| `directory_recognised_issuing_accounts` | directory addresses | **Terminal census, not part of the narrowing.** |

The last stage matters most. It counts recognised, unflagged issuing entities
this index holds **no token for**, and `unreached_entities[]` names a bounded
sample. No requirement refuses them; no token of theirs was ever collected.
Franklin Templeton and Spiko are both here (measured 2026-09-10). It takes no
part in the stage arithmetic, because it counts rows the first stage already
dropped.

When no curated-directory contract reader is wired, the contract arm is
**absent** from `stages[]` and `basis` says it was not measured, rather than a
run of zeros.

Each stage's drops account exactly for the difference to the next stage of the
same unit, and `funnel.balanced` states whether that held. When membership
cannot be established the funnel is **empty and unbalanced** rather than a
column of zeros. `funnel.imbalance` names which check failed; it is empty
exactly when `balanced` is true.

### Dating the set

`membership.built_at` is when the rebuild that produced the served set
finished. It is **not** the envelope's `as_of`, the response's own instant.
The set is cached with a ten-minute lifetime and a lapsed copy is served while
a rebuild runs; `membership.stale` says when. `membership.rebuild_failed_at`
appears only when the most recent attempt to replace the set **failed**, and it
is the field to act on.

Every drop carries an `actor` naming who can move it: `operator`, `issuer`, or
`definition` for a drop that is the rule working as intended. Similar counts can
be opposite findings:

- `entry_declares_another_issuer` is R2 refusing an impersonator. It is
  expected to be the largest bucket and needs no action.
- `sep1_attestation_never_fetched` is an issuer whose domain nothing has yet
  tried to fetch. It is a refresh cron that has not reached the account, and a
  coverage lever an operator holds.
- `domain_served_no_sep1_attestation` is an issuer whose domain **was**
  reached and served nothing usable. Nobody here can fetch a file that is not
  published. Of 40,838 domain-bearing issuers holding no payload, exactly
  **one** had never been attempted.

No per-attempt outcome is stored, so a 404, a dead name, a TLS failure and an
undecodable document are one count. The reason says what was observed, not
whose fault it was.

Coverage grows by fetching the attestations nobody has tried, or by extending
the curated directory so an issuer with a correctly-bound real-world
declaration can be recognised. It never means loosening R1–R4 until a bucket
empties.

## Valuation — the market basis

Every figure here comes from the existing `/v1/assets` read path: the same
catalogue query, thin-market substance gate, supply-derived market cap,
dust-liquidity guard and scam-issuer payload suppression. This surface adds no
market-price path, so it cannot publish a figure `/v1/assets` would have
withheld.

[The reference
basis](#the-second-valuation-what-the-backing-is-claimed-to-be-worth) values
the same float at an oracle's price and is never summed into anything below.
Membership is decided **before** valuation, from identity and attestation only:
a withheld price cannot shrink the set, and a large market cap cannot buy a
place in it.

Each row carries a `valuation.status`, and only `published` carries money:

| Status | Meaning |
| --- | --- |
| `published` | A price and a supply were both available and no gate withheld them. |
| `withheld_issuer_flagged` | The issuer acquired a scam-class directory tag after the membership set was built. The row stays; its valuation does not. |
| `unpriced` | No USD price: the market produced none, or the substance gate withheld it as too thin. |
| `withheld_low_liquidity` | A price exists but the dust-liquidity guard refused to turn it into a market cap. |
| `supply_unavailable` | A price exists but no circulating-supply reading does. |
| `decimals_unavailable` | Contract rows only. The token's declared scale could not be read. Refused rather than defaulted to 7. |

When a price is served but is not a direct market observation (an
operator-declared fiat peg, or a value derived through one substance-gated
intermediate hop), the row carries `valuation.price_basis` naming which,
exactly as `/v1/assets` does.

A withheld or unavailable valuation is **absent**, never zero and never a
stale figure. `circulating_supply` is served regardless: it is a raw chain
fact, not a price claim.

`summary.market_cap_usd` is the exact sum of the rows' own published market
caps. It is **absent**, not `"0.00"`, when no asset publishes one.
`summary.lower_bound` is true whenever any member is unvalued. It is also true
whenever a rebuild cap bound the set (the issuer cap, the contract scan cap, or
a member issuer whose classic assets fill one listing page), because a member
may then be missing entirely; `summary.truncated` is set and `summary.basis`
names the cap.

## The second valuation: what the backing is claimed to be worth

A real-world asset is bought and held, not traded: four of the six classic
members carry an independent oracle valuation of their instrument and **two**
have ever produced a market price this platform will publish. So each row
carries a second figure,
`reference_valuation`, and the set carries its own total in
`summary.reference_valuation`:

```
reference_valuation.value_usd = circulating_supply ÷ 10^decimals × reference.price_usd
```

`decimals` is served on the row (7 for a classic asset, the contract's declared
value otherwise). The arithmetic is exact rational with a single rounding to
2 dp at the end (ADR-0003), so the summary total is the exact sum of the
per-row strings.

### It is a claim, not an observation

The second figure is not called a market cap anywhere:

| | `valuation.market_cap_usd` | `reference_valuation.value_usd` |
| --- | --- | --- |
| What the price is | What buyers were observed paying | What an oracle says the instrument is worth |
| Evidence | A settled trade | A published feed, plus the issuer's declaration that one token is one unit of the instrument |
| Gates | Substance gate, dust-liquidity guard, scam-issuer suppression can withhold it | None apply; there is no market to measure |
| Adversarial standing | Somebody paid it | Nobody was observed paying it |
| An asset that never trades | Publishes nothing | Publishes the full figure |

**The two are never folded together.** A reference valuation never substitutes for a withheld
market cap, never enters `summary.market_cap_usd`, and never moves a row from
unvalued to valued on the market basis. A test serves the same set with and
without the oracle stream and compares every market-basis field and every
membership funnel stage.

### The same discipline, on the new total

- **Absent, never zero.** `summary.reference_valuation.value_usd` is omitted
  when no member is reference-valued.
- **Counted separately.** `assets_valued` and `assets_unvalued` inside
  `reference_valuation` are that basis's own counts, because the two bases
  admit different rows.
- **`lower_bound`** is true whenever any member carries no reference
  valuation, or a rebuild cap bound the set.
- **No fallback is ever invented.** `XAU` and `AUMTL` are unvalued on this
  basis. An oracle's `XAU` feed prices a troy ounce of spot metal, which values
  no token, so no binding may target it. Nothing prices `AUMTL` at all.
- **Provenance travels with the figure.** Every reference-valued row carries
  the `reference` block that produced it, and
  `summary.reference_valuation.sources` names the oracles behind the total.

### One refusal, two fields

Every rule that refuses a reference refuses both figures that depend on it, so
`reference_valuation.status` and `premium.status` carry the **same string** on
those rows, assigned together from one constant. They differ only where the
reasons genuinely do:

| Situation | `premium.status` | `reference_valuation.status` |
| --- | --- | --- |
| A reference, and no market price | `no_market_price` | `published` |
| A reference, and a declared-peg price | `market_price_not_observed` | `published` |
| A reference, and no supply reading | `published` or a market refusal | `supply_unavailable` |
| No reference at all | the rule that refused it | the same string |

`supply_unavailable` is the valuation's own: a premium compares two prices and
needs no supply. The rest are under [the other five rules](#the-other-five-rules)
and the section below.

### Contract-issued members are refused by name

A contract member has a supply from the certified lake and its own declared
decimals, and is still **not** reference-valued. Nothing binds a contract
**address** to an oracle feed.

The available join is refused deliberately. A contract admitted on
`contract_oracle_rwa_feed` got in on its self-authored SEP-41 symbol, and
pricing by it is the code-keyed join
[this whole section refuses](#the-binding-is-on-the-pair-never-on-the-code),
with a *weaker* key. The [curated contract set](#c4--real-world-instrument)
records instrument and class rather than a feed, and the funds bound there have
no oracle feed to bind to.

So a row nothing else priced carries `reference_contract_not_bound`, its own
status and not the `reference_not_bound` that names a `(code, issuer)` pair a
contract does not have.

#### The one price a contract row can carry

A contract the independent listing directory **names** is the exception, and
the join above is not relaxed. That directory publishes a USD price in the same
row in which it names the address: price bound to address, by a party that did
not read our curated directory. No code is matched, so a token wearing a bound
instrument's symbol gets nothing.

This is a **wider** set than the rows
[C2 arm 2](#two-ways-to-satisfy-it-and-why-they-are-not-the-same-shape)
admitted, deliberately. Whichever arm admitted
it, a row whose declared **scale** could not be read carries no valuation on
either basis (`decimals_unavailable`).

It is a **different claim** from an oracle NAV and is published as one. Each
served reference carries a `provenance`:

| `provenance` | what it values | what it rests on |
|---|---|---|
| `oracle_instrument_nav` | the **instrument** | an oracle's published valuation, plus the issuer's domain-bound declaration that one token is one unit of it |
| `fund_nav` | the **fund share** | the fund's own SEC-reported daily NAV per share, relayed by Tiingo, for a token bound on the exact `(code, issuer)`; taken only when no oracle binding exists, and ahead of any listing price or prospectus NAV |
| `listing_platform_price` | the **token** | a listing platform's aggregate of what the token trades at on the venues it tracks |
| `prospectus_constant_nav` | the **share class** | the issuer's published NAV for a constant-NAV share class, bound on the exact `(code, issuer)`; taken only when no oracle binding, fund NAV or usable listing price exists |
| `curator_uploaded_price` | the **token** | a curator's uploaded price per token. Curated rows are served apart, in `curated_assets`, and never enter `summary.reference_valuation` |

**No premium is published against a listing price.** A listing price
aggregates the same markets our own price samples, so the gap measures the
disagreement of two samples of one market. The row carries
`reference_is_a_listing_price`, a status that refuses the premium while the
reference valuation beside it is **published**. A prospectus constant NAV and a
curator's uploaded price are refused the same way, under
`reference_is_a_prospectus_nav` and `reference_is_a_curator_price`.

**A fund NAV is published to the cent.** The administrator rounds the NAV to
two decimal places, and the reference says so in `decimals_published: 2`. The
figure cannot resolve a difference smaller than half a cent, which on a
one-dollar share is half a percent, so no premium is published against it
either (`reference_is_a_fund_nav`). `as_of` is the business day the NAV was
struck, so a Friday NAV is served `stale: true` from the following Monday until
Monday's NAV lands. A fund NAV older than five calendar days is refused as
`reference_expired`. When a row carries an oracle NAV and its fund's own NAV is
also fresh, the oracle figure is served and `nav_disagreement: true` marks a
gap of more than half a cent; the values are never swapped.

`summary.reference_valuation.provenances` lists the distinct kinds in the
total, and the basis prose is **derived from the rows that contributed**.

### Comparing the two totals

The two totals are sums over **different rows**, so subtracting one from the
other measures nothing. `summary.both_bases` gives both figures restricted to
the members carrying both, the only meaningful comparison. Per asset the gap is
`premium.pct`.

### Where the uncovered rows are accounted for

The funnel's `valuation` arm continues past the served set and counts every row
carrying no reference figure under the reason that refused it, with the same
`actor` vocabulary as the membership arms. Its reasons are read back off the
rows, so the two cannot disagree. It admits and refuses nothing.

`operator` is used where somebody here can act: no oracle publishes the
instrument, the read did not answer or has gone stale, no supply reading covers
the asset, or the curated contract binding does not exist yet. `definition` is
used where the rule is working: a flagged issuer, an unbound pair, a feed that
prices an ounce, a value denominated in a reserve asset, a non-positive value.
None is the `issuer`'s, because an issuer cannot make an oracle price its
instrument.

## Premium and discount to the instrument's value

Each row carries `reference`, an oracle's valuation of the instrument with its
publisher, canonical feed id, denominator and vintage, and `premium`, the
market price measured against it:

```
premium.pct = (market − reference) ÷ reference × 100
```

Positive means the token trades **above** the instrument's independent
valuation, negative **below**. Arithmetic is exact rational, served as a
decimal string.

### The binding is on the pair, never on the code

A reference joined on the code alone would answer every token called `USTRY`,
`BENJI` or `XAU` with the real instrument's net asset value, so an unrelated
token trading at $0.20 would be published at an 81% discount to a security it
has nothing to do with.

So the join runs through an explicit, curated table of `(code, issuer) → feed`
bindings, served in full as `definition.bound_instruments`. It is matched
**exactly** on both halves (a case variant is a different token) and fails
closed: a pair absent from the table gets `reference_not_bound` and no figure.

This is the curated-set mechanism
[ADR-0040](../adr/0040-completing-contract-gating.md) sanctions: an allow-list,
review-gated by living in code, where an unlisted candidate is refused and
*reported*. Each entry records its evidence in
`internal/rwa/oracle_reference.go`, and needs all three of:

1. the issuer's domain-bound SEP-1 entry for that exact pair, naming the
   instrument (requirement R2);
2. the curated directory attributing that account to a named entity. Where
   ADR-0028 also attributes the feed to an entity the two must *agree*. It does
   so only for USDY, USST, XAUm, deJAAA and deJTRSY, and lists CETES, USTRY,
   TESOURO, GILTS, KTB and SPXU by ticker alone; for those requirement 3
   carries the binding;
3. something tying the feed to that issuer specifically rather than to an
   instrument of that name. Price agreement between the token's Stellar market
   price and the feed is the strongest form; a documented product-line
   correspondence is the weaker one and is marked as such.

Entries carrying only the weaker form are challenged first in review.

### The other five rules

A row failing any of these carries no figure and states which rule refused it
in `premium.status`.

| Rule | Refusal |
| --- | --- |
| The oracle value must be **dollars**. A bare `_FUNDAMENTAL` feed publishes net asset value in the token's *reserve* asset, a ratio and not a price. The denominator is read off the stored row and must be `fiat:USD`; it is never converted. | `reference_not_usd_denominated` |
| The feed must price **one token**. `rwa:XAU` is spot gold per troy ounce and `rwa:SPXU` is one share of an exchange-traded fund. No binding may target one (a test enforces it). | `reference_not_instrument_scoped` |
| The publisher must be an **oracle**. Aggregators write into the same table for divergence comparison, and a premium against one compares the market with itself. Their rows are dropped when the snapshot is built, so a bound pair left with no oracle row has no feed. | `no_reference_feed` |
| The market price must be **observed**. A price on `price_basis` is a declared peg or a transitive derivation, and a premium against it reports the issuer's own claim back as a market finding. | `market_price_not_observed` |
| The reference must be **positive**. A zero or negative value, from an oracle, a listing or a constant-NAV binding, is bad data. Both the premium and the reference valuation carry this status. | `reference_not_positive` |

A scam-flagged issuer gets no valuation of any kind, including a third
party's (`withheld_issuer_flagged`). Handing an impersonator the real
instrument's net asset value would publish a *larger* claim than the one the
flag suppressed.

### Absence, outage and expiry are three different statements

Each `premium.status` means one thing:

- `reference_not_bound` — the pair is not in the curated table. Usually a code
  collision.
- `no_reference_feed` — the pair *is* bound, but no oracle is publishing its
  feed.
- `reference_unavailable` — the oracle read did not answer. Nothing is known
  either way, and on the wire a failed read must not look like an absence.
- `reference_expired` — the bound feed's most recent observation is older than
  the seven-day window. Reachable only when a snapshot is carried across a
  sustained read failure.
- `reference_isin_mismatch` — the well-formed ISIN the issuer declares in
  `anchor_asset` contradicts a prospectus constant-NAV binding: the pair is
  bound to a different share class, or the ISIN is a class bound on another
  pair. No reference of any provenance is served until the binding is
  re-verified, because the row would otherwise name one security and be valued
  as another.

### What the comparison rests on

That one unit of the token is one unit of the named instrument. The evidence is
the issuer's own domain-bound SEP-1 declaration (R2) plus the independent
recognition of the issuing account (R3), and **not** a separate measurement of
denomination. The surface states this beside the figure.

An unavailable comparison is **absent**, never `0`, which would read as
"trades at par". A reference older than 72 hours, the longest ordinary gap
between two strikes of a real-world instrument's value, is labelled `stale`
rather than withheld.

The outer bound is absolute and enforced on the **observation**, not the
snapshot: an oracle reading older than seven days is not served, however it
reached the request. A live read cannot return one, but a snapshot carried
forward across a failed read would otherwise age past the bound.

## Coverage

`summary.earliest_first_seen_ledger` is the lowest ledger any member was first
observed at, read from an index complete since genesis.

`summary.assets_with_reference` and `summary.assets_compared` state how many
members carry an independent valuation, and how many of those also have a
market price to measure it against. The difference is the set of blank premium
cells.

`circulating_supply` is served on every row, including rows with no served
price, because the reference basis needs it where the market basis has nothing
to say. The per-asset `decimals` beside it turns it into a whole-token float.

The membership set is rebuilt at most once per ten minutes, off the request
path; on a rebuild failure the previous set is served. The oracle reference
snapshot is cached separately for 30 seconds only, because reusing a
ten-minute-old net asset value against a live price would report a premium
neither figure supports. A read that does not answer leaves the previous
snapshot in place. Until the first successful read, rows say
`reference_unavailable`.

## Known limits

- **The set is small.** It is a handful of assets from a handful of issuers,
  the answer to "which Stellar assets are real-world assets whose backing an
  independent party has vouched for, per `(code, issuer)`". A company list
  answers a different question with a bigger number and no per-asset identity.
- **R3 delegates recognition to one third-party directory.** A real issuer the
  directory has not yet labelled is refused, and the refusal is counted. On the
  CONTRACT arm a second source exists, under the asymmetric rule
  [C2 states](#two-ways-to-satisfy-it-and-why-they-are-not-the-same-shape).
  The classic arm has one source, reached directly or through the
  [sibling route](#the-sibling-route-and-the-assumption-it-rests-on), which
  adds no second party.
- **Arm 2 is only as wide as the curated binding set.** It admits nothing this
  repository has not already bound to a named instrument at the
  [three-part evidence bar](#c4--real-world-instrument), so growing it is a
  reviewed code change and not a sync. Addresses the listing names that no
  binding covers are counted on the funnel rather than admitted.
- **A listing price is not an oracle valuation,** and rows carrying one are
  labelled rather than blended. It prices the token, carries no premium, and
  asserts nothing about the backing.
- **No historical series.** Nothing on this surface is back-projected from
  current state.
- **The membership cache and the listing cache decay at different rates.**
  Membership rebuilds every ten minutes; the listing snapshot is filled hourly
  and may be reused for recognition for 48 hours and for pricing for 24. An
  address the listing platform corrects therefore stays admitted for up to two
  days.
- **A frozen membership rebuild has no upper age bound, the one gap in the
  fail-closed claim.** On a rebuild failure the previous set is served, and the
  reads that decide membership all go through the same Postgres, so a sustained
  outage there freezes the served set indefinitely rather than shrinking it.
  Arm 2's recognition is not carried forward *within a rebuild*; a rebuild that
  never happens is a different mechanism.
  [`membership.built_at`, `stale` and `rebuild_failed_at`](#dating-the-set)
  date the served set, but nothing yet refuses a set past an absolute age.

## References

- Implementation: `internal/rwa` (the definition and the comparable-instrument
  classification), `internal/storage/timescale/sep1_bound_currencies.go` (the
  provenance rule), `internal/api/v1/rwa.go` (the read path and wire shape),
  `internal/api/v1/rwa_reference.go` (the oracle reference, the
  reference-priced valuation and the premium), `internal/api/v1/rwa_funnel.go`
  (the accounting, including the valuation arm),
  `internal/api/v1/rwa_listing.go` (C2's second arm and its candidate
  population), `internal/storage/timescale/asset_listing_directory.go` (the
  cached listing directory and its two staleness bounds), and
  `internal/ops/ingest/listing_sync.go` (the hourly sync that fills it).
- [ADR-0028](../adr/0028-rwa-asset-representation.md) — the `rwa:`
  reference-asset namespace and the oracle feed allow-list R4 reads.
- [dex-tvl.md](dex-tvl.md) — the same posture applied to a different
  aggregate: exclusions named, lower bounds labelled, and silence preferred to
  a plausible-looking wrong figure.
