# DeFindex — contract & event verification

> **For the DeFindex team:** this is the set of DeFindex factories, vaults,
> and strategy contracts Stellar Index ingests. Please confirm the four
> factories and help us with the **open question below** about how vaults
> and strategies relate (so we attribute strategy events correctly).
>
> - **Enumeration method:** ADR-0040 §3 multi-proof classification — every
>   lake emitter cross-checked against (A) creation-transaction
>   correlation, (B) factory create-event bodies, (C) your published WASM
>   hashes (`mainnet.contracts.json`), (D) your Dune vault registry. See
>   "Verification 2026-07-05".
> - **Last verified:** 2026-07-05 (r1 lake, tip ledger 63,343,398).
> - **Gate status:** ✅ GATED (ADR-0035/0040, shipped 2026-07-05): curated
>   evidence-verified set — 85 vaults + 16 strategies in-code
>   (`defindex.MainnetGatedSet`), 4 factory trust roots; **9 no-proof
>   emitters excluded + flagged below**. New vaults do NOT self-register
>   (the create event omits the child address — the open question below
>   still stands): they fail-close into recognition gaps until verified and
>   seeded via `protocol_contracts` (no redeploy needed).

## Factories (4)

`DeFindexFactory` `create` events announce new vaults. There is more than
one factory (like other protocols, DeFindex appears to have been
redeployed):

| Factory | events | first → last ledger |
|---|---:|---|
| `CDKFHFJIET3A73A2YN4KV7NSV32S6YGQMUFH3DNJXLBWL4SKEGVRNFKI` | 108 | 57,057,068 → 62,972,282 |
| `CDHPT7OBQKIUFHIJMLI4W7TNOQUHEVOOVMCW7HA4O5SPFNLDRCE6DQ5F` | 10 | 60,947,911 → 60,966,531 |
| `CAVP2QLPIG7FQNHI57KXF7KS6NIAAUQKHZZDM3AGVADE64WHFBC5YURX` | 3 | 55,484,403 → 55,511,450 |
| `CDOIC7245ONYVOTEDLGKUM263EQ7SEEQ74ZQCN4SSH4TSYXOCMU6254O` | 2 | 56,891,213 → 56,927,232 |

## Vaults & strategies (lake counts)

- **34 vault contracts** emit `DeFindexVault` events (deposit / withdraw /
  rebalance / fee / manager changes), 59.37M → 62.99M.
- **7 strategy contracts** emit `BlendStrategy` events (deposit / withdraw
  / harvest), 62.85M → 62.99M (recent).

The full vault + strategy address lists are derivable from the lake; we'll
attach them once the open question is settled. A hand-seeded vault list
already exists (`migrations/0033_seed_defindex_vaults`).

## ⚠️ Open question (please advise)

We verified the factory `create` events against the lake and found a
gating obstacle: **the `create` event does not carry the new vault's own
address.** The 4 factories emit 107 `create` events whose bodies hold the
vault's *configuration* (assets, strategy addresses, manager/role
addresses) — but **0 of the 34 vault-emitting contracts appear anywhere in
those bodies.** So unlike Blend (`deploy` → pool address) or Soroswap
(`new_pair` → pair address), we can't enumerate DeFindex vaults from the
creation event; the vault's address is the deterministically-deployed
contract, recorded in the transaction's `create_contract` op, not the
event.

To gate correctly we need one of:

1. A **factory view function** that lists deployed vault addresses (a
   `query_vaults()` / registry), OR
2. Confirmation that the vault address is recoverable from the `create`
   event another way (e.g. a salt/deployer derivation), OR
3. The **authoritative vault + strategy address list** directly.

And separately: are the **7 `BlendStrategy`** contracts **created by their
vaults** (fan-out), or **shared / independently deployed** (need their own
allowlist)?

> **Note:** DeFindex topics are namespaced (`DeFindexVault`,
> `BlendStrategy`), so collision risk is low and the urgency is lower than
> for Blend/Soroswap (whose generic `supply`/`swap` topics collide widely).

## Events decoded

| Layer (topic[0]) | topic[1] examples | Where it lands |
|---|---|---|
| `DeFindexFactory` | `create`, `n_fee` | registers the vault |
| `DeFindexVault` | `deposit`, `withdraw`, `rebalance`, `n_wasm`, … | `defindex_flows` (vault layer) |
| `BlendStrategy` | `deposit`, `withdraw`, `harvest` | `defindex_flows` (strategy layer) |

`n_wasm` (vault-layer topic[1], likely a WASM-upgrade announcement —
"new wasm") was added to `classifyVault` 2026-07-10 (ROADMAP #89
residual): a read-only lake census found 2 real occurrences with no
decoder, the same classification-only treatment as `rescue` /
`paused` / `nreceiver` / … above — recognised so the drop-counter
doesn't file it as "unmatched topic", no flow modelled yet (count too
small to justify the ClickHouse scan cost; see
`internal/sources/defindex/events.go`'s `EventNWasm` doc).


## Vault enumeration (53 — from the team's own Dune registry, 2026-06-12)

Cross-checked via the paltalabs Dune dashboard pipeline (query 5926821,
"DeFindex Latest Vaults Data") — the team's own registry. **All 34 of our
lake `DeFindexVault`-event emitters appear in it; the registry has 19
MORE vaults we have never seen emit a `DeFindexVault` event** (they carry
TVL, and lake-probing shows only SEP-41 `burn` events from them) — i.e. a
vault WASM version exists whose business events are not the namespaced
`DeFindexVault` kind we decode. ~36%% of vaults are currently invisible
to `defindex_flows`.

| Vault | Name | USD TVL | Our lake coverage |
|---|---|---:|---|
| `CCA2ZJP5BVRXYTQH4FAGHCAUMRYCXVC4CRYC2NXHWMR7TIVX36U7F5HR` | Meru - CCA..5HR | $2,092,894 | emits DeFindexVault events |
| `CBNKCU3HGFKHFOF7JTGXQCNKE3G3DXS5RDBQUKQMIIECYKXPIOUGB2S3` | BeansUsdcVault - CBN..2S3 | $717,007 | emits DeFindexVault events |
| `CAIZ3NMNPEN5SQISJV7PD2YY6NI6DIPFA4PCRUBOGDE4I7A3DXDLK5OI` | BeansEurcVault - CAI..5OI | $216,611 | emits DeFindexVault events |
| `CA2FIPJ7U6BG3N7EOZFI74XPJZOEOD4TYWXFVCIO5VDCHTVAGS6F4UKK` | USDC Soroswap Earn - CA2..UKK | $53,987 | emits DeFindexVault events |
| `CCUZC3HC5TH2VCYZFUG57E6IGKPL45YUN2SI3UEYQUBA7RCYHUIZBSFV` | Neko USDC - CCU..SFV | $37,424 | emits DeFindexVault events |
| `CAWM7NKSYG2ITJW2MYYJWJ5ULGCJLDB6MXZIWPL3VPRG5TDVLJ66IMWR` | NormalUSDC - CAW..MWR | $18,378 | emits DeFindexVault events |
| `CD4B5WJDJQ6G5K6MVC3VHTBI2PNLWJBWLXHV75S245Q3PIQWC262UZ4C` | Greep Vault - CD4..Z4C | $14,610 | emits DeFindexVault events |
| `CBUJZL5QAD5TOPD7JMCBQ3RHR6RZWY34A4QF7UHILTDH2JF2Z3VJGY2Y` | Hana USDC - CBU..Y2Y | $6,745 | emits DeFindexVault events |
| `CD4JGS6BB5NZVSNKRNI43GUC6E3OBYLCLBQZJVTZLDVHQ5KDAOHVOIQF` | xPortal - CD4..IQF | $4,760 | emits DeFindexVault events |
| `CCDRFMZ7CH364ATQ5YSVTEJ3G3KPNFVM6TTC6N4T5REHWJS6LGVFP7MY` | Rozo - CCD..7MY | $4,715 | **NO DeFindexVault events in lake** |
| `CCKTLDG6I2MMJCKFWXXBXMA42LJ3XN2IOW6M7TK6EWNPJTS736ETFF2N` | EURC Soroswap Earn - CCK..F2N | $3,680 | **NO DeFindexVault events in lake** |
| `CD455S6D4A2G36TXWSYUQNDX4YJBFJJSFRSXBSU7H6TVM6FC67ZMIFGQ` | EbioroUsdcVault - CD4..FGQ | $3,528 | emits DeFindexVault events |
| `CC767WIU5QGJMXYHDDYJAJEF2YWPHOXOZDWD3UUAZVS4KQPRXCKPT2YZ` | SeevVault - CC7..2YZ | $2,156 | emits DeFindexVault events |
| `CAB4JOLSCNELJVDQKZLVGHKWJCLXFDBZZMITJAFL4GBGTHIKWO47PYFH` | Peridot USDC Vault - CAB..YFH | $1,101 | emits DeFindexVault events |
| `CDI7QVDTNDFEHB25VFQGMNFALGCXXKAWUSHOTQR2D4O44CATQJ5ZQMN6` | USTRY Soroswap Earn - CDI..MN6 | $1,025 | emits DeFindexVault events |
| `CC24OISYJHWXZIFZBRJHFLVO5CNN3PQSKZE5BBBZLSSI5Z23TKC6GQY2` | CETES Soroswap Earn - CC2..QY2 | $496 | emits DeFindexVault events |
| `CBP2R5KYAWJCOCVDTSNTEVL3O6JBTWOOH7SZOX7DX5DLGVZCAMLBDZM3` | Peridot EURC Vault - CBP..ZM3 | $386 | emits DeFindexVault events |
| `CDIHXKZ4PFKAIONK52JAR6ZNMP62F3UP7XTIBSJTQLMLHQ44PQ5Q2H3J` | OduroVault - CDI..H3J | $201 | **NO DeFindexVault events in lake** |
| `CBMERS7MJHO6TGKUVWWU34ZSKWCFOWPG2ZCIRIT75IC3YDWBIPBMV5LB` | Neko TESOURO - CBM..5LB | $150 | emits DeFindexVault events |
| `CANBU7T77SCJOOAU6VQAOGR7DN36JBQFUN56XS2WA2VPJYUSRUBIPYDS` | Neko Cetes - CAN..YDS | $141 | emits DeFindexVault events |
| `CCPKQH3K5XUGP5GXCT6WTABS7TGXRR745BJ4MEFSGNATB7AOBRL4VEOT` | Neko USDC - CCP..EOT | $108 | **NO DeFindexVault events in lake** |
| `CB3FUMFGCF6DHSFK6N2TOKHRMYXS34HFKQR45UKVORCRUM35AF3ES7WQ` | Neko EURC - CB3..7WQ | $108 | emits DeFindexVault events |
| `CCB2AR5X3KP4WQKE7HNSUSDS7SHFMC2WPVSZ2ZXJ6DHXOKHFFKOZE6GK` | Peridot XLM Vault - CCB..6GK | $63 | emits DeFindexVault events |
| `CDTCSXSKRIFYLDMMF3UABU63LEXSAR2CRCJVSL2PUJGVLNCQWU7XGWCN` | CodeLn USDC - CDT..WCN | $42 | emits DeFindexVault events |
| `CCIRVAW3IZVAYLHR7YYMZFOQVYEW67OKFFXR3J6ZR2T6YJC5V7GTSNQ5` | Neko USTRY - CCI..NQ5 | $40 | emits DeFindexVault events |
| `CAHGILQRWEGTAWIYGLVFKFPRPNH4NN6KZIDBGHMWABIWZZLW2ZHLHQOG` | CashAbroadUSDC - CAH..QOG | $35 | emits DeFindexVault events |
| `CDRSZ4OGRVUU5ONTI6C6UNF5QFJ3OGGQCNTC5UXXTZQFVRTILJFSVG5D` | CASHBRD USDC PROD - CDR..G5D | $32 | **NO DeFindexVault events in lake** |
| `CBGE43WF5GBDCHMN2XPKIAC7TYMWCR6FOJTVFMBR6QQM6WKZB7BM23LL` | xPortal - CBG..3LL | $31 | **NO DeFindexVault events in lake** |
| `CBDZ2L4HHEPPL4ABHPORQC72E5S2GLNRPJ467XV3CW5FDWICUNH6SF4B` | test - CBD..F4B | $30 | **NO DeFindexVault events in lake** |
| `CD7T34Y5SZ6MBEZDMXDIQWQ6JICO7TYH7E6DKZJ7BHXOMR2EQ65WYSZG` | boostAPY - CD7..SZG | $21 | emits DeFindexVault events |
| `CB5YXWIDBQAOTTPEQE3SRNUFM2PTOXFHKGUWCBJJSF2GPW37DN725FDA` | controlAPY - CB5..FDA | $21 | emits DeFindexVault events |
| `CAEPJIHET2TBI2VCLJZI6QHMN366KUGNK4AOKE3YY7AOKMU4KX4RDRGB` | targetAPY - CAE..RGB | $20 | emits DeFindexVault events |
| `CD3HR7WNGPDUGK5ITNMZSRM36O2IFJF3N4RFHOITP4DCXMVGHMANN3XR` | variableAPY - CD3..3XR | $20 | emits DeFindexVault events |
| `CBBAH2OAJ6N3UBJGXNFYH4QF6C6OWO4RHGGOOGDER3IJB7SGLR3Y56JO` | Decaf Vault USDC - CBB..6JO | $18 | emits DeFindexVault events |
| `CBDZYJVQJQT7QJ7ZTMGNGZ7RR3DF32LERLZ26A2HLW5FNJ4OOZCLI3OG` | BeansStgUsdc - CBD..3OG | $16 | **NO DeFindexVault events in lake** |
| `CAVL4BSHMU5ECWZCB6ETYSBV4EWTRMHAGMVUEJ5PXM3P3E3AOJPX2TLU` | BeansStgEurc - CAV..TLU | $13 | **NO DeFindexVault events in lake** |
| `CAGERKFCDHHCES64L43EU242KIVQMPYAL37CFYIGMLBGJIQTYWXFRWIT` | Decaf USDC - CAG..WIT | $12 | **NO DeFindexVault events in lake** |
| `CDSM6RP3GP6MSV7PXN7OSXCJ5EGMSLGLYFJ4QEPPMQWABD5JU5UPAOZM` | XLM xPortal - CDS..OZM | $11 | emits DeFindexVault events |
| `CCM3CKJI7BBMZ357644KLAE6NH4D7JQ6MUJHSV4UBRWJY7IMGHBJRNGR` | Neko USDC - CCM..NGR | $8 | emits DeFindexVault events |
| `CAQ6PAG4X6L7LJVGOKSQ6RU2LADWK4EQXRJGMUWL7SECS7LXUEQLM5U7` | Demo vault - CAQ..5U7 | $8 | **NO DeFindexVault events in lake** |
| `CBYTDU4JKTMFG5CNIUYJTOVMNIN5ADU2PDG4QWIUVT6SSVK3VTXYTF4K` | Neko - CBY..F4K | $7 | emits DeFindexVault events |
| `CD65RR656FIX5LRC7M5RP46IE2MQFK5OEWRM6L6KLIJVUU222U3PFUAP` | Cofrinho PigFy - CD6..UAP | $6 | emits DeFindexVault events |
| `CDI2ZW5CKT4OIHX3IGMVJ4VGOH6Z64N2M3URKATYJIX7JRITJFQJPFD7` | Neko Cetes - CDI..FD7 | $5 | emits DeFindexVault events |
| `CAXRLUOSI7DL3SYNZW5UGRIPVNRKKSZTW35OX5DWKZSJ4PFEVA2VEFCQ` | Neko TESOURO - CAX..FCQ | $5 | **NO DeFindexVault events in lake** |
| `CD6GVZTGH7L6NELM2YFCMBF7QAI6DFR25GPOQFKKAZQHVRLB5ZP46CYM` | TurboTestVaultNN - CD6..CYM | $4 | emits DeFindexVault events |
| `CDKNDBBVLTSO2DSLTZOIF2A4NJWPXTGHD3WYSWBHYBJDKAX4JCKEFMHT` | TurboTestVaultNN - CDK..MHT | $3 | **NO DeFindexVault events in lake** |
| `CAARFNLSJSACT7OWWJP6H5KFFD7T4BWL67OT5K3RRULHGW3C5DZP6Y6D` | TurboTestVaultNN - CAA..Y6D | $2 | emits DeFindexVault events |
| `CAIFV6BSPN2UHGDSOJK7RLOEVBLQX6EAGIVJWVWSEI7ROLUGI3U2XDTP` | MultiAssetTestVault - CAI..DTP | $2 | **NO DeFindexVault events in lake** |
| `CDPJEMZOYZLITC4MRLGJQHPMNCIB3TZ4R42J6M37PWP5Q2FGO4WFIXAD` | MultiAssetTestVault - CDP..XAD | $2 | **NO DeFindexVault events in lake** |
| `CCFWKCD52JNSQLN5OS4F7EG6BPDT4IRJV6KODIEIZLWPM35IKHOKT6S2` | Palta Vault - CCF..6S2 | $1 | **NO DeFindexVault events in lake** |
| `CDONBLOOTYZ7QN62ZLJFHK7CT3JCP3JEZDCRSG3VLGAP73QAXS7HF6HU` | XLM Boring - CDO..6HU | $1 | **NO DeFindexVault events in lake** |
| `CA25XTGHKQ6PUMFJ4SDNRFMUABIFX46U7VAZBFDZKAOX5C3KZXUAR2KQ` | TurboTestVaultD2 - CA2..2KQ | $0 | **NO DeFindexVault events in lake** |
| `CBHB2G4TMSVWE4YFDTFYRYNCP5KUT6RQVWQGIM4LQO2IKKHVDB7N5JJQ` | CETES Soroswap Earn - CBH..JJQ | $0 | **NO DeFindexVault events in lake** |

## ⚠️ Updated questions for the DeFindex team

1. The 19 no-event vaults: **which vault WASM versions emit the
   `DeFindexVault` events, and what do the others emit** (if anything)
   for deposit/withdraw/rebalance? We need per-version event schemas to
   decode them (contract-schema-evolution).
2. (Still open) Are the `BlendStrategy` contracts vault-created or
   shared/independent?
3. Is the Dune registry (53 vaults) the complete authoritative set, and
   how is it maintained — factory `create` call parsing?

## ✅ Verification 2026-07-05 — gate evidence (lake tip 63,343,398)

The 2026-07-02 blocker ("emitter growth cannot be deploy-graph-verified")
was resolved by running the ADR-0040 §3 procedure: classify EVERY lake
emitter against independent provenance proofs instead of trusting the raw
emitter list.

### Census

- **88 contracts** emit `DeFindexVault` events, **22** emit
  `BlendStrategy` events, **4** emit `DeFindexFactory` events —
  121,911 events total; a pre-55.4M sweep confirms no earlier emitter of
  any of the three topic shapes exists (soroban-era genesis onward).
- The 4 factories emitted **109 `create` events** (104 from the current
  `CDKFHFJI…`, 3 from `CAVP2QLP…`, 2 from `CDOIC724…`; `CDHPT7OB…` emits
  only `n_fee`). As before, **no create body carries the new vault's
  address** — the deploy-graph is not reconstructible from events, so the
  in-code curated seed is the trust root.

### The four proofs (per ADR-0040 §3)

| Proof | Meaning | Coverage |
|---|---|---|
| **A** | The contract's FIRST event is inside a factory `create` transaction (created-with-deposit) | 71/110 |
| **B** | The contract address appears in a factory `create` event body (strategies are referenced by vault configs) | 16/22 strategies |
| **C** | The contract's live instance runs a WASM hash the team itself publishes in `mainnet.contracts.json` (`defindex_vault` = `ae3409a4…468b`, `blend_strategy` = `11329c24…988` — the ONLY two hashes the lake shows across all resolvable emitters) | 61/110 resolvable (dormant instances have no live state to check) |
| **D** | Listed in the team's own Dune vault registry (query 5926821) | most vaults |

**101 of 110 emitters carry at least one proof → gated.** The 7
strategies named in `mainnet.contracts.json` are all in the gated set.

### ⚠️ Flagged — the 9 no-proof emitters (excluded, NOT silently dropped)

155 events total (0.13% of the source's lake activity). The five 1-event
strategies and the two 55.46M-era strategies predate the first factory
create event — early dev/rehearsal deployments; `CBGCGVKH…` (133 events)
is the one materially active unknown: it emits vault-shaped events over a
long window but has no creation correlation, no live instance to
WASM-check, and appears in no team registry. If the team vouches for any
of these, the unblock is one `INSERT INTO protocol_contracts` (or a seed
extension) + `projector-replay`.

| Contract | Layer | Events | First → last ledger | Proofs | Status |
|---|---|---:|---|---|---|

### Full per-contract evidence table (110 emitters)

<details>
<summary>Expand — every emitter with its proofs (A/B/C/D as defined above)</summary>

| Contract | Layer | Events | First → last ledger | Proofs | Status |
|---|---|---:|---|---|---|
| `CDB2WMKQQNVZMEBY7Q7GZ5C7E7IAFSNMZ7GGVD6WKTCEWK7XOIAVZSAP` | strategy | 46694 | 57056389 → 63343513 | BC | gated |
| `CCA2ZJP5BVRXYTQH4FAGHCAUMRYCXVC4CRYC2NXHWMR7TIVX36U7F5HR` | vault | 38269 | 59368573 → 63343513 | ACD | gated |
| `CCSRX5E4337QMCMC3KO3RDFYI57T5NZV5XB3W3TWE4USCASKGL5URKJL` | strategy | 8295 | 57056395 → 63313628 | BC | gated |
| `CBNKCU3HGFKHFOF7JTGXQCNKE3G3DXS5RDBQUKQMIIECYKXPIOUGB2S3` | vault | 5405 | 57296604 → 63343415 | CD | gated |
| `CC767WIU5QGJMXYHDDYJAJEF2YWPHOXOZDWD3UUAZVS4KQPRXCKPT2YZ` | vault | 3700 | 58867278 → 63337110 | CD | gated |
| `CBUJZL5QAD5TOPD7JMCBQ3RHR6RZWY34A4QF7UHILTDH2JF2Z3VJGY2Y` | vault | 2416 | 60169229 → 63327649 | ACD | gated |
| `CAIZ3NMNPEN5SQISJV7PD2YY6NI6DIPFA4PCRUBOGDE4I7A3DXDLK5OI` | vault | 1825 | 57296719 → 63343503 | CD | gated |
| `CC5CE6MWISDXT3MLNQ7R3FVILFVFEIH3COWGH45GJKL6BD2ZHF7F7JVI` | strategy | 1620 | 57056391 → 63343503 | BC | gated |
| `CA33NXYN7H3EBDSA3U2FPSULGJTTL3FQRHD2ADAAPTKS3FUJOE73735A` | strategy | 1520 | 57056397 → 62688003 | BC | gated |
| `CCIRVAW3IZVAYLHR7YYMZFOQVYEW67OKFFXR3J6ZR2T6YJC5V7GTSNQ5` | vault | 1233 | 62581951 → 63114180 | ACD | gated |
| `CANBU7T77SCJOOAU6VQAOGR7DN36JBQFUN56XS2WA2VPJYUSRUBIPYDS` | vault | 1137 | 62581949 → 63114177 | ACD | gated |
| `CCUZC3HC5TH2VCYZFUG57E6IGKPL45YUN2SI3UEYQUBA7RCYHUIZBSFV` | vault | 1025 | 62627725 → 63298131 | ACD | gated |
| `CB3FUMFGCF6DHSFK6N2TOKHRMYXS34HFKQR45UKVORCRUM35AF3ES7WQ` | vault | 950 | 62435111 → 63227631 | ACD | gated |
| `CDI2ZW5CKT4OIHX3IGMVJ4VGOH6Z64N2M3URKATYJIX7JRITJFQJPFD7` | vault | 894 | 62422783 → 63114157 | ACD | gated |
| `CCM3CKJI7BBMZ357644KLAE6NH4D7JQ6MUJHSV4UBRWJY7IMGHBJRNGR` | vault | 874 | 62424741 → 63114172 | ACD | gated |
| `CBYTDU4JKTMFG5CNIUYJTOVMNIN5ADU2PDG4QWIUVT6SSVK3VTXYTF4K` | vault | 851 | 62424668 → 63113840 | ACD | gated |
| `CA2FIPJ7U6BG3N7EOZFI74XPJZOEOD4TYWXFVCIO5VDCHTVAGS6F4UKK` | vault | 563 | 58943684 → 63296622 | ACD | gated |
| `CA3SO5RRKOONAPWVR5XY6CMOYZGN4M4QKVIGX5DFRIIJUJW2SFSELBXL` | strategy | 476 | 62424668 → 63108072 | ABC | gated |
| `CCDRFMZ7CH364ATQ5YSVTEJ3G3KPNFVM6TTC6N4T5REHWJS6LGVFP7MY` | vault | 461 | 60752839 → 63261981 | ACD | gated |
| `CDPWNUW7UMCSVO36VAJSQHQECISPJLCVPDASKHRC5SEROAAZDUQ5DG2Z` | strategy | 297 | 57056393 → 63313627 | BC | gated |
| `CAEPJIHET2TBI2VCLJZI6QHMN366KUGNK4AOKE3YY7AOKMU4KX4RDRGB` | vault | 252 | 62258912 → 62938850 | ACD | gated |
| `CD3HR7WNGPDUGK5ITNMZSRM36O2IFJF3N4RFHOITP4DCXMVGHMANN3XR` | vault | 251 | 62259002 → 62938864 | ACD | gated |
| `CD7T34Y5SZ6MBEZDMXDIQWQ6JICO7TYH7E6DKZJ7BHXOMR2EQ65WYSZG` | vault | 250 | 62258943 → 62938871 | ACD | gated |
| `CB5YXWIDBQAOTTPEQE3SRNUFM2PTOXFHKGUWCBJJSF2GPW37DN725FDA` | vault | 249 | 62258964 → 62938855 | ACD | gated |
| `CAWM7NKSYG2ITJW2MYYJWJ5ULGCJLDB6MXZIWPL3VPRG5TDVLJ66IMWR` | vault | 212 | 61174614 → 63207934 | ACD | gated |
| `CBDOIGFO2QOOZTWQZ7AFPH5JOUS2SBN5CTTXR665NHV6GOCM6OUGI5KP` | strategy | 207 | 57056399 → 63313630 | BC | gated |
| `CD4B5WJDJQ6G5K6MVC3VHTBI2PNLWJBWLXHV75S245Q3PIQWC262UZ4C` | vault | 198 | 62773692 → 63338521 | ACD | gated |
| `CCPKQH3K5XUGP5GXCT6WTABS7TGXRR745BJ4MEFSGNATB7AOBRL4VEOT` | vault | 184 | 62435210 → 62630443 | ACD | gated |
| `CD4JGS6BB5NZVSNKRNI43GUC6E3OBYLCLBQZJVTZLDVHQ5KDAOHVOIQF` | vault | 182 | 59377904 → 63017451 | ACD | gated |
| `CCBTSHPUVNKCT5V675AAVYNANHXBU26PTZK2QLS7ZLFNYRJZT5HW3VL6` | strategy | 162 | 62424741 → 63329920 | ABC | gated |
| `CBGCGVKHVA4TG6MGQ3XTOEHEJXK4DYLOKTMR4UT4PZFPTQKLYXYRF6KV` | vault | 133 | 56976897 → 62470936 | — | **FLAGGED — excluded** |
| `CAHXQWU2HB74PIBT2BUIPYUZXMGZJEQUCNMQLEZR4OMNXMCEHYNEUWZQ` | strategy | 103 | 56891453 → 62470936 | B | gated |
| `CAVL4BSHMU5ECWZCB6ETYSBV4EWTRMHAGMVUEJ5PXM3P3E3AOJPX2TLU` | vault | 92 | 57285919 → 58570017 | D | gated |
| `CBDZYJVQJQT7QJ7ZTMGNGZ7RR3DF32LERLZ26A2HLW5FNJ4OOZCLI3OG` | vault | 89 | 57281028 → 62217052 | CD | gated |
| `CAHGILQRWEGTAWIYGLVFKFPRPNH4NN6KZIDBGHMWABIWZZLW2ZHLHQOG` | vault | 66 | 61814121 → 63285504 | ACD | gated |
| `CC24OISYJHWXZIFZBRJHFLVO5CNN3PQSKZE5BBBZLSSI5Z23TKC6GQY2` | vault | 50 | 58946778 → 62651799 | ACD | gated |
| `CDRSZ4OGRVUU5ONTI6C6UNF5QFJ3OGGQCNTC5UXXTZQFVRTILJFSVG5D` | vault | 41 | 58166484 → 61789218 | ACD | gated |
| `CAZ3LLLKPWEOVK6K4G5NCQ2VXWABLFIPKKNMN5GLKMZKEN7JSKTEMIKN` | strategy | 39 | 62422783 → 62819135 | ABC | gated |
| `CBBAH2OAJ6N3UBJGXNFYH4QF6C6OWO4RHGGOOGDER3IJB7SGLR3Y56JO` | vault | 39 | 62240525 → 63307595 | CD | gated |
| `CBP2R5KYAWJCOCVDTSNTEVL3O6JBTWOOH7SZOX7DX5DLGVZCAMLBDZM3` | vault | 32 | 62431769 → 63175118 | ACD | gated |
| `CD455S6D4A2G36TXWSYUQNDX4YJBFJJSFRSXBSU7H6TVM6FC67ZMIFGQ` | vault | 28 | 62301499 → 62999497 | CD | gated |
| `CD65RR656FIX5LRC7M5RP46IE2MQFK5OEWRM6L6KLIJVUU222U3PFUAP` | vault | 27 | 62972282 → 63329920 | ACD | gated |
| `CAQ6PAG4X6L7LJVGOKSQ6RU2LADWK4EQXRJGMUWL7SECS7LXUEQLM5U7` | vault | 25 | 57197829 → 61960947 | CD | gated |
| `CBTSRJLN5CVVOWLTH2FY5KNQ47KW5KKU3VWGASDN72STGMXLRRNHPRIL` | strategy | 23 | 58943896 → 61353122 | BC | gated |
| `CAB4JOLSCNELJVDQKZLVGHKWJCLXFDBZZMITJAFL4GBGTHIKWO47PYFH` | vault | 22 | 62431734 → 63146280 | ACD | gated |
| `CCKTLDG6I2MMJCKFWXXBXMA42LJ3XN2IOW6M7TK6EWNPJTS736ETFF2N` | vault | 22 | 58943686 → 62848528 | ACD | gated |
| `CDTCSXSKRIFYLDMMF3UABU63LEXSAR2CRCJVSL2PUJGVLNCQWU7XGWCN` | vault | 22 | 60121234 → 63276919 | CD | gated |
| `CAGERKFCDHHCES64L43EU242KIVQMPYAL37CFYIGMLBGJIQTYWXFRWIT` | vault | 20 | 61104926 → 61175124 | CD | gated |
| `CD6GVZTGH7L6NELM2YFCMBF7QAI6DFR25GPOQFKKAZQHVRLB5ZP46CYM` | vault | 19 | 60903063 → 60905382 | D | gated |
| `CAARFNLSJSACT7OWWJP6H5KFFD7T4BWL67OT5K3RRULHGW3C5DZP6Y6D` | vault | 18 | 60966199 → 60977959 | D | gated |
| `CDTBBS6KNIWKG6PJUQBWWGBMIE5AANF7CBDT5JRAJDD3L5JHN75LZBET` | vault | 16 | 63014286 → 63257164 | AC | gated |
| `CCB2AR5X3KP4WQKE7HNSUSDS7SHFMC2WPVSZ2ZXJ6DHXOKHFFKOZE6GK` | vault | 14 | 62518177 → 63266605 | ACD | gated |
| `CBMERS7MJHO6TGKUVWWU34ZSKWCFOWPG2ZCIRIT75IC3YDWBIPBMV5LB` | vault | 11 | 62581952 → 63187196 | ACD | gated |
| `CCFWKCD52JNSQLN5OS4F7EG6BPDT4IRJV6KODIEIZLWPM35IKHOKT6S2` | vault | 11 | 57057068 → 62197696 | ACD | gated |
| `CDONBLOOTYZ7QN62ZLJFHK7CT3JCP3JEZDCRSG3VLGAP73QAXS7HF6HU` | vault | 10 | 58943682 → 61275379 | ACD | gated |
| `CAQKDOORT6G3VP7MTQ6NEFQOYLSJFER7M7Z4BCQ6IWW7DVA2TUHQYEHO` | strategy | 9 | 55466999 → 55467613 | — | **FLAGGED — excluded** |
| `CBGE43WF5GBDCHMN2XPKIAC7TYMWCR6FOJTVFMBR6QQM6WKZB7BM23LL` | vault | 9 | 60061738 → 61426326 | AD | gated |
| `CDIHXKZ4PFKAIONK52JAR6ZNMP62F3UP7XTIBSJTQLMLHQ44PQ5Q2H3J` | vault | 9 | 59001787 → 59012429 | D | gated |
| `CAIFV6BSPN2UHGDSOJK7RLOEVBLQX6EAGIVJWVWSEI7ROLUGI3U2XDTP` | vault | 8 | 60905894 → 60969156 | D | gated |
| `CDI7QVDTNDFEHB25VFQGMNFALGCXXKAWUSHOTQR2D4O44CATQJ5ZQMN6` | vault | 8 | 58946780 → 62200689 | ACD | gated |
| `CDSM6RP3GP6MSV7PXN7OSXCJ5EGMSLGLYFJ4QEPPMQWABD5JU5UPAOZM` | vault | 8 | 59377938 → 61426478 | AD | gated |
| `CDDXPBOF727FDVTNV4I3G4LL4BHTJHE5BBC4W6WZAHMUPFDPBQBL6K7Y` | strategy | 6 | 58362344 → 61353186 | BC | gated |
| `CDPJEMZOYZLITC4MRLGJQHPMNCIB3TZ4R42J6M37PWP5Q2FGO4WFIXAD` | vault | 6 | 60969163 → 60969356 | D | gated |
| `CAXRLUOSI7DL3SYNZW5UGRIPVNRKKSZTW35OX5DWKZSJ4PFEVA2VEFCQ` | vault | 5 | 62434976 → 62540393 | ACD | gated |
| `CBDZ2L4HHEPPL4ABHPORQC72E5S2GLNRPJ467XV3CW5FDWICUNH6SF4B` | vault | 5 | 62312173 → 62312189 | ACD | gated |
| `CBM3VVKTQJBHI2LCZCVFJZCKX7UECRFE2MYBUKAMBBSGC4DB2CUO25IB` | vault | 5 | 63265320 → 63298153 | C | gated |
| `CDKNDBBVLTSO2DSLTZOIF2A4NJWPXTGHD3WYSWBHYBJDKAX4JCKEFMHT` | vault | 5 | 60964396 → 60964947 | D | gated |
| `CA4ZXVRFEB4QGN2CTIY57B3FF3AEOMWZYW3CJHDCI23VVOGJTR3L4SBR` | vault | 4 | 60058071 → 60058071 | A | gated |
| `CDX7DJZFV2DWP2JGI7DNB4XLLLI4JFZ52235A2YO25RZ2UQEPSJ4FDEO` | vault | 4 | 56891387 → 56992451 | — | **FLAGGED — excluded** |
| `CAFI7WOCU33VOVTTORUFGRLBT43LJUY62CGHLVKUA5XWUGXMK7CEHQP6` | vault | 3 | 60058200 → 60058200 | A | gated |
| `CAK6SYGM3GXIHEBXI4FDCW47KL4QH7WYAOJQL4FFC4SVXE4AF3HWYJJP` | vault | 3 | 60117387 → 60117387 | A | gated |
| `CBHB2G4TMSVWE4YFDTFYRYNCP5KUT6RQVWQGIM4LQO2IKKHVDB7N5JJQ` | vault | 3 | 58943688 → 58943896 | AD | gated |
| `CBTX63BX2I6E2VG2SMFQXDHLAPDOANUWBTMXQNWBV2FT6DIMVQPCSOBW` | strategy | 3 | 55483698 → 55511456 | B | gated |
| `CBWSIUHTONZRZJSJS7XABKCBMNGDVWH6UMCW665A5OIRJ7FGCJM6F2VP` | vault | 3 | 62431742 → 62431742 | AC | gated |
| `CC3JKADE5M5KYIHLSS2GB55AT7FIO2VHQITUGMHS6NHDPVSCVDT3UMBX` | vault | 3 | 60058106 → 60058106 | A | gated |
| `CC67IVEYVNW2TC7ELFDNQP2IYSYH6LJDWH6MNP2WOSWEQFXJQZZRN2I5` | vault | 3 | 61502761 → 61502761 | AC | gated |
| `CCEYLML2C7YLWQA4BAQMZKHA6X7FUBLM5PXWVDOYDOP7TNT54GPHP7ZV` | vault | 3 | 60102564 → 60102564 | A | gated |
| `CCSZUL5AVTHWCDV32HFPKOXRBDWIFJFSRSRYJI64OS2ESMNZDJD7HED3` | vault | 3 | 60102708 → 60102708 | A | gated |
| `CCW3PEFVDPRCLQ7YTSLGC3P37VEU6RZ7M3DEVD4NZSM5ZNVDG4N5NFOI` | vault | 3 | 60058237 → 60058237 | A | gated |
| `CDQ272FPRZQFBGUOZSSERBMDD7AYVO3CUOIZYRA7LGDHKU4VO5QWMOR6` | vault | 3 | 62431625 → 62431625 | AC | gated |
| `CDXXQPPZPDY4TMRDOM4RCL6NEUPDGYTYEYN4BVRVDDJT5MVILVH7L7WF` | strategy | 3 | 55466585 → 55466598 | — | **FLAGGED — excluded** |
| `CDYH7U7YYI4AGRXK3NFEN637TR6ID7WP2PW47QZ7KSVPU35LB2ZNDIDG` | vault | 3 | 60102460 → 60102460 | A | gated |
| `CA25XTGHKQ6PUMFJ4SDNRFMUABIFX46U7VAZBFDZKAOX5C3KZXUAR2KQ` | vault | 2 | 60902942 → 60963991 | AD | gated |
| `CC6YDVFTWSHFWTIK5FLLN4TOSE3L7M6TLRA6UPR524GY2T3NIPAVNUXD` | vault | 2 | 55511454 → 55511456 | — | **FLAGGED — excluded** |
| `CCV43DIK4TLUHFKNE3XL4QMNU6P7EGKMYNCDFRIB4HX4FTRM2IBDQEPS` | vault | 2 | 58943690 → 58943748 | A | gated |
| `CDSCVJHJWUZQMR64FVK3XMND5NKSN7Z23KPRCHKFHVGOEJBWPVH5B5XA` | strategy | 2 | 62435017 → 62581952 | BC | gated |
| `CA5RG7DCLMNJFRMG3LP2VDUBWCZ4QTZ776VCEQKWBPGDUAJAT26K2OXM` | vault | 1 | 58123161 → 58123161 | A | gated |
| `CA7AURROFFMNWA4GE6LUEBXNIGUUGSSQYDCEBGJVUXL3UZZ4JHH7NGYC` | vault | 1 | 59001545 → 59001545 | A | gated |
| `CAFIN4DOSWRGSZO454VKGTVUQRZ4IEJMJJZKOND3Z3HWDY7YC6YY7JLF` | vault | 1 | 58928649 → 58928649 | A | gated |
| `CAGP3V7WKOJX2W3J24OZQRTGPZFPW2DXVIHY2TDP7CUPM6CKAL5WR4LU` | vault | 1 | 59376956 → 59376956 | A | gated |
| `CAKHX4CGFV56MFAMMMNYBEX3IUYFDVTMVZXIZRM3BBQMDRDYZLAJVRNF` | vault | 1 | 60966122 → 60966122 | A | gated |
| `CB3LKO733H3STIDCKWY4H25FH426HA7WSERMJ3CZBQTKPOESKZ7LGOWA` | vault | 1 | 58943579 → 58943579 | A | gated |
| `CB5FP32DQKDA7Z7SJ7DGP2FRJRQLPXBSRMSK2KNLGC3V4SXWIIWLJWKM` | strategy | 1 | 58946652 → 58946652 | BC | gated |
| `CBAU3UYY3WMTUZ23XS7I4YXU63GCKXFHO26MNZ7RPIRXP3YUJMYGJRAV` | vault | 1 | 57715803 → 57715803 | A | gated |
| `CBCDAA5URMD55FQ5UQX3SCXLJTJQ35FNZHKNICVMAWLPSTJOX4BITYD6` | vault | 1 | 58943576 → 58943576 | A | gated |
| `CBGC65JVYZZTGPVHURM32GFMMTUQJZVRTB6QAQDLNP4FG3LVFS5XJ7L2` | vault | 1 | 58943541 → 58943541 | A | gated |
| `CBHK3QURQ7OFTNMYQS7TBKPRWN3QDGNLIGANFGUFHG2SEE7WRJEMWWGE` | vault | 1 | 57272759 → 57272759 | A | gated |
| `CBSU24OXATTHBPNLWVEXIN3OZZSONBGVH6J4S3STQMR27ZR54MD4WEOL` | vault | 1 | 58943651 → 58943651 | A | gated |
| `CBWD2EKIMVG6PM6VZEOCFJ3HPRZ2MDOIT3JCCK6WX2JURISQ3LWWBUIT` | strategy | 1 | 58946654 → 58946654 | BC | gated |
| `CBZWNB2B7TNLCSFDOXZ24F54CWWUPDQ76DSHFLHBYC4KSPBCFHOHNYYN` | vault | 1 | 58943647 → 58943647 | A | gated |
| `CC3KWIAIQGHJL7CRUBZMUO5R2M4IV352ACE2MLAI2B3236GGJ7X6Z5E7` | vault | 1 | 58943539 → 58943539 | A | gated |
| `CCAOBAAWWWP7R24ZZ5S2C7GUUKZTOXPHFNQXYICBXMQZY2IBYNCVOWOL` | vault | 1 | 58943649 → 58943649 | A | gated |
| `CCLJFYWNVMNLF3TFO2AJMYFGMI2EBP5U3BWPOL437IKGHNUJYOWIHTV3` | vault | 1 | 57287191 → 57287191 | A | gated |
| `CCMJUJW6Z7I3TYDCJFGTI3A7QA3ASMYAZ5PSRRWBBIJQPKI2GXL5DW5D` | strategy | 1 | 58362342 → 58362342 | — | **FLAGGED — excluded** |
| `CCTLQXYSIUN3OSZLZ7O7MIJC6YCU3QLLS6TUM3P2CD6DAVELMWC3QV4E` | strategy | 1 | 58362346 → 58362346 | — | **FLAGGED — excluded** |
| `CCURKY2V3URC6COTTBZZNL33L5AJLXWEIFIKVNCJOSMIJ2KNJQJQTQ6V` | vault | 1 | 60062574 → 60062574 | A | gated |
| `CCY2V6WZDC7UZL225KHUE6YZOM44XCU3CNE5WYQGTDKP67QQ6U6W46UD` | strategy | 1 | 58946650 → 58946650 | — | **FLAGGED — excluded** |
| `CD45F76UVOSMUMSHLP2OMCNF7N662DQA76IM6PP7PWTCITGMZITULHZ7` | vault | 1 | 58943577 → 58943577 | A | gated |
| `CDQPH6PYFPMZU37OBZU44UOTXHCMH2RFRMXZXGOX3KAR6REDSWY6W3KI` | vault | 1 | 58943538 → 58943538 | A | gated |
| `CDXZESX452QH2NIINQDTJ2S7G2CF2QCL43Q3K2VN3VAVKIE4QRS43GHC` | strategy | 1 | 58946656 → 58946656 | — | **FLAGGED — excluded** |

</details>

### Operator rollout (ADR-0040 §2 — deploy preconditions)

1. Deploy the gated build (the in-code seed is the trust root;
   `seed-protocol-contracts -source defindex` is a no-op today since the
   factories' create events announce no children — install it anyway for
   the day a factory WASM starts announcing them).
2. Re-derive: `projector-replay -source defindex -from 57056338` (under
   `run-heavy-job.sh`). Replay is upsert-only, so ALSO delete the flagged
   contracts' rows:

   ```sql
   DELETE FROM defindex_flows WHERE contract_id IN (
     'CAQKDOORT6G3VP7MTQ6NEFQOYLSJFER7M7Z4BCQ6IWW7DVA2TUHQYEHO',
     'CBGCGVKHVA4TG6MGQ3XTOEHEJXK4DYLOKTMR4UT4PZFPTQKLYXYRF6KV',
     'CC6YDVFTWSHFWTIK5FLLN4TOSE3L7M6TLRA6UPR524GY2T3NIPAVNUXD',
     'CCMJUJW6Z7I3TYDCJFGTI3A7QA3ASMYAZ5PSRRWBBIJQPKI2GXL5DW5D',
     'CCTLQXYSIUN3OSZLZ7O7MIJC6YCU3QLLS6TUM3P2CD6DAVELMWC3QV4E',
     'CCY2V6WZDC7UZL225KHUE6YZOM44XCU3CNE5WYQGTDKP67QQ6U6W46UD',
     'CDX7DJZFV2DWP2JGI7DNB4XLLLI4JFZ52235A2YO25RZ2UQEPSJ4FDEO',
     'CDXXQPPZPDY4TMRDOM4RCL6NEUPDGYTYEYN4BVRVDDJT5MVILVH7L7WF',
     'CDXZESX452QH2NIINQDTJ2S7G2CF2QCL43Q3K2VN3VAVKIE4QRS43GHC');
   ```

   (`defindex_flows` keys rows by the emitting contract, so unlike the
   aquarius `trades` cleanup this is a direct per-contract delete.)
3. Verdict watch: one green `compute-completeness -ch` cycle for defindex.

### Updated asks for the DeFindex team (2026-07-05)

1. (Unchanged, still the structural gap) Can the vault address be
   recovered from the `create` event or a factory view? Until then, every
   new vault needs manual verification + seeding on our side.
2. Please confirm or disown the 9 flagged emitters above — especially
   `CBGCGVKHVA4TG6MGQ3XTOEHEJXK4DYLOKTMR4UT4PZFPTQKLYXYRF6KV`.
3. (Unchanged) The 19 Dune-registry vaults that never emit
   `DeFindexVault` events: which WASM versions emit what?
