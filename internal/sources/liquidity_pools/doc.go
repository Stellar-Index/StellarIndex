// Package liquidity_pools is the canonical LiquidityPoolEntry
// observer per ADR-0022. Plugs into the dispatcher's
// LedgerEntryChange hook (#297) and emits one Observation per
// asset-side of a pool change touching an operator-watched
// classic credit asset.
//
// Operator usage: the same `[supply] watched_classic_assets`
// list used by the trustlines + claimable observers drives this
// one too. A pool with two assets emits up to TWO observations
// per change — one per side that's in the watched set. Pools
// where neither side is watched are skipped at Match time.
//
// # Variant scope
//
// Stellar has one classic LP variant today: ConstantProduct.
// LiquidityPoolEntryConstantProduct carries
// `Params.AssetA` + `ReserveA` + `Params.AssetB` + `ReserveB`.
// The observer reads those fields directly. Future LP variants
// (none on the protocol roadmap at v1) would extend the type
// switch in `extractFromChange`.
//
// # Removed-variant (pool emptied and deleted)
//
// LedgerKey for an LP carries only the PoolId — not the asset
// pair. So a Removed-variant change can't be asset-key-filtered
// from the change itself.
//
// Same handling as claimable_balances: stellar-core emits the full
// pre-image as a LEDGER_ENTRY_STATE change immediately before the
// LEDGER_ENTRY_REMOVED for the same entry, so the observer consumes
// State changes, memoizes pool_id → watched asset_keys for the
// duration of the ledger walk, and emits one removal Observation per
// watched side (Balance zero, IsRemoval true) when the pool entry is
// deleted. Removals it cannot attribute stay unmatched.
//
// This matters for money: the served total is
// Trustline + Claimable + LPReserve + SACWrapped
// ([supply.ClassicComputer.Compute]), so reserves of a withdrawn-and-
// deleted pool would otherwise be counted forever, drifting total and
// circulating supply (and market cap / FDV) upward without bound.
// (audit-2026-07-23 DAT-10)
package liquidity_pools
