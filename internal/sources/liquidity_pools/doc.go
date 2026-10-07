// Package liquidity_pools is the LiquidityPoolEntry observer (ADR-0022): a
// LedgerEntryChange decoder emitting one Observation per watched side of a
// pool change, so up to two per change; pools with neither asset in
// `[supply] watched_classic_assets` are skipped at Match. ConstantProduct is
// the only variant; its reserves are read directly.
//
// A Removed change's key carries only the PoolId, so, as in
// claimable_balances, the observer memoises pool_id → watched asset_keys
// from the LEDGER_ENTRY_STATE pre-image stellar-core emits immediately
// before the removal and emits one removal Observation (Balance 0,
// IsRemoval) per watched side; unattributable removals stay unmatched.
// Without them a deleted pool's reserves stay in the served total
// ([supply.ClassicComputer.Compute]) forever and over-report supply.
package liquidity_pools
