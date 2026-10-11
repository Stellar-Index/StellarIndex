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

	// IsRemoval is true when the change deleted the pool entry. Balance is zeroed and
	// SumLPReservesAtOrBefore excludes the row, so the vanished reserves stop counting toward supply.
	IsRemoval bool

	// IntraLedgerSeq is the change's within-ledger position in the dispatcher's canonical
	// meta-walk order; both side rows of one pool change share it. The writer guards its
	// last-writer-wins upsert on it so an out-of-order PersistEvents worker cannot overwrite a
	// later change with an earlier one.
	IntraLedgerSeq uint32
}
