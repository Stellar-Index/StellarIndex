// Package claimable_balances is the canonical
// ClaimableBalanceEntry observer per ADR-0022. Plugs into the
// dispatcher's LedgerEntryChange hook (#297) and emits one
// Observation per change touching a claimable balance whose
// asset matches an operator-watched classic credit asset.
//
// Operator usage: the same `[supply] watched_classic_assets`
// list used by the trustlines observer (#304) drives this one
// too. Match fast-path is type discriminator
// (LedgerEntryTypeClaimableBalance) + asset variant + asset_key
// map lookup.
//
// # Removed-variant handling (a claim)
//
// The XDR LedgerKey for claimable balances carries only the
// BalanceId — not the asset. So a Removed change cannot be
// asset-key-filtered from the change itself.
//
// The asset is nevertheless available in the substrate: stellar-core
// emits the full pre-image as a LEDGER_ENTRY_STATE change
// immediately before the LEDGER_ENTRY_REMOVED for the same entry
// (LedgerTxn::getChanges; the SDK's
// ingest.GetChangesFromLedgerEntryChanges pairs on exactly that
// adjacency). The observer therefore consumes State changes for
// watched assets, memoizes claimable_id → asset_key for the
// duration of the ledger walk, and uses it to attribute the
// removal. Removals it still cannot attribute (unwatched asset, or
// no pre-image seen) stay unmatched.
//
// Emitting the removal is not optional bookkeeping: the served
// total is Trustline + Claimable + LPReserve + SACWrapped
// ([supply.ClassicComputer.Compute]), so a claimable balance that is
// created and never removed is counted forever — total AND
// circulating supply (and market cap / FDV downstream) drift upward
// without bound. The pre-fix behaviour OVER-reported both; it was
// not the conservative direction. (audit-2026-07-23 DAT-10)
//
// Removal is written as an absorbing state (is_removal=true,
// balance 0) rather than a decrement, so re-ingesting a ledger
// rewrites the identical row instead of double-subtracting, and a
// create+claim inside one ledger collapses to the removal via the
// writer's intra_ledger_seq-guarded upsert.
//
// # Why classic-only
//
// Native (XLM) claimable balances exist but contribute to
// Algorithm 1, not Algorithm 2. The reader path for XLM doesn't
// consume claimable_observations.
package claimable_balances
