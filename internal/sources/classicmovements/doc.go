// Package classicmovements reconstructs pre-P23 classic-Stellar
// asset movements (payments, path payments, account merges,
// clawbacks, claimable balances, liquidity-pool deposits/
// withdrawals) from the ClickHouse raw lake — never Horizon
// (ADR-0001), never a MinIO walk (ADR-0034). See
// docs/adr/0047-pre-p23-classic-movement-reconstruction.md for the
// full decision and docs/architecture/pre-p23-classic-movements-research.md
// for the evidence base.
//
// # Phase 1-3 scope (op-only decode surface)
//
// This package currently decodes eight classic operation types via
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
// full correlation design, including the Postgres second-pass
// fallback and its memory-scaling caveat). None of Phase 1-3 needs
// ledger_entry_changes. SupportedOpTypes, matchesSupportedOp, and
// decodeOp's switch all cover exactly these eight types;
// recognition_test.go pins that coverage so a future phase's author
// must extend all three deliberately (ADR-0047 D4.2).
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
// Later phases (account merge + liquidity-pool deposit/withdraw +
// the CAP-0038 revocation edge case, Phase 4) are NOT implemented
// here yet. Adding an op-only-surface kind means: a new decodeXxx
// function, a case in decodeOp's switch, an addition to
// matchesSupportedOp and to SupportedOpTypes, and an update to
// recognition_test.go's expected set. LiquidityPoolDeposit/Withdraw
// and the CAP-0038 edge case will need a SEPARATE, entry-changes-
// correlated decode surface (dispatcher.OpContext has no room for a
// correlated ledger_entry_changes group) — see the Phase 4
// implementation notes once they land. The migration 0105 schema
// already admits all ten movement_kind values and both provenance
// values, so no schema change is needed for any of this.
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
