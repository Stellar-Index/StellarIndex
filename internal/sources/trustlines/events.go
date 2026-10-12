package trustlines

import (
	"math/big"
	"time"
)

// SourceName is the stable identifier for the trustlines observer, stamped on metric labels and every [Observation.Source].
const SourceName = "trustlines"

// ObservationKind is the consumer.Event.EventKind the observer emits; the sink type-switches on it to route to `trustline_observations`.
const ObservationKind = "trustlines.observation"

// Observation is the post-change TrustLineEntry state at the producing ledger, one per (account, asset, ledger) with last-writer-wins ON CONFLICT keeping the final state. A removal sets IsRemoval=true with Balance zeroed: the holder no longer has the trustline.
type Observation struct {
	// AccountID is the G-strkey of the trustline's holder.
	AccountID string

	// AssetKey is the supply.AssetKey form of the asset
	// (CODE:ISSUER for classic credits). Stamped at decode time
	// so the storage row carries it directly.
	AssetKey string

	// Ledger is the ledger sequence at which this delta landed.
	Ledger uint32

	// ObservedAt is the ledger close time, UTC.
	ObservedAt time.Time

	// Balance is the post-change trustline balance in stroops.
	// big.Int per ADR-0003.
	Balance *big.Int

	// IsRemoval is true when the change removed the trustline.
	// Balance is zero on these rows.
	IsRemoval bool

	// IntraLedgerSeq is the change's within-ledger position in the dispatcher's canonical
	// meta-walk order. The writer guards its last-writer-wins upsert on it so an out-of-order
	// PersistEvents worker cannot overwrite a later change with an earlier one.
	IntraLedgerSeq uint32
}
