---
title: BLNT V3 — contract & event verification
last_verified: 2026-11-15
status: current
---

# BLNT V3 — contract & event verification

BLNT V3 is a Blend-derived lending protocol deployed independently on Stellar mainnet. It is
indexed as the separate protocol identifier `blnt`; none of its contracts are attributed to the
official `blend` protocol.

- **Enumeration method:** deployment records published by BLNT Protocol, cross-checked
  against the contracts' mainnet creation transactions.
- **Last verified:** 2026-11-15.
- **Coverage status:** directory, contract roster, raw-lake activity, and a dedicated typed
  BLNT V3 event decoder/projector. Typed lending-position analytics remain deferred, so no Blend
  V1/V2 lending totals are borrowed from incompatible tables.

## Core contracts

| Component | Contract | Creation ledger |
|---|---|---:|
| Emitter | `CDSO6FQSBJ3U3AJHBNNLHHKSMKTTDDN3GLGF3KZTMXBFV7SVB43ZJERT` | 65,134,154 |
| Pool factory | `CAKLFWAHB2RIYRBSPUL6EH43AIQ5ILBEM7HTMWYSJRL7PHZQ676KMCV5` | 65,134,160 |
| BLNT backfill | `CANIE64BDFKLHROLOIY2KTQVBWUZNUKZP75DLXC272QM7TIN5N3M4LVA` | 65,134,164 |
| Backstop | `CAHCUKBPTW5DR5RNXOHCTJD5U76A6EEZXYZAQFRQEEQRXQ3CRUZ6Z3MO` | 65,134,174 |

## Supplemental BLNT V3 module

| Component | Contract | Creation ledger |
|---|---|---:|
| Lockup backfill | `CBNRUFL5KYTT7TQMYKRMHQU5KY4LDQEUVXLI6ECKJU7OJGHC3OLGJFBV` | 65,369,820 |

The lockup backfill is a later, BLNT V3-specific supplemental distribution contract. It is included in
the `blnt` module roster, but is not part of the frozen V3 core WASM release.

## Factory-deployed pools

| Pool | Contract | Creation ledger |
|---|---|---:|
| Fixed Pool V3 | `CACSQWCMKG45TUHCQ3LXHOB7YEEJH4W376ZHQPXH2IZ3TE4NJHFZMCY7` | 65,134,362 |
| YieldBlent | `CAUTPSQDAJEUV5XEU3EXFVYRGDJLEJGVTFJCFZCSPTDCREAMZ573VF4D` | 65,327,498 |

Both pools use the BLNT V3 pool WASM hash
`0e4f8f45b94e6b0d017a496ac5620ad0e3f866533cf45acd2b070bedda1e7356`.

## Assets and protocol dependencies

These contracts are necessary to interpret or operate the deployment, but are not attributed as
BLNT V3 core contracts in the protocol directory:

| Dependency | Contract or asset | Index ownership |
|---|---|---|
| BLNT classic asset | `BLNT:GAP2ACGH3O6TRFAERGM4USJ6XWU3U2BFIK224JPV6NI3CUNEEEGTBTFB` | verified currency and classic-supply observer |
| BLNT Stellar Asset Contract | `CCBNWP5L7ZSXA3DBWCMFAVKFVRW3ZHWO7B5J557W42O64DDZNPJP7VDS` | SEP-41 and SAC-wrapper supply observers |
| BLNT/USDC Comet V1.1 pool | `CDQMIKIHKA5UY3FQJ2UDTLNX3GOPD3GPNZWSPTOBVOHEUMQJQXOPC7Y3` | `comet` source |
| BLNT/XLM Comet V1.1 pool | `CA5HX3D47PQZWC2E7VWXCKH4534P2NOJEOIXAWU47MI4QV2FQUYY3FLC` | `comet` source |
| Fixed Pool V3 oracle | `CCVTVW2CVA7JLH4ROQGP3CU4T3EXVCK66AZGSM4MUQPXAI4QHCZPOATS` | external oracle dependency |
| YieldBlent oracle | `CDN7JD6SP32RZHB3DHK4S4AWCCQF6GYIUO455UCR4RGRZILZDRD24N36` | external oracle dependency |

The two BLNT LPs run released Comet V1.1 WASM
`d735c3395f59510172cf5cf838823a1389b97cfec6bf24e580e9bd77d2b3e687`. Their trade and
liquidity events are decoded by the Comet integration so the same contract is never attributed to
both `blnt` and `comet`.

## Operational contracts

The following contracts administer or automate specific deployed pools. They are documented as
operational dependencies, not immutable BLNT V3 core contracts and not part of the `blnt` roster:

| Component | Contract |
|---|---|
| Fixed Pool V3 status admin | `CAEV4AKHVMUV3OKKVJBMDRF4JSQS4C5ZBIVWXPASQC3QVZJVIDEPMTEE` |
| YieldBlent backstop manager | `CAJUD2IQOVKPDZFJZEX2MSG7PKIE2NF4CJQ2ERBU7VF52SEM5GQL7XEB` |
| YieldBlent backstop-manager helper | `CANSTTPOGIB5S2YYUSTVORV7QO5L5KTLQAYZYUQEAIEYAPAUT6RFDP4M` |

## API behavior

`GET /v1/protocols/blnt` returns this verified roster and scopes its activity series and event
breakdown to these contracts plus the factory. Individual raw event history remains available at
`GET /v1/contracts/{contract_id}`. The dedicated `blnt` source decodes every event into typed columns in
`blnt_events`, separately from Blend's `blend_*` tables. Typed lending-position totals remain
deferred rather than presenting Blend-derived values as BLNT V3 data.

The source is production-enabled from ledger 65,134,154. Replay is gated by the
[six-lineage WASM audit](../operations/wasm-audits/blnt.md), and real Galexie
fixtures cover the pool, factory, backstop, emitter and both backfill roles.

### Emissions history

`GET /v1/lending/pools/{pool}/emissions/history` serves BLNT emissions per bucket in BLNT base units, with the
streams and rules described in [Blend's section](blend.md#emissions-history-get-v1lendingpoolspoolemissionshistory).
BLNT differs in three ways:

- The schedule is rebuilt from `reserve_emission_update` events and flagged `approximate`
  (`stored_eps_rewritten_without_event`): user actions rewrite the stored eps by dust with no event (under 1.4e-10
  relative, about 44 base units per reserve-week). It is not marked a lower bound, since the gap is immaterial.
- `backstop_alloc` has no per-tier split (`tier_split_not_on_chain`): `gulp_emissions` names the pool only.
- `backstop_claims` is served per tier: `blnt_amount` (BLNT), `lp_amount` (backstop LP tokens) and `shares`
  (tier shares), each labelled in `units`.

## Emission data inventory

What the repo establishes about BLNT emission data, read 2026-11-27 from the
typed ABI (`internal/sources/blnt/abi.go`) and the chain fixtures under
`test/fixtures/blnt/`. The Blend equivalent is in
[`blend.md`](blend.md#emission-data-inventory).

| Contract | Event | Body (abi.go) | Identifies | Does not identify |
|---|---|---|---|---|
| Pool | `reserve_emission_update` | `(res_token_id u32, eps u64, expiration u64)` (:94) | pool; reserve and side through `res_token_id`, as in Blend | start time other than the event's ledger |
| Pool | `gulp_emissions` | `emissions` (:95) | pool; its cycle allocation | the split across reserves |
| Pool | `claim` | topics `[sym, from]`; `(reserve_token_ids, amount_claimed)` (:96) | pool, claimer, the reserve token id set | the amount per reserve when the set has more than one id |
| Backstop | `distribute` | `new_tokens_emitted` (:126) | an amount | the pool or tier |
| Backstop | `gulp_emissions` | topics `[sym, pool]`; `(new_backstop_emissions, new_pool_emissions)` (:127) | pool | the split across tiers |
| Backstop | `claim` | topics `[sym, tier, user, pool]`; `(blnt_amount, lp_amount, shares)` (:130) | tier, claimer, pool, and both units | — |
| Emitter | `distribute` | `(backstop, amount)` (:135) | which backstop received BLNT | anything below it |
| Emitter | `drop` | `list` (:139) | one-off recipients | — |
| Backfill, lockup backfill | `claim`, `claim_grant`, `swap_blnd`, `fund`, `claim_fund`, `expire` (:142-151) | — | migration payouts | these are not lending emissions and are never counted as such |

Chain-verified by fixtures (pool WASM `0e4f8f45…`, backstop `0469cfb4…`):

- All three pool emission events are in the Blend V2 body shape; the decoded
  rows are pinned in `internal/sources/blnt/testdata/decoded.golden.json`.
- One gulp, ledger 65,135,638, tx `f13dec85…`: the backstop's
  `gulp_emissions` gives the Fixed Pool `new_pool_emissions` 21,960,000,000
  (golden :137-142). The pool's own `gulp_emissions` in the same tx carries the
  same 21,960,000,000 (:143-147). Its `reserve_emission_update` sets
  `res_token_id` 1 to eps 72,619,047,619, expiring 1,793,568,773 (:212-218).
  That is 604,800 s after the ledger closed, and
  72,619,047,619 × 604,800 / 10^7 = 4,391,999,999.997, i.e. 20.000% of the
  gulp. So the event eps is at the 10^7 scale the storage reader assumes
  (`internal/storage/clickhouse/lending_emissions_reader.go:14`).
- A backstop `claim` (ledger 65,148,565, tier `FirstLoss`) carries
  `blnt_amount` 1,033,567,161, `lp_amount` 84,386,953 and `shares` 84,386,953
  (golden :19-28). The claim reports both the BLNT claimed and the LP tokens
  it bought, so it is the control for Blend's single-amount backstop claim.

Known holes: multi-reserve pool claims cannot be split per reserve; the
backstop's per-pool allocation has no on-chain per-tier split; and the API
reads current `EmisData`/`BEmisData` only. Pools deployed by the factory after
the two listed above are covered by the factory gate but are not named here.

Settled by the lake census (2026-11-27,
[`blend.md`](blend.md#settled-by-the-lake-census-2026-11-27)): event eps
equals the `EmisData` written in the same transaction, but 4 of 9 current keys
were rewritten +14 to +719 above their last event (at most 1.4e-10 relative)
by user actions with no event: the writes sit at ledgers
65,694,368–65,694,563, which are `repay`, `withdraw_collateral` and `claim`
ledgers. Events therefore rebuild the BLNT schedule exactly only to dust (at
most about 44 base units per reserve-week); an exact schedule must read the
`EmisData` writes. In five traced backstop claims `lp_amount` equals the
Comet `mint`, and the claim emits no backstop `deposit`, so the account
earnings route counts the claimed LP tokens in `accrued`, not `net_flow`.
Pool claims naming one reserve id are 26.3% of claims and 10.8% of the
claimed amount; two or more ids, 73.7% and 89.2%.

Still open: why the pool WASM `0e4f8f45…` rewrites eps on user actions;
likely a recomputation that carries dust, not checked.

## Primary evidence

- BLNT V3 core source and deployment records:
  <https://github.com/BLNT-Protocol/blnt-contracts-v3>
- BLNT V3 immutable release:
  <https://github.com/BLNT-Protocol/blnt-contracts-v3/releases/tag/v3.0.2>
- BLNT backfill source:
  <https://github.com/BLNT-Protocol/blnt-backfill-contract>
- Lockup-backfill source:
  <https://github.com/BLNT-Protocol/blnt-lockup-backfill-contract>
- Deployment orchestration and operational-contract records:
  <https://github.com/BLNT-Protocol/blnt-v3-migration>
- YieldBlent backstop-manager source:
  <https://github.com/BLNT-Protocol/backstop-manager>
- YieldBlent backstop-manager helper source:
  <https://github.com/BLNT-Protocol/blnt-backstop-manager-helper>
- Fixed Pool V3 status-admin source:
  <https://github.com/zenith-protocols/pool-status-admin>
- Comet V1.1 source release:
  <https://github.com/CometDEX/comet-contracts-v1/releases/tag/v1.1.0>
- Mainnet interface:
  <https://blnt.trade/>
