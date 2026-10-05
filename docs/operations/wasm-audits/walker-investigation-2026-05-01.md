---
title: wasm-history walker investigation + wide-net walk plan (2026-05-01)
last_verified: 2026-05-01
status: investigation complete; wide-net walk recommendations below
related:
  - docs/operations/wasm-audits/r1-walk-2026-05-01.md
  - docs/operations/wasm-audits/protocol-epochs.md
  - configs/audit/wasm-walk-contracts.yaml
  - cmd/stellarindex-ops/main.go (wasm-history implementation)
---

# Walker investigation + wide-net walk plan

> Pre-flight before re-running the wasm-history walk with broader scope.
> Triggered by pushback that "TTL-evicted" was hand-wavy: with the full Galexie
> archive, why do 3 contracts emit `ranges: null`?

## Headline findings

1. **The walker is not broken.** Every real mainnet anchor used by
   `internal/sources/*` is captured by the existing walk:

   | Source | Anchor | Walker output |
   |---|---|---|
   | Soroswap factory | `CA4HEQTL…` | 6 ranges, 1 unique WASM (TTL-restamps to same hash) |
   | Soroswap router | `CAG5LRYQ…` | 1 range, 1 unique WASM |
   | Aquarius router | `CBQDHNBF…` | 10 ranges, 6 unique WASMs |
   | Phoenix factory | `CB4SVAWJ…` | captured |
   | Phoenix multihop | `CCLZRD4E…` | 3 ranges, 3 unique WASMs |
   | Reflector mainnet × 3 | `CALI2BYU…`, `CAFJZQWS…`, `CBKGPWGK…` | captured (v2 → v3 transition) |
   | Comet pool | `CAS3FL6T…` | 1 range, 1 unique WASM |
   | Redstone adapter | `CA526Y2N…` | 2 ranges (hotfix → production) |
   | Band StandardReference | `CCQXWMZV…` | 1 range, 1 unique WASM |

2. **The 3 `ranges: null` "TTL-evicted" contracts previously flagged are
   Reflector TESTNET addresses**, absent on mainnet (cross-checked against the
   Reflector testnet contract table + `internal/sources/aquarius/events.go:37`,
   the real Aquarius router):

   | Address | Previously claimed | Actually |
   |---|---|---|
   | `CAVLP5DH…` | "Aquarius router" | Reflector **testnet** "Stellar Mainnet DEX" oracle |
   | `CCYOZJCO…` | "Aquarius admin" | Reflector **testnet** "External CEXs & DEXs" oracle |
   | `CCSSOHTBL…` | "Phoenix multihop" | Reflector **testnet** "Fiat exchange rates" oracle |

   They were in the 532-contract input list because the Reflector audit walked
   legacy/testnet addresses; remove them on the next walk.

3. **Coverage is complete** for all 7 mainnet protocols in scope (Soroswap,
   Aquarius, Phoenix, Reflector, Comet, Redstone, Band). Blend remains pending
   the full-history walk.

## Walker filter chain (mapped end-to-end)

Positive-match path through one `LedgerCloseMeta`
(`cmd/stellarindex-ops/main.go:2865-2987`):

```
LedgerCloseMeta → V == 1 ? else skip
  ↓
v1.TxProcessing[].TxApplyProcessing → V3 or V4 ? else skip
  ↓
{V3,V4}.Operations[].Changes (per-op LedgerEntryChanges)
  ↓
change.Type ∈ {Created, Updated, Restored} else skip
  ↓
entry.Data.Type == ContractData else skip
  ↓
cd.Key.Type == ScValTypeScvLedgerKeyContractInstance else skip
  ↓
cd.Contract.Type == ScAddressTypeScAddressTypeContract else skip
  ↓
cd.Contract.ContractId in watch[] else skip
  ↓
cd.Val.Type == ScValTypeScvContractInstance else skip
  ↓
inst.Executable.Type == {Wasm, StellarAsset}
  ↓
recordWasmTransition(contract, hash, ledger)
```

**Implications**:

- A contract whose initial `Created` change falls inside the walked range
  produces at least one range entry.
- A contract in `watch[]` never touched during the walk emits `ranges: null`
  (correct).
- The walker does **not** track: removals (`LedgerEntryRemoved` carries a
  LedgerKey, not bytes); `LedgerEntryState` (redundant pre-image);
  `TxChangesBefore` / `TxChangesAfter`; other LedgerEntry types (Account,
  Trustline, Offer, LiquidityPool, ContractCode, etc.); other ContractData
  storage keys (what storage-rotation tracking would need); Soroban `events`
  (the event-decoder's domain).

## What the wide-net walk should cover

### A. Curated input list refresh

The 532-contract input from the 2026-04-30 walk had three issues:

1. **3 testnet Reflector addresses** (`CAVLP5DH…`, `CCYOZJCO…`,
   `CCSSOHTBL…`) — remove.
2. **Pair-set completeness uncertain.** 194 Soroswap pairs were walked, but no
   live `all_pairs_length()` query confirmed that is the full set.
3. **Comet pool enumeration** — only 1 known mainnet pool (the Blend
   backstop); standalone Comet DEX pools, if any, are invisible. Stellar.expert
   or a contract-deploy-event walk against Comet's pool factory (if one exists)
   would surface them.

Action: refresh `configs/audit/wasm-walk-contracts.yaml` per source via the
documented `provenance:` enumeration calls before the next walk:

| Source | Refresh command |
|---|---|
| Soroswap | `simulateTransaction` with `all_pairs(i)` + `all_pairs_length()` against factory `CA4HEQTL…` |
| Aquarius | `get_pools_for_tokens_range` paginated against router `CBQDHNBF…` |
| Phoenix | `query_pools()` against factory `CB4SVAWJ…` |
| Reflector | hand-curated 3-contract mainnet set; remove testnet addresses |
| Comet | scan for `CreateContract` ops referencing Comet WASM hash `8abc28913035c074…` |
| Redstone | single Adapter, no enumeration |
| Band | single StandardReference, no enumeration |
| Blend | scan `deploy` events on Pool Factory `CDSYOAVX…` |

### B. Walker capability expansion (future-event coverage)

Each is a different filter on the same `LedgerCloseMeta` stream; none need
new ingest infrastructure.

| Category | LedgerEntry shape | Why we'd want it |
|---|---|---|
| **Storage rotations** | `ContractData` with non-`ScvLedgerKeyContractInstance` keys | Catches admin storage flips like Soroswap's `set_pair_wasm` (outstanding step 3 from the soroswap.md v2 follow-up) and factory parameter changes (fee_to_setter, fees_enabled, etc.) |
| **Contract code uploads** | `ContractCode` entries | Catches `UploadContractWasm` independent of instance transitions; preserves WASM bytes for hashes referenced but not yet seen on an instance (currently Soroban-RPC; an archive walk is a fallback for TTL-evicted hashes) |
| **Contract removals** | `LedgerEntryRemoved` of `ContractData` | Shows when an instance entry is destroyed (rare, e.g. proxy migration); currently invisible |
| **Events emitted** | Soroban `txMeta.SorobanMeta.Events` | Captures ALL contract events in range, for retroactive coverage when we add sources / event types (liquidations, governance, reward emissions) |
| **Account changes** | `Account`, `Trustline`, `LiquidityPool` | Already covered by the LCM-AccountEntry observer (5e94ba76e) and classic-supply observers (3e215c2e2..029849a62). Not in scope for the wasm walker |

The first three are within-scope additions (new cases in
`scanLedgerEntryChange`).

**Status:** all three ship in `wasm-history`
(`internal/ops/archive/wasm_history.go`). Storage rotations write to
`-storage-rotations-out` and code uploads write to `-code-uploads-out`. An
instance-entry removal on a watched contract closes its hash range and
opens a `removed` range in the main timeline.

The fourth (events emitted) is essentially a generic "contract event archive"
and out of scope unless explicitly wanted.

### C. Universe enumeration (out-of-scope contracts)

A complementary pass would scan the archive for ALL contracts emitting ANY
event and bucket them by known WASM hash (one of our 52, already covered) vs
unknown (a new contract). An unknown set surfacing (a) a new pool factory,
(b) a new oracle, or (c) a new SEP-41 token of meaningful volume signals a
source to add. Cost: probably 4-6× the current walk runtime, needed only
quarterly.

## Recommended next walk scope

**Tier 1 (must, before any backfill replay)**:

1. Refresh `configs/audit/wasm-walk-contracts.yaml` per the table above. Drop
   the 3 testnet Reflector addresses; add any Soroswap pairs missing from the
   194; verify Aquarius 313 + Phoenix 11 are still complete.
2. Run a full-history walk (`from = 50457424, to = current-tip`) against the
   refreshed list with `parallel = 8`. Expected runtime ~10h (2026-04-30 timing).
3. Include Blend's 11 contracts — the 60M-62.3M narrow walk emitted only
   `ranges: null` (no transitions in that 3-week window), so Blend's full
   upgrade history is uncaptured.

**Tier 2 (recommended)**:

4. Add the storage-rotation case (catches Soroswap `set_pair_wasm`, fee
   changes, etc.): new cases in `scanLedgerEntryChange` matching `ContractData`
   with keys other than `ScvLedgerKeyContractInstance`.
5. Add a `ContractCode` observer: archives bytes at upload and gives an
   archive-only fallback for TTL-evicted hashes, ending reliance on Soroban-RPC
   for live state.

**Tier 3 (optional)**:

6. Universe-enumeration pass: scan all contract events in range, bucket by
   WASM hash, surface unknown hashes for review.

## What we DON'T need to do

- **Don't extend TTL on the 3 testnet Reflector addresses**; their
  `ranges: null` is correct.
- **Don't worry about the original 50.4M start ledger being before P20
  launch**; the walker handles the empty pre-P20 span.
- **Don't widen `change.Type` to include `Removed`/`State`**: Removed carries
  no entry bytes; State is redundant with Created/Updated.
- **Don't change `cd.Key.Type` to a wildcard**: 100× the hot-path matches; use
  a separate storage-rotation walker (Tier 2) on specific factory storage keys.

## Action items

1. ☐ Refresh `wasm-walk-contracts.yaml` via per-source `simulateTransaction`
   enumeration calls (~1h operator).
2. ☐ Implement Tier 2 walker enhancements (storage-rotation + ContractCode
   observer) (~1 day eng).
3. ☐ Run wide-net walk on r1 against the refreshed list with the enhanced
   walker (~10-12h wall, mostly background).
4. ☐ Refresh the synthesis docs from the new walk output.
