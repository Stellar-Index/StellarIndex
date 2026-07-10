# classicmovements

Reconstructs pre-P23 classic-Stellar asset movements from the
ClickHouse raw lake (ADR-0047). Not Horizon (ADR-0001), not a MinIO
walk (ADR-0034) — reads `stellar.operations` / `operation_results`
(and, from a later phase, `ledger_entry_changes`).

**Write target (ADR-0048 D2, 2026-07-10):** this package's decode
layer is unchanged, but the archive it feeds is now
**ClickHouse-native** — `stellar.account_movements`
(`deploy/clickhouse/tier1_schema.sql`,
`internal/storage/clickhouse/account_movements.go`), feed-shaped (TWO
rows per movement, one per participant, a `direction` discriminator).
ADR-0047 D1's original Postgres `classic_movements` hypertable
(migration 0105) stays applied but **UNPOPULATED** — see
`migrations/README.md`'s 0105 row. `stellarindex-ops
classic-movements-backfill` opens no Postgres connection at all
("no Postgres in the loop," ADR-0048 D2).

See [docs/adr/0047-pre-p23-classic-movement-reconstruction.md](../../../docs/adr/0047-pre-p23-classic-movement-reconstruction.md),
[docs/adr/0048-serve-by-query-shape.md](../../../docs/adr/0048-serve-by-query-shape.md),
and [docs/architecture/pre-p23-classic-movements-research.md](../../../docs/architecture/pre-p23-classic-movements-research.md)
for the full decision + evidence base.

## What this ingests (Phases 1-4 — complete)

### Op-only decode surface (body/result only, no `ledger_entry_changes`)

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
| `AccountMerge` | `OperationTypeAccountMerge` | `account_merge` | 1 row/op |

`Payment`/`CreateAccount`/`CreateClaimableBalance`/`Clawback`
reconstruct from the operation **body** alone once the operation
**result**'s success code is confirmed (research §2 path (a)) — none
need `ledger_entry_changes`. The two path-payment types and
`AccountMerge` reconstruct from the operation **result** (path (b)):
path payments' `asset`/`amount` columns hold the **destination** leg
(`result.Success.Last.{Asset,Amount}`, exact for both types);
`attributes.send_asset`/`attributes.send_amount` hold the **source**
leg — exact from the body (`SendAmount`) for StrictSend, derived from
the result's `Offers` for StrictReceive (`SendMax` is only a ceiling
— see `decode.go`'s `pathPaymentStrictReceiveSourceAmount` for the
hop-order derivation). `AccountMerge`'s amount is
`AccountMergeResult.SourceAccountBalance` — never derivable from the
body, which carries only the destination. `ClaimClaimableBalance`/
`ClawbackClaimableBalance` reconstruct via research's "b+own-index"
path: neither op carries an asset/amount, only a `BalanceId`,
resolved against the `CreateClaimableBalance` row this package itself
derived earlier — see Q5. Every kind above is exactly one row per op
(`leg_index` always 0) — none of these ops have a second asset leg,
and the per-hop `ClaimAtom` trade legs of a path payment stay in
`trades` via `internal/sources/sdex` and are never duplicated here. A
failed op (bare result code that never reached the op's own result
union, OR an inner union whose own code is a failure) decodes to
**zero** movements, never an error.

### Entry-changes-correlated decode surface (needs `ledger_entry_changes`)

| Operation | Op type code | Movement kind | Row cardinality |
| --- | --- | --- | --- |
| `LiquidityPoolDeposit` | `OperationTypeLiquidityPoolDeposit` | `liquidity_pool_deposit` | 2 rows/op (leg_index 0/1, one per pool asset) |
| `LiquidityPoolWithdraw` | `OperationTypeLiquidityPoolWithdraw` | `liquidity_pool_withdraw` | 2 rows/op (leg_index 0/1) |
| `AllowTrust` / `SetTrustLineFlags` (CAP-0038 edge only) | `OperationTypeAllowTrust` / `OperationTypeSetTrustLineFlags` | `liquidity_pool_withdraw` | 0 rows (common case) or 2 rows (revocation-triggered liquidation) |

`LiquidityPoolDeposit`/`Withdraw` results are bare success codes with
zero data fields (research §2 path (c)) — the only ground truth is
the pool's `LiquidityPoolEntryConstantProduct` `ReserveA`/`ReserveB`
before vs. after the op, which lives ONLY in `ledger_entry_changes`.
The CAP-0038 trustline-revocation auto-liquidation edge case is the
same story: an `AllowTrust`/`SetTrustLineFlags` op that deauthorizes
an account holding LP-share trustlines mixing the revoked asset
auto-redeems those shares into two new `ClaimableBalanceEntry` rows,
detectable ONLY by consulting entry changes at that op's index (the
op body alone can't tell you whether the trustor actually held a
matching position) — modelled as `movement_kind='liquidity_pool_withdraw'`
rows with `attributes.revocation=true` since it IS functionally a
forced withdrawal, just routed through escrow. See `entrychanges.go`
and Q6 for the full design, including why an empty entry-changes
group means something different for LP deposit/withdraw
(unconditionally unavailable) than for the CAP-0038 check (the
expected common case — a window-level fidelity probe is required to
tell the two apart).

Migration 0105's schema admitted all ten `movement_kind` values from
day one, so no phase needed a new migration — see `doc.go`.

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

Migration 0105's `movement_kind` CHECK admitted all ten ADR-0047 D1
kinds and both `provenance` values from day one, so every phase added
decode arms, never schema churn. `recognition_test.go` pins
`Matches()` / `SupportedOpTypes()` / `decodeOp`'s switch to exactly
this package's op-only in-scope kinds (all nine, as of Phase 4) and
asserts every other value in the closed 27-value `xdr.OperationType`
enum is rejected loudly (`ErrUnsupportedOpType`) rather than silently
producing zero rows. A SECOND, parallel guard
(`TestRecognition_EntryChangeOpTypesIsExhaustiveAndDisjoint`) pins
`EntryChangeOpTypes()` to exactly the four Phase 4 entry-changes
types and asserts the two surfaces never overlap.

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

### Q5 — ClaimableBalance claim/clawback correlation: in-run index + ClickHouse fallback

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
catches this without a ClickHouse round trip); (2)
`clickhouse.FindClaimableBalanceCreate` (ADR-0048 D2; previously
`timescale.Store.FindClaimableBalanceCreate` against Postgres) — a
ClickHouse query against previously-written `claimable_balance_create`
rows in `stellar.account_movements` (matches on
`JSONExtractString(attributes, 'balance_id')`); (3) if neither
resolves it, the op is counted as **unresolved** and logged — never a
guessed amount. This is the "in-window index with a lookup fallback"
design named in ADR-0047's Phase 3 scope, not a full second pass over
the whole range.

**Memory-scaling caveat**: the in-run index has no eviction and grows
with the ledger range one command invocation processes — a
genesis-to-P23 run in a single invocation risks accumulating on the
order of the full `CreateClaimableBalance` row count (research §5:
~1.5B) in memory. Operators MUST chunk `-from`/`-to` into
multi-million-ledger invocations for Phase 3 ranges, same discipline
as any other heavy job; the ClickHouse fallback is what keeps
chunked, resumed invocations correct despite each invocation starting
with an empty index.

### Q6 — Phase 4's entry-changes surface: a second, parallel decode path

`LiquidityPoolDeposit`/`Withdraw` and the CAP-0038 edge case
(`entrychanges.go`) do NOT go through `Decoder.Decode` — they can't,
because `dispatcher.OpContext` (the op-only surface's input) has no
field for a correlated `ledger_entry_changes` group, and this package
deliberately doesn't extend that shared, live-path type for a
historical-only need (nor does it implement
`dispatcher.LedgerEntryChangeDecoder`, whose one-change-at-a-time
contract is a mismatch for before/after delta computation — see
`entrychanges.go`'s package doc for the full reasoning). Instead,
`DecodeLiquidityPoolOp` / `DecodeCAP0038Revocation` are plain
functions that take an already-correlated `[]EntryChangeXDR` — the
caller (`classic-movements-backfill`) does the correlation itself via
`clickhouse.StreamEntryChanges`, grouping by `(ledger, tx_hash,
op_index)`.

**Two different meanings for "no correlated entry changes,"
per-op-type:**

- `LiquidityPoolDeposit`/`Withdraw`: a REAL deposit/withdraw always
  mutates the pool entry, so an empty group ALWAYS means
  `ErrEntryChangesUnavailable` (fidelity absent for this ledger) —
  never "nothing happened," since the op only reaches this decode
  path after `opSucceeded` + a Success result code already confirmed
  something DID happen.
- `AllowTrust`/`SetTrustLineFlags`: the CAP-0038 liquidation is RARE
  — the overwhelming majority of these ops trigger nothing, so an
  empty group is the EXPECTED common case, indistinguishable at the
  SQL layer from "fidelity is absent and we can't tell." The caller
  MUST run a window-level fidelity probe
  (`clickhouse.CountOpScopedEntryChanges`) BEFORE trusting "zero
  movements" as "definitely no liquidation" — `classic-movements-backfill`
  skips these ops entirely (counted separately, `CAP-0038 skipped`)
  when the probe finds zero fidelity for the window, rather than
  quietly under-reporting liquidations.

**Current era**: as of this writing, `ledger_entry_changes`' real
per-op fidelity starts at ~ledger 61,996,000 (research §3.2) —
ALREADY PAST the P23 boundary (58,762,517) this command hard-clamps
to. Every window this decode surface can address today therefore
reports `LP entry-changes N/A` for 100% of LP ops and skips 100% of
CAP-0038 checks, honestly and by design — confirmed against REAL
mainnet LP deposit/withdraw op bytes in `real_bytes_test.go`'s
`TestRealBytes_liquidityPoolDeposit_entryChangesUnavailable` /
`_liquidityPoolWithdraw_entryChangesUnavailable`, and against a live
end-to-end run through `stellarindex-ops classic-movements-backfill`
over real r1 ClickHouse data during implementation. Phase 0's
separate, operator-scheduled `ch-backfill` over `[38115806,
61999000]` is the prerequisite that flips this; once it lands, the
SAME code correctly derives real amounts with no further changes —
re-running an already-processed range is safe (ClickHouse's
ReplacingMergeTree absorbs the duplicate insert).

## Files

| File | Role |
| --- | --- |
| [`doc.go`](doc.go) | Package overview: phase scope, historical-only rationale, serving + retention deferrals |
| [`events.go`](events.go) | `SourceName`, `Kind`/`Provenance` enums, `Movement`, `MovementEvent`, `PendingClaimableBalanceRef`, decode error sentinels |
| [`decode.go`](decode.go) | Op-only surface: `SupportedOpTypes` / `matchesSupportedOp` / `decodeOp` / per-kind decoders |
| [`entrychanges.go`](entrychanges.go) | Entry-changes-correlated surface: `EntryChangeOpTypes` / `DecodeLiquidityPoolOp` / `DecodeCAP0038Revocation` (see Q6) |
| [`decode_test.go`](decode_test.go) | Op-only surface synthetic-fixture unit tests (success, both failure shapes, malformed-amount defensive path, BalanceId correlation incl. the same-window-out-of-order case) |
| [`entrychanges_test.go`](entrychanges_test.go) | Entry-changes surface synthetic-fixture unit tests (deposit/withdraw before/after derivation, new-pool implicit-zero case, CAP-0038 liquidation + no-liquidation cases) |
| [`real_bytes_test.go`](real_bytes_test.go) | Real pre-P23 mainnet bytes pulled from r1's ClickHouse lake, byte-for-byte golden assertions, incl. real failed-op negative cases and the real LP entry-changes-unavailable proof |
| [`recognition_test.go`](recognition_test.go) | ADR-0047 D4.2 recognition guards: exhaustive closed-enum switch-coverage for BOTH decode surfaces, plus their disjointness |
| [`dispatcher_adapter.go`](dispatcher_adapter.go) | `Decoder` — the `dispatcher.OpDecoder` implementation `classic-movements-backfill` calls; owns the Phase 3 in-run BalanceId index + pending list (see Q5) |

(No `consumer.go` — like SDEX, this source has no live-stream
consumer of its own; its only caller is the backfill command.)

## Operational notes

- **Class**: not an `external.Registry` source — this is a purely
  on-chain, historical, lake-derived writer. No `BackfillSafe` flag
  applies (that flag gates Soroban WASM-upgrade risk; classic
  operation semantics don't change across a protocol version the way
  a Soroban contract's bytecode does).
- **Backfill**: `stellarindex-ops classic-movements-backfill -from N
  -to N [-window N] [-ch-addr H:P] [-resume] [-write] [-verify]`.
  Windowed, resumable (data-derived — the highest ledger already in
  `stellar.account_movements`, no Postgres cursor), idempotent
  (ReplacingMergeTree). No `-config` flag — this command opens no
  Postgres connection at all (ADR-0048 D2). Always run under
  `/usr/local/sbin/run-heavy-job.sh` for anything beyond a small range
  (CLAUDE.md heavy-job doctrine).
- **Serving**: none yet — write-path only (see `doc.go`).

## References

- ADR-0001 — "Horizon is not in our architecture."
- ADR-0034 — ClickHouse raw lake / Postgres served tier split.
- ADR-0048 — "Serve by query shape": the account-movement archive
  (`stellar.account_movements`) is ClickHouse-native, not Postgres.
- Related source: [`sdex`](../sdex/README.md) — the precedent for a
  classic-op decoder living outside the projector.
