---
title: Reflector WASM-history audit
last_verified: 2026-05-03
status: ratified — v2 walk confirms two-hash inventory
sources: reflector-dex, reflector-cex, reflector-fx
backfill_safe: true
---

# Reflector WASM audit

Audit log for the three Reflector source variants —
`reflector-dex`, `reflector-cex`, `reflector-fx`. All three share
**one decoder** and **one event shape**, differing only in the emitting
contract. Audited as one unit for wire format, with per-variant
`BackfillSafe` decisions (own deploy history each).

See `README.md` for the full procedure.

> **2026-05-03 update — v2 walk confirms two-hash inventory.**
> The 2026-04-30 wide-net r1 walk re-observed the v2 (`4a64c8c8…`)
> → v3 (`df88820e…`) transition on DEX (`CALI2BYU…`) + CEX
> (`CAFJZQWS…`) at L51,656,689-91, and confirmed the FX
> (`CBKGPWGK…`) contract has been on `df88820e…` since first
> deploy at L56,733,481. **No further upgrades observed** through
> the walk's upper bound (L62,249,727). All three contracts
> currently run `df88820e…`. Bytes SHA-256-verified
> for both hashes at `evidence/r1-walk-2026-05-01/wasm-bytes/`
> on r1.
>
> **2026-05-01 update.** Hashes cross-checked against the 2026-04-30 r1 walk;
> see [r1-walk-2026-05-01.md](r1-walk-2026-05-01.md).

## Status

**Ratified 2026-04-29.** All three Reflector variants (DEX, CEX,
FX) flip `BackfillSafe: false → true` in this PR. Two unique WASM
hashes across the three contracts; both fetched via `stellar contract fetch` against
mainnet.sorobanrpc.com — interface diff between v2 (`4a64c8c8…`)
and v3 (`df88820e…`) is **cosmetic** (one removed governance
function, struct definition reordering); event-emitting types and
SDK-family are identical, so the wire format is preserved.

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

Captured from `internal/sources/reflector/{events,decode}.go` at
HEAD as of 2026-04-29. Re-verified 2026-04-23 against the upstream
`#[contractevent]` macro expansion.

### Topic structure

    topic[0] = ScvSymbol("REFLECTOR")
    topic[1] = ScvSymbol("update")
    topic[2] = ScvU64(timestamp)        // unix milliseconds
    body     = ScvVec<(ScVal, ScI128)>  // per-entry tuple

`timestamp` is hoisted into a `#[topic]` slot by the `#[contractevent]`
macro. The body is NOT `Map{"prices": Vec<(Asset, i128)>, "timestamp": u64}`:
`#[contractevent]` expands tuple-shaped fields to ScvVec in declaration order.

Classification is byte-equal against `TopicSymbolReflector` +
`TopicSymbolUpdate`; drift silently drops every event.

### Body extraction

Each tuple in the outer `Vec<(ScVal, I128)>` is one (asset, price)
pair. First element:

- `ScvAddress` (Soroban contract address — DEX/CEX)
- `ScvSymbol` (fiat code like "USD" or asset symbol — FX)

Other types are skipped (`ErrUnknownAssetIdentifier`). The second
element is the price as `i128` at the documented 14-decimal scale.

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
2. **Topic[2] type change** — `u64` → `i64` or `Symbol` errors per
   event (`AsU64FromTopic` strict); fail-loud, range dropped.
3. **Body shape change Vec → Map** — every event errors at extraction.
4. **Per-entry tuple field reorder** — `(asset, price)` → `(price, asset)`
   (i128 parsed as Address). **Almost certainly fail-loud
   per entry**; every event dropped under that WASM.
5. **Per-entry tuple length change** (e.g. a confidence
   score) — errors at the AsTupleN(2) check; entries skipped.
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
- **`df88820e…`** — current production hash on **all three**
  variants. DEX + CEX adopted at the v2→v3 upgrade (~2024-04-26);
  FX deployed fresh on this hash at L56,733,481 (~2025-06) and
  has never been on any other.

Matches Reflector's documented v2→v3 transition; every fixture in
`internal/sources/reflector/` was captured against the v3-era binary.

Live ingest from walk-end (L59,301,651) through r1's tip
(L62,342,614): no further upgrades; `df88820e` is still production.

## Per-hash review findings

| variant | hash (first 16) | active range | reviewer | finding |
| --- | --- | --- | --- | --- |
| FX | `df88820e231ad8f3` | L56,733,481 → L59,301,651 (walk-end; current per live ingest) | maintainer@2026-04-29 | matches current decoder |
| DEX (post-v3) | `df88820e231ad8f3` | L51,656,692 → L59,301,651 (walk-end) | maintainer@2026-04-29 | matches current decoder |
| CEX (post-v3) | `df88820e231ad8f3` | L51,656,689 → L59,301,651 (walk-end) | maintainer@2026-04-29 | matches current decoder |
| DEX (pre-v3) | `4a64c8c8502df326` | L50,644,229 → L51,656,691 | maintainer@2026-04-29 | matches current decoder (disassembly) |
| CEX (pre-v3) | `4a64c8c8502df326` | L50,644,239 → L51,656,688 | maintainer@2026-04-29 | matches current decoder (disassembly) |

### `df88820e231ad8f3` — current production, all three variants

- Fixtures (`internal/sources/reflector/decode_test.go`,
  `real_fixture_test.go`) captured from this WASM's events; topic shape
  `("REFLECTOR", "update", <u64 ms>)` and body
  `Vec<(asset, i128)>` match.
- All three variants emit the SAME wire format from
  this WASM (decoder is variant-agnostic apart from the
  ScvAddress vs ScvSymbol asset slot, handled by
  `ErrUnknownAssetIdentifier` skipping).
- 14-decimal price scale matches the constant in the decoder.
- Live ingest health: 0 `ErrMalformedPayload` /
  `ErrUnknownAssetIdentifier` rate spikes since FX support landed
  (PR #161, 2026-03 cutover).
- No `update_current_contract_wasm` from
  L51,656,689 (DEX+CEX) / L56,733,481 (FX) through walk-end and live ingest.

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
4. **Source at the v3-era release** (the only one in our
   `.discovery-repos/reflector-contract` checkout) shows the
   `#[contractevent(topics = ["REFLECTOR", "update"])] struct
   UpdateEvent { #[topic] timestamp: u64, update_data: Vec<(Val,
   i128)> }` pattern that matches the decoder's expected
   `topic[0..2] = ("REFLECTOR", "update", <u64>)` + `body =
   Vec<(Val, i128)>`. With spec, data section and SDK
   family identical between v2 and v3, the event shape is preserved.

**Conclusion**: the v3-tuned decoder will correctly decode v2-era
events. Backfill replays of L50,644,229 → L51,656,691 are safe.

## Decision

| source | BackfillSafe | rationale |
| --- | --- | --- |
| `reflector-fx` | **`true`** (flipped in commit 950891bde) | Single WASM hash since first deploy; matches current decoder; live ingest healthy. |
| `reflector-dex` | **`true`** (flipped in this PR) | v2 (`4a64c8c8…`) + v3 (`df88820e…`) hashes both verified. v3 from fixtures + production health; v2 from disassembly + interface diff (cosmetic) + SDK-family compat. |
| `reflector-cex` | **`true`** (flipped in this PR) | Same evidence as DEX — both contracts share the same two hashes and the same disassembly findings apply. |

## References

- Procedure: `docs/operations/wasm-audits/README.md`
- Decoder source: `internal/sources/reflector/{events,decode}.go`
- Schema-evolution stance: `docs/architecture/ingest-pipeline.md#contract-schema-evolution`
- Backfill gate: `internal/sources/external/registry.go` —
  `Registry["reflector-{dex,cex,fx}"].BackfillSafe` (three entries)
- Upstream contract source: `https://github.com/reflector-network/reflector-contract`
- WASM-history walk JSON (full): `r1:/var/log/wasm-history-all.json`
