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
// # Removed-variant
//
// LedgerKey for an LP carries only the PoolId — not the asset
// pair. So Removed-variant changes can't be asset-key-filtered
// at the observer level. Same handling as claimable_balances:
// Match returns false for all Removed pool changes; the writer-
// side lookup follow-up lands when the Sum overcount is
// measurable in production.
package liquidity_pools
