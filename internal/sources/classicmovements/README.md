# classicmovements

Reconstructs pre-P23 classic-Stellar asset movements from the
ClickHouse raw lake (ADR-0047). Not Horizon (ADR-0001), not a MinIO
walk (ADR-0034) — reads `stellar.operations` / `operation_results`
(and, from a later phase, `ledger_entry_changes`).

See [docs/adr/0047-pre-p23-classic-movement-reconstruction.md](../../../docs/adr/0047-pre-p23-classic-movement-reconstruction.md)
and [docs/architecture/pre-p23-classic-movements-research.md](../../../docs/architecture/pre-p23-classic-movements-research.md)
for the full decision + evidence base.

## What this ingests (Phases 1-3)

| Operation | Op type code | Movement kind | Row cardinality |
| --- | --- | --- | --- |
| `Payment` | `OperationTypePayment` | `payment` | 1 row/op |
| `CreateAccount` | `OperationTypeCreateAccount` | `create_account` | 1 row/op |
| `PathPaymentStrictReceive` | `OperationTypePathPaymentStrictReceive` | `path_payment` | 1 row/op (leg_index always 0) |
| `PathPaymentStrictSend` | `OperationTypePathPaymentStrictSend` | `path_payment` | 1 row/op (leg_index always 0) |
| `CreateClaimableBalance` | `OperationTypeCreateClaimableBalance` | `claimable_balance_create` | 1 row/op |
| `ClaimClaimableBalance` | `OperationTypeClaimClaimableBalance` | `claimable_balance_claim` | 1 row/op (0 if unresolved — see Q5) |
| `ClawbackClaimableBalance` | `OperationTypeClawbackClaimableBalance` | `claimable_balance_clawback` | 1 row/op (0 if unresolved — see Q5) |
| `Clawback` | `OperationTypeClawback` | `clawback` | 1 row/op |

`Payment`/`CreateAccount`/`CreateClaimableBalance`/`Clawback`
reconstruct from the operation **body** alone once the operation
**result**'s success code is confirmed (research §2 path (a)) — none
need `ledger_entry_changes`. The two path-payment types reconstruct
from the operation **result** (path (b)): the row's `asset`/`amount`
columns hold the **destination** leg
(`result.Success.Last.{Asset,Amount}`, exact for both types);
`attributes.send_asset`/`attributes.send_amount` hold the **source**
leg — exact from the body (`SendAmount`) for StrictSend, derived from
the result's `Offers` for StrictReceive (`SendMax` is only a ceiling
— see `decode.go`'s `pathPaymentStrictReceiveSourceAmount` for the
hop-order derivation). `ClaimClaimableBalance`/`ClawbackClaimableBalance`
reconstruct via research's "b+own-index" path (b+own-index): neither
op carries an asset/amount, only a `BalanceId`, resolved against the
`CreateClaimableBalance` row this package itself derived earlier —
see Q5. Every kind above is exactly one row per op (`leg_index`
always 0) — none of these ops have a second asset leg, and the
per-hop `ClaimAtom` trade legs of a path payment stay in `trades` via
`internal/sources/sdex` and are never duplicated here. A failed op
(bare result code that never reached the op's own result union, OR
an inner union whose own code is a failure) decodes to **zero**
movements, never an error.

Later phase (not yet implemented): account merge + liquidity-pool
deposit/withdraw + the CAP-0038 trustline-revocation edge case
(Phase 4, gated on the `ledger_entry_changes` backfill). Migration
0105's schema already admits all ten `movement_kind` values, so none
of that needs a new migration — see `doc.go`.

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
kinds and both `provenance` values from day one, so Phase 4 adds
decode arms, not schema churn. `recognition_test.go` pins
`Matches()` / `SupportedOpTypes()` / `decodeOp`'s switch to exactly
this package's op-only in-scope kinds (Phases 1-3 today) and asserts
every other value in the closed 27-value `xdr.OperationType` enum is
rejected loudly (`ErrUnsupportedOpType`) rather than silently
producing zero rows — the forcing function for a future phase's
author.

### Q4 — Op-level source resolution happens upstream

`clickhouse.StreamClassicOps` (the CH reader `stellarindex-ops
classic-movements-backfill` uses) reads `stellar.operations.source_account`,
which the ClickHouse extractor already resolves to "op's own
`SourceAccount` if set, else the tx source" — the caller wires this
straight into `dispatcher.OpContext.TxSource` and leaves `OpSource`
empty, exactly as `ch-rebuild`'s SDEX pass does. `Decoder.Decode`
therefore reads `ctx.TxSource` directly as the movement's
`FromAddress` with no fallback logic of its own. **Exception:**
`Clawback` and `ClawbackClaimableBalance` flip this — `FromAddress`
is the holder (`Clawback`'s own `body.From`, a DIFFERENT account from
`ctx.TxSource`); `ToAddress` is `ctx.TxSource`, the issuer performing
the clawback (protocol-enforced: only the asset's issuer can submit
these ops). Confirmed against real mainnet data in
`real_bytes_test.go`'s `TestRealBytes_clawback_success`.

### Q5 — ClaimableBalance claim/clawback correlation: in-run index + Postgres fallback

Neither `ClaimClaimableBalance` nor `ClawbackClaimableBalance` carries
an asset or amount — only a `BalanceId`. `Decoder` keeps an in-memory
index of every `claimable_balance_create` it has itself decoded
(`dispatcher_adapter.go`'s `balances` map, keyed by the same hex
`balance_id` string this package writes to
`attributes.balance_id`) and resolves claims/clawbacks against it for
free. A claim/clawback the index can't resolve (create in an earlier,
already-completed backfill invocation, most commonly) is recorded as
a `PendingClaimableBalanceRef` instead of erroring —
`Decoder.TakePendingClaimableBalances()` drains that list.
`classic-movements-backfill` (`chops`) drains it once per streamed
window and resolves each pending ref in three tiers: (1)
`Decoder.ResolveBalance` — a free re-check of the SAME in-memory
index, which closes the one same-window gap the index has (a claim
whose tx_hash sorts before its own create's tx_hash in
`StreamClassicOps`' `(ledger_seq, tx_hash, op_index)` order is decoded
first, landing in pending, even though the create is indexed moments
later in the SAME window — re-checking after the whole window is done
catches this without Postgres); (2)
`timescale.Store.FindClaimableBalanceCreate`, a real Postgres query
against previously-written `claimable_balance_create` rows (matches
on `attributes ->> 'balance_id'`); (3) if neither resolves it, the
op is counted as **unresolved** and logged — never a guessed amount.
This is the "in-window index with a PG-lookup fallback" design named
in ADR-0047's Phase 3 scope, not a full second pass over the whole
range.

**Memory-scaling caveat**: the in-run index has no eviction and grows
with the ledger range one command invocation processes — a
genesis-to-P23 run in a single invocation risks accumulating on the
order of the full `CreateClaimableBalance` row count (research §5:
~1.5B) in memory. Operators MUST chunk `-from`/`-to` into
multi-million-ledger invocations for Phase 3 ranges, same discipline
as any other heavy job; the Postgres fallback is what keeps chunked,
resumed invocations correct despite each invocation starting with an
empty index.

## Files

| File | Role |
| --- | --- |
| [`doc.go`](doc.go) | Package overview: phase scope, historical-only rationale, serving + retention deferrals |
| [`events.go`](events.go) | `SourceName`, `Kind`/`Provenance` enums, `Movement`, `MovementEvent`, decode error sentinels |
| [`decode.go`](decode.go) | `SupportedOpTypes` / `matchesSupportedOp` / `decodeOp` / per-kind decoders |
| [`decode_test.go`](decode_test.go) | Synthetic-fixture unit tests (success, both failure shapes, malformed-amount defensive path, BalanceId correlation incl. the same-window-out-of-order case) |
| [`real_bytes_test.go`](real_bytes_test.go) | Real pre-P23 mainnet bytes pulled from r1's ClickHouse lake, byte-for-byte golden assertions, incl. real failed-op negative cases |
| [`recognition_test.go`](recognition_test.go) | ADR-0047 D4.2 recognition guard: exhaustive closed-enum switch-coverage test |
| [`dispatcher_adapter.go`](dispatcher_adapter.go) | `Decoder` — the `dispatcher.OpDecoder` implementation `classic-movements-backfill` calls; owns the Phase 3 in-run BalanceId index + pending list (see Q5) |

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
