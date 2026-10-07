---
title: Blend WASM-history audit
last_verified: 2026-05-03
status: Phases 1–4 complete. BackfillSafe=true. Also covers Comet (Blend backstop is the only mainnet Comet deployment).
source: blend
backfill_safe: true
also_covers: comet
---

# Blend WASM audit

Audit log for the `blend` source's `BackfillSafe` flag. See
[`README.md`](README.md) for the full procedure.

> **2026-05-03 update: Comet's v2 audit is folded in here.** The only
> mainnet Comet pool is Blend's Backstop V2 contract (`CAQQR5SW...`, WASM
> `c1f4502a757e25c6...`); Comet is a Balancer-v1-style weighted-AMM library used
> as Blend's backstop module. Our decoder (`internal/sources/comet/`) classifies
> trades as `Trade.Source = "comet"`; its WASM audit is the Backstop V2 row in
> the Phase 2 results table below. Protocol framing: [`comet.md`](comet.md).
>
> **2026-05-02 update: audit complete.** Phase 2's wide-net wasm-history walk
> on r1 finished after 5h4m39s, 8 parallel workers, ledgers [50,457,424,
> 62,249,727] (verified-clean range per
> [r1-deployment-state.md §3a](../r1-deployment-state.md)). The contract list
> held all 11 Blend contracts (9 pools + backstop + factory) plus 528 other
> Soroban contracts. **Zero mid-life upgrades observed across all 11 Blend
> contracts**: each has exactly one `(wasm_hash, ledger_range)` entry matching
> its Phase 1 current-state hash. With Phase 3's disassembly of all three WASMs
> (decoder symbols present), every WASM that ran for any Blend contract in our
> backfill window is audited. `BackfillSafe` flipped `false` -> `true` in
> `internal/sources/external/registry.go`.
>
> **2026-05-01 update.** All 11 contracts cross-checked against Soroban-RPC
> current-state: **9 lending pools + 1 backstop module + 1 pool factory**. WASM
> bytes preserved for all three roles, disassembly confirms the API match. See
> [`r1-walk-2026-05-01.md`](r1-walk-2026-05-01.md) §Blend.

## Status

**Phase 1 complete (2026-04-30).** Pool enumeration via stellar.expert option 3
landed all nine pool addresses and deploy timestamps. Current pool WASM
`a41fc53d6753b6c04eb15b021c55052366a4c8e0e21bc72700f461264ec1350e` fetched via
`stellar contract fetch --network mainnet` and verified against the decoder's
expected event topics + AuctionData field names. `BackfillSafe` stayed `false`
until **Phase 2** (per-pool `wasm-history` walk on r1): current WASMs matching
cannot rule out a pool upgraded A->B mid-history.

Dispatch matches Blend by topic: every pool emits the same
`("new_auction", ...)`, `("fill_auction", ...)`, `("delete_auction", ...)`
shapes; the WASM-bytes audit lives at pool-instance level, not the Pool Factory.

## Contracts under audit

Verified 2026-04-22 via stellar.expert + the `blend-contracts-v2` deploy manifest:

| Role | Contract | WASM hash (v2) |
| --- | --- | --- |
| Pool Factory V2 | `CDSYOAVXFY7SM5S64IZPPPYB4GVGGLMQVFREPSQQEZVIWXX5R23G4QSU` | `31328050548831f63d2b72e37bcfd0bb7371b7907135755dbe09ed434d755ca9` |
| Backstop V2 | `CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7` | `c1f4502a757e25c611f5a159bc1ab0eef64085adac6c68123dca66e87faffbc2` |

Pools are deployed at runtime by the factory's `deploy()`, whose only event is:

```text
topics: [Symbol("deploy")]
body:   pool_address: Address
```

Walking these events from the factory's deploy ledger (L51,499,546) lists every
Blend pool on mainnet. As of 2026-04-30 the factory emitted only **9 events**
(per stellar.expert), so <=9 pools.

## Phase 1 results — pool addresses (executed 2026-04-30)

Via stellar.expert events API (`/explorer/public/contract/<factory>/events`):
9 lifetime events, all `Symbol("deploy")`, body an `Address` SCVal holding the
pool. Decoded with `scripts/dev/decode-scval`. Oldest first:

| # | Pool address | Deploy ts (UTC) | Initiator |
| --- | --- | --- | --- |
| 1 | `CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD` | 2025-04-14 17:46:46 | `GAX2VVWVHU5YQY5J3NJBXKHI3FFKZN54BE6GRJCWSIKSBZTQWJJNJMPC` |
| 2 | `CBNR7PYFY775UG7W37B4OJG2OBBUKLFW6VIBHFDKKLR2HECPRMRZMDK3` | 2025-04-15 18:42:52 | `GBCAS7XIGDRZY4BMABJMGGW7J3YTITRRV5BTEMFQE5ZZSSVWHHX2ZSS4` |
| 3 | `CCCCIQSDILITHMM7PBSLVDT5MISSY7R26MNZXCX4H7J5JQ5FPIYOGYFS` | 2025-04-17 14:35:16 | `GBCAS7XIGDRZY4BMABJMGGW7J3YTITRRV5BTEMFQE5ZZSSVWHHX2ZSS4` |
| 4 | `CB4OFHAY2TAEYUVPOJS36S657C6NYMSIFUNCCA5AHYT46Y5XUID3O2ED` | 2025-05-01 15:04:09 | `GBIWJGAOSFC4KUPHXM573TKTWHMI7VW7D4GCHYZYH243Q6HVBV7ORBIT` |
| 5 | `CAE7QVOMBLZ53CDRGK3UNRRHG5EZ5NQA7HHTFASEMYBWHG6MDFZTYHXC` | 2025-05-01 21:54:53 | `GBIWJGAOSFC4KUPHXM573TKTWHMI7VW7D4GCHYZYH243Q6HVBV7ORBIT` |
| 6 | `CBYOBT7ZCCLQCBUYYIABZLSEGDPEUWXCUXQTZYOG3YBDR7U357D5ZIRF` | 2025-07-13 22:39:10 | `GCCI7K6QU6FVVIXWSLKRPTBKJCFBLEJKPTZMP27A2KL37N4ZL3OCM3GI` |
| 7 | `CALRF5I2OCJCU577R6MZBCY5IIXNMAAG6PNMN7GUKEYIXBJCJN2FJRVI` | 2025-11-22 02:11:29 | `GDH3FRHOOWXYXEASH43N2VOVFOPJSVJF3EQFSLBLJYFPHOUAF4N4AETH` |
| 8 | `CADR6Q2UOCDJAGXMAB2E6SRT35STLZ2IGLZUCXJQG7TC2LNKCU5RTQVY` | 2025-11-25 04:49:43 | `GDH3FRHOOWXYXEASH43N2VOVFOPJSVJF3EQFSLBLJYFPHOUAF4N4AETH` |
| 9 | `CDMAVJPFXPADND3YRL4BSM3AKZWCTFMX27GLLXCML3PD62HEQS5FPVAI` | 2025-11-25 04:53:09 | `GDH3FRHOOWXYXEASH43N2VOVFOPJSVJF3EQFSLBLJYFPHOUAF4N4AETH` |

The factory was deployed 2025-04-14 17:42:07 UTC, 4 minutes before the first pool.

## Phase 3 partial — current WASM verification (executed 2026-04-30)

Current WASM hash per pool via `/explorer/public/contract/<pool>`. **All nine share:**

```
a41fc53d6753b6c04eb15b021c55052366a4c8e0e21bc72700f461264ec1350e
```

WASM bytes downloaded via
`stellar contract fetch --network mainnet --wasm-hash a41fc53d6753b6c04eb15b021c55052366a4c8e0e21bc72700f461264ec1350e`
(57,328 bytes). Saved as
[`evidence/blend/pool-a41fc53d6753b6c0.wasm`](evidence/blend/pool-a41fc53d6753b6c0.wasm)
against RPC TTL eviction; `stellar contract info interface` dump at
[`evidence/blend/pool-a41fc53d6753b6c0.interface.txt`](evidence/blend/pool-a41fc53d6753b6c0.interface.txt).

Decoder-compatibility checks (Phase 3 step 3 below):

- ✅ Event topics: `strings` finds `new_auction`, `fill_auction`,
  `delete_auction` (all three the decoder switches on).
- ✅ AuctionData field names: `bid`, `lot`, `block` — all three
  match `internal/sources/blend/auction_data.go`'s constants
  (`auctionDataKeyBid`, `auctionDataKeyLot`, `auctionDataKeyBlock`).
- ✅ `stellar contract info interface --wasm` shows the canonical
  Blend pool surface (`submit`, `flash_loan`, `gulp_emissions`,
  `set_status`, `get_reserve_list`, etc.).
- ⚠️  stellar.expert validation is `unverified` for the pool WASM (`verified`
  against `blend-contracts-v2` for the factory only). Non-blocking; a Phase-3
  step 1 source-build diff against `blend-contracts-v2/pool/` would close it.

**Open item: WASM history.** Current WASM matching cannot rule out an earlier
upgrade; Phase 2 (the `wasm-history` walk on r1) was required (done below).

## Phase 2 results — per-pool wasm-history walk (executed 2026-05-02)

Wide-net walk on r1, all 11 Blend contracts within a 539-contract watch list:

- **Range**: ledgers [50,457,424, 62,249,727], the galexie-archive
  verified-clean range per [`r1-deployment-state.md §3a`](../r1-deployment-state.md);
  the factory deployed in late 2024 (~ledger 51M), so the lower bound covers all Blend history.
- **Workers**: 8 parallel chunks (`-parallel 8`).
- **Checkpointing**: `-checkpoint-dir /tmp/walk-checkpoint` —
  per-worker JSONL transition logs (the merge
  tool from commit 18db4123d wasn't needed).
- **Runtime**: 5h4m39s total.

**Per-contract findings:**

| # | Contract | Role | Ranges | Hash | Walk-observed range |
|---|---|---|---|---|---|
| 1 | `CADR6Q2UOCDJAGXMAB2E6SRT35STLZ2IGLZUCXJQG7TC2LNKCU5RTQVY` | pool | 0 | `a41fc53d…` (per Phase 1 RPC) | deployed pre-50,457,424; no upgrades observed |
| 2 | `CAE7QVOMBLZ53CDRGK3UNRRHG5EZ5NQA7HHTFASEMYBWHG6MDFZTYHXC` | pool | 1 | `a41fc53d…` | [56,875,363, 57,827,613] |
| 3 | `CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD` | pool | 1 | `a41fc53d…` | [56,615,475, 57,827,613] |
| 4 | `CALRF5I2OCJCU577R6MZBCY5IIXNMAAG6PNMN7GUKEYIXBJCJN2FJRVI` | pool | 0 | `a41fc53d…` (per Phase 1 RPC) | deployed pre-50,457,424; no upgrades observed |
| 5 | `CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7` | backstop | 1 | `c1f4502a…` | [56,615,429, 57,827,613] |
| 6 | `CB4OFHAY2TAEYUVPOJS36S657C6NYMSIFUNCCA5AHYT46Y5XUID3O2ED` | pool | 1 | `a41fc53d…` | [56,871,010, 57,827,613] |
| 7 | `CBNR7PYFY775UG7W37B4OJG2OBBUKLFW6VIBHFDKKLR2HECPRMRZMDK3` | pool | 1 | `a41fc53d…` | [56,630,960, 57,827,613] |
| 8 | `CBYOBT7ZCCLQCBUYYIABZLSEGDPEUWXCUXQTZYOG3YBDR7U357D5ZIRF` | pool | 1 | `a41fc53d…` | [57,992,199, 59,301,651] |
| 9 | `CCCCIQSDILITHMM7PBSLVDT5MISSY7R26MNZXCX4H7J5JQ5FPIYOGYFS` | pool | 1 | `a41fc53d…` | [56,658,268, 57,827,613] |
| 10 | `CDMAVJPFXPADND3YRL4BSM3AKZWCTFMX27GLLXCML3PD62HEQS5FPVAI` | pool | 0 | `a41fc53d…` (per Phase 1 RPC) | deployed pre-50,457,424; no upgrades observed |
| 11 | `CDSYOAVXFY7SM5S64IZPPPYB4GVGGLMQVFREPSQQEZVIWXX5R23G4QSU` | factory | 1 | `31328050…` | [56,615,428, 57,827,613] |

"0" ranges: zero `update_current_contract_wasm` events in the window, so the
contract was deployed *before* ledger 50,457,424 and never upgraded. Phase 1's
Soroban-RPC current-state query (2026-04-30, see
[evidence/blend/](evidence/blend/)) confirms those three are on `a41fc53d…`.

"1" range: a single `(wasm_hash, from_ledger, to_ledger)` entry; from_ledger is
the first observation of the instance row, to_ledger is the worker's chunk-end
(not a real transition end). **No mid-life upgrades.**

**Three unique WASM hashes** observed across all 11 Blend
contracts:

- `a41fc53d6753b6c04eb15b021c55052366a4c8e0e21bc72700f461264ec1350e` — pool WASM (9 contracts)
- `c1f4502a757e25c611f5a159bc1ab0eef64085adac6c68123dca66e87faffbc2` — backstop WASM (1 contract)
- `31328050548831f63d2b72e37bcfd0bb7371b7907135755dbe09ed434d755ca9` — factory WASM (1 contract)

All three match the Phase 1 stellar.expert + Soroban-RPC results (2026-04-30)
and were disassembled in Phase 3 with decoder-expected topics + fields present.

**Filtered evidence saved at**
[`evidence/blend/phase2-2026-05-02/wasm-history-blend.json`](evidence/blend/phase2-2026-05-02/wasm-history-blend.json).
The full 540-contract output (`/tmp/wide-net-walk-3.json`) and 200KB
per-worker JSONL checkpoints (`/tmp/walk-checkpoint/`) are kept on r1.

## Audit plan (the canonical procedure)

### Phase 1 — Enumerate pool contracts

The factory has no enumeration view (only `deploy()`), so pool addresses come
from its emitted events. Options:

1. **Walk Pool Factory `deploy` events on r1** (preferred). `stellarindex-ops wasm-history`
   watches `update_current_contract_wasm`, not generic event publishes (every
   pool deploy creates a `LedgerEntryChange`), so this needs a small tool (or
   extension to `extract-wasm-from-galexie`) walking LCM and emitting
   `(ledger, pool_address)` for each factory `("deploy")` event.
2. **`stellar events`** on a public RPC. Retention ~7 days; the factory is live
   since 2025-04-14, so only last-week events.
3. **Manual lookup via Blend Capital docs / stellar.expert**: 9 lifetime events
   give the pool list directly. Fastest; least scalable for re-audits.

**Recommended for v1 audit**: option 1, emitting a `pool_address` list. Then audit proceeds as phoenix / aquarius.

### Phase 2 — Per-pool wasm-history walk

For each pool address from Phase 1:

```sh
mkdir -p /var/log/wasm-audit/blend-checkpoint
stellarindex-ops wasm-history \
  -config /etc/stellarindex.toml \
  -from 51499546 -to <r1-tip> -parallel 8 \
  -checkpoint-dir /var/log/wasm-audit/blend-checkpoint \
  -contracts <pool-1>,<pool-2>,...
```

Captures every `update_current_contract_wasm` event per pool.

### Phase 3 — Per-WASM-hash review

For each unique WASM hash discovered in Phase 2:

1. `stellar contract fetch --wasm-hash <h>` from public RPC; if TTL-evicted,
   `stellarindex-ops extract-wasm-from-galexie` against r1.
2. `stellar contract info interface --wasm <h>.wasm`, compared against the
   canonical interface (most-recent pool's WASM).
3. `strings <h>.wasm | grep -E "new_auction|fill_auction|delete_auction|bid|lot|block"` — confirm the auction event-topic strings + AuctionData field names are present.
4. Compare against the internal/sources/blend decoder's expectations (`Decoder expectations` below).

Document findings in the per-hash table at the bottom.

### Phase 4 — Decision + flip

If every pool WASM is decoder-compatible, flip
`Registry["blend"].BackfillSafe = true` in
`internal/sources/external/registry.go`, update
`framework_test.go` to move blend from `wantUnsafe` to `wantSafe`,
update CHANGELOG.md, and set this doc's `status: ratified`.

## Decoder expectations

From `internal/sources/blend/{events,decode,auction_data}.go` at HEAD 2026-04-30; verified against
`.discovery-repos/blend-contracts/pool/src/events.rs` (commit
`c19abee5b9be4f49e0cda9057e87d343e5dcc095`).

### Topic structure (auction events)

Every auction event has 3 topics:

```text
topic[0] = Symbol("new_auction" | "fill_auction" | "delete_auction")
topic[1] = u32(auction_type)           // 0=UserLiquidation, 1=BadDebt, 2=Interest
topic[2] = Address(user)               // G or C strkey
```

Classification is byte-equal against pre-encoded `ScvSymbol` constants.

### `new_auction` body

```text
Vec(
    percent: u32,
    auction_data: AuctionData,
)
```

### `fill_auction` body

```text
Vec(
    filler:               Address,
    fill_percent:         i128,
    filled_auction_data:  AuctionData,
)
```

### `delete_auction` body

Empty (`()` — Soroban unit).

### `AuctionData` shape

`pool/src/auctions/auction.rs::AuctionData` is a `#[contracttype]` struct, emitted as `ScvMap` with sorted-by-symbol keys:

```text
ScvMap{
  "bid":   Map<Address, i128>,  // assets the filler spends
  "block": u32,                 // auction-start block
  "lot":   Map<Address, i128>,  // assets the filler receives
}
```

Decoder extracts by name — resilient to field reordering.

### Auction type discriminants

Verified against `pool/src/auctions/auction.rs`:

| `auction_type` | Name | Bid asset | Lot asset |
| --- | --- | --- | --- |
| `0` | UserLiquidation | dTokens | bTokens |
| `1` | BadDebt | dTokens | Underlying (backstop) |
| `2` | Interest | Underlying (backstop) | Underlying |

Decoder rejects values outside this set with `ErrUnknownAuctionType`.

## Failure modes specific to Blend

1. **Topic[0] symbol change** (`"new_auction"` -> other): silently drops every event of that variant.
2. **Topic[1] type change** (`u32` -> other): `ErrMalformedPayload`, fail-loud, every event under that WASM dropped.
3. **AuctionData field rename**: `bid` / `lot` / `block` looked up by name; returns `auction_data missing "bid"`, fail-loud per event.
4. **Inner Map<Address, i128> shape change** (e.g. Vec<(Address, i128)>): `scval.AsMap` errors on non-Map.
5. **i128 type drift**: `scval.AsAmountFromI128` is strict; type-tag change errors per amount.
6. **New auction_type value**: `ErrUnknownAuctionType`, prompting an audit.

## WASM timeline

Three unique hashes across all 11 contracts. **Zero mid-life upgrades observed
in the walked range** [50,457,424, 62,249,727].

| Hash (first 16) | Role | Contracts | First observation | Last observation | Upgrade chain |
| --- | --- | --- | --- | --- | --- |
| `a41fc53d6753b6c0` | Pool | 9 (all lending pools) | 56,615,475 (earliest pool) | 59,301,651 (latest chunk end) | None — single hash |
| `c1f4502a757e25c6` | Backstop V2 | 1 (`CAQQR5SW…`) | 56,615,429 | 57,827,613 | None — single hash |
| `31328050548831f6` | Pool Factory V2 | 1 (`CDSYOAVX…`) | 56,615,428 | 57,827,613 | None — single hash |

WASM bytes: `evidence/r1-walk-2026-05-01/wasm-bytes/{a41fc53d…,c1f4502a…,31328050…}.wasm`
on r1, SHA-256-verified against on-chain hashes; disassembly (`.wat` + `strings`)
under `evidence/r1-walk-2026-05-01/disasm/`.

## Per-hash review findings

| variant | hash (first 16) | active range | reviewer | finding |
| --- | --- | --- | --- | --- |
| Pool | `a41fc53d6753b6c0` | from each pool's first observation through r1 tip; no upgrades | maintainer@2026-04-30 | Decoder symbols present (`new_auction`, `fill_auction`, `delete_auction`); `AuctionData` field names `bid`/`lot`/`block` present; matches `internal/sources/blend` decoder expectations. |
| Backstop V2 (Comet pool) | `c1f4502a757e25c6` | from L56,615,429 through r1 tip; no upgrades | maintainer@2026-05-02 | Comet decoder symbols present (`POOL`, `swap`, `caller`, `token_in`, `token_out`, `token_amount_in`, `token_amount_out`); SEP-41 LP-share surface (`Allowance`, `Balance`, `SwapFee`); matches `internal/sources/comet/{events,decode}.go`. |
| Pool Factory V2 | `31328050548831f6` | from L56,615,428 through r1 tip; no upgrades | maintainer@2026-05-02 | `deploy` event symbol present; factory enumeration produced exactly 9 pool addresses, all on `a41fc53d…`. |

### `c1f4502a757e25c6` — Backstop V2 (the Comet pool)

The WASM Blend deploys as its single backstop module; on mainnet the only
contract running Comet code.

**Disassembly evidence:**

1. **Contract interface:** `swap_exact_amount_in`,
   `swap_exact_amount_out`, `join_pool`, `exit_pool`, plus the
   SEP-41 LP-share surface (`Allowance`, `Balance`, `SwapFee`).
   Matches `comet-contracts/src/c_pool/call_logic/pool.rs` the
   decoder was verified against (2026-04-23).
2. **Binary strings:** all five Comet body field names
   (`caller`, `token_in`, `token_out`, `token_amount_in`,
   `token_amount_out`) present in the data section, plus the
   topic symbols `POOL` and `swap`.
3. **Decoder verdict:** matches current Comet decoder; the topic-based
   dispatcher routes every swap event from `CAS3FL6T...` correctly. (Backstop
   V2 `CAQQR5SW...` and the Comet pool token `CAS3FL6T...` are distinct
   contract IDs; data-of-record is `CAQQR5SW...`. The Comet pool is
   instantiated by Backstop V2 at bootstrap, same `c1f4502a…` WASM lineage.)

## Decision

**`BackfillSafe: true`** — flipped in
`internal/sources/external/registry.go` for both `blend` and
`comet` source rows.

Rationale:

- All 11 contracts on the same three WASMs; **zero mid-life upgrades observed**
  across [50,457,424, 62,249,727].
- All three WASM bytes preserved, SHA-256-verified, disassembled; decoder
  symbols/fields present.
- Backstop V2's WASM is the Comet pool WASM on mainnet; one audit covers both rows.
- Live ingest health: 0 `ErrMalformedPayload` rate spikes against
  either `blend` or `comet` source.

## References

- Procedure: [`README.md`](README.md)
- Decoder source: `internal/sources/blend/{events,decode,auction_data}.go`
- Schema-evolution stance: [`../../architecture/ingest-pipeline.md#contract-schema-evolution`](../../architecture/ingest-pipeline.md#contract-schema-evolution)
- Backfill gate: `internal/sources/external/registry.go` —
  `Registry["blend"].BackfillSafe`
- Upstream contracts: <https://github.com/blend-capital/blend-contracts-v2>
- Local checkout: `.discovery-repos/blend-contracts/`

## Backstop historical replay decision (2026-07-04, BACKLOG #10a)

`blend_backstop` went live 2026-06-15 with its cursor fast-forwarded to tip,
leaving ~52k historical events in the lake unprojected. Evidence the replay is
safe without further confirmation:

- The Phase-2 walk above covered the backstop contract explicitly
  (11 contracts = 9 pools + backstop + factory; 3 unique WASMs; no
  mid-life upgrades observed over [50457424, 62249727]).
- The decoder's 10 event schemas were lake-reverse-engineered and golden-tested
  (2026-06-15); 2,474 rows of live capture since prove the current-WASM schema.
- Catch-up is `projector-replay` (idempotent, ADR-0032), NOT `backfill`:
  BackfillSafe doesn't gate it, and blend_backstop deliberately has no
  external.Registry row (projected lending surface, not a VWAP venue).

Operator command (queued in the serialized r1 heavy-job chain):

    stellarindex-ops projector-replay -source blend_backstop -from 55000000

(55.0M ~ first backstop activity; empty ranges no-op.)

## V1 pool WASM `baf978f10efdbcd8` — auction-family shapes (2026-10-05)

Hash `baf978f10efdbcd85747868bef8832845ea6809f7643b67a4ac0cd669327fc2c`
runs the four V1-factory pools (`CDVQVKOY…`, `CBP7NO6F…`, `CDE65QK2…`,
`CAQF5KNO…`); `stellar.contract_instance_changes` shows it for each pool's
whole life (never upgraded). The ADR-0033 verdict's 1,175
"undecodable-but-matched" `blend` events are all from these pools: 737
`fill_auction`, 435 `new_auction`, 3 `bad_debt`, ledgers 51,612,222 to
62,625,124. The V2 decoders rejected them; all decode now, samples pinned in
`test/fixtures/blend/v1-pool-auctions/`.

| event | V1 topics | V1 body | decoded to |
| --- | --- | --- | --- |
| `new_auction` | `[Symbol, u32(auction_type)]` | `AuctionData` Map `{bid, block, lot}` | `blend_auctions` `new`: `auction_type` from topic[1] (BadDebt or Interest only), `user` = V1 backstop `CAO3AGAM…`, `percent` = 100 |
| `fill_auction` | `[Symbol, Address(user), u32(auction_type)]` | `(filler: Address, fill_percent: i128)` | `blend_auctions` `fill`; `bid`/`lot`/`block` NULL (absent on the wire) |
| `bad_debt` | `[Symbol, Address(user)]` | `(asset: Address, d_tokens: i128)` | `blend_emissions` `bad_debt` |

V1 announces user liquidations with `new_liquidation_auction`, so `new_auction`
carries only BadDebt (3 events) and Interest (432 events); a UserLiquidation
there is rejected. V1 keys those auctions on the pool's backstop: all 435 V1
BadDebt and Interest `fill_auction` events name `CAO3AGAM…`
(`MainnetBackstopV1`) as user. Percent 100 comes from the bad-debt rows: the
`bad_debt` `d_tokens` sum exactly to the next `new_auction` bid (ledgers
52,428,304→52,428,305 and 55,570,394/5→55,570,396); V2 also emits 100 for all
3,678 BadDebt and Interest `new_auction` events in ledgers 57.0M to 62.7M.
Dispatch is by topic arity for `new_auction` and `bad_debt` (2 topics means V1),
and by the type of `fill_auction` topic[1] (Address means V1, u32 means V2).

## Backstop V1 WASM — unaudited, replays refused (2026-10-05)

The decision above covers Backstop V2 only (Phase-2 walk saw one backstop WASM,
`c1f4502a…`). The `blend_backstop` decoder also admits the V1 backstop `CAO3AGAMZVRMHITL36EJ2VZQWKYRPWMQAPDQD5YEOF3GIF7T44U4JAL3`,
whose only WASM is
`62f61b32fff99f7eec052a8e573c367759f161c481a5caf0e76a10ae4617c3b4`
(`stellar.contract_instance_changes` FINAL, ledgers 51,499,492–51,499,549,
read 2026-10-05). No audit attests that hash: V1 has no published source,
and the decoder's V1 shapes are pinned against lake bytes only
(`internal/sources/blend_backstop/README.md` §Provenance).

So `62f61b32…` is not in `internal/wasmaudit/audited_wasm.json`, and the
per-WASM replay gate refuses every `blend_backstop` replay that reaches
ledger 51,499,492: the V1 contract is in the decoder's contract set and its
WASM is active from there on, whether or not V1 emits. Adding the hash needs a
Phase-3 review of the V1 WASM bytes against every V1 event shape `decode.go`
reads, recorded here in the same PR.

## V1 pool-factory WASM `0287f4ad` (2026-10-07)

Hash `0287f4ad7350935b83d94e046c0bcabc960b233dbce1531008c021b71d406a1d`
runs `MainnetPoolFactoryV1` (`CCZD6ESM…`) for its whole life from ledger
51,499,491 (`stellar.contract_instance_changes`, never upgraded). One other
instance, `CAGJKRMN…`, ran it from 51,498,926; it is outside the decoder's
contract set and has emitted no events.

- **Bytes.** The 2,904-byte WASM (`stellar.ledger_entry_changes`,
  `contract_code`, ledger 51,498,921) exports `initialize`, `deploy` and
  `is_pool`; its spec names `PoolFactoryDataKey`, `PoolInitMeta`,
  `pool_hash`, `backstop`, `blnd_id`. The only event it can publish is
  `deploy`.
- **Lake census.** `stellar.contract_events` for `CCZD6ESM…` holds 17
  events, ledgers 51,499,915–55,857,910: all `deploy`, one topic, successful
  calls, every body an `ScVal::Address` (`AAAAEg…`).
- **Decoder.** `decodeDeploy` reads exactly that shape (one `Symbol` topic,
  `Address` body). `TestGolden_DeployV1Factory` pins the first event: ledger
  51,499,915 deploys `CDVQVKOY…`, the first V1 pool.

No V1-specific decoding is needed, so the hash is in
`internal/wasmaudit/audited_wasm.json` and `blend` replays from 51,499,491
pass the gate.
