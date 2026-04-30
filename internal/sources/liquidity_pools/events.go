package liquidity_pools

import (
	"math/big"
	"time"
)

const (
	SourceName      = "liquidity_pools"
	ObservationKind = "liquidity_pools.observation"
)

// Observation is one per-(pool, asset_side, ledger) reserve
// delta. A single LP change emits up to two Observations — one
// per asset side that's in the watched set.
type Observation struct {
	// PoolID is the pool's identity hex (LiquidityPoolId).
	PoolID string

	// AssetKey is the supply.AssetKey form for this asset side.
	AssetKey string

	Ledger     uint32
	ObservedAt time.Time

	// Balance is the post-change reserve for this side.
	Balance *big.Int

	// IsRemoval reserved for v2 (writer-side lookup follow-up).
	IsRemoval bool
}
