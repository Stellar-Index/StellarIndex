package claimable_balances

import (
	"math/big"
	"time"
)

const (
	SourceName      = "claimable_balances"
	ObservationKind = "claimable_balances.observation"
)

// Observation is one ClaimableBalanceEntry-delta record, identified per claimable balance id (not
// per account). Removed variants (a claim) carry Balance zero and IsRemoval true.
type Observation struct {
	// ClaimableID is the BalanceID's hex form, unique across the network.
	ClaimableID string

	// AssetKey is the supply.AssetKey form (CODE:ISSUER for
	// classic credits) of the asset the claimable balance pays.
	AssetKey string

	Ledger     uint32
	ObservedAt time.Time

	// Balance is the post-change Amount in stroops. big.Int per
	// ADR-0003.
	Balance *big.Int

	// IsRemoval is true when the change removed the balance (a claim). Balance is zeroed and
	// SumClaimableBalancesAtOrBefore excludes the row, so the amount stops counting toward supply.
	IsRemoval bool

	// IntraLedgerSeq is the change's within-ledger position in the dispatcher's canonical
	// meta-walk order. The writer guards its last-writer-wins upsert on it so an out-of-order
	// PersistEvents worker cannot overwrite a later change with an earlier one.
	IntraLedgerSeq uint32
}
