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

## 4a. Validation result (2026-06-05, `ch-reproject` on 62,700,000–62,710,000)

`ratesengine-ops ch-reproject` re-derives a range from the CH lake with the
existing decoders and diffs against the served Postgres tables. Run on the
dense partition-62 sample:

- **Decoders re-derive identically from CH** where the served tables are
  complete: `reflector-dex/cex/fx` (7872 / 2723 / 3456 — exact), `comet/trades`
  (16), `blend_auctions` (17). This proves the input-adapter thesis: CH rows
  feed the decoders and reproduce the served output exactly.
- **CH recovers silently-dropped / never-projected rows** (the migration's
  point): `aquarius/trades` 3143 vs 1947 served (**+61%**), `blend_positions`
  +162, `comet_liquidity` +13, `phoenix/trades` +6, and whole sources absent
  from the served tables — `defindex_flows` 254 vs 0, `blend_emissions` 19 vs 0,
  `cctp_events` 8 vs 0. aquarius is 1 event → 1 trade (no correlation) and CH's
  event totals are census-verified, so CH cannot over-count here — the served
  table genuinely under-counts (the `event_index` collision class).
- **Two CH-side items:**
  - `redstone` 0 vs 474 — was a real gap: the extractor left `op_args_xdr` nil
    and redstone needs op-args (feed_ids live in the `write_prices` op args, not
    the event body). **FIXED 2026-06-05** (`opArgsByIndex` in extract.go):
    re-backfilling 62.7M with op-args, `ch-reproject` re-derives redstone
    474 == 474 served. The op-args binary was deployed to the live backfill at
    window 32, ahead of the redstone era (ledger 58.7M ≈ window 58), so the
    forward walk captures it with no re-backfill. (Windows 2–31 lack op-args but
    pre-date redstone/band, which is the only op-args consumer.)
  - `soroswap` 0 vs 266 — `ch-reproject` runs the soroswap decoder unseeded
    (no RPC pair registry), so it can't resolve pre-range pairs. Tool
    limitation, not a lake gap; seed the decoder (as verify-reconciliation
    does) to compare soroswap.

Tooling note: each oracle variant shares one `EventKind` but routes under a
distinct source filter, so `ch-reproject` buckets re-derived output **per
source** (and applies each source's `contractIDs` prefilter) — otherwise the
three reflector variants merge into one count.

## 5. Sequencing / non-goals

- Built **after** Phase 3 (full historic backfill) is census-verified, so the
  adapter reads a complete lake.
- This doc is the input-adapter (read CH → decoders). The Postgres
  right-sizing (CAGGs + bounded recent window, drop the 2.89 B-row `trades`)
  and the live dual-sink are the rest of Phase 4, tracked separately.
- `ledger_entry_changes` population is a prerequisite for the supply-observer
  re-derivation and is its own small Phase-2 follow-up.
