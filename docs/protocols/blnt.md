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
- **Coverage status:** directory, contract roster, and raw-lake activity. A dedicated BLNT V3 event
  decoder and lending-state projection are not yet present, so decoded `event_kinds`, lending
  position analytics, and completeness verdicts are intentionally absent rather than borrowed
  from Blend's incompatible tables.

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
`GET /v1/contracts/{contract_id}`. Until a dedicated decoder is added, BLNT V3 events are not written
to Blend's `blend_*` tables and the API does not present Blend-derived lending totals as BLNT V3 data.

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
