---
title: RWA coverage — reconciliation against the public Stellar RWA dashboard
last_verified: 2026-09-10
status: current
---

# RWA coverage — reconciliation against the public dashboard

The public Stellar RWA dashboard at `dune.com/stellar/rwas` reports
**$4,032,317,094.79** across sixteen issuers (2026-09-08).
`GET /v1/rwa/assets` reports **$3,879,296.97** across six assets and four
issuers (r1, 2026-09-10).

This document is the per-issuer account of that difference: what we
report, what they report, and — where the two differ — which of four
distinct causes is responsible. It is the answer to "have we met the
bar", and the honest answer today is no. What follows is why, at a level
of detail an operator can act on.

## The two numbers do not measure the same thing

The single largest cause is not coverage at all, and no amount of
widening the definition touches it.

**We publish `supply × observed market price`.** Every figure on
`/v1/rwa/assets` comes from the `/v1/assets` read path unchanged: the
same substance gate, the same dust-liquidity guard, the same
scam-issuer suppression. A token with no USD price on this network has
`valuation.status: unpriced` and contributes **nothing** to the total.

**The dashboard publishes `supply × net asset value`.** A tokenized
treasury or money-market fund is held, not traded. Most of these tokens
have no Stellar market at all, so their dashboard valuation is the
instrument's NAV — commonly the issuer's own stated figure, or a
fixed $1.00 — multiplied by on-chain supply.

Both are defensible. They are not comparable, and a reconciliation that
ignored the difference would attribute a valuation-basis gap to a
coverage gap and send an operator chasing the wrong thing.

The consequence is concrete: **even a set that admitted all sixteen
issuers would still report a small fraction of $4.03B**, because almost
none of those tokens has a price this platform is willing to publish.
Closing the dollar gap requires a decision about valuation basis, not
about membership. See [What would actually close it](#what-would-actually-close-it).

## The four causes

Every row below is attributed to exactly one.

| Cause | Meaning | Who can act |
| --- | --- | --- |
| **NOT COLLECTED** | The entity is recognised in the curated directory, but this index holds no Stellar token for it. Nothing was refused; nothing was ever evaluated. | Operator |
| **NO ATTESTATION** | A classic asset exists, but the issuer's `stellar.toml` has never been fetched, so requirement R2 cannot be evaluated. | Operator |
| **REFUSED** | A candidate reached the definition and failed a stated requirement. | Nobody — the rule working |
| **VALUED DIFFERENTLY** | The asset is in our set, and the figure differs because of the basis above. | Maintainer decision |

## Per-issuer reconciliation

Figures in the "Dune" column are as published 2026-09-08. The "Ours"
column is what `/v1/rwa/assets` reports at r1 2026-09-10.

Cells marked *(not measured)* are ones this work could not verify: it
ran with no host access, and a value written without measuring it would
be exactly the fabricated figure the whole definition exists to prevent.

| Issuer | Dune | Ours | Cause | Detail |
| --- | ---: | ---: | --- | --- |
| Spiko | $1,605.0M | $0 | NOT COLLECTED | `GD2ELXTH…` is in the directory, tagged `issuer`, correct domain. Issues no classic asset. `sep1-refresh -issuer` returns `sql: no rows in result set` — absent from the `issuers` table entirely. Now reported in `unreached_entities`. |
| Realiz | $558.9M | $0 | NOT COLLECTED | Directory presence stated; address form *(not measured)*. |
| Tradable | $548.1M | $0 | NOT COLLECTED | Directory presence stated; address form *(not measured)*. |
| Ondo | $535.6M | *(unvalued)* | VALUED DIFFERENTLY | **In our set today.** Admitted on `oracle_rwa_feed` under `GAJMPX5N…`, and its ADR-0028 binding is recorded in `internal/rwa/oracle_reference.go`. The token has no Stellar market price, so `valuation.status: unpriced` and it contributes nothing to the total. This is the valuation-basis gap in its purest form: same asset, same supply, no publishable price. |
| Franklin Templeton | $527.2M | $0 | NOT COLLECTED | Two G-addresses in the directory, domain `franklintempleton.com`, tag `issuer`. Both issue no classic asset and both are absent from `issuers`. Now reported in `unreached_entities`. **19 distinct `BENJI` issuers exist in the lake and every one is an impersonator** — `franklintempleton.co.com`, `benji.qlumen.co`, `stellar.dtcc.network` — which is why the address, not the code, has to be the thing that is recognised. |
| Red Swan | $71.7M | $0 | NOT COLLECTED | Directory presence stated; address form *(not measured)*. |
| WisdomTree | $40.3M | **$0, 12 members** | **WAS NO ATTESTATION — corrected 2026-09-16** | The refusal was OURS. `stellar.wisdomtree.com` serves a real SEP-1 whose `ACCOUNTS` array ends with an unterminated string on line 20, and a whole-document parse discarded all eighteen of its well-formed `[[CURRENCIES]]` tables. Section recovery (v0.86.0) reads them and **12 assets are admitted** — 8 bond, 3 stock, 1 commodity — carrying **7,023,543 tokens** across ~30,000 trustlines each. `CRDT` stays out: its issuer is the one named on the broken line and is in no directory. All 12 are UNPRICED — only WTGX is in the independent listing directory, and the RWA classic arm does not read listing prices. |
| Centrifuge | $26.9M | $0 → *see detail* | NOT COLLECTED → **now a candidate** | The directory names `CBI7UCH5KG…` — a **contract**, not an account. Before this change no arm could see it. It is now a contract-arm candidate; whether it is admitted depends on its directory tags and its on-chain symbol, neither *(measured)*. If its tag is `issuer`/`anchor`/`custodian` and its symbol is ADR-0028 allow-listed, it is admitted; otherwise it is refused under C3 or C4 and **the refusal is reported**. |
| Rivool | $26.8M | $0 | NOT COLLECTED | Directory presence stated; address form *(not measured)*. |
| Cometum | $23.2M | $0 | NOT COLLECTED | In the directory; issues no classic asset (measured). |
| Finexity | $21.6M | $0 | NOT COLLECTED | Directory presence stated; address form *(not measured)*. |
| Etherfuse | $17.5M | *(part of $3.88M)* | VALUED DIFFERENTLY | **In our set today**, three assets: USTRY, CETES, TESOURO under `GCRYUGD5…`. Only CETES carries a served price; the other two are `unpriced` pending the substance gate. Our supply reading is trustline-derived, which omits claimable balances and LP-locked holdings — a structural undercount against a total-supply figure. |
| Mercado Bitcoin | $12.4M | $0 | NOT COLLECTED | In the directory; issues no classic asset (measured). |
| Black Manta | $9.3M | $0 | NOT COLLECTED | Directory presence stated; address form *(not measured)*. |
| Matrixdock | $4.6M | $0 | NOT COLLECTED | `XAUm` is ADR-0028 allow-listed, so a directory-named contract for it would be admitted on `contract_oracle_rwa_feed` today. Address form *(not measured)*. |
| Bitbond | $3.1M | $0 | NOT COLLECTED | Directory presence stated; address form *(not measured)*. |
| **Total** | **$4,032.3M** | **$3.88M** | | |

### Reading the table

Thirteen of sixteen are **NOT COLLECTED** — and that phrase is doing
precise work. They were not refused. The `issuers` table the classic arm
walks is written from exactly one call site, `registerIssuerSeen`, on
classic-asset registration. An entity whose Stellar presence is
contract-issued never gets a row there, therefore never gets a SEP-1
fetch, therefore never becomes a candidate. It was not excluded by
requirement R1 so much as never assembled into the population R1 runs
over.

That is a silent discard, and it is the same defect class the funnel
work closed for the classic path, one level further out. It is now
visible: `unreached_entities` names them, and the funnel's
`directory_recognised_issuing_accounts` stage counts them exactly.

## What this change does and does not close

**Closed — the definition no longer excludes contract-issued assets.**
`internal/rwa/contract.go` adds a second arm keyed on the contract
address, with the provenance argument and the attacker analysis recorded
beside it. Centrifuge, and any other entity whose contract the directory
names, is now a candidate that gets evaluated and — if refused — gets a
counted, attributed refusal.

**Closed — the population is drawn from a table that can see them.**
The contract arm walks `account_directory`, not `issuers`. That is the
only table we hold that ties a real-world entity to a Stellar address
without passing through classic issuance.

**Closed — contract assets can now be valued at all.** Both existing
reads of a Soroban asset gate on a 24h volume rollup (60 of ~117k
contracts on r1), so a held-not-traded fund was invisible to the
catalogue entirely. `ContractCatalogueRows` reads the admitted set
without that gate. Supply comes from the certified lake — the same
`stellar.supply_flows` sum `/v1/assets/{id}/supply` serves — and market
cap uses the token's real on-chain decimals rather than the hardcoded 7
the catalogue row carries.

**Closed — a contract asset is now subject to the scam gate.**
`fillIssuerDirectoryTags` keys on the issuer G-address and skips every
row without one, so contract assets have never been subject to the
directory scam suppression on any surface. On a page that admits a
contract on the strength of a directory entry, that gap was
indefensible. The contract arm re-reads the entry at valuation time.

**Not closed — the dollar figure.** For the reason in
[the section above](#the-two-numbers-do-not-measure-the-same-thing).

**Not closed — thirteen entities still have no address we can value.**
A recognised G-account tells us an entity exists. It does not tell us
which contract holds its assets, and we hold no deployer edge that would
(the `contractid` registry is factory-anchored per ADR-0035 and covers
protocol children, not token issuance). Until the contract address is
named — upstream in the curated directory, or in the in-repo curated set
— there is nothing to value.

## What would actually close it

In descending order of how much of the gap each closes.

### 1. A NAV valuation basis (maintainer decision — the whole dollar gap)

The machinery is already on this surface and already gated. Every row
carries a `reference` block: an independent oracle's valuation of the
real-world instrument, with its publisher, feed id, denominator and
vintage, joined through a curated `(code, issuer) → feed` table that
fails closed. Today it feeds only the `premium` column.

Multiplying that reference by circulating supply would produce a
NAV-based market cap — the same measure the dashboard publishes —
sourced from a named third party rather than from the issuer.

This work deliberately did not do it. It is a new kind of claim, not a
new input to an existing one, and it needs its own decision:

- It must be a **separate, labelled** figure, never folded into
  `market_cap_usd`. A page that summed market prices and NAVs into one
  headline would publish a number that means neither.
- It inherits the binding problem entirely. A NAV joined on a code
  hands every impersonator the real instrument's value — which is the
  2026-08 attacker-authored-pricing incident in a new coordinate.
- ADR-0028 covers 14 instrument codes. Of the sixteen issuers here it
  reaches Franklin Templeton (BENJI), Ondo (USDY), Matrixdock (XAUm)
  and Etherfuse (USTRY/CETES/TESOURO). The other ten would need feeds
  that do not exist yet.

### 2. Curated directory entries for the contract addresses (operator — most of the count gap)

Thirteen entities are recognised as entities and unreachable as tokens.
Each needs its contract address named, either:

- **upstream**, in `stellar-expert/public-directory`, which is the
  preferred route: it keeps the recognition independent of us, which is
  the entire evidential basis of the contract arm; or
- **in-repo**, in `contractInstruments` in `internal/rwa/contract.go`,
  which is the ADR-0040 curated-set mechanism — review-gated by living
  in code, fail-closed, published in full on the wire.

The in-repo set shipped **empty** with this work, deliberately: populating
it requires contract addresses from a primary source, and this work had
none. An address inferred from a dashboard screenshot is a fabricated
identity for a financial instrument. It has since grown to ten bindings
at that bar, nine Spiko fund share classes and Matrixdock's XAUm; the
list in `contract.go` is the authority, not this count.

The evidence bar for an entry is recorded beside the type: the address
from a primary source (a block explorer is corroboration, not a source —
explorers read the same curated directories we do), the instrument named
specifically enough to be falsifiable, and a class from the closed
`anchor_classes` vocabulary.

### 3. Drain the SEP-1 refresh queue (operator — WisdomTree, and the classic long tail)

29,741 issuers publish a `home_domain` whose `stellar.toml` has never
been fetched, against 14,635 fetched. It is why Etherfuse and Ondo are
the only two issuers with a payload, and therefore the only two the
classic arm can serve.

The drain was the binding constraint. `sep1-refresh.timer` ran once a
day at `-limit 500` against 76,658 domains — a 153-day cycle — and 58%
of every run went to domains that have never returned a document,
because a failure was re-queued on exactly the same schedule as a
success. Both halves are fixed: the timer runs hourly at `-limit 750`
(18,000 attempts/day) and a failing domain climbs a 1d/2d/4d/8d/16d/30d
retry ladder, so the ~35.8k domains that answer cycle in about two days
and the ~40.8k that do not cost roughly one attempt a month each.

This is tracked as its own operator task. It closes WisdomTree's four
classic-issuing addresses and an unknown share of the long tail; it does
**nothing** for the contract path, because a SEP-1 fetch for a bare
C-address has nothing to bind to.

### 4. SEP-1 `contract` field (engineering — the strongest available strengthener)

The current SEP-1 draft carries a `contract` field on `[[CURRENCIES]]`.
Reading it would let a contract be corroborated by the domain the
**curator** attributed it to — so an attacker could not choose which
domain has to vouch for them, and defeating the pair would require both
a directory merge and control of the real entity's domain. That is
strictly stronger than anything either arm has today.

It is not built here for a measured reason: our SEP-1 parser does not
read the field, and 14 of the 16 entities have no fetched payload at
all. Building it today would admit nothing and would bury lever 3 behind
machinery. It becomes worthwhile once the refresh queue is drained.

## The curated arm — reading the curator's own tables

The external figures this page reconciles against are not measurements
of Stellar; they are a curation. The "RWAs on Stellar" dashboard's own
query (dune.com/queries/6961846) values `stellar.token_balances` against
`dune.stellar.dataset_asset_prices` and takes membership, company and
subclass from `dune.stellar.dataset_recognized_assets`. Both are CSV
uploads by the Stellar team — `dune.<team>.dataset_<name>` is Dune's
upload namespace by its own documentation — and the private-credit line
above is those 24 contracts at an uploaded `close_usd` of exactly `1.00`.

Those two tables cannot be read from outside the curator's team.
Verified 2026-09-17 with a live key: a SQL execution over either is
refused with "Uploaded table (dune.stellar.dataset_recognized_assets)
does not exist or it is private", on any outside account. So the
per-asset comparison this arm was built for (migration 0161, an
address-keyed cache of the curator's list and prices) cannot be filled
by anyone, and stays empty.

What the curator does let anyone read is the latest result of its
dashboard's **public** queries — 6961845 "RWA Mcap by Month" and 6961847
"Mcap by Month by Asset Subclass". Since 2026-09-17 this index reads
exactly those (`stellarindex-ops curated-rwa-sync`, migration 0162) and
serves them under a **third arm**, apart from the two verified ones:

- `curated.published` — the curator's headline: its latest monthly RWA
  market-cap total (`total_usd`, for the month ending `as_of`, last
  computed at `executed_at`), that month's split by the curator's own
  subclass labels, the full monthly series, and `gap_vs_verified_usd` —
  the published total minus this index's verified reference total,
  signed. The curator's arithmetic over inputs this index cannot see:
  no per-asset breakdown reaches it, so no row in it can be checked.
  `executed_at` is the curator's clock, not this index's sync: past 48
  hours the block carries `stale: true`, and past 7 days it is not
  served at all.
- `curated.status: published_totals` says exactly that state: the
  totals answered and no per-asset row is readable.
- `curated_assets[]` and the three-figure comparison
  (`additional_value_usd`, `combined_value_usd`) remain the shape a
  readable per-asset list would be served in — `basis:
  third_party_curated`, the curator's labels verbatim, its price times
  the lake's supply — and are empty until a curator whose list is
  public exists.

**What it does not do.** A curated row never reaches `assets`, `summary`,
`by_class` or `by_issuer`. The test that pins this
(`TestRWACurated_ServesTheCuratorsRowsApart`) fails the moment one does,
and was proven red by merging them. The arm exists so that "why does
your number differ from that dashboard's" is answered by a row on the
page rather than by this document — not because this index vouches for
any figure in it. The gap is the worked example: the curator publishes
$4,004,795,860 for the month ending 2025-08-31; this index's verified
reference total sits beside it, and `gap_vs_verified_usd` is the
difference — a number on the page, signed, not a paragraph here.

## The bar, restated

The requirement was that our tracked RWA value equal or surpass the
dashboard's. It does not, and this document says exactly why and what
each remaining step costs.

What was **not** done to close it: admit self-declared contract tokens.
Nineteen distinct issuers publish a token called `BENJI` in this lake
and every one is an impersonator. A definition that admitted a contract
on its own say-so would have published those as real-world assets
holding hundreds of millions of dollars, and it would have reported a
number far closer to $4.03B while being worth less than nothing.
Under-reporting is strictly better, and the funnel now makes the
under-reporting legible instead of silent.
