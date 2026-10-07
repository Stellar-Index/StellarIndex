// Package claimable_balances is the ClaimableBalanceEntry observer
// (ADR-0022): a LedgerEntryChange decoder emitting one Observation per
// change to a claimable balance whose asset is in `[supply]
// watched_classic_assets`. Native balances belong to Algorithm 1 and are
// not observed.
//
// A Removed change's key carries only the BalanceId, so the observer
// memoises claimable_id → asset_key, for the ledger walk, from the
// LEDGER_ENTRY_STATE pre-image stellar-core emits immediately before each
// removal; unattributable removals stay unmatched. Emitting removals is
// required: the served total sums Trustline + Claimable + LPReserve +
// SACWrapped ([supply.ClassicComputer.Compute]), so an unremoved balance
// is counted forever and over-reports total and circulating supply.
//
// A removal is an absorbing state (is_removal=true, balance 0), not a
// decrement, so re-ingesting a ledger rewrites the same row and a
// create+claim in one ledger collapses to the removal via the writer's
// intra_ledger_seq-guarded upsert.
package claimable_balances
