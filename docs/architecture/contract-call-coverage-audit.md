---
title: Contract-call coverage audit
last_verified: 2026-05-21
status: point-in-time audit
---

# Contract-call coverage audit (2026-05-21)

> **Note (audit 2026-06-12):** point-in-time audit snapshot;
> `last_verified` is its capture date, not a fresh re-verification.

> Comparing per-contract activity on Stellar Expert vs our internal
> `source_entry_counts` to find decoder coverage gaps. Companion to
> [storage-considerations.md](storage-considerations.md) — both came
> out of the post-#44 audit work.

## Methodology

Script: `coverage-audit` (see `feedback_window_snapshots_not_state.md`
for the lesson on why a single-window check isn't enough).

For each Soroban contract we ingest, fetch:

- Stellar Expert metadata (free endpoint, returns `subinvocation` and
  `events` totals across the contract's lifetime)
- Our internal `source_entry_counts.entry_count` for that source

Compute `gap = SE_events / our_entries` as a rough undercount factor.
Caveats per-contract — factories emit few events themselves, child
pool contracts emit independently, etc. The ratio is informational
unless the contract being tracked IS the emitter (e.g. ContractCallDecoder
sources).

## Results — 2026-05-21

```
source             label                  SE_subinv     SE_events    our_entries   gap
soroswap           factory                    3,324           196        389,339   0.0x  ✓ (child pairs)
soroswap-router    router                   197,830       192,046             22  8729x  ✗ DECODER GAP
aquarius           router                   449,326     1,954,797      1,998,022   1.0x  ✓ matched
phoenix            factory                   51,925            18        195,199   0.0x  ✓ (child pools)
phoenix            multihop                   2,159             3        195,199   0.0x  ✓ (child pools)
blend              pool_factory                  11             9          8,240   0.0x  ✓ (child pools)
blend/comet        backstop                  16,277        48,054          8,240   5.8x  ⚠ incomplete backfill
defindex           factory                       31           106            143   0.7x  ✓ caught
reflector-dex      dex                       32,913        20,355        419,192   0.0x  ✓ caught
reflector-cex      cex                       85,691        20,239        297,574   0.1x  ✓ caught
reflector-fx       fx                       104,288        20,391        366,966   0.1x  ✓ caught
redstone           adapter                       35       194,454         42,749   4.5x  ⚠ incomplete backfill
band               standard_reference             0             0          4,455   n/a   ✓ (events not emitted)
cctp               TokenMessenger               176           248              0   miss  ⚠ Phase-1 decoder not yet wired
cctp               MessageTransmitter           179           218              0   miss  ⚠ Phase-1 decoder not yet wired
cctp               CctpForwarder                  0            95              0   miss  ⚠ Phase-1 decoder not yet wired
rozo               payment                        0           140              0   miss  ⚠ Phase-1 decoder not yet wired
```

## Findings

### 1. Real decoder gap: soroswap-router (8,729× undercount) — task #48

`internal/dispatcher/dispatcher.go::extractInvokeContractCalls`
walks only `tx.Envelope.Operations()` (top-level operations). It
never walks sub-invocations. Most soroswap users (sampled at 6/10)
hit the router via an aggregator contract (`yeet` function in our
sample) that wraps the router as a sub-invocation — so the
ContractCallDecoder never sees the router call.

**Confirmation evidence (three independent lines):**

- **Sample (Horizon-traced):** 6/10 recent soroswap pair trades have an aggregator top-level wrapping the router. Decoder doesn't see those.
- **Aggregate (Stellar Expert):** 192,046 events emitted from router's call tree over 14 months → ~38-48k user swaps; we caught 22.
- **Code review:** `extractInvokeContractCalls(tx.Envelope.Operations())` — top-level only, never the auth tree or diagnostic events.

### 2. Incomplete-backfill candidates (NOT decoder bugs)

- **redstone adapter** — 4.5× under. Coverage gap relative to live events; same source's decoder works in principle (event-based). Genesis L58.7M; full coverage requires backfill catching up.
- **blend/comet backstop** — 5.8× under. Same pattern; genesis L51.5M.

Both are gated on **task #35** (Soroban-era multi-decoder backfill resume), which itself depends on **task #7** disk-headroom.

### 3. Phase-1 decoders not yet ingesting

- **cctp** (3 contracts, 0 entries each)
- **rozo** (1 contract, 0 entries)

Decoder packages exist (see `internal/sources/cctp/`, `internal/sources/rozo/`) but aren't wired into the indexer dispatcher or backfilled. Tasks #40 + #41 cover the wiring; storage shape decision (bridge_events vs per-source tables) blocks finalization.

### 4. Cleanly verified working

- **aquarius** — 1.0× ratio over ~2M events; essentially perfect coverage.
- **reflector-dex/cex/fx** — caught MORE than SE shows (per-feed sampling).
- **All factories** (soroswap, phoenix, blend pool_factory, defindex) — 0.0× ratio because we count child-pool/pair events, not factory events. Working as designed.
- **defindex** — 0.7× ratio. We're catching most BlendStrategy events.
- **band** — 0 SE events (Band emits no events by design), 4,455 caught calls via ContractCallDecoder. Working because Band tends to be called directly, not via aggregators (unlike soroswap-router).

## Implementation plan for task #48

### Design

The Stellar SDK exposes the call tree via TWO sources:

| Source | What | Reliability |
|---|---|---|
| `op.Body.MustInvokeHostFunctionOp().Auth[i].RootInvocation` + `.SubInvocations[]` (recursive) | The auth tree the user signed for | What was authorized; doesn't always reflect actual execution |
| `tx.UnsafeMeta.V3.SorobanMeta.DiagnosticEvents` with `event_type == System` + topic `[fn_call, contract_address, function_name]` | Actual execution trace | What actually happened — canonical |

For dispatcher purposes, **diagnostic events are the right source**. They reflect every contract call that actually executed, regardless of whether it was top-level or nested, authorized via root or sub-auth, etc.

### Code change

1. **New walker:** `extractAllContractCalls(tx)` returns `[]ContractCallContext` — one entry per `fn_call` diagnostic event. Includes top-level (no longer extracted separately) + every nested call.
2. **Wire into Dispatcher.Run():** Replace the existing `extractInvokeContractCalls(tx.Envelope.Operations()) → dispatchContractCall(...)` loop with a call-tree walk via the new function. Top-level still gets its OpIndex from the operation; nested calls get a synthetic OpIndex of the parent op + an additional CallPath identifier for dedup.
3. **Widen Event types:** Add `CallPath []int` (e.g. `[0, 2, 1]` = "op 0, then 3rd inner call, then 2nd inner call") to `RouterSwap` and any other ContractCallDecoder source. The full identifier becomes `(TxHash, OpIndex, CallPath)`.
4. **Backward-compat:** event-based decoders unaffected (they listen to `tx.GetTransactionEvents()` which already includes events from nested contracts).

### Tests

- Fixture: a real LCM containing a known router-via-aggregator transaction (use `e80fde59...` from this session's investigation). Assert decoder fires once.
- Fixture: a direct-router transaction. Assert decoder fires once.
- Fixture: a multi-hop router transaction with N pair sub-calls. Assert decoder fires once for the router invocation (not N times for the inner pair calls — those aren't router calls).

### ADR

Needed: this widens the dispatcher contract. ContractCallDecoder.Matches() will be called for every call in the tree, not just top-level. Need to document the semantics and the dedup expectation (decoders should be idempotent in `Decode()` since the same (contract, function) signature might match nested context).

### Backfill replay

After fix lands and verifies on live tip:

1. Snapshot current `source_entry_counts` so we can compare delta.
2. Backfill across `[soroswap-router genesis L50,746,272, tip]` for the router decoder only — a focused 11M-ledger replay.
3. Validate: post-replay entries should land in the ~40-65k range matching the Stellar Expert event count (proportional).

This is heavy I/O at a 93% pool with snapshot held. **Gated on task #7's snapshot destroy** (post-7-day window) which frees ~7 TB.

## Sequencing

The audit confirms the gap but the fix is non-trivial:

```
NOW   (this session) ──► audit done, doc shipped, #48 task design captured
+0d   trim snapshot held, pool at 93%
+7d   snapshot destroy frees 7 TB, pool → ~43%
+7d   #48 implementation (design + walker + tests + ADR) — dedicated session
+8d   backfill replay across [L50.7M, tip] for router decoder
+9d   verify cross-check matrix: router entry count climbs to ~40-65k
+9d   #35 resume (other source backfills) with the same headroom
```

## Walker terrain check (2026-07-05) — what the lake has vs what the walker needs

Re-verified while building the lake-ordered MEV detectors (BACKLOG #28),
which share the "what does the lake actually carry?" question:

**Available today (and now consumed):**

- **Intra-ledger transaction ordering IS in the lake.**
  `stellar.transactions` / `stellar.operations` carry `tx_index`
  (application order within the ledger), and `stellar.tx_hash_index`
  (ORDER BY tx_hash) resolves hash → (ledger_seq, tx_index) as a PK
  point lookup. Any earlier "no intra-ledger ordering" claim is true
  only of the Postgres served tier. The MEV sandwich detectors consume
  this via `clickhouse.TxIndexReader` (`internal/storage/clickhouse/tx_index_reader.go`).
- **Nested-contract EVENTS are captured.** The extract
  (`internal/storage/clickhouse/extract.go::extractEvents`) reads
  `tx.GetTransactionEvents()`, which includes contract events emitted
  by sub-invoked contracts — event-based decoders already see the whole
  tree.
- **The auth tree is recoverable from the lake.**
  `stellar.operations.body_xdr` retains the full InvokeHostFunction op,
  so `Auth[i].RootInvocation` + `SubInvocations[]` can be walked from
  ClickHouse for ALL history — with the known caveat that the auth tree
  is what was *authorized*, not what *executed*.

**Genuinely absent — the walker's real gap:**

- **Diagnostic events (`fn_call` / `fn_return` — the execution call
  tree) are captured NOWHERE in the lake.** Two independent reasons:
  1. `extract.go::eventRow` gates on
     `xdr.ContractEventTypeContract` (matching
     `dispatcher.captureEligible`), so System/Diagnostic events are
     skipped even when present, and `tier1_schema.sql` has no
     `diagnostic_events` table.
  2. Upstream of us they mostly don't exist: stellar-core only emits
     diagnostic events into tx meta when
     `ENABLE_SOROBAN_DIAGNOSTIC_EVENTS = true`, and our galexie
     captive-core template
     (`configs/ansible/roles/archival-node/templates/stellar-core.cfg.j2`)
     does not set it — so the LCMs already in MinIO / the lake were
     captured without them. **This is not recoverable by re-reading the
     archive**; producing historical diagnostic events requires a
     replay with the flag on.

**Capture change the diagnostic-events walker needs (forward-only):**

1. Set `ENABLE_SOROBAN_DIAGNOSTIC_EVENTS = true` in the captive-core
   template (both galexie captives), via `configs/ansible/` in the same
   PR as the rollout (r1 ansible-drift rule). Note this inflates LCM
   size in the galexie bucket.
2. Add a `stellar.diagnostic_events` lake table + an extract arm that
   captures `event_type == System` topic `[fn_call, contract, fn]`
   rows (LCM path: `tx.UnsafeMeta.V3.SorobanMeta.DiagnosticEvents`;
   SDK also exposes `tx.GetDiagnosticEvents()`).
3. Dispatcher walker per the plan above dispatches ContractCallDecoders
   over that trace instead of `tx.Envelope.Operations()` only.

**Until then**, the honest historical option for the router undercount
is the auth-tree walk over `operations.body_xdr` (available for all
history today, authorized≠executed caveat documented), or accepting
forward-only coverage from the flag-flip date.

## References

- `internal/dispatcher/dispatcher.go` lines 430-494 — current top-level-only walk
- `internal/dispatcher/dispatcher.go` lines 807-881 — `extractInvokeContractCalls` implementation
- `docs/architecture/ingest-pipeline.md` — binding rules for the ingest path
- Stellar Expert API: `https://api.stellar.expert/explorer/public/contract/<C-strkey>` (free)
- SDK types: `SorobanAuthorizedInvocation` (auth tree), `SorobanTransactionMeta.DiagnosticEvents` (execution trace)
