---
title: Reflector WASM-history audit
last_verified: 2026-10-21
status: partial — the two attested hashes emit no events; the event-emitting hash is unaudited
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
> came from a later, event-emitting WASM, and it is not in
> `audited_wasm.json`. The replay gate therefore
> refuses any Reflector range that holds events until those hashes are
> string-checked against the decoder below and attested. Their values
> are not recorded in this repo and are unverified here.

## Status

**Attested 2026-04-29:** `4a64c8c8…` (DEX, CEX) and `df88820e…`
(DEX, CEX, FX), as event-less builds. Fetched via `stellar contract fetch`
against mainnet.sorobanrpc.com. `Backfill: BackfillPerWASM` admits only
ranges where every contract ran one of those two; no event-bearing Reflector
range is replay-safe yet.

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
| DEX, CEX, FX (post-walk) | unrecorded | after L59,301,651 → tip | — | not audited; the replay gate refuses these ranges |

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

## Decision

| source | Backfill | rationale |
| --- | --- | --- |
| `reflector-fx` | `BackfillPerWASM` | `df88820e…` attested as event-less; event-bearing ranges refused until the later hash is audited. |
| `reflector-dex` | `BackfillPerWASM` | `4a64c8c8…` + `df88820e…` attested as event-less; same refusal. |
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
