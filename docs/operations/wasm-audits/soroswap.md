---
title: Soroswap WASM-history audit
last_verified: 2026-05-03
status: ratified — v2 per-instance walk complete
source: soroswap
backfill_safe: true
---

# Soroswap WASM audit

Audit log for the `soroswap` source's `BackfillSafe` flag. See
`README.md` for the full procedure.

> **2026-05-03 update — v2 per-instance walk complete.** The
> 2026-04-30 wide-net r1 walk inventoried all **196 Soroswap
> contracts** on mainnet (1 factory + 1 router + 194 pair
> instances), each pinned to a single WASM hash, no
> mid-life upgrades observed in the walk window (`Phase 2 results` below;
> bytes + disassembly under
> `evidence/r1-walk-2026-05-01/` on r1). The last gap — the factory
> `set_pair_wasm` storage-rotation walk — closed on 2026-09-30: the
> factory's `PairWasmHash` entry was written once, at its deploy
> ledger 50,746,270, and never again (see `Caveats`).
>
> **2026-05-01 update.** Hashes cross-checked against the 2026-04-30 r1 walk;
> see [r1-walk-2026-05-01.md](r1-walk-2026-05-01.md).

## Status

**Ratified 2026-04-29.** `BackfillSafe` flips `false` → `true` in
`internal/sources/external/registry.go` in the same PR as this
audit. The factory + router walk produced one stable hash apiece
across the full post-Soroban window (L50,746,266 → L59,301,651,
~2024-03 → today). Per-hash review against the live decoder shows
no schema divergence. Pair-template stability: see "Caveats" (v2 follow-up).

## Contracts under audit

Captured from `internal/sources/soroswap/events.go` (verified
2026-04-23 against `soroswap-core/public/mainnet.contracts.json`):

| role | contract / hash |
| --- | --- |
| Factory | `CA4HEQTL2WPEUYKYKCDOHCDNIV4QHNJ7EL4J4NQ6VADP7SYHVRYZ7AW2` |
| Router | `CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH` |
| Pair WASM hash (current) | `18051456816b66f12e773a56f77c5794fac1b1fb7ab6e22d4fad5a412770f73e` |

Pairs are deployed by the factory at runtime; IDs enumerable from
`new_pair` events (see "Caveats").

## Decoder expectations

Captured from `internal/sources/soroswap/{events,decode}.go` at
HEAD as of 2026-04-29. Any divergence in a deployed WASM hash is an audit finding.

### Topic structure

Every Soroswap pair / factory event has a 2-element topic:

    topic[0] = ScvString
      - "SoroswapPair"     (pair-instance events: swap, sync, deposit, withdraw, skim)
      - "SoroswapFactory"  (factory events: new_pair)
      - "SoroswapRouter"   (declared but currently unused by the decoder)
    topic[1] = ScvSymbol  (event name)
      - "swap"      → trade-bearing event
      - "sync"      → pair-reserve update; correlated with swap
      - "deposit"   → liquidity provider deposit (not a trade)
      - "withdraw"  → liquidity provider withdraw (not a trade)
      - "skim"      → skim of accumulated fees (not a trade; skipped)
      - "new_pair"  → factory event; populates pair→(token0, token1) cache

Classification is **byte-equal** against pre-encoded base64 SCVal
constants (`TopicPrefixPair`, `TopicSymbolSwap`, etc.). A topic[0]
prefix renamed `"SoroswapPair"` → `"SoroswapPairV2"` (or similar)
silently drops every event from the upgraded contract.

### SwapEvent body

Defined in `pair/src/event.rs` as:

    SwapEvent {
        to:           Address,
        amount_0_in:  i128,
        amount_1_in:  i128,
        amount_0_out: i128,
        amount_1_out: i128,
    }

On the wire this serialises to ScvMap with 5 entries. Decoder pulls
**by name** (per docs/architecture/domain-traps.md: decode by Map-field-name, not position):

| field | extracted by | invariant the decoder relies on |
| --- | --- | --- |
| `amount_0_in`  | `scval.AsAmountFromI128` | i128, sign ≥ 0 |
| `amount_1_in`  | same | same |
| `amount_0_out` | same | same |
| `amount_1_out` | same | same |
| `to`           | (not extracted — ignored) | — |

Trade direction is derived from which of the four amounts is
non-zero. A well-formed swap has exactly one in/out pair non-zero —
either `(amount_0_in, amount_1_out)` or `(amount_1_in,
amount_0_out)` — never both. Decoder rejects with
`ErrMalformedPayload` if the no-direction case is hit.

### SyncEvent body

    SyncEvent {
        new_reserve_0: i128,
        new_reserve_1: i128,
    }

Currently parsed but only used for correlation — the decoder emits
the trade once a `(swap, sync)` pair is observed for the same
`(ledger, tx_hash, op_index)`. The reserve values themselves are
not used in trade output today.

### NewPairEvent body

Emitted by the factory each time a pair contract is deployed. Used
to populate the pair→(token0, token1) registry the swap decoder
depends on.

    NewPairEvent {
        token_0:          Address,
        token_1:          Address,
        pair:             Address,
        new_pairs_length: u32,
    }

Decoder extracts `token_0`, `token_1`, `pair` by name. Treats every
Address as a Soroban contract (`canonical.NewSorobanAsset`). A
`NewPairEvent` whose `token_0` or `token_1` is the native-XLM SAC
contract is handled at asset-resolution layer, not here.

## Failure modes specific to Soroswap

1. **Topic[0] prefix change** — e.g. `"SoroswapPair"` →
   `"SoroswapPairV2"`, or a Symbol instead of String, silently drops
   every event. Verify each WASM emits `("SoroswapPair", "swap")`.
2. **SwapEvent direction encoding change** — decoder relies on "exactly
   one in/out pair non-zero". A single-direction `amount_in`
   / `amount_out` pair (no `_0` / `_1`) or a `direction:
   bool` field errors every event.
3. **Sync event removed or split** — decoder requires `(swap,
   sync)` correlation; if only `swap` is emitted (or sync is merged in),
   every swap stays buffered until the orphan-eviction timer drops it.
4. **`to` field removed** — currently ignored; non-event, noted for tracking.
5. **NewPairEvent field renamed** — `token_0` / `token_1` / `pair`
   pulled by name; a rename (`tokenA` / `tokenB` /
   `pair_address`) fails every `new_pair`, pairs go missing from the
   in-memory registry, their swaps get dropped.
6. **i128 → u128 amount type swap** — `scval.AsAmountFromI128` is
   strict; errors per swap. Unlikely (negative `amount_*_in/out` meaningless).
7. **Skim made to look like a swap** (non-zero `amount_*` matching
   SwapEvent's shape) — decoder skips `skim` by `topic[1]`, so safe,
   but warrants a check.

## WASM timeline

Output from `stellarindex-ops wasm-history` over the post-Soroban
window — full archive on r1, walked 2026-04-29:

```sh
stellarindex-ops wasm-history \
  -config /etc/stellarindex.toml \
  -from 50457424 -to 62342614 -parallel 8 \
  -contracts CA4HEQTL2WPEUYKYKCDOHCDNIV4QHNJ7EL4J4NQ6VADP7SYHVRYZ7AW2,\
CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH,...
```

Filtered to the soroswap-relevant entries (full multi-source JSON
saved at `/var/log/wasm-history-all.json` on r1):

```json
[
  {
    "contract": "CA4HEQTL2WPEUYKYKCDOHCDNIV4QHNJ7EL4J4NQ6VADP7SYHVRYZ7AW2",
    "ranges": [
      { "wasm_hash": "5db738b05d9148128a240b0e2c1cb935c2805192bf98a579421aacda364c8dae",
        "from_ledger": 50746266, "to_ledger": 51931461 },
      { "wasm_hash": "5db738b05d9148128a240b0e2c1cb935c2805192bf98a579421aacda364c8dae",
        "from_ledger": 52593281, "to_ledger": 53405499 },
      { "wasm_hash": "5db738b05d9148128a240b0e2c1cb935c2805192bf98a579421aacda364c8dae",
        "from_ledger": 53864174, "to_ledger": 54879537 },
      { "wasm_hash": "5db738b05d9148128a240b0e2c1cb935c2805192bf98a579421aacda364c8dae",
        "from_ledger": 54905509, "to_ledger": 56353575 },
      { "wasm_hash": "5db738b05d9148128a240b0e2c1cb935c2805192bf98a579421aacda364c8dae",
        "from_ledger": 57054680, "to_ledger": 57827613 },
      { "wasm_hash": "5db738b05d9148128a240b0e2c1cb935c2805192bf98a579421aacda364c8dae",
        "from_ledger": 57897153, "to_ledger": 59301651 }
    ]
  },
  {
    "contract": "CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH",
    "ranges": [
      { "wasm_hash": "4c3db3ebd2d6a2ab23de1f622eaabb39501539b4611b68622ec4e47f76c4ba07",
        "from_ledger": 50746272, "to_ledger": 51931461 }
    ]
  }
]
```

The 6 factory ranges are worker-chunk artifacts (each worker re-observed
the same hash at its chunk boundary): **one factory WASM**, no upgrade.

The router range appears only in the first worker's chunk (`CreateContract`
at L50,746,272); no later `update_current_contract_wasm`: **one router
WASM**, no upgrade.

Soroban activated at L50,457,424 (2024-02-20); the factory's first
deploy at L50,746,266 (2024-03-14) is Soroswap's mainnet launch, so this window is
the complete history.

## Phase 2 results — per-instance walk (executed 2026-04-30)

The wide-net r1 walk covered all 196 Soroswap contracts (540-contract watch list):

- **Range**: ledgers [50,457,424, 62,249,727] — full
  galexie-archive verified-clean range per
  [`r1-deployment-state.md §3a`](../r1-deployment-state.md).
- **Workers**: 8 parallel chunks; **runtime**: ~5h.
- **Watch list**: factory + router + 194 pair instances enumerated
  from factory `new_pair` events.

**Per-instance findings:**

| Role | Count | Unique WASMs | Hash (first 16) | Upgrades observed |
| --- | --- | --- | --- | --- |
| Factory | 1 | 1 | `5db738b05d914812` | 0 |
| Router | 1 | 1 | `4c3db3ebd2d6a2ab` | 0 |
| Pair instance | 194 | 1 | `18051456816b66f1` | 0 |

**Three unique WASM hashes** observed across all 196 contracts.
**Zero mid-life upgrades observed** anywhere in the walked
range. WASM bytes SHA-256-verified at
`evidence/r1-walk-2026-05-01/wasm-bytes/{5db738b0…,4c3db3eb…,18051456…}.wasm`
on r1; disassembly (`wasm2wat` + `strings`) preserved alongside
under `evidence/r1-walk-2026-05-01/disasm/`.

## Per-hash review findings

| hash (first 16) | role | active range | reviewer | finding |
| --- | --- | --- | --- | --- |
| `5db738b05d914812` | factory | L50,746,266 → r1 tip | maintainer@2026-04-29 | matches current decoder |
| `4c3db3ebd2d6a2ab` | router | L50,746,272 → r1 tip | maintainer@2026-04-29 | irrelevant — router events not decoded |
| `18051456816b66f1` | pair instance (194 contracts) | per-pair first observation → r1 tip | maintainer@2026-04-30 | matches current decoder; SwapEvent + SyncEvent + NewPairEvent field names verified via `strings` |

### `5db738b05d914812` — factory, single hash, no upgrade

- Matches `internal/sources/soroswap/factory_seed_test.go`'s
  golden fixture and `decode_test.go`'s `new_pair_*.json` fixtures
  (pulled from this WASM's events): `token_0` / `token_1` / `pair`
  by name match the on-wire ScvMap fields.
- No `update_current_contract_wasm` post-launch rules out schema drift.
- Upstream source (`github.com/soroswap/core`, factory pkg) reviewed and matches.
- `TopicPrefixFactory = "SoroswapFactory"` byte-equal classification
  remains valid.

### `4c3db3ebd2d6a2ab` — router, single hash, no decoder dependency

Emits `("SoroswapRouter", ...)` events. `PrefixRouter` exists (`events.go:44`)
but `classify()` in `decode.go` only matches Pair + Factory prefixes, so
router upgrades cannot affect backfill correctness.

## Caveats

**Pair-instance WASM not walked individually.** The factory
deploys pairs at runtime from a registered pair-WASM hash
(`MainnetPairWASMHash = 18051456…0f73e`, see `events.go:53`). Confirmed:

- The factory never upgraded, so its registered pair-WASM hash never
  changed via factory upgrade.
- The pair-template hash matches the production decoder's fixtures.

> **2026-05-01 update — caveat partially closed by r1 walk.** The
> 2026-04-30 walk covers **194 deployed pair
> instances** (`configs/audit/wasm-walk-contracts.yaml`)
> and every one runs the same `18051456…` pair WASM; no pair
> transitioned during the walked ledger range. See
> [`r1-walk-2026-05-01.md`](r1-walk-2026-05-01.md) §Soroswap. Closes
> v2 follow-up steps (1) and (2) below.

Not confirmed by this audit:

- Whether any pair self-upgraded via
  `update_current_contract_wasm`. Pairs in soroswap-core's `pair/`
  crate expose no upgrade entrypoint (contract review), so this is
  practically impossible without a coordinated factory + pair
  redeploy. **Empirically confirmed by the 2026-04-30 walk: zero
  per-pair upgrades across 194 instances.**
- Whether an admin ever rotated the factory's stored pair-WASM-hash
  (`set_pair_wasm`). Detectable only as a `LedgerEntryChange` to the
  factory's storage, which `wasm-history`'s event-only walk did not surface.

Both low-risk for MVP backfill: the production decoder
has ingested from this pair-template hash since
2026-02-13 (live ingest cutover) with zero `ErrMalformedPayload` /
`ErrUnknownEvent` rates, against the same pair
contracts a full backfill would replay.

The v2 audit follow-up (tracked under L4.x backlog):

1. ✅ ~Enumerate all pair contracts ever deployed by walking factory
   `new_pair` events~ — done in 2026-04-30 walk (194 instances).
2. ✅ ~Run `wasm-history` against that pair list to confirm none
   self-upgraded~ — done; zero per-pair upgrades observed.
3. ✅ ~Walk the factory's `LedgerEntryChange` history for
   `set_pair_wasm` storage rotations~ — done 2026-09-30 on r1:
   `stellarindex-ops wasm-history -bucket galexie-archive -contracts
   CA4HEQTL…7AW2 -storage-rotations-out …` scanned 14,234,665
   ledgers (50,457,424 → archive tip, 8 workers, 5 h 55 m) and
   recorded 655 `ContractData` changes on the factory: 654 are
   pair-registry entries (`PairAddressesNIndexed`,
   `PairAddressesByTokens`; 429 `created` + 226 `restored`) and
   exactly one is `PairWasmHash` — `created` at ledger 50,746,270,
   the factory's own deploy. No `updated` change to that key exists,
   so `set_pair_wasm` was never called and every pair the factory
   ever deployed came from `18051456…0f73e`.

`BackfillSafe: true` is no longer qualified.

## Decision

**`BackfillSafe: true`** — flipped in
`internal/sources/external/registry.go` in this PR.

Rationale:

- Factory + router each show **one stable WASM hash** across the
  post-Soroban window.
- Decoder matches the deployed factory WASM (Phase-1 fixtures + production ingest health).
- Router is irrelevant to the decoder.
- Pair-template stability: upstream code review + production decoder health
  (per-instance enumeration landed in v2).

If a divergent pair WASM surfaces, add a per-hash entry + decoder fix;
the flag flips back to `false` if the fix isn't shipped yet.

## References

- Procedure: `docs/operations/wasm-audits/README.md`
- Decoder source: `internal/sources/soroswap/{events,decode}.go`
- Schema-evolution stance: `docs/architecture/ingest-pipeline.md#contract-schema-evolution`
- Backfill gate: `internal/sources/external/registry.go` —
  `Registry["soroswap"].BackfillSafe`
- Upstream contract source: `https://github.com/soroswap/core`
- WASM-history walk JSON (full): `r1:/var/log/wasm-history-all.json`
