# W24 — Contract schema evolution + WASM history

## Scope

Every Soroban contract upgrade risk.

## Inputs

- `docs/architecture/contract-schema-evolution.md`
- `docs/operations/wasm-audits/`: per-source audit log
- `docs/operations/wasm-audits/decoder-wasm-matrix.md`
- `docs/operations/wasm-audits/protocol-epochs.md`
- `migrations/0017_create_wasm_history.up.sql`
- `cmd/ratesengine-ops/wasm_extract.go`,
  `wasm_history.go`
- `cmd/ratesengine-ops/extract-wasm-from-galexie` (NEW),
  `wasm-history-merge-jsonl` (NEW)
- `internal/sources/external/registry.go` `BackfillSafe`

## Checks

| # | Check | Method |
| --- | --- | --- |
| W24.1 | Architecture doc invariants honoured by every decoder (read-by-name, topic[0] symbol not contract id) | per-decoder |
| W24.2 | Per-source wasm-audits docs exist for: aquarius, band, blend, comet, phoenix, redstone, reflector, soroswap, cctp (NEW), rozo (NEW) | ls |
| W24.3 | decoder-wasm-matrix.md current | doc |
| W24.4 | protocol-epochs.md current | doc |
| W24.5 | migration 0017 wasm_history schema; reader/writer | code |
| W24.6 | wasm-history subcommand: walks ledgers, extracts wasm_hash per contract | code |
| W24.7 | extract-wasm-from-galexie: NEW; pulls wasm bytecode from galexie partition | code |
| W24.8 | wasm-history-merge-jsonl: NEW; merges per-chunk JSONL outputs | code |
| W24.9 | BackfillSafe flag matrix: true only for sources with completed wasm audits | registry.go vs docs |
| W24.10 | NEW: 2026-05-26 audit walk on CCTP + Rozo (zero upgrades observed; BackfillSafe flipped true) — re-test cold | wasm audit re-walk on r1 |
| W24.11 | NEW: every Soroban source's audit doc records its WASM hashes + audit decision SQL queries | per-doc |

## Closure criteria

Every Soroban source's audit doc terminal. Findings on:
- any source with `BackfillSafe=true` but no audit doc
- any source whose decoder reads-by-position not -by-name
  (breaks under upgrades)
- any source whose decoder dispatches by contract-id not by
  topic-symbol (breaks under shared-topic-namespace sources like
  Comet)
