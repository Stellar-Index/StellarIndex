---
title: SushiSwap V3 WASM audit
last_verified: 2026-09-30
status: string-checked — factory + 58 pools, 4 hashes, lake lineage to ledger 64,700,224
source: sushiswap_v3
backfill_safe: false
---

# SushiSwap V3 WASM audit

Audit log for the `sushiswap_v3` source. See [`README.md`](README.md)
for the procedure. The hashes below are the `sushiswap_v3` entries in
`internal/ops/chops/audited_wasm.json`, which `stellarindex-ops
wasm-drift` checks every gated contract against
([runbook](../runbooks/wasm-drift.md)).

## Status

**String-checked 2026-09-30.** Every WASM the factory and its 58 pools
have ever run — four hashes — carries every topic literal the decoder
routes on. `BackfillSafe` is **not** changed by this log; it stays
`false` in `internal/sources/external/registry.go` until a separate
change decides the flip.

No `stellarindex-ops wasm-history` walk was run. The lineage comes from
the certified lake's `stellar.contract_instance_changes`, which records
the executable on every instance change, so each hash's install ledger
is exact rather than sampled.

## Contracts under audit

| role | contract | how the set is known |
| --- | --- | --- |
| factory | `CD3KRKGDRVWPXVB3VXLUMQKMX6XZ6Q2H334IVZD4XXNAMKSRVQL5GLYF` | `sushiswap_v3` gated registry (`internal/pipeline/gated_registry.go`) |
| pool | 58 contracts | every `pool_created` event the factory has emitted, ledgers 61,487,379 – 64,116,662 |

Pools are deployed by the factory; upgrades are factory-driven
(`wasm_approved` on the factory, then per-pool `upgraded` /
`migrated`), so the pools move between hashes in cohorts.

## Method

All queries ran read-only on r1's ClickHouse lake on 2026-09-30.
Contract strkeys were converted to the lower-hex 32-byte contract id
locally.

```sql
-- 1. factory lineage
SELECT contract_hash, wasm_hash, min(ledger_seq), max(ledger_seq), count()
FROM stellar.contract_instance_changes
WHERE contract_hash IN (<factory hex>)
GROUP BY contract_hash, wasm_hash ORDER BY contract_hash, 3;

-- 2. pool enumeration (58 rows; pool_address decoded locally)
SELECT ledger_seq, data_xdr FROM stellar.contract_events
WHERE contract_id = 'CD3KRKGDRVWPXVB3VXLUMQKMX6XZ6Q2H334IVZD4XXNAMKSRVQL5GLYF'
  AND ledger_seq >= 61487379 AND topic_0_sym = 'pool_created'
ORDER BY ledger_seq;

-- 3. pool lineage: query 1 over the 58 pool ids

-- 4. per-hash string check, one statement per hash
SELECT '<hash>', ledger_seq, length(c),
       [position(c, 'swap') > 0, position(c, 'mint') > 0, ...]
FROM (SELECT ledger_seq, base64Decode(entry_xdr) c
      FROM stellar.ledger_entry_changes
      WHERE entry_type = 'contract_code'
        AND ledger_seq BETWEEN 40000000 AND 64700000
        AND key_xdr = <base64(0x00000007 || hash)>
      LIMIT 1);
```

Literals checked, per role, are the decoder's topic constants in
`internal/sources/sushiswap_v3/events.go`:

- factory: `pool_created`
- pool: `swap`, `mint`, `burn`, `collect`, `init`, `upgraded`, `migrated`

### Limits of the string check

- It is a substring `position()` over the `contract_code` ledger
  entry's bytes. A hit shows the literal occurs somewhere in the module
  (an export name, a data-section string), not that the contract
  publishes an event under it.
- Short literals match incidentally: `init` is inside `initialize`,
  and `swap`, `mint`, `burn` and `collect` are also pool method names.
  A hit on those four proves little beyond the method surface.
- A Soroban `Symbol` of up to 9 characters can be compiled into a
  packed integer rather than stored as a string, so a miss on a short
  literal would not prove absence either. There were no misses.
- The bytes were matched by `LedgerKey` hash, not re-hashed with
  SHA-256.

The runtime evidence is separate and stronger for the swap path:
`docs/protocols/sushiswap_v3.md` verified all 97,349 lake `swap`
events carry the same seven body fields across every pool version, and
the decoder reads them by name.

## Per-hash findings

| wasm_hash | role | contracts | first seen | last instance change | literals |
| --- | --- | --- | --- | --- | --- |
| `9f94c577ea24c2f71cbb0c964c9c9d321e02448d1fe946020afb18974b1c2602` | factory | 1 (18 instance changes) | 61,472,401 | 63,585,030 | `pool_created` present |
| `41ae735d2312f50975f5b7c70c3f71c2a0785113014dc24e474c3f3688e6224f` | pool | 3 | 61,487,379 | 61,594,998 | all 7 present |
| `48b28121451497952c1c35d58d4556f315a5b468ccde9fcd03f510aefa07c117` | pool | 54 | 61,594,973 | 62,898,525 | all 7 present |
| `003710b383f9da7d650a7f719a7be479110266427817ebbed61d924505fcd7c7` | pool | 58 (all) | 62,898,378 | 64,700,224 | all 7 present |

The factory has run one build for its whole observed life, so both of
its `wasm_approved` events approve pool code, not factory upgrades.

### The two `wasm_approved` events

`docs/protocols/sushiswap_v3.md` records factory `wasm_approved` at
ledgers 61,594,963 and 62,898,168. Their bodies were not decoded for
this log; tied by ledger order to the pool lineage:

| approval ledger | most plausible pool hash | why |
| --- | --- | --- |
| 61,594,963 | `48b28121…` | 10 ledgers later the 3 pools then live move `41ae735d…` → `48b28121…` (61,594,973 – 61,594,998); every pool created after it starts on `48b28121…` |
| 62,898,168 | `003710b3…` | the 54 pools on `48b28121…` move to `003710b3…` at 62,898,378 – 62,898,525; every pool created after it starts on `003710b3…` |

`41ae735d…` is the factory's install-time pool template: the first
three pools were created on it before any approval. Decoding the
`wasm_approved` body would turn "most plausible" into a proof.

## Pool lineage

Every pool, in creation order, with each hash's first instance-change
ledger:

| pool | hash @ first ledger | last instance change |
| --- | --- | --- |
| `CCR2CH4GQVCZHG7CHFVMNANCK45CU5DVKXZIIITDZQAU3CEJZ7RQH2MQ` | `41ae735d…` @ 61,487,379 → `48b28121…` @ 61,594,973 → `003710b3…` @ 62,898,378 | 64,700,221 |
| `CC22BLZAFTS7M6Z25HOMKDLV65PBP5CIHFER2OTZB5IRNL3YBWDXKDFF` | `41ae735d…` @ 61,525,514 → `48b28121…` @ 61,594,987 → `003710b3…` @ 62,898,381 | 64,699,572 |
| `CA75VVHLWSM7W6ULNQI7ZJYDFOMQCCPKIDDDHBAL5KOKHWWKWQ5S7MHO` | `41ae735d…` @ 61,525,536 → `48b28121…` @ 61,594,998 → `003710b3…` @ 62,898,383 | 64,698,605 |
| `CBBXZDNNIVCGGLLCH43KTH6WDZ5MFVYFQ5LTGD236A4A6OJMH2K4H6LA` | `48b28121…` @ 61,725,403 → `003710b3…` @ 62,898,386 | 64,539,126 |
| `CDAUN7SBYWRAVKJA453ZGLEP4LFCR25ANTI6EIHKTWSSTWHYJXLD3EXT` | `48b28121…` @ 61,725,448 → `003710b3…` @ 62,898,389 | 64,492,936 |
| `CDO6MTO4RYHFWWIG4DA3ZDHJ7GQMWA3BCZW4YTSBHEEDOJZV4BTJZB7M` | `48b28121…` @ 61,725,489 → `003710b3…` @ 62,898,391 | 64,532,771 |
| `CC22KKT4G3MSL5STSZDOC4KA5CTLCFTQX4ASASIGOE7Z3HI7HOY5F33H` | `48b28121…` @ 61,903,770 → `003710b3…` @ 62,898,393 | 64,601,806 |
| `CCOLSF35JVRGLONMJJMFYXUAEQ5IM4QOMYEWMXEFJEOQK3WVN4EJ3OUR` | `48b28121…` @ 61,903,819 → `003710b3…` @ 62,898,395 | 64,608,466 |
| `CAMUA6N6SLCMSIQRICXIQBRYYG3SCEPILS2HJTBXGMCOBCRWQUHAS2OJ` | `48b28121…` @ 61,903,837 → `003710b3…` @ 62,898,397 | 63,372,489 |
| `CD2JLIZ5TSF746SVPNU224CY3ZIXTEII37CC4DNCTSDRLK4NAXBCQZLB` | `48b28121…` @ 61,903,895 → `003710b3…` @ 62,898,399 | 64,594,798 |
| `CBV7CK2DDLODRLTWFBWE7CN5WKN5B6O26ERTULNQIWJQ7ZBTHTGQ5YFI` | `48b28121…` @ 61,903,924 → `003710b3…` @ 62,898,401 | 64,577,814 |
| `CDGIQQBPGXATIEXWTFN5O6J7LM5IMQLMVIQ47Q4H44VIMMOBZ4N6KRVZ` | `48b28121…` @ 61,903,939 → `003710b3…` @ 62,898,403 | 64,120,170 |
| `CCUVVQJNVI3UBXHZ6LDW2BTULOKMFZWVYLY26QTP62AWUQCDPVNZWEPJ` | `48b28121…` @ 61,903,977 → `003710b3…` @ 62,898,405 | 64,120,222 |
| `CBBHUYY3YE7AOCLHFFVTOFHPMV3WBANNDIQBJ3NCNLXC7BLHRAM5YD7M` | `48b28121…` @ 61,918,196 → `003710b3…` @ 62,898,408 | 64,668,194 |
| `CB5NNLVWQWN26VBOI576UVA4EGDGPKOHGNVTVYXREXAN2TO3XT6SCJL4` | `48b28121…` @ 61,918,241 → `003710b3…` @ 62,898,411 | 64,601,342 |
| `CBVKO35SAF2ZT75FCLCGLYQG3S6B32YZTOJ2G5F7M746UGBRAWZ5BNZ6` | `48b28121…` @ 61,918,259 → `003710b3…` @ 62,898,414 | 64,128,412 |
| `CAKWXQDEVVUF2ABUEM3M2G7QJGJNDZNNVXJZYG4Z4QP6K54QTWV4DW2S` | `48b28121…` @ 61,918,281 → `003710b3…` @ 62,898,417 | 64,698,082 |
| `CDVBYETOFG7UYJAD6CMOAQZXBHEK3PD5ZDZKWMWIY5OXIWATPX4VGMY2` | `48b28121…` @ 61,918,332 → `003710b3…` @ 62,898,420 | 64,573,444 |
| `CCQ4WKGF5PDJZ3PTDQBOOWD6HE67DEB3QHOGLF2VAWO2RBCGDT2DPDTI` | `48b28121…` @ 61,918,446 → `003710b3…` @ 62,898,426 | 64,602,059 |
| `CBVZXBW5E5Q72J5ETZT4ZJLNS6GTLQC5BLUOERJNFQMMTKQCVY4N5YSX` | `48b28121…` @ 61,918,465 → `003710b3…` @ 62,898,428 | 64,578,720 |
| `CA5MIPAAG3UULVAHK7U3U6VBBM52YIHMCZOOSHNTPUSLYR7NKNHVD6WK` | `48b28121…` @ 61,918,509 → `003710b3…` @ 62,898,434 | 64,699,939 |
| `CCAQO6MKWCD463JZEIY5JUSU425H77LNYM3R6ZNQPO4KBU5C3SAEJDDB` | `48b28121…` @ 61,918,544 → `003710b3…` @ 62,898,437 | 64,128,489 |
| `CAPT5THGW7WOCX47TICCB5JZZK4Y24CHQIBSM57Y472WFFV6FGTRKJQD` | `48b28121…` @ 61,918,562 → `003710b3…` @ 62,898,439 | 64,655,307 |
| `CAUBW4ARD42U2UEIA7GDUB5LNKTRTVYJHXKL3CV27YZRDFADDGKLZWFD` | `48b28121…` @ 61,918,580 → `003710b3…` @ 62,898,442 | 64,653,533 |
| `CBGBQUEHEDIMA2P5JVHROPYVENY7Y3EA3DDYEMIW2HICRKFW7YDI635B` | `48b28121…` @ 61,918,601 → `003710b3…` @ 62,898,445 | 64,541,889 |
| `CAPUAZDFH4VBQTC7PYL7UM2KSXER2ZY3D462WW6DCE2HGUSO646S4F2X` | `48b28121…` @ 61,918,648 → `003710b3…` @ 62,898,447 | 63,461,750 |
| `CAWWOFOEGWPPNP6QKVHTJYB7UHRXC6W6EAFMUPGHMJL7K46E6UCOSNDM` | `48b28121…` @ 61,918,664 → `003710b3…` @ 62,898,449 | 64,650,969 |
| `CCMPJ2PZDNG7DCQWU6YHF56U3QULE3JGMUVEZWBIWQOYHU6Z6QLEOEJQ` | `48b28121…` @ 61,918,690 → `003710b3…` @ 62,898,451 | 64,437,305 |
| `CD6BHNV26Z7FOUU7VCFMYPRIB5JOG7724R42O5EJ4KGQXK3USO4NYOLS` | `48b28121…` @ 61,918,698 → `003710b3…` @ 62,898,453 | 64,662,639 |
| `CA5R5L7QE7WC2M4YAPSBITV7M2R6LX5366UURH3REHQMOJV6R5QWTH2K` | `48b28121…` @ 61,918,847 → `003710b3…` @ 62,898,455 | 64,654,412 |
| `CAXJ2FDV6S3L46EFEFRXUBLQ5U5CZLZOG35RPCJRNQVLM5MH2HCK5I7J` | `48b28121…` @ 61,918,865 → `003710b3…` @ 62,898,458 | 64,699,939 |
| `CDEXGN6YJXIPSMJZPU4NTJH6IVD47ZOG6GNTTMYK4X3U4JKLKD34F4XT` | `48b28121…` @ 61,919,010 → `003710b3…` @ 62,898,463 | 64,244,164 |
| `CCWJAQURY64U4VWI5GDLF42SH4RWFJZTYXO5BGA7SLNLVBGGV2L4EBIP` | `48b28121…` @ 61,919,041 → `003710b3…` @ 62,898,465 | 64,650,559 |
| `CAFLJXGUAURAMBA3AIHC7ZJOAQKGZ7WEFFGMH5XRC35IMNU7PWIBXVTP` | `48b28121…` @ 61,919,062 → `003710b3…` @ 62,898,467 | 64,680,127 |
| `CABMZD6BYKKLHRJNS5MURYOBX77NPAH767AI7EVFGWV3WZV55QFN5YNE` | `48b28121…` @ 61,919,082 → `003710b3…` @ 62,898,469 | 64,654,412 |
| `CCT74MCKWWPKHK5T7LXBPXK7AAPZIH35HJZY5CRZGVEJ4UUCMFQXBNVH` | `48b28121…` @ 61,919,119 → `003710b3…` @ 62,898,472 | 64,653,527 |
| `CCI2P2I4MWU3J2RQ6YKJXQN46CB42LWAFCUYNWY2J2JVHZBIDR74TLBG` | `48b28121…` @ 61,919,136 → `003710b3…` @ 62,898,478 | 64,650,985 |
| `CCKC4L6LI5ZNFKJCE7TVOEGGNJQPVHCCRJWMDYWEBL26SY62INCJLZMP` | `48b28121…` @ 61,919,169 → `003710b3…` @ 62,898,480 | 64,654,401 |
| `CCF7MYFIILNDCDZ37QGYJQRYW455MPHA5PJIQ5D3KB3KEYLZA2WB26S2` | `48b28121…` @ 61,919,188 → `003710b3…` @ 62,898,483 | 64,574,456 |
| `CCFWRAC3MB7JSPKAWSWT3MCRHVQECG3SU2JAMNAL3K2UHBQIYHBSR5LY` | `48b28121…` @ 61,919,228 → `003710b3…` @ 62,898,489 | 64,654,391 |
| `CA6LYAEDN7XHOKD5TNRFBM3IDFD22VRVVXTGPGK77FZFC7X2OYUQ7BAZ` | `48b28121…` @ 61,919,353 → `003710b3…` @ 62,898,492 | 62,898,492 |
| `CBTZO2GHYS23PW4QSHRNT66X62YZQJU7X3ZCZOKJTNEXKLR4WBYV54D7` | `48b28121…` @ 61,919,373 → `003710b3…` @ 62,898,495 | 62,898,495 |
| `CBQIPFKHCXBLXJKSFSZJJLODGZDA6C33D5TUL3VAPXM25LH2YKNAROC2` | `48b28121…` @ 61,919,417 → `003710b3…` @ 62,898,498 | 64,683,066 |
| `CDDPO65MGM3HYYMFJJB7Y7NCRV2DAJ7YAFZYQQRMBLCR3WHFIPAZQCTS` | `48b28121…` @ 61,919,436 → `003710b3…` @ 62,898,501 | 64,542,429 |
| `CDSOL7SBO2ZEASXAFZNDXGJUMIM7YOUBHQOXCCVQEKKYACIR43ZBH6MZ` | `48b28121…` @ 61,995,405 → `003710b3…` @ 62,898,503 | 64,601,806 |
| `CALM7JTAJC7AJ7ZGTQKXZNNILJUCD2AZNN7QA7FVM3YYIJBCJGUABEDH` | `48b28121…` @ 62,144,875 → `003710b3…` @ 62,898,505 | 62,898,505 |
| `CAWN3BM2ADBMA4CQZLIHTBXA3BQHV4VAPK42LWT5ONAKZW6PH2BBCKLS` | `48b28121…` @ 62,342,709 → `003710b3…` @ 62,898,508 | 62,898,508 |
| `CAOGXY6DW2KWUVOWCGGPLW7MIJNP7XCMXY736LNLOKYEQA3CBKXVIDEA` | `48b28121…` @ 62,342,715 → `003710b3…` @ 62,898,511 | 62,898,511 |
| `CCXRRORTOXXP53HEKJ6RCG7CDRWZAJHIS4N7PDL32PUNMNN7VWPJVQWS` | `48b28121…` @ 62,343,134 → `003710b3…` @ 62,898,513 | 64,675,654 |
| `CCRKQ2RHBWB5ZCHOSBSYEC2QNVSU3MGVUF56BWWKJMJIJ3ZF2A6W7KEC` | `48b28121…` @ 62,343,147 → `003710b3…` @ 62,898,515 | 64,225,184 |
| `CBOLCGXDSRU22SLUIJMFCJBOOURQLZHX5ZFIFJBYK2VAB3AEK5M2T5GM` | `48b28121…` @ 62,653,278 → `003710b3…` @ 62,898,518 | 62,898,518 |
| `CDMJRRH5MAJLB7T5SQAWQOOL7UJ3BABGTURSVKIE6AEDSXESDDJIBVCT` | `48b28121…` @ 62,653,663 → `003710b3…` @ 62,898,521 | 64,700,224 |
| `CD7ZJUQEJODTJXWJMDJRV7UHCANOBX3FC6KXJV3DMIFX3JXUWMF3U3T5` | `48b28121…` @ 62,669,385 → `003710b3…` @ 62,898,523 | 64,700,197 |
| `CCLXJ4F5STIZ7WWC5D2J57VCLLFIQMWUI5PSYJORPZTAUUQTS2XFU5ZK` | `48b28121…` @ 62,785,922 → `003710b3…` @ 62,898,525 | 64,486,078 |
| `CACU7KU33MFWMOP334RNQT7CZV3M7DNDAAXL5O3I4ATQRZCMXFJI4RMZ` | `003710b3…` @ 62,952,420 | 62,952,436 |
| `CBRKPTX4TWYEVGDTTVLSBVN6RKTFC2PKD6F5ZRSGBHQE7GC2UYMALTXY` | `003710b3…` @ 63,147,255 | 63,147,255 |
| `CDNHCFJ6LGPV4OWZL2SAQPFQHAUFFVMWGBZFVPA5Z5HTL4A6RWEZYNM4` | `003710b3…` @ 63,434,881 | 64,660,573 |
| `CBVHBZSZOS6KRDJ4D44FU2YLIENOVSSLM3UGKW6XQMVIFUAMWIWCVH2U` | `003710b3…` @ 64,116,662 | 64,690,294 |

Every pool is on `003710b3…` at its last instance change.

## Decision

The four hashes above are the audited set for `sushiswap_v3` in
`audited_wasm.json`. `BackfillSafe` is unchanged (`false`).

Re-audit trigger: `wasm-drift` reports a `sushiswap_v3` contract on a
hash not listed here — a new factory approval, a pool upgrade, or a
factory upgrade.

## Out of scope

- Running `wasm-drift` on r1 or putting it on a timer.
- A `wasm-history` walk of the pools: the lake's instance-change table
  is the evidence here.

## References

- Procedure: [`README.md`](README.md)
- Decoder: `internal/sources/sushiswap_v3/{events,decode}.go`
- Protocol verification: [`../../protocols/sushiswap_v3.md`](../../protocols/sushiswap_v3.md)
- Drift check: `internal/ops/chops/wasm_drift.go`, manifest `internal/ops/chops/audited_wasm.json`
