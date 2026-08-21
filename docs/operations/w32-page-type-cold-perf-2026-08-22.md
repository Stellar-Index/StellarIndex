# W3.2 — Cold full-page performance: every other page type (first live measurement, 2026-08-22)

**What this is.** W3.1 proved the contract page subsecond-on-cold bar with
`scripts/ops/contract-page-audit.py`. W3.2 extends that harness — per the
launch plan, "a per-page-type panel map rather than a new harness" — to every
other explorer page type, and this doc records the FIRST cold→fully-populated
run against the live API.

**Method.** For each page type the harness fires every API read the page
issues before it is fully populated (panel lists enumerated from the explorer
view components and kept in lockstep — the component is named next to each
map entry in the script), concurrently the way a browser does; serial hops a
browser cannot parallelize (the asset shell's detail→price) are chained the
same way. The page score is wall-clock to the slowest panel; a non-2xx panel
(and a 404 where 404 is not an honest "there is none") counts as UNLOADED and
is reported separately from latency.

- Run: `BASE=https://api.stellarindex.io/v1 BUDGET=1.0 PACE=1.0` (PACE=1
  isolates single cold pages — this is NOT the sustained-crawl stress).
- Cold ids, drawn read-only from the r1 lake / live API on 2026-08-22
  (queries documented in the script docstring):
  - **accounts (23) + tx (25):** `stellar.transactions` sampled from 9 ledger
    windows spread over seqs 3M…63.9M (old + recent mixed, `ORDER BY rand()`).
  - **ledgers (25):** uniform random seqs over [2, 64063683] (tip at draw time).
  - **assets (25) + pairs (25):** stellar-native identifiers from the live
    `/v1/markets` listing (crypto:*/fiat:* skipped), shuffled; pairs as
    `base~quote` slugs. 41 asset ids / 55 native pairs existed in total.
  - **contracts (3, gate only):** `stellar.contract_events` ledger_seq > 63M.
  - **protocol (16):** the full static registry (bounded set).
  - **singletons (operations, home, network, protocols):** RUNS=3
    iterations — run 1 is the coldest; later runs mostly show server TTL
    caches.

## Summary — X/N pages breaching the 1.0s full-page budget

| page type | pages | breaches >1s | worst page | worst slowest panel | UNLOADED pages |
|---|---|---|---|---|---|
| account | 23 | 23/23 | 8.23s `GCCHHX2VWDRSUTQH3UAD2636P2WHGLKMFPK47TP2Y5AKRWCDMIZN4DPW` | operations 8.22s (blame: 14x operations, 6x transactions, 3x activity) | 16 (12x activity, 12x transactions, 9x operations) |
| asset | 25 | 1/25 | 3.07s `CETES-GCRYUG` | price 3.06s (blame: 1x price) | 0 (0) |
| asset-shell | 25 | 3/25 | 1.95s `CAUP7NFABXE5` | detail 1.84s (blame: 3x detail) | 0 (0) |
| ledger | 25 | 0/25 | 0.20s `50758738` | transactions 0.20s (blame: —) | 0 (0) |
| tx | 25 | 0/25 | 0.23s `6a4e53df2b3e4343bd0ed47113ef2cd515fd3da291a6604f654b0ba943f80e08` | tx 0.23s (blame: —) | 0 (0) |
| pair | 25 | 2/25 | 1.93s `CCW67TSZV3SS` | price 1.93s (blame: 2x price) | 0 (0) |
| protocol | 16 | 0/16 | 0.21s `soroswap` | detail 0.21s (blame: —) | 0 (0) |
| operations | 3 | 1/3 | 5.84s `run-1` | directory 5.84s (blame: 1x directory) | 0 (0) |
| home | 3 | 0/3 | 0.32s `run-2` | hero-ohlc 0.31s (blame: —) | 0 (0) |
| network | 3 | 2/3 | 8.12s `run-1` | op-type-mix 8.12s (blame: 2x op-type-mix) | 1 (1x op-type-mix) |
| protocols | 3 | 0/3 | 0.11s `run-1` | protocols 0.11s (blame: —) | 0 (0) |

### account

== worst per panel: operations:8.22s transactions:8.19s activity:8.11s state:5.42s movements:3.39s positions:2.40s issuers:1.23s trades:0.15s

| flag | total | id | slowest panel | unloaded |
|---|---|---|---|---|
| BREACH | 8.23s | `GCCHHX2VWDRSUTQH3UAD2636P2WHGLKMFPK47TP2Y5AKRWCDMIZN4DPW` | operations 8.22s |  |
| FAILED | 8.22s | `GB3XTXHNCUMG4LAJ57X53BGP5AWKRJQVZUJO3B6LGOL2AUDLBUJH72SR` | operations 8.22s | transactions |
| FAILED | 8.19s | `GC7B6HW7DN34OJ77HRG62LW53UJDFXETJ2SRATPITCFKLUSDQ74UV2TZ` | operations 8.19s | activity |
| FAILED | 8.17s | `GDSPS3SXDXVDL5U3HCYZYUR2WP463UQ5PBFNA6T3ATBLT5AKSPZ3M755` | operations 8.17s | activity |
| FAILED | 8.17s | `GDTEQM7TUIJDMKHOQ5NDRWRJCRIXGNDFOK2EQQRMMGA22WRKPQ7QGEYY` | operations 8.17s | activity,operations |
| BREACH | 8.16s | `GB6UR5Y7F3DZJ2WZWYWWNPKALJBB4EVLSFGHHGEBLHPT333AUOSC2EXD` | operations 8.15s |  |
| BREACH | 8.15s | `GA4JOGNSERBO2VZWZWZRKTR3WAS6Y7CE4HUXHXKDIMLLNJUOHRKN4M37` | operations 8.14s |  |
| BREACH | 8.15s | `GAYRHRXF2Z5PCWQDYA7WDBDUZFUPQR3NLQYIRFKARGKS3HVZA2EC6UIX` | operations 8.15s |  |
| FAILED | 8.15s | `GB2FVAA4Z3FDIMRCOOHCLTOSG5K35EK5CPD4MVLUIR75OARNQT7K3TBM` | operations 8.15s | transactions |
| FAILED | 8.14s | `GAE3II5GERMAOURIRB3BZAJ3JXK4PHSISRZ6N4ZAQFXKMKE6GPVB7CE2` | operations 8.14s | transactions |
| FAILED | 8.14s | `GBDOGNQHR3S3IEVAOYP6C4F6COURVP4HYUTHYRSLT7DWIJFWNKJ3TBI5` | operations 8.14s | transactions |
| FAILED | 8.14s | `GDMIFW7V34O6NWMR2RAZH5R2YUDJV2OCL3AAM7DGD4K6DENKTIE7COGC` | transactions 8.14s | activity,operations,transactions |
| FAILED | 8.12s | `GCBZ4XFAKG4IT6JGEZKL47VVAVEDTJNEHV7Y35W3WIGLBOT6DJN47EIJ` | transactions 8.11s | activity,operations,transactions |
| FAILED | 8.11s | `GA5XIGA5C7QTPTWXQHY6MCJRMTRZDOSHR6EFIBNDQTCQHG262N4GGKTM` | transactions 8.11s | activity,operations,transactions |
| FAILED | 8.11s | `GAMNKS2U6YZSXBZTTWDSFQVBJ2GWTR54JX44WRG3B27G45RLJM6SW2AI` | activity 8.11s | activity,operations,transactions |
| BREACH | 8.11s | `GBDJ5ZNSM5YGI2W6COAOZ3OPAZBLR4WNVN4LF4JPW4D4BNPWKUZWIPEJ` | operations 8.11s |  |
| FAILED | 8.11s | `GBKSAWRUPUDMILP7OJ6OGNJSVZ7YVPRAPPF63OL7BYKQ6PVFFRVZFUMP` | activity 8.11s | activity,operations,transactions |
| FAILED | 8.11s | `GDANZTKJEKLAX6BWKRFX7XKZB2D2IIXLNPB5MDBJJKQHZ4Y4Z5E2IYKT` | activity 8.11s | activity,operations |
| FAILED | 8.11s | `GDYP5WJWZAOEGP2IMBKAR5W3AFSLEKXSSEPL4RX2BBS7MNN4BL4SPKEL` | transactions 8.11s | activity,operations,transactions |
| FAILED | 8.10s | `GB4GU2ZTFAFBWFCF64FCQYDZ6URSQTEF752GU3TGJ7XSL4YS3D4R7CRF` | transactions 8.10s | activity,operations,transactions |
| FAILED | 8.10s | `GB6WQS5ZG43OWQPMTVI4N2TKAVASABK2XLAKEHOHBWS2V3R3KQM65L6N` | transactions 8.10s | activity,transactions |
| BREACH | 7.19s | `GAB7GBPE7UEMWMK2737PMB2RSC4ZY2YVG7SQB3S2LKPZYDEOQLW6Q6D2` | operations 7.19s |  |
| BREACH | 2.93s | `GB2IDL2LP3FRDPXDZABJ22ZJ55YUWH3TOMVWVHKAXN5QR3YUGZUSLMYN` | operations 2.92s |  |

### asset

== worst per panel: price:3.06s issuer:0.59s ohlc:0.45s fx-batch:0.25s changes:0.24s

| flag | total | id | slowest panel | unloaded |
|---|---|---|---|---|
| BREACH | 3.07s | `CETES-GCRYUG` | price 3.06s |  |
| ok | 0.76s | `sUSD-GCHW7CWI7GMIYQYFXMFJNJX5645XGWIINIAEQK3SABQO6CAYL5T7JYIH` | price 0.76s |  |
| ok | 0.60s | `USDCAllow-GD` | issuer 0.59s |  |
| ok | 0.54s | `AUDD-GDC7X2M` | issuer 0.53s |  |
| ok | 0.53s | `RON-GDE6EMCC` | issuer 0.52s |  |
| ok | 0.53s | `LDEX-GCTAWY7` | issuer 0.52s |  |
| ok | 0.53s | `XAUa-GB2I4YQVCMSKOFRAQYPRNLMCTGMO7OKHQ4YOBSDJ77BG2MYZYNET27YE` | issuer 0.52s |  |
| ok | 0.45s | `yUSDC-GDGTVWSM4MGS4T7Z6W4RPWOCHE2I6RDFCIFZGS3DOA63LWQTRNZNTTFF` | ohlc 0.45s |  |
| ok | 0.45s | `VND-GCALLXDIIJM5LKZYFYFXYJKSMMLGVB3SGRWC777ZDQMKGMSLVUJHXVND` | issuer 0.44s |  |
| ok | 0.42s | `WLFIBANK-GDUEID5PTLBP4OOEQ4Q4OK55NFH6HX5JTIQNPYQM6DUFCTYMDVMORC4K` | issuer 0.41s |  |
| ok | 0.39s | `USDV-GBLAJOKBIIT7P32BJQFCSRJVOE2SXHI4D5ZGLFJ4DLMFJXI2NN6R37G5` | issuer 0.39s |  |
| ok | 0.38s | `XRP-GBXRPL45` | issuer 0.38s |  |
| ok | 0.37s | `AUD-GAIF52QZUPYCADXF7I7RNPMED7DT2B5JGPR7DEHCC5TPDPUJTMLGGAUD` | issuer 0.36s |  |
| ok | 0.36s | `SCOP-GC6OYQJIZF3HFXCYPFCBXYXNGIBQ4TNSFUBUXQJOZWIP6F3YZK4QH3VQ` | issuer 0.36s |  |
| ok | 0.35s | `AUDR-GAAVW6EQ4N4SHNTKBLTOBXKS6CEIMT2KZI7YQ5B37ECNVPFLBIGRKLIL` | issuer 0.35s |  |
| ok | 0.33s | `AQUA-GBNZILS` | ohlc 0.33s |  |
| ok | 0.32s | `GBP-GBN2FSV3` | issuer 0.32s |  |
| ok | 0.30s | `CAAV3AE3VKD2P4TY7LWTQMMJHIJ4WOCZ5ANCIJPC3NRSERKVXNHBU2W7` | ohlc 0.29s |  |
| ok | 0.24s | `CAUIKL3IYGME` | ohlc 0.23s |  |
| ok | 0.23s | `CAO7DDJNGMOYQPRYDY5JVZ5YEK4UQBSMGLAEWRCUOTRMDSBMGWSAATDZ` | ohlc 0.23s |  |
| ok | 0.19s | `CCCRWH6Q3FNP3I2I57BDLM5AFAT7O6OF6GKQOC6SSJNDAVRZ57SPHGU2` | ohlc 0.19s |  |
| ok | 0.17s | `CAUP7NFABXE5` | ohlc 0.16s |  |
| ok | 0.16s | `CCW67TSZV3SS` | fx-batch 0.16s |  |
| ok | 0.15s | `CBIJBDNZNF4X` | fx-batch 0.15s |  |
| ok | 0.14s | `USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN` | fx-batch 0.14s |  |

### asset-shell

== worst per panel: detail:1.84s price:0.28s

| flag | total | id | slowest panel | unloaded |
|---|---|---|---|---|
| BREACH | 1.95s | `CAUP7NFABXE5` | detail 1.84s |  |
| BREACH | 1.94s | `CAO7DDJNGMOYQPRYDY5JVZ5YEK4UQBSMGLAEWRCUOTRMDSBMGWSAATDZ` | detail 1.84s |  |
| BREACH | 1.10s | `CBIJBDNZNF4X` | detail 0.99s |  |
| ok | 0.96s | `CETES-GCRYUG` | detail 0.68s |  |
| ok | 0.83s | `CAAV3AE3VKD2P4TY7LWTQMMJHIJ4WOCZ5ANCIJPC3NRSERKVXNHBU2W7` | detail 0.71s |  |
| ok | 0.66s | `sUSD-GCHW7CWI7GMIYQYFXMFJNJX5645XGWIINIAEQK3SABQO6CAYL5T7JYIH` | detail 0.44s |  |
| ok | 0.59s | `CCW67TSZV3SS` | detail 0.48s |  |
| ok | 0.48s | `SCOP-GC6OYQJIZF3HFXCYPFCBXYXNGIBQ4TNSFUBUXQJOZWIP6F3YZK4QH3VQ` | detail 0.36s |  |
| ok | 0.42s | `LDEX-GCTAWY7` | detail 0.31s |  |
| ok | 0.40s | `USDV-GBLAJOKBIIT7P32BJQFCSRJVOE2SXHI4D5ZGLFJ4DLMFJXI2NN6R37G5` | detail 0.28s |  |
| ok | 0.40s | `USDCAllow-GD` | detail 0.26s |  |
| ok | 0.40s | `XAUa-GB2I4YQVCMSKOFRAQYPRNLMCTGMO7OKHQ4YOBSDJ77BG2MYZYNET27YE` | detail 0.29s |  |
| ok | 0.39s | `WLFIBANK-GDUEID5PTLBP4OOEQ4Q4OK55NFH6HX5JTIQNPYQM6DUFCTYMDVMORC4K` | detail 0.27s |  |
| ok | 0.39s | `AUDD-GDC7X2M` | detail 0.28s |  |
| ok | 0.36s | `RON-GDE6EMCC` | detail 0.25s |  |
| ok | 0.35s | `CAUIKL3IYGME` | detail 0.24s |  |
| ok | 0.33s | `AUDR-GAAVW6EQ4N4SHNTKBLTOBXKS6CEIMT2KZI7YQ5B37ECNVPFLBIGRKLIL` | detail 0.23s |  |
| ok | 0.33s | `AUD-GAIF52QZUPYCADXF7I7RNPMED7DT2B5JGPR7DEHCC5TPDPUJTMLGGAUD` | detail 0.22s |  |
| ok | 0.31s | `CCCRWH6Q3FNP3I2I57BDLM5AFAT7O6OF6GKQOC6SSJNDAVRZ57SPHGU2` | detail 0.20s |  |
| ok | 0.30s | `yUSDC-GDGTVWSM4MGS4T7Z6W4RPWOCHE2I6RDFCIFZGS3DOA63LWQTRNZNTTFF` | price 0.19s |  |
| ok | 0.30s | `VND-GCALLXDIIJM5LKZYFYFXYJKSMMLGVB3SGRWC777ZDQMKGMSLVUJHXVND` | detail 0.19s |  |
| ok | 0.28s | `GBP-GBN2FSV3` | detail 0.18s |  |
| ok | 0.24s | `AQUA-GBNZILS` | price 0.14s |  |
| ok | 0.23s | `XRP-GBXRPL45` | price 0.13s |  |
| ok | 0.22s | `USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN` | price 0.11s |  |

### ledger

== worst per panel: transactions:0.20s ledger:0.14s

| flag | total | id | slowest panel | unloaded |
|---|---|---|---|---|
| ok | 0.20s | `50758738` | transactions 0.20s |  |
| ok | 0.20s | `36927949` | transactions 0.19s |  |
| ok | 0.20s | `38971890` | transactions 0.20s |  |
| ok | 0.20s | `49332950` | transactions 0.20s |  |
| ok | 0.20s | `34239997` | transactions 0.19s |  |
| ok | 0.19s | `33494837` | transactions 0.19s |  |
| ok | 0.19s | `53899293` | transactions 0.19s |  |
| ok | 0.19s | `41745870` | transactions 0.19s |  |
| ok | 0.19s | `59257057` | transactions 0.19s |  |
| ok | 0.19s | `62337937` | transactions 0.19s |  |
| ok | 0.19s | `44284886` | transactions 0.19s |  |
| ok | 0.19s | `52739786` | transactions 0.19s |  |
| ok | 0.18s | `53629122` | transactions 0.18s |  |
| ok | 0.18s | `56898297` | transactions 0.18s |  |
| ok | 0.17s | `32229415` | transactions 0.17s |  |
| ok | 0.15s | `19568477` | transactions 0.15s |  |
| ok | 0.13s | `1394724` | ledger 0.13s |  |
| ok | 0.13s | `12278606` | ledger 0.13s |  |
| ok | 0.13s | `4713739` | ledger 0.13s |  |
| ok | 0.13s | `2711035` | ledger 0.13s |  |
| ok | 0.13s | `22228519` | transactions 0.13s |  |
| ok | 0.13s | `11566415` | ledger 0.13s |  |
| ok | 0.13s | `21056453` | ledger 0.13s |  |
| ok | 0.12s | `2075197` | ledger 0.12s |  |
| ok | 0.12s | `13406265` | ledger 0.12s |  |

### tx

== worst per panel: tx:0.23s

| flag | total | id | slowest panel | unloaded |
|---|---|---|---|---|
| ok | 0.23s | `6a4e53df2b3e4343bd0ed47113ef2cd515fd3da291a6604f654b0ba943f80e08` | tx 0.23s |  |
| ok | 0.22s | `9b7e87b0e8d9a68cbd1f3ac85ae4f02070570bc6e1afeb186e86ac738a871148` | tx 0.22s |  |
| ok | 0.22s | `d32e7bd4bbb5996f1679f96287c3b7f5807293b198b6b15b26786d4515e60edb` | tx 0.22s |  |
| ok | 0.21s | `8e0a6d15bdb10a7b64eb224267053a72ae622632f21905c6ee5892ca74bbe77b` | tx 0.21s |  |
| ok | 0.19s | `bdf35fb51ad60600cbc81b14debf65fcf9fb8def30a3f82ab8f9de84bd14b903` | tx 0.19s |  |
| ok | 0.19s | `e67d51d23875680c5e1ac88f9edc0a98dbb3b0e1fb3b8720eb60872715468292` | tx 0.19s |  |
| ok | 0.18s | `dd0474215d65a76c5e7fd90216d281768eb5fb51a08d83abf0eeb96f3e8be41b` | tx 0.18s |  |
| ok | 0.17s | `b45003c176ac0b830d7fe30165b7d65397c00afd666d5b8216e20e1b2abfc7df` | tx 0.17s |  |
| ok | 0.17s | `36c79fdb991d3f66d8b2274f2c380606103c98080347f84aff2a67c0f2154a5f` | tx 0.17s |  |
| ok | 0.17s | `1beb87e32b1f6a6cc5b68d54c1882bf9b201cb5040a6aeae9da5277034c8d2c9` | tx 0.17s |  |
| ok | 0.17s | `2ec054b4aaeeab4c6ae3c5a834fc2e31f53112b2835e01e50d2500a5e44bf8bf` | tx 0.17s |  |
| ok | 0.16s | `ab646b35cc88422a94a112030651fd1d14974aacd2b77954430dcb5dd062d64b` | tx 0.16s |  |
| ok | 0.16s | `4e239be61c3beb9a33421ab0c583bbb6c18b968ced01183e86a2f8cf059df8c9` | tx 0.16s |  |
| ok | 0.16s | `c2ab46d539ce84e5862c7dc7e2e86315bb314e4d0f1311ba67f94b40d738eba6` | tx 0.16s |  |
| ok | 0.16s | `752f1abb222362416bb3edd26d01854db331249aad605a6c51c6b97afedc8504` | tx 0.16s |  |
| ok | 0.15s | `67a1953699c709165851071df1d158fde9ba44f2f8a56601da9ebdc9f3cfaa49` | tx 0.15s |  |
| ok | 0.15s | `c01eaa99b651e2cfac61e64fd7e60cb8b3d26fc7d57beac2a8822c68a5f95ddf` | tx 0.14s |  |
| ok | 0.15s | `fb4fcb6f256cffee3bc2b61b09ed099eeabd5d78d8fbb1143ffe0bb9ce696c12` | tx 0.15s |  |
| ok | 0.15s | `b33d4b00fa377785a574c4d4cf481c0e76031007ab16fce33267d16b6dab7dd1` | tx 0.15s |  |
| ok | 0.15s | `26fa65f5040a7db73f955a02c89f0a2940d7c9e6832776ca20be4c70ebe53888` | tx 0.15s |  |
| ok | 0.14s | `67655e2898b6d1e5e45b202b45d55c9c69c8006f4284b868ecefc5945e73b81c` | tx 0.14s |  |
| ok | 0.14s | `4692921e9d19805cc27dfb909bd42dec79f9c4c8de1f34b164cd6dbb7eb9d7fd` | tx 0.13s |  |
| ok | 0.13s | `4c68b57289ec06bfe3effb5051bffe3536afdb3512f155f94d15e78703f4a5fc` | tx 0.13s |  |
| ok | 0.13s | `7b463fc6045bb55dfc0df3d84b052ab57143774798fcff4a6d97fbd57837cdac` | tx 0.13s |  |
| ok | 0.13s | `d1c3eea554b051a804c6162b368a44555b08ec19d1d0ed125741e9f07fc7c345` | tx 0.13s |  |

### pair

== worst per panel: price:1.93s sources:0.39s ohlc:0.26s orderbook:0.18s

| flag | total | id | slowest panel | unloaded |
|---|---|---|---|---|
| BREACH | 1.93s | `CCW67TSZV3SS` | price 1.93s |  |
| BREACH | 1.20s | `CAUP7NFABXE5` | price 1.19s |  |
| ok | 0.93s | `LDEX-GCTAWY7` | price 0.93s |  |
| ok | 0.74s | `CBIJBDNZNF4X` | price 0.74s |  |
| ok | 0.61s | `CCW67TSZV3SS` | price 0.61s |  |
| ok | 0.52s | `BTCLN-GDPKQ2TSNJOFSEE7XSUXPWRP27H6GFGLWD7JCHNEYYWQVGFA543EVBVT~native` | price 0.52s |  |
| ok | 0.40s | `CETES-GCRYUG` | price 0.40s |  |
| ok | 0.34s | `CAUIKL3IYGME` | price 0.34s |  |
| ok | 0.31s | `GBP-GBN2FSV3` | price 0.31s |  |
| ok | 0.30s | `native~USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN` | sources 0.29s |  |
| ok | 0.27s | `yXLM-GARDNV3` | ohlc 0.26s |  |
| ok | 0.26s | `SHX-GDSTRSHXHGJ7ZIVRBXEYE5Q74XUVCUSEKEBR7UCHEUUEK72N7I7KJ6JH~native` | ohlc 0.26s |  |
| ok | 0.26s | `yXLM-GARDNV3` | ohlc 0.26s |  |
| ok | 0.24s | `BTC-GDPJALI4` | price 0.24s |  |
| ok | 0.24s | `EURC-GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2~USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN` | ohlc 0.24s |  |
| ok | 0.24s | `VELO-GDM4RQUQQUVSKQA7S6EM7XBZP3FCGH4Q7CL6TABQ7B2BEJ5ERARM2M5M~native` | ohlc 0.24s |  |
| ok | 0.24s | `BTC-GDPJALI4` | ohlc 0.24s |  |
| ok | 0.24s | `XRP-GBXRPL45` | ohlc 0.23s |  |
| ok | 0.24s | `native~PYUSD-GDQE7IXJ4HUHV6RQHIUPRJSEZE4DRS5WY577O2FY6YQ5LVWZ7JZTU2V5` | ohlc 0.24s |  |
| ok | 0.23s | `RON-GDE6EMCC` | ohlc 0.23s |  |
| ok | 0.21s | `AQUA-GBNZILS` | ohlc 0.21s |  |
| ok | 0.21s | `XRP-GBXRPL45` | ohlc 0.21s |  |
| ok | 0.16s | `AUDD-GDC7X2M` | orderbook 0.16s |  |
| ok | 0.15s | `EURCAllow-GDTZLTO7FFA6575TBETH6UH6UTZNS2UJE6FHS2V5LUEJNXQGX4FZJT2I~EURC-GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2` | orderbook 0.15s |  |
| ok | 0.15s | `USDCAllow-GD` | orderbook 0.15s |  |

### protocol

== worst per panel: detail:0.21s

| flag | total | id | slowest panel | unloaded |
|---|---|---|---|---|
| ok | 0.21s | `soroswap` | detail 0.21s |  |
| ok | 0.20s | `aquarius` | detail 0.20s |  |
| ok | 0.17s | `phoenix` | detail 0.16s |  |
| ok | 0.17s | `blend` | detail 0.17s |  |
| ok | 0.17s | `cctp` | detail 0.17s |  |
| ok | 0.14s | `sdex` | detail 0.14s |  |
| ok | 0.14s | `sorocredit` | detail 0.14s |  |
| ok | 0.14s | `defindex` | detail 0.14s |  |
| ok | 0.14s | `reflector-dex` | detail 0.14s |  |
| ok | 0.14s | `reflector-cex` | detail 0.14s |  |
| ok | 0.14s | `reflector-fx` | detail 0.14s |  |
| ok | 0.14s | `redstone` | detail 0.14s |  |
| ok | 0.13s | `comet` | detail 0.13s |  |
| ok | 0.11s | `rozo` | detail 0.10s |  |
| ok | 0.11s | `band` | detail 0.11s |  |
| ok | 0.10s | `soroswap-router` | detail 0.10s |  |

### operations

== worst per panel: directory:5.84s op-type-mix:5.81s throughput:0.11s

| flag | total | id | slowest panel | unloaded |
|---|---|---|---|---|
| BREACH | 5.84s | `run-1` | directory 5.84s |  |
| ok | 0.14s | `run-2` | directory 0.14s |  |
| ok | 0.14s | `run-3` | directory 0.14s |  |

### home

== worst per panel: hero-ohlc:0.31s verified:0.31s cursors:0.29s movers:0.26s top-assets:0.19s xlm-price:0.19s fx-batch:0.17s chart-24h:0.15s network-stats:0.12s sources:0.12s top-markets:0.12s recent-trades:0.12s

| flag | total | id | slowest panel | unloaded |
|---|---|---|---|---|
| ok | 0.32s | `run-2` | hero-ohlc 0.31s |  |
| ok | 0.31s | `run-3` | verified 0.31s |  |
| ok | 0.29s | `run-1` | cursors 0.29s |  |

### network

== worst per panel: op-type-mix:8.12s pools:1.74s ledgers:0.15s network-stats:0.12s throughput:0.12s sources:0.12s

| flag | total | id | slowest panel | unloaded |
|---|---|---|---|---|
| FAILED | 8.12s | `run-1` | op-type-mix 8.12s | op-type-mix |
| BREACH | 6.34s | `run-2` | op-type-mix 6.34s |  |
| ok | 0.15s | `run-3` | ledgers 0.14s |  |

### protocols

== worst per panel: protocols:0.11s sources:0.11s

| flag | total | id | slowest panel | unloaded |
|---|---|---|---|---|
| ok | 0.11s | `run-1` | protocols 0.11s |  |
| ok | 0.11s | `run-2` | protocols 0.11s |  |
| ok | 0.11s | `run-3` | protocols 0.11s |  |


## Verdicts — which page types have systemic breaches worth W3.3-style fixes

1. **account — SYSTEMIC, the worst surface measured (23/23 breaching,
   16/23 with UNLOADED panels).** The page's heavy family —
   `/accounts/{g}/operations`, `/transactions`, `/activity` — pins at the
   ~8.1s server deadline on cold keys and frequently 503s outright
   (refresh-gate class). Median full page ≈ 8.1s: every cold account page is
   effectively a deadline page. This is exactly W3.3's already-written-up
   "account family 8s" residue (per-entity ClickHouse reads O(scan) for
   non-prewarmed keys), now confirmed page-level on the live lake. Even the
   lighter panels are not subsecond cold: `state` up to 5.4s, `movements` up
   to 3.4s, `positions` up to 2.4s. **W3.3's account-family fix is the
   single highest-leverage item this measurement surfaces.**
2. **operations directory + network insight — SYSTEMIC cold-start, fine
   warm.** The `/operations?limit=50` directory and the `limit=1`
   op-type-stats variant took 5.8s cold (operations run-1) and on the
   network page the same op-type-stats panel 503'd at 8.1s cold then took
   6.3s on run 2 before its detached refresh landed (runs 3: 0.1s). Two
   W3.3-shaped aggravators: the directory TTL cache is keyed by `limit`, so
   the page's own `limit=50` + `limit=1` pair cold-misses twice; and the
   `op_type_stats` detached refresh leaves the panel 503/absent for the
   first viewers after every TTL expiry.
3. **asset-shell — MODERATE tail (3/25 breaching, all on `/assets/{id}`
   detail, 1.8–1.9s, SAC C-address ids).** The long-tail asset path (194k
   non-prerendered assets land here) is O(scan)-shaped on cold keys — same
   W3.3 class as the census/quiet-contract items. The pre-rendered asset
   page's runtime panels are healthy (1/25, a single 3.1s `/price` outlier
   on CETES).
4. **pair — MODERATE tail (2/25), same `/price` culprit** (1.2–1.9s cold on
   thin/SAC pairs). Together with the asset `/price` outlier this names a
   cold-`/price`-on-thin-asset class rather than a pair-page problem.
5. **ledger, tx, protocol, home, protocols — HEALTHY.** 0 breaches, 0
   unloaded across 25+25+16+3+3 pages; worst full page 0.32s. No W3.3 work
   needed on these surfaces.

## Unmeasured residue (named, deliberate)

- **Dependent second hops the harness cannot derive statically:** the
  account Positions `/price/batch` (needs the trustline list from the state
  response; non-fatal to the panel) and home's `/history?…&limit=12` ×3
  (needs the top-markets response). Both are response-derived; both are
  non-fatal decorations on already-measured pages.
- **Global chrome** (`/status`, `/account/me`, SSE `/ledger/stream`) fires
  on every page and is excluded from per-page budgets, as in W3.1.
- **SSE streams** (`/price/tip/stream`, `/ledger/stream`) are long-lived
  connections, not bounded GETs — excluded from wall-clock.
- **Pair pages baked at build time:** the top-500 pairs' entity data is
  static HTML; the measured panels are the runtime set. The build-time set
  (`/chart`, `/history`, `/pools` per pair) is a build cost, not a user
  wait.
- 404s on `/price` for unpriced assets and `/changes/coin/{id}` are honest
  empty answers (panels render nothing) and count as loaded, per the
  per-panel `ok404` flags in the map.

## Reproduce

```
# ids: see the draw queries in scripts/ops/contract-page-audit.py --help
BUDGET=1.0 PACE=1.0 python3 scripts/ops/contract-page-audit.py --type account accounts.txt
BUDGET=1.0 PACE=1.0 python3 scripts/ops/contract-page-audit.py --type home   # singleton, RUNS=3
```

Contract-type behavior is unchanged (byte-compatible: same defaults, same
output format; verified old-vs-new against a deterministic stub and 3 live
lake-drawn contracts — 0/3 breaches on the live gate).
