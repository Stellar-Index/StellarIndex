// Package classicmovements reconstructs pre-P23 classic-Stellar
// asset movements (payments, path payments, account merges,
// clawbacks, claimable balances, liquidity-pool deposits/
// withdrawals) from the ClickHouse raw lake — never Horizon
// (ADR-0001), never a MinIO walk (ADR-0034). See
// docs/adr/0047-pre-p23-classic-movement-reconstruction.md for the
// full decision and docs/architecture/pre-p23-classic-movements-research.md
// for the evidence base.
//
// # Phase 1 scope
//
// This package currently decodes exactly two classic operation
// types: Payment and CreateAccount (ADR-0047 D3 Phase 1). Both
// reconstruct from the operation body alone once the operation
// result's success code is confirmed — neither needs
// ledger_entry_changes. SupportedOpTypes, matchesPhase1Op, and
// decodeOp's switch all cover exactly this pair; recognition_test.go
// pins that coverage so a future phase's author must extend all
// three deliberately (ADR-0047 D4.2).
//
// Later phases (path payments; claimable-balance create/claim/
// clawback + Clawback; account merge + liquidity-pool deposit/
// withdraw) are NOT implemented here yet. Adding one means: a new
// decodeXxx function, a case in decodeOp's switch, an addition to
// matchesPhase1Op (rename it — it stops being Phase-1-only) and to
// SupportedOpTypes, and an update to recognition_test.go's expected
// set. The migration 0105 schema already admits all ten
// movement_kind values and both provenance values, so no schema
// change is needed for any of this.
//
// # Historical-only — never live-wired (ADR-0047 D2)
//
// Every ledger from P23 onward (58,762,517, Whisk/CAP-67,
// 2025-09-03) already emits a unified classic-movement event that
// internal/sources/sep41_transfers decodes. This package's Decoder
// therefore implements dispatcher.OpDecoder (mirroring
// internal/sources/sdex's shape) but is NEVER registered with the
// live dispatcher (internal/pipeline/dispatcher.go's
// BuildDispatcher has no case for it, and none should be added) and
// its consumer.Event type (MovementEvent) has no persist arm in
// internal/pipeline/sink.go's HandleEvent (see that file's sibling
// internal/pipeline/lockstep_ast_test.go notSunkEvents entry). The
// only writer is `stellarindex-ops classic-movements-backfill`
// (internal/ops/chops), which streams clickhouse.ClassicOp values
// via clickhouse.StreamClassicOps and hard-clamps its ledger range
// below the P23 boundary regardless of what an operator requests —
// see that command's flag help for the exact clamp behavior.
//
// # Serving — write-path only
//
// No read endpoint serves classic_movements yet. ADR-0047 D1's
// account-activity read surface (a future merged read across
// classic_movements and sep41_transfers' post-P23 'transfer' rows,
// e.g. /v1/accounts/{g}/movements) is deliberately deferred to a
// later unit, once more phases exist to make a merged feed
// worthwhile. Neither table knows about the other at write time.
//
// # Retention — deferred
//
// The served-tier retention question for classic_movements (serve
// every row forever vs. a recent window backed by the lake for deep
// history, per ADR-0034's lake/served split) is deliberately NOT
// decided by this package or by migration 0105. ADR-0047's
// consequences section sizes the eventual row count at the order of
// 10-11B across all four phases; the retention call is deferred
// until the first real Phase-1 backfill measures actual row bytes
// on disk. No `drop_after` policy exists on this table; if one
// appears later without a documented sizing pass behind it, that's
// drift (see CLAUDE.md's ADR-0034 invariant on this exact pattern
// for `trades`).
package classicmovements
