---
name: add-onchain-source
description: Add a new on-chain Soroban source (DEX/lending/bridge/oracle) to Stellar Index — the six-file package, the six wiring edits, contract gating, and the executable checks that catch a missed edit. Use when integrating a new Stellar protocol, adding a decoder, or when a source "compiles but emits nothing".
---

# /add-onchain-source

The canonical checklist is `docs/contributing/add-onchain-source.md`
— READ IT FIRST; this skill adds the execution order, the gating
decision, and the machine checks. Template package:
`internal/sources/soroswap/`.

## 0. Before writing any code

1. `CAPABILITY-INVENTORY.md` — the helper you're about to write
   exists (scval decode, Amount, cache keys, census helpers).
2. Decide the **dispatcher seam** by what the contract emits:
   events → `Decoder`; no events but InvokeContract state writes →
   `ContractCallDecoder` (the band pattern); classic ops →
   `OpDecoder`; LedgerEntry deltas → `LedgerEntryChangeDecoder`.
3. Decide the **gate** (ADR-0035/0040 — topic-only matching is
   FORBIDDEN for new sources): factory-descended registry, curated
   set (`childgate.WithSeed`), or WASM-hash. Get the contract set
   from the lake, not docs — and write the
   `docs/protocols/<name>.md` page in the same PR.
4. Reverse-engineer event schemas FROM THE LAKE (CH `contract_events`
   topics_xdr/data_xdr for real ledgers), not from protocol docs —
   Soroban contracts upgrade in place; decode Map-by-field-name,
   dispatch on topic[0] symbol (see
   docs/architecture/contract-schema-evolution.md).

## 1. The package (six files) + wiring (six edits)

Files: `README.md`, `events.go`, `decode.go`, `consumer.go`,
`dispatcher_adapter.go` (the seam production actually calls),
`source_test.go` (golden tests from lake-captured fixtures under
`test/fixtures/<venue>/<wasm-hash>/`).

Wiring edits — miss one and the source silently emits nothing:
1. `internal/config` KnownSources
2. `internal/pipeline/dispatcher.go` BuildDispatcher
3. `internal/pipeline/sink.go` HandleEvent (persist arm)
4. `internal/pipeline/sink.go` IsProjectedEvent
5. `internal/projector/registry.go` buildSource
6. `internal/sources/external/registry.go` Metadata
(+ a `reconSource` entry in
`cmd/stellarindex-ops/reconciliation_catalogue.go` so the ADR-0033
verdict covers it, + migration for the table — see
docs/contributing/add-migration.md.)

## 2. Machine checks (the point of this skill)

```sh
go test -run TestLockstep ./internal/pipeline/        # catches missed wiring edits 3/4/5 (F-1316 class)
go test -run TestReconciliationCatalogue ./cmd/stellarindex-ops/
go test ./internal/sources/<name>/ ./internal/pipeline/ ./internal/projector/
go run ./scripts/ci/lint-pk-discriminators            # new table's PK has a per-event discriminator
bash scripts/ci/lint-imports.sh                       # no xdr outside scval, no rpc in ingest
```

Golden-test the REJECT path too: a foreign contract emitting your
topic shape must NOT match (this is the gate working).

## 3. Non-negotiables

- Amounts: `scval.AsAmountFromI128/U128` → `canonical.Amount` —
  `int64(parts.Lo)` is rejected in review every time (ADR-0003).
- New projected source's cursor inits near genesis — fast-forward to
  tip on deploy or it crawls empty history (blend_backstop lesson).
- Deploy order: seed contracts on r1 BEFORE the gated binary, then
  lake re-derive (`projector-replay`), then one green
  `compute-completeness -ch -source <name>` cycle (ADR-0040 §2).
- Event dedup: multi-event ops need `EventIndex` in the PK
  (Phoenix emits 8/swap; event_index=0 silently collapsed them once).

## 4. Finish

CHANGELOG entry in the same commit; `docs/protocols/<name>.md` page;
then run **/verify-done**.
