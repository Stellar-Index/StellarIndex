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

	// IntraLedgerSeq is the within-ledger position of the change that
	// produced this observation, in the dispatcher's canonical meta-walk
	// order (see dispatcher.LedgerEntryChangeContext.IntraLedgerSeq). Both
	// asset-side rows from one pool-change share the same position (they come
	// from the same change). The writer guards its last-writer-wins upsert on
	// it so an out-of-order PersistEvents worker can never overwrite a later
	// intra-ledger change with an earlier one (audit-2026-07-16 C2-6).
	IntraLedgerSeq uint32
}
