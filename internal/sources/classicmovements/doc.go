// Package classicmovements reconstructs pre-P23 classic-Stellar
// asset movements (payments, path payments, account merges,
// clawbacks, claimable balances, liquidity-pool deposits/
// withdrawals) from the ClickHouse raw lake — never Horizon
// (ADR-0001), never a MinIO walk (ADR-0034). See
// docs/adr/0047-pre-p23-classic-movement-reconstruction.md for the
// full decision and docs/architecture/pre-p23-classic-movements-research.md
// for the evidence base.
//
// # Phase 1-4 scope (op-only decode surface)
//
// This package currently decodes nine classic operation types via
// its op-only decode surface (Matches / decodeOp / Decoder.Decode):
// Payment and CreateAccount (ADR-0047 D3 Phase 1, both reconstruct
// from the operation BODY alone once the operation result's success
// code is confirmed — research §2 path (a)); PathPaymentStrictReceive
// / PathPaymentStrictSend (Phase 2, reconstructed from the operation
// RESULT — research §2 path (b): the destination leg is
// result.Success.Last.{Asset,Amount} for both op types uniformly; the
// source leg is body.SendAmount (exact) for StrictSend, or derived
// from the result's Offers for StrictReceive since SendMax is only a
// ceiling — see decode.go's pathPaymentStrictReceiveSourceAmount doc
// comment for the exact hop-order derivation, verified against real
// multi-hop mainnet data in real_bytes_test.go); and
// CreateClaimableBalance / ClaimClaimableBalance /
// ClawbackClaimableBalance / Clawback (Phase 3 — CreateClaimableBalance
// and Clawback are path (a); Claim/ClawbackClaimableBalance are path
// (b+own-index): neither op carries an asset or amount, only a
// BalanceId, resolved against Decoder's in-run index of previously-
// decoded creates — see dispatcher_adapter.go's Decoder doc for the
// full correlation design, including the ClickHouse second-pass
// fallback (ADR-0048 D2; previously Postgres) and its memory-scaling
// caveat). None of Phase 1-3 needs
// ledger_entry_changes. SupportedOpTypes, matchesSupportedOp, and
// decodeOp's switch all cover exactly these eight types (plus
// AccountMerge below, for nine); recognition_test.go pins that
// coverage so a future phase's author must extend all three
// deliberately (ADR-0047 D4.2).
//
// A path payment emits exactly ONE 'path_payment' row per op
// (leg_index always 0) — never a row per hop; the per-hop ClaimAtoms
// already live in `trades` via internal/sources/sdex and are
// deliberately NOT duplicated here. The row's primary Asset/Amount
// columns hold the destination leg; Movement.Attributes carries the
// source leg (send_asset/send_amount) since the schema has one
// asset per row. Every Phase 1-3 kind is one row per op (leg_index
// always 0) — none of these ops have a second asset leg.
//
// Phase 4 adds AccountMerge to this op-only surface (research §2 path
// (b): the exact amount is AccountMergeResult.SourceAccountBalance,
// never derivable from the body, which carries only the destination)
// — the NINTH and last op-only-surface type.
//
// # Phase 4 entry-changes-correlated decode surface
//
// LiquidityPoolDeposit/Withdraw and the CAP-0038 AllowTrust/
// SetTrustLineFlags trustline-revocation auto-liquidation edge case
// (research §2 path (c)) are a SEPARATE decode surface —
// EntryChangeOpTypes / DecodeLiquidityPoolOp /
// DecodeCAP0038Revocation in entrychanges.go — because their results
// are bare success codes with zero data fields; the only ground
// truth is the pool's LiquidityPoolEntryConstantProduct
// ReserveA/ReserveB before vs. after the op (or, for CAP-0038, the
// created ClaimableBalanceEntry rows the revocation side-effect
// produces), which lives ONLY in ledger_entry_changes.
// dispatcher.OpContext (the op-only surface's input) has no room for
// a correlated ledger_entry_changes group, so these are plain
// functions the caller (classic-movements-backfill) invokes
// directly after correlating clickhouse.StreamEntryChanges output by
// (ledger, tx_hash, op_index) itself — see entrychanges.go's package
// doc for the full design, including why an empty entry-changes
// group means something DIFFERENT for LP deposit/withdraw
// (ErrEntryChangesUnavailable, always) than for AllowTrust/
// SetTrustLineFlags (zero movements is the expected common case; the
// caller must run its own window-level fidelity probe before trusting
// that as "no liquidation" rather than "can't tell yet").
//
// LiquidityPoolDeposit/Withdraw emit TWO rows per op (leg_index 0/1,
// one per pool asset); a CAP-0038 liquidation emits one row per
// created ClaimableBalanceEntry (always two for a real event, since
// every classic AMM pool has exactly two assets) — both are the only
// Phase 1-4 kinds with more than one row per op.
//
// The migration 0105 schema already admits all ten movement_kind
// values and both provenance values, so no schema change was needed
// for any phase.
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
// via clickhouse.StreamClassicOps (both decode surfaces) plus
// clickhouse.EntryChange via clickhouse.StreamEntryChanges (the
// Phase 4 entry-changes surface only), and hard-clamps its ledger
// range below the P23 boundary regardless of what an operator
// requests — see that command's flag help for the exact clamp
// behavior. Per ADR-0048 D2 (2026-07-10), that command writes
// ClickHouse's stellar.account_movements — a lake-in/lake-out job
// with NO Postgres connection at all; see
// internal/storage/clickhouse/account_movements.go.
//
// # Storage target — amended by ADR-0048 (2026-07-10)
//
// The rest of this doc comment (and migration 0105's row/README)
// describe ADR-0047 D1's ORIGINAL plan: a Postgres `classic_movements`
// hypertable. ADR-0048 D2 amended that: the archive is
// `stellar.account_movements` in ClickHouse, feed-shaped (two rows
// per movement, one per participant, direction discriminator),
// populated by the same backfill command above. Migration 0105 stays
// applied but UNPOPULATED — see migrations/README.md's 0105 row. The
// decode layer this package provides (everything above this section)
// is unaffected: only the write target moved.
//
// # Serving — write-path only
//
// No read endpoint serves stellar.account_movements yet. ADR-0048
// D5's account-activity read surface (a future merged read across
// stellar.account_movements and sep41_transfers' post-P23 'transfer'
// rows, e.g. /v1/accounts/{g}/movements) is deliberately deferred to
// a later unit, once more phases exist to make a merged feed
// worthwhile. Neither table knows about the other at write time.
//
// # Retention — deferred
//
// The retention question for stellar.account_movements (serve every
// row forever vs. a recent window, per ADR-0034's lake/served split
// applied to a CH-native serving table) is deliberately NOT decided
// by this package or by deploy/clickhouse/tier1_schema.sql. ADR-0047's
// consequences section sizes the eventual row count at the order of
// 10-11B across all four phases; the retention call is deferred until
// the first real Phase-1 backfill measures actual row bytes on disk.
package classicmovements
