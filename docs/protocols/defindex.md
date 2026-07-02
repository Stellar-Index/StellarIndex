# DeFindex — contract & event verification

> **For the DeFindex team:** this is the set of DeFindex factories, vaults,
> and strategy contracts Stellar Index ingests. Please confirm the four
> factories and help us with the **open question below** about how vaults
> and strategies relate (so we attribute strategy events correctly).
>
> - **Enumeration method:** lake deploy-graph (`DeFindexFactory` /
>   `DeFindexVault` / `BlendStrategy` events; topics are namespaced, so
>   collision risk is low).
> - **Last verified:** 2026-06-12 (r1 lake).
> - **Gate status:** ⏳ BLOCKED on verified enumeration (2026-07-02): the
>   lake now shows **88** DeFindexVault + **22** BlendStrategy emitters vs
>   the 34+7 verified here on 2026-06-12, and the open question below means
>   the growth CANNOT be deploy-graph-verified from creation events. Gating
>   on raw emitter lists would bake potential look-alikes into the trust
>   root — defindex follows the ADR-0040 §3 enumeration procedure (creation-
>   op chain + wasm-hash cross-check) before its gate ships.

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
| `DeFindexVault` | `deposit`, `withdraw`, `rebalance`, … | `defindex_flows` (vault layer) |
| `BlendStrategy` | `deposit`, `withdraw`, `harvest` | `defindex_flows` (strategy layer) |


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
