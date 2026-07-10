# classicmovements

Reconstructs pre-P23 classic-Stellar asset movements from the
ClickHouse raw lake (ADR-0047). Not Horizon (ADR-0001), not a MinIO
walk (ADR-0034) — reads `stellar.operations` / `operation_results`
(and, from a later phase, `ledger_entry_changes`).

See [docs/adr/0047-pre-p23-classic-movement-reconstruction.md](../../../docs/adr/0047-pre-p23-classic-movement-reconstruction.md)
and [docs/architecture/pre-p23-classic-movements-research.md](../../../docs/architecture/pre-p23-classic-movements-research.md)
for the full decision + evidence base.

## What this ingests (Phase 1)

| Operation | Op type code | Movement kind |
| --- | --- | --- |
| `Payment` | `OperationTypePayment` | `payment` |
| `CreateAccount` | `OperationTypeCreateAccount` | `create_account` |

Both reconstruct from the operation **body** alone once the
operation **result**'s success code is confirmed (research §2 path
(a)) — neither needs `ledger_entry_changes`. A failed op (bare
result code that never reached the op's own result union, OR an
inner union whose own code is a failure) decodes to **zero**
movements, never an error.

Later phases (not yet implemented) add: path payments (Phase 2);
claimable-balance create/claim/clawback + `Clawback` (Phase 3);
account merge + liquidity-pool deposit/withdraw + the CAP-0038
trustline-revocation edge case (Phase 4, gated on the
`ledger_entry_changes` backfill). Migration 0105's schema already
admits all ten `movement_kind` values, so none of that needs a new
migration — see `doc.go`.

## Quirks

### Q1 — No events, just op body + result XDR

Same shape as `internal/sources/sdex`: this package decodes raw
`xdr.Operation` + `xdr.OperationResult`, not a Soroban `events.Event`.
`Decoder` implements `dispatcher.OpDecoder`.

### Q2 — Historical-only, never live-wired

Post-P23 every classic movement already emits a unified CAP-67 event
(`internal/sources/sep41_transfers`). This package's `Decoder` is
therefore **never** registered with the live dispatcher — its only
caller is `stellarindex-ops classic-movements-backfill`
(`internal/ops/chops`), which hard-clamps its ledger range below the
P23 boundary (58,762,517) regardless of what an operator requests.
`MovementEvent` has no persist arm in `internal/pipeline/sink.go`'s
`HandleEvent` by design — see that file's sibling
`lockstep_ast_test.go`'s `notSunkEvents` entry.

### Q3 — The `Kind`/`Provenance` enums are wider than what's decoded

Migration 0105's `movement_kind` CHECK admits all ten ADR-0047 D1
kinds and both `provenance` values from day one, so Phases 2-4 add
decode arms, not schema churn. `recognition_test.go` pins
`Matches()` / `SupportedOpTypes()` / `decodeOp`'s switch to exactly
Phase 1's two kinds and asserts every other value in the closed
27-value `xdr.OperationType` enum is rejected loudly
(`ErrUnsupportedOpType`) rather than silently producing zero rows —
the forcing function for a future phase's author.

### Q4 — Op-level source resolution happens upstream

`clickhouse.StreamClassicOps` (the CH reader `stellarindex-ops
classic-movements-backfill` uses) reads `stellar.operations.source_account`,
which the ClickHouse extractor already resolves to "op's own
`SourceAccount` if set, else the tx source" — the caller wires this
straight into `dispatcher.OpContext.TxSource` and leaves `OpSource`
empty, exactly as `ch-rebuild`'s SDEX pass does. `Decoder.Decode`
therefore reads `ctx.TxSource` directly as the movement's
`FromAddress` with no fallback logic of its own.

## Files

| File | Role |
| --- | --- |
| [`doc.go`](doc.go) | Package overview: phase scope, historical-only rationale, serving + retention deferrals |
| [`events.go`](events.go) | `SourceName`, `Kind`/`Provenance` enums, `Movement`, `MovementEvent`, decode error sentinels |
| [`decode.go`](decode.go) | `SupportedOpTypes` / `matchesPhase1Op` / `decodeOp` / per-kind decoders |
| [`decode_test.go`](decode_test.go) | Synthetic-fixture unit tests (success, both failure shapes, malformed-amount defensive path) |
| [`real_bytes_test.go`](real_bytes_test.go) | Real pre-P23 mainnet bytes pulled from r1's ClickHouse lake, byte-for-byte golden assertions, incl. a real failed-payment negative case |
| [`recognition_test.go`](recognition_test.go) | ADR-0047 D4.2 recognition guard: exhaustive closed-enum switch-coverage test |
| [`dispatcher_adapter.go`](dispatcher_adapter.go) | `Decoder` — the `dispatcher.OpDecoder` implementation `classic-movements-backfill` calls |

(No `consumer.go` — like SDEX, this source has no live-stream
consumer of its own; its only caller is the backfill command.)

## Operational notes

- **Class**: not an `external.Registry` source — this is a purely
  on-chain, historical, lake-derived writer. No `BackfillSafe` flag
  applies (that flag gates Soroban WASM-upgrade risk; classic
  operation semantics don't change across a protocol version the way
  a Soroban contract's bytecode does).
- **Backfill**: `stellarindex-ops classic-movements-backfill -config
  PATH -from N -to N [-window N] [-resume] [-write]`. Windowed,
  resumable, idempotent (PK's `ON CONFLICT DO NOTHING`). Always run
  under `/usr/local/sbin/run-heavy-job.sh` for anything beyond a
  small range (CLAUDE.md heavy-job doctrine).
- **Serving**: none yet — write-path only (see `doc.go`).

## References

- ADR-0001 — "Horizon is not in our architecture."
- ADR-0034 — ClickHouse raw lake / Postgres served tier split.
- Related source: [`sdex`](../sdex/README.md) — the precedent for a
  classic-op decoder living outside the projector.
