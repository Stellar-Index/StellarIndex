// Package classicmovements reconstructs pre-P23 classic asset movements
// (payments, path payments, account merges, clawbacks, claimable balances,
// liquidity-pool deposits/withdrawals) from the ClickHouse lake, never
// Horizon (ADR-0001); README.md has the operation inventory (ADR-0047).
//
// Op-only types decode through [Decoder]; LP deposit/withdraw and the
// CAP-0038 revocation have bare-success results, so they decode from
// ledger_entry_changes in entrychanges.go. A path payment emits two rows
// (source and destination leg), never one per hop: hops are already in
// `trades`. LP deposit/withdraw emit one row per paying asset.
// A CAP-0038 liquidation emits a liquidity_pool_withdraw and a
// claimable_balance_create row per created balance: four rows for a real
// two-asset pool.
//
// Historical only (ADR-0047 D2): from P23 (ledger 58,762,517)
// sep41_transfers covers these, so [Decoder] is never registered with the
// live dispatcher and has no sink arm. The only writer is `stellarindex-ops
// classic-movements-backfill`, which clamps below P23 and writes ClickHouse
// stellar.account_movements (ADR-0048 D2).
package classicmovements
