---
title: Reflector WASM-history audit
last_verified: 2026-11-21
status: partial — the two older attested hashes emit no events; event-emitting 25fc3060… is attested with captured fixtures; 7944a96d… and 8ecd1857… are reviewed but unattested
sources: reflector-dex, reflector-cex, reflector-fx
backfill_safe: false
---

# Reflector WASM audit

Audit log for the three Reflector source variants —
`reflector-dex`, `reflector-cex`, `reflector-fx`. All three share
**one decoder** and **one event shape**, differing only in the emitting
contract. Audited as one unit for wire format, with per-variant
`BackfillSafe` decisions (own deploy history each).

See `README.md` for the full procedure.

> **The two attested hashes emit no events.** Neither `4a64c8c8…` (v2)
> nor `df88820e…` (v3) imports the host's `contract_event` function
> (`x.1`); see the committed bytes at
> `evidence/r1-walk-2026-05-01/wasm-bytes/`. A replay over the ranges
> they ran writes no rows, which is correct, and their attestation says
> nothing about the event wire format. The walk ended at L59,301,651
> ([WASM timeline](#wasm-timeline)); the captured events from L62.25M
> came from a later, event-emitting WASM (`7944a96d…`), and it is not in
> `audited_wasm.json`. The replay gate therefore refuses any Reflector
> range that ran `7944a96d…` or `8ecd1857…`. The current hash,
> `25fc3060…`, is attested with captured events (see
> [below](#25fc306065cbb9b0--v710-all-three-variants)).

## Status

**Attested 2026-04-29:** `4a64c8c8…` (DEX, CEX) and `df88820e…`
(DEX, CEX, FX), as event-less builds. Fetched via `stellar contract fetch`
against mainnet.sorobanrpc.com.

**Attested 2026-11-21:** `25fc3060…` (DEX, CEX, FX), the event-emitting
v7.1.0 build, with captured events in
`test/fixtures/reflector/25fc306065cbb9b05461db78ec259802cb916053ba5b3ab911b0a72dd1a81439/`.

`Backfill: BackfillPerWASM` admits only ranges where every contract ran one
of those three: the event-less early ranges, and L65,079,084 (CEX) /
L65,079,108 (DEX) / L65,079,132 (FX) onward. Ranges that ran `7944a96d…` or
`8ecd1857…` are refused.

## Contracts under audit

Per AGENTS.md "Reflector is three separate contracts (DEX / CEX /
FX), not one.":

| variant | source name | mainnet contract |
| --- | --- | --- |
| DEX | `reflector-dex` | `CALI2BYU2JE6WVRUFYTS6MSBNEHGJ35P4AVCZYF3B6QOE3QKOB2PLE6M` |
| CEX | `reflector-cex` | `CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN` |
| FX  | `reflector-fx`  | `CBKGPWGKSKZF52CFHMTRR23TBWTPMRDIYZ4O2P5VS65BMHYH4DXMCJZC` |

Three legacy / placeholder IDs
(`CAVLP5DH…`, `CCYOZJCO…`, `CCSSOHTB…`) walked: **NO_EVENTS**; inactive on mainnet, not in
the decoder's contract list.

## Decoder expectations

From `internal/sources/reflector/{events,decode}.go`; the wire shape is
pinned by the captured events in `test/fixtures/reflector/v6-2026-04-23/`
(`real_fixture_test.go`). That directory is named for the upstream release,
not a WASM hash, so it does not identify which attested hash emitted them.

### Topic structure

    topic[0] = ScvSymbol("REFLECTOR")
    topic[1] = ScvSymbol("update")
    topic[2] = ScvU64(timestamp)        // unix milliseconds
    body     = ScvMap{"update_data": ScvVec<(ScVal, ScI128)>}

`update_data` is looked up by name. A body that is not a Map, lacks
`update_data`, or whose `update_data` is not a Vec fails the whole event
with `ErrMalformedPayload`.

Classification is byte-equal against `TopicSymbolReflector` +
`TopicSymbolUpdate`; drift silently drops every event.

### Body extraction

Each tuple in `update_data` is one (asset, price) pair. First element:

- `ScvAddress` (Soroban asset contract) → `canonical.NewSorobanAsset`
- `ScvSymbol` (ticker: fiat for FX, crypto for CEX) →
  `canonical.MapOracleSymbol`, which records an unmapped symbol as
  `raw:<symbol>` rather than dropping it

Any other type, a tuple that is not a 2-tuple, or a non-`i128` price fails
the whole event with `ErrMalformedPayload`. The second element is the price
as `i128` at the documented 14-decimal scale.

**One event** fans out into **N OracleUpdate rows**, one per tuple; to keep
`(source, ledger, tx_hash, op_index)` unique, fanout uses a per-entry op_index
stride (as SDEX).

### Asset identification

For `reflector-dex` / `reflector-cex` (Soroban Address tuples), the
asset is `canonical.NewSorobanAsset(strkey)`. For `reflector-fx`
(Symbol tuples), it's `canonical.NewFiatAsset(symbol_str)`.

Swapping DEX from Address to Symbol (or vice versa) would still decode but misclassify assets.

## Failure modes specific to Reflector

1. **Topic[0] / topic[1] symbol change** — `"REFLECTOR"` or
   `"update"` to anything else silently drops every event.
2. **Topic[2] type change** — `u64` → `i64` or `Symbol` does NOT fail:
   the decoder falls back to the ledger close time and keeps the prices.
   **Silent**: every row is restamped; only timestamp drift reveals it.
3. **Body shape change Map → bare Vec** (or `update_data` renamed) — every
   event fails with `ErrMalformedPayload`; the source goes dark for the range.
4. **Per-entry tuple field reorder** — `(asset, price)` → `(price, asset)`
   (i128 parsed as Address). Fail-loud: the whole event fails with
   `ErrMalformedPayload`; every event dropped under that WASM.
5. **Per-entry tuple length change** (e.g. a confidence
   score) — fails the AsTupleN(2) check; the whole event fails.
6. **Asset identifier type mix-up across variants** — DEX/CEX
   emitting Symbols (or FX Addresses): silent misclassification.
   Per-WASM source review must verify each variant's tuple type.
7. **Price scale change** — documented 14 decimals; a switch to E18 or
   similar still decodes but is off by 10^N. **No automated
   detection** — only cross-check against external oracle data.
8. **Vector overflow past OpIndex fanout stride** — more than
   `opIndexFanoutStride` (1024) entries in one event collides op_index synthesis.
   `ErrPriceVectorOverflow` surfaces it; needs a stride bump.

## WASM timeline

Output from `stellarindex-ops wasm-history` over the post-Soroban
window — full archive on r1, walked 2026-04-29:

```json
[
  {
    "contract": "CALI2BYU...",
    "ranges": [
      { "wasm_hash": "4a64c8c8502df326f4ce06d98998dc7d8a61575a11d6c0fbd4c60d10dfe28ffa",
        "from_ledger": 50644229, "to_ledger": 51656691 },
      { "wasm_hash": "df88820e231ad8f3027871e5dd3cf45491d7b7735e785731466bfc2946008608",
        "from_ledger": 51656692, "to_ledger": 59301651 }
    ]
  },
  {
    "contract": "CAFJZQWS...",
    "ranges": [
      { "wasm_hash": "4a64c8c8502df326f4ce06d98998dc7d8a61575a11d6c0fbd4c60d10dfe28ffa",
        "from_ledger": 50644239, "to_ledger": 51656688 },
      { "wasm_hash": "df88820e231ad8f3027871e5dd3cf45491d7b7735e785731466bfc2946008608",
        "from_ledger": 51656689, "to_ledger": 59301651 }
    ]
  },
  {
    "contract": "CBKGPWGK...",
    "ranges": [
      { "wasm_hash": "df88820e231ad8f3027871e5dd3cf45491d7b7735e785731466bfc2946008608",
        "from_ledger": 56733481, "to_ledger": 59301651 }
    ]
  }
]
```

Two unique hashes total across all three contracts:

- **`4a64c8c8…`** — DEX + CEX only. Active L50,644,229 →
  L51,656,691 (~1.0M ledgers, roughly 2024-02-19 → 2024-04-26 in
  wall time). Replaced at L51,656,689 (CEX) / L51,656,692 (DEX) —
  the 3-second offset between contracts indicates a coordinated
  upgrade pushed in the same operator session.
- **`df88820e…`** — DEX + CEX from the v2→v3 upgrade (~2024-04-26);
  FX from its first deploy at L56,733,481 (~2025-06). Ran on all three
  through the walk's end (L59,301,651).

Neither hash imports `contract_event`, so neither emitted the events the
decoder reads. The captured events in `test/fixtures/reflector/v6-2026-04-23/`
are from L62.25M, after the walk's end, so a later, unattested WASM was running
by then. Which hash, and when each contract switched, has not been recorded.

## Per-hash review findings

| variant | hash (first 16) | active range | reviewer | finding |
| --- | --- | --- | --- | --- |
| FX | `df88820e231ad8f3` | L56,733,481 → at least L59,301,651 (walk-end) | maintainer@2026-04-29 | emits no events |
| DEX (post-v3) | `df88820e231ad8f3` | L51,656,692 → at least L59,301,651 (walk-end) | maintainer@2026-04-29 | emits no events |
| CEX (post-v3) | `df88820e231ad8f3` | L51,656,689 → at least L59,301,651 (walk-end) | maintainer@2026-04-29 | emits no events |
| DEX (pre-v3) | `4a64c8c8502df326` | L50,644,229 → L51,656,691 | maintainer@2026-04-29 | emits no events |
| CEX (pre-v3) | `4a64c8c8502df326` | L50,644,239 → L51,656,688 | maintainer@2026-04-29 | emits no events |
| DEX, CEX, FX | `7944a96d512f6e51` | L61,602,762–L61,603,662 → L63,690,357–L63,690,399 | maintainer@2026-11-21 | upstream `v6.0.0` pulse build; emits the event the decoder reads; the `v6-2026-04-23` fixtures fall in this range; not attested |
| DEX, CEX, FX | `8ecd1857496df2c1` | L63,690,358–L63,690,400 → L65,079,083–L65,079,131 | maintainer@2026-11-21 | upstream `v6.0.1` pulse build; event spec byte-identical to v6.0.0; not attested |
| DEX, CEX, FX | `25fc306065cbb9b0` | L65,079,084 (CEX) / L65,079,108 (DEX) / L65,079,132 (FX) → tip | maintainer@2026-11-21 | upstream `v7.1.0` pulse build; matches current decoder (see below); attested, fixtures under `test/fixtures/reflector/25fc3060…/` |

### `df88820e231ad8f3` — v3, all three variants

- No `x.1` (`contract_event`) import in the committed bytes: it emits no
  events, so a replay over its ranges writes no rows.
- Not the source of the fixtures in `test/fixtures/reflector/` (see above).

### `4a64c8c8502df326` — DEX + CEX pre-v3 hash (disassembly-confirmed)

Active on DEX (L50,644,229) / CEX (L50,644,239) from first deploy in
February 2024 through the v2→v3 upgrade at ~L51,656,690 (late
April 2024); ~1M ledgers / ~9 weeks.

**Disassembly evidence** (2026-04-29): bytes via `stellar contract fetch --wasm-hash
4a64c8c8…` against mainnet.sorobanrpc.com, compared with
v3 (`df88820e…`) using `stellar contract info
interface` + data-section string analysis:

1. **Contract interface diff is cosmetic.** The v2→v3 transition
   removed one governance function (`bump(env, ledgers_to_live:
   u32)`, storage TTL extension) and reordered the `PriceData` /
   `ConfigData` struct definitions. **Every
   public method signature relevant to event emission is
   unchanged** — `set_price(env, updates: Vec<i128>, timestamp:
   u64)`, the `Asset { Stellar(Address) |
   Other(Symbol) }` enum and `PriceData { price: i128, timestamp:
   u64 }`. The event-publish wire format is unaffected.
2. **Data-section field names are identical.** Both v2 and v3
   binaries contain the same `Symbol::new` strings: `price`, `prices`, `timestamp`, `last_timestamp`,
   `asset`, `assets`, `base_asset`, `quote_asset`, `decimals`,
   `period`, `resolution`, `update_contract`, `updates`, `records`,
   `lastprice`. ("REFLECTOR" / "update" are small-symbol u64 constants,
   not raw strings in either binary — verified via `strings <wasm>`.)
3. **SDK family is the same.** v2 was built against soroban-sdk
   20.2.0 (commit 6e198b79); v3 against 20.3.2 (1d7f9bd8). Both are
   in the 20.x line where `#[contractevent]` yields stable wire formats
   (tuple field -> `ScvVec` in declaration order; topics -> small-symbol
   `ScvSymbol`). No change between 20.2.0 and 20.3.2
   touches event encoding.
4. **The upstream `#[contractevent]` `UpdateEvent` source** is a later
   release than v2/v3, which import no `contract_event` and emit nothing.
   Its body is `Map{"update_data": Vec<(Val, i128)>}`, as the decoder reads.

**Conclusion**: v2 and v3 emit no events, so replays of their ranges are
admitted and correctly write no rows. Items 1-3 do not bear on the event
wire format.

### Post-walk timeline (2026-11-21)

From `GET /v1/contracts/<id>/code-history` on the public API (lake-derived),
not a `wasm-history` walk:

| contract | `7944a96d…` from | `8ecd1857…` from | `25fc3060…` from |
| --- | --- | --- | --- |
| CEX `CAFJZQWS…` | L61,603,662 (2026-03-11) | L63,690,358 (2026-07-28) | L65,079,084 (2026-10-22) |
| DEX `CALI2BYU…` | L61,603,345 | L63,690,379 | L65,079,108 |
| FX `CBKGPWGK…` | L61,602,762 | L63,690,400 | L65,079,132 |

Each hash is byte-identical (sha256) to the `reflector-pulse-contract`
asset of an upstream release in `reflector-network/reflector-contract`:
`7944a96d…` = `v6.0.0` (cli 25.1.0), `8ecd1857…` = `v6.0.1` (cli 27.0.0),
`25fc3060…` = `v7.1.0` (cli 28.1.0, 20,417 bytes, the size the API serves).
The release source is therefore the source of the deployed bytes.

### `25fc306065cbb9b0` — v7.1.0, all three variants

Evidence, and where it came from:

1. **Source.** `oracle/src/events.rs` is unchanged from `v6.0.1` to `v7.1.0`
   (GitHub compare). `UpdateEvent` is `#[contractevent(topics = ["REFLECTOR",
   "update"])]` with `#[topic] timestamp: u64` and `update_data: Vec<(Val,
   i128)>`; it publishes one `(asset, price)` per asset with a non-zero price,
   in `assets()` order, `Asset::Stellar` as an Address and `Asset::Other` as a
   Symbol. `set_price` still calls `publish_update_event` with the same
   arguments. The release's other changes are read-side or admin-side: the
   base asset is quoted as exactly `10^decimals` by `price`/`lastprice`/`prices`
   and cannot be added as a feed; `lastprice` no longer returns `None` when
   the last update is older than two periods; `load_prices` walks back at most
   256 periods; `version()` moved; soroban-sdk 27.0.2 → 28.0.0. None touches
   the event, `decimals` or `base`.
2. **Bytes.** The v7.1.0 WASM imports `x.1` (`contract_event`) and its data
   section carries `update_data`. Its `contractspecv0` section has no
   `UpdateEvent` entry, unlike v6.0.0/v6.0.1 (whose entries are
   byte-identical); the spec is metadata and does not change what is
   published.
3. **Served rows against the contract's own input.** For the 11:55:00Z update
   on 2026-11-21, the served rows (`/v1/oracle/streams`, the live decoder's
   output on events from this hash) equal, as exact raw integers, the
   non-zero `prices` of that update's `set_price` call (`/v1/tx/<hash>`
   `args_xdr`), with one row per price: DEX 25 of 25 (tx `86aef038…`, L65,595,216),
   CEX 12 of 12 (`4e6dfb7a…`, L65,595,220), FX 24 of 24 (`072d062a…`,
   L65,595,216). Row timestamps equal the call's millisecond timestamp. No
   row is `raw:`; every slot maps.
4. **Scale against independent oracles** (decimals 14, same minute):
   CEX XLM 0.125399 vs `/v1/price/at` 0.125362 and RedStone 0.125290; CEX BTC
   58,078.44 vs 58,039.63 and RedStone 58,075.26; CEX ETH 1,651.45 vs
   1,648.17; FX EUR 1.097445 vs CEX EURC 1.095716 (no aggregated EUR/USD is
   served). A 10^N scale change would be obvious; there is none.

Field-by-field: topics, body Map, `update_data` Vec of 2-tuples, Address/Symbol
asset slot, `i128` price at 14 decimals, millisecond `u64` topic timestamp,
the USD (CEX, FX) and USDC SAC (DEX) quotes, and no new event kind all match
the decoder expectations above. **Matches current decoder.**

**Attested 2026-11-21.** Nine events (three per contract, L65,595,156–L65,595,280)
were captured from a public mainnet stellar-rpc with
`scripts/dev/capture-reflector-fixtures.sh` into
`test/fixtures/reflector/25fc306065cbb9b05461db78ec259802cb916053ba5b3ab911b0a72dd1a81439/`;
each file's `wasm_hash`, read from the contract's ledger entry, is this hash.
Three are the item 3 transactions. Their `.golden` files were generated with
`go test ./internal/sources/reflector -run TestRealMainnetFixtures -update`,
then checked against an independent decode of the raw event XDR (`i128` at
14 decimals): every row of the three item 3 events matches, as an exact
integer and in order (DEX 25, CEX 12, FX 24), e.g. CEX XLM 0.125399, BTC
58,078.444805, ETH 1,651.446658; FX EUR 1.097445; DEX XLM SAC 0.125226.

To capture events for `7944a96d…` (the `v6-2026-04-23` fixtures are from it:
L62,251,160–L62,251,319 lies in its range on all three contracts) or
`8ecd1857…`, on a host with a stellar-rpc whose retention covers its range:

    WASM_HASH=<hash> \
      scripts/dev/capture-reflector-fixtures.sh -e http://127.0.0.1:8000 -c <contract> -n 3

for each of the three contracts, then `go test ./internal/sources/reflector
-run TestRealMainnetFixtures -update`, review the goldens, and add the hash to
`audited_wasm.json`.

## Decision

| source | Backfill | rationale |
| --- | --- | --- |
| `reflector-fx` | `BackfillPerWASM` | `df88820e…` attested as event-less and `25fc3060…` as event-emitting; ranges that ran `7944a96d…` or `8ecd1857…` refused until those are attested. |
| `reflector-dex` | `BackfillPerWASM` | `4a64c8c8…` + `df88820e…` attested as event-less and `25fc3060…` as event-emitting; same refusal. |
| `reflector-cex` | `BackfillPerWASM` | Same as DEX. |

## References

- Procedure: `docs/operations/wasm-audits/README.md`
- Decoder source: `internal/sources/reflector/{events,decode}.go`
- Schema-evolution stance: `docs/architecture/ingest-pipeline.md#contract-schema-evolution`
- Backfill gate: `internal/sources/external/registry.go` —
  `Registry["reflector-{dex,cex,fx}"].Backfill` (three entries), checked per
  range against `internal/wasmaudit/audited_wasm.json`
- Upstream contract source: `https://github.com/reflector-network/reflector-contract`
- WASM-history walk JSON (full): `r1:/var/log/wasm-history-all.json`
