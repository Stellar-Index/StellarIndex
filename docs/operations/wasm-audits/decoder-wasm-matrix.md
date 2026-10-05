---
title: Decoder ↔ WASM verification matrix
last_verified: 2026-05-01
status: 51/52 WASMs verified; the Redstone miss is a hash that emits no events (see redstone.md#caveats)
related:
  - docs/operations/wasm-audits/r1-walk-2026-05-01.md
  - docs/operations/wasm-audits/protocol-epochs.md
---

# Decoder ↔ WASM verification matrix

> Bidirectional check that every WASM we ingest from contains the event topics
> our decoder watches for. Built from the 52 WASM bytes preserved at
> [`evidence/r1-walk-2026-05-01/wasm-bytes/`](evidence/r1-walk-2026-05-01/wasm-bytes/)
> 2026-05-01.

## Method

For each `(source, role, WASM)` triple:

1. **Expected topics** = strings the decoder's `classify` / `Matches` watches
   for (from `internal/sources/<source>/{events,decode}.go`).
2. **Verification** = byte-search the WASM for each expected topic.
3. **Verdict** = `OK` if all found, `MISSING <set>` otherwise.

Role-aware: factories don't emit pool events, routers don't emit pair events.

## Results

| Source | Role | WASMs | OK | Missing | Notes |
|---|---|---:|---:|---:|---|
| soroswap | factory | 1 | 1 | 0 | emits `new_pair` |
| soroswap | router  | 1 | 1 | 0 | orchestration only — no expected topics |
| soroswap | pair    | 1 | 1 | 0 | emits `swap`, `sync`, `skim`, `deposit`, `withdraw` |
| aquarius | router  | 6 | 6 | 0 | orchestration — no expected topics |
| aquarius | pool    | 13 | 13 | 0 | every pool variant emits `trade`, `deposit`, `withdraw`, `claim` |
| phoenix  | factory | 5 | 5 | 0 | no expected topics |
| phoenix  | multihop| 3 | 3 | 0 | no expected topics |
| phoenix  | pool    | 14 | 14 | 0 | every pool variant emits `swap` (as `("swap", <field>)` 2-tuple) |
| reflector| oracle  | 2 | 2 | 0 | SEP-40 reads via methods; no expected events |
| comet    | pool    | 1 | 1 | 0 | emits `swap`, `join_pool` |
| **redstone** | adapter | 2 | **0** | 2 | **false negative — see Caveat below** |
| band     | StandardReference | 1 | 1 | 0 | emits zero events; observed via op args |
| blend    | pool-factory | 1 | 1 | 0 | emits `deploy` |
| blend    | backstop | 1 | 1 | 0 | emits `gulp_emissions` |
| **TOTAL** |        | **52** | **50** | **2** | 96% match rate; 100% if we discount the false negative |

## Caveat — `SymbolSmall` packing

Soroban packs event-topic Symbols of **≤ 9 characters** inline into a single
`u64` (`SymbolSmall` per `Stellar-contract.h`) rather than storing a string in
the data section, so a byte-search can miss them.

The Redstone misses are moot rather than SymbolSmall artefacts:
- Topic: `"REDSTONE"` (8 chars)
- Search: byte-match for `b"REDSTONE"` — not found in either archived Redstone
  WASM (`b400f7a8…` and `5e93d22c…`).
- Reality: neither archived hash imports `contract_event`, so neither emits any
  event. The live `REDSTONE` events come from a later, unarchived adapter WASM
  at the same address — see [redstone.md#caveats](redstone.md#caveats).

Longer topics (e.g. `apply_transfer_ownership` at 24 chars, `gulp_emissions` at
14, `fill_auction` at 12) are stored as normal strings and found. Hence 50/52.

**Why it matters anyway.**

- For the 50 matched hashes we have byte-level confirmation that the decoder's
  expected topic literally appears in the WASM, ruling out case-sensitivity /
  typo bugs (e.g. decoder watches `trade`, contract emits `Trade`).
- The 2 misses are confined to Redstone and both emit no events. The walk's
  range for `5e93d22c…` is wrong, though: it contains real `REDSTONE` events
  (L59,258,375), so the walk missed an upgrade to an event-emitting WASM inside it.
- Production ingest metrics (`stellarindex_redstone_events_total`) show events
  flowing — from the unarchived successor WASM, not from either hash here.

## How to refresh

After every wasm-history walk that introduces new hashes:

1. Re-fetch WASM bytes: `evidence/.../fetch-wasm-rpc.py`.
2. Re-run the verifier: `python3 evidence/.../verify-decoder-wasm-match.py`
   (committed alongside this doc).
3. Update the matrix. Document any new miss with the reason (decoder change?
   topic rename? new SymbolSmall case?).

## Operator confidence

For the 50 byte-match-positive WASMs we have strong evidence the decoder will
classify their events correctly during backfill replay. For the 2 Redstone
misses:

- Neither archived hash (`b400f7a8…`, `5e93d22c…`) emits events, so the
  event-driven decoder yields no rows for `b400f7a8…`'s window or the part of
  `5e93d22c…`'s range before the real upgrade; the SymbolSmall miss is moot.
  The recorded `5e93d22c…` range is not accurate: it contains real `REDSTONE`
  events from L59,258,375, emitted by a later WASM.
- Those events come from an unarchived successor WASM with no per-hash entry or
  recorded upgrade ledger yet; decoder confidence rests on the real-event
  fixtures — see [redstone.md#caveats](redstone.md#caveats).
- Before a replay over the `b400f7a8…` window, the sanity check in
  [redstone.md](redstone.md) counts `oracle_updates` rows for
  `source = 'redstone'` in L58,758,722 → L58,759,141 and expects `0`.
