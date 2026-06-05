---
title: Phase 4 design — ClickHouse → decoder input adapter (re-derive Postgres)
last_verified: 2026-06-05
status: design
---

# Phase 4 — ClickHouse → decoder input adapter

**Status: design.** Implements ADR-0034 Phase 4: re-derive the Postgres
semantic/pricing tier *from ClickHouse*, not from a second galexie walk. The
hard-won protocol decoders are **reused unchanged**; only their input source
moves from "LCM walked by the dispatcher" to "rows read from ClickHouse".

## 1. The load-bearing fact: CH rows ARE serialized decoder inputs

The Tier-1 extractor and the production dispatcher encode contract events the
same way — verified in code, not assumed:

| `events.Event` field | CH `contract_events` column | encoding (both sides) |
|---|---|---|
| `Type` | `event_type` | literal `"contract"` |
| `Ledger` | `ledger_seq` | uint32 |
| `LedgerClosedAt` | `close_time` | RFC3339 (format on read) |
| `ContractID` | `contract_id` | strkey `C…` |
| `OperationIndex` | `op_index` | int |
| `EventIndex` | `event_index` | int (the ADR-0033 collision fix) |
| `TxHash` | `tx_hash` | hex |
| `InSuccessfulContractCall` | `in_successful_call` | bool/uint8 |
| `Topic[]` | `topics_xdr[]` | `base64.Std(scval.MarshalBinary())` |
| `Value` | `data_xdr` | `base64.Std(scval.MarshalBinary())` |
| `OpArgs[]` | `op_args_xdr[]` | `base64.Std(scval.MarshalBinary())` |

Encoders that must agree:
- dispatcher `contractEventToEventsEvent` — `internal/dispatcher/dispatcher.go:857`
  (Topic/Value at :881/:907).
- CH extractor `eventRow` — `internal/storage/clickhouse/extract.go:167`
  (topics/data at :181/:206).

Both call `v0.Topics[i].MarshalBinary()` / `v0.Data.MarshalBinary()` then
`base64.StdEncoding.EncodeToString`. **Byte-identical.** So the adapter
reconstructs `events.Event` from a CH row with a plain field copy — no
re-encoding, no XDR re-touch — and feeds the existing decoders verbatim.

## 2. The four decoder classes and their CH source tables

| dispatcher interface | input | CH source | Phase-4 status |
|---|---|---|---|
| `Decoder` (event) | `events.Event` | `contract_events` | **ready** (schema populated) |
| `OpDecoder` (classic op) | `xdr.Operation` + result | `operations.body_xdr` + `operation_results.result_xdr` | ready (unmarshal base64) |
| `ContractCallDecoder` | contractID + fn + args | `operations` (InvokeContract) + `op_args` | ready |
| `LedgerEntryChangeDecoder` | `xdr.LedgerEntryChange` | `ledger_entry_changes` | **blocked** — table not yet populated (Phase 2 deferred it) |

Event-based decoders (soroswap, phoenix, comet, blend, reflector, redstone,
sep41, cctp, rozo) are the bulk and are unblocked now. SDEX + change_trust +
band (op / contract-call) read `operations`/`operation_results`. Supply
observers that key off `LedgerEntryChange` wait on populating
`ledger_entry_changes` (a Phase-2 follow-up: extend `ExtractLedger` to walk
`tx.GetChanges()` — the schema + row type already exist).

## 3. Architecture

```
ClickHouse stellar.contract_events ──► chEventReader ──► events.Event stream
  (FINAL, ORDER BY ledger,tx,op,event)         │
                                                ▼
                                  existing projector decoder set
                                  (internal/projector + internal/sources/*)
                                                │ []consumer.Event
                                                ▼
                                  internal/pipeline/sink ──► Postgres (Tier 3)
```

- **`chEventReader`** (new, `internal/storage/clickhouse`): streams
  `contract_events` rows for `[from,to]` ordered by
  `(ledger_seq, tx_hash, op_index, event_index)`, mapping each to
  `events.Event`. Uses `FINAL` (or partition-replace) for dedup. Streams in
  bounded batches so a full-history re-projection holds constant memory.
- **Decoder set:** reuse `internal/projector` registry's `buildSource` — the
  same `Matches()`/`Decode()` chain the live dispatcher runs. No decoder code
  changes.
- **Sink:** reuse `internal/pipeline/sink` to write `consumer.Event` →
  Postgres, into **new** right-sized tables (clean rebuild, §Phase 4 of the
  plan), never the old collided ones.

## 4. Validation (gate before cutover)

Re-projection correctness reuses the census/reconciliation oracle:

1. Over the backfilled sample, run the CH event-reader → decoders → count
   trades per ledger. Compare to `ch-gate`'s census `classic_trade_effect`
   (SDEX) and to the per-source recognition/reconciliation
   (`verify-reconciliation`, ADR-0033 Claim 2b) re-pointed at CH.
2. Assert no `(contract, topic[0])` shape is unrecognized
   (`verify-recognition` over CH `contract_events.topic_0_sym`).
3. Only after the rebuilt Postgres tables pass do we cut the API over and
   drop the old tables (plan §10e clean-cutover guarantee).

## 5. Sequencing / non-goals

- Built **after** Phase 3 (full historic backfill) is census-verified, so the
  adapter reads a complete lake.
- This doc is the input-adapter (read CH → decoders). The Postgres
  right-sizing (CAGGs + bounded recent window, drop the 2.89 B-row `trades`)
  and the live dual-sink are the rest of Phase 4, tracked separately.
- `ledger_entry_changes` population is a prerequisite for the supply-observer
  re-derivation and is its own small Phase-2 follow-up.
