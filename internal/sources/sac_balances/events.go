package sac_balances

import (
	"math/big"
	"time"
)

const (
	SourceName      = "sac_balances"
	ObservationKind = "sac_balances.observation"
)

// Observation is one SAC balance entry delta. Identity is
// (contract_id, holder).
type Observation struct {
	// ContractID is the SAC wrapper's C-strkey.
	ContractID string

	// AssetKey is the classic asset that this SAC wraps, in
	// supply.AssetKey form (CODE:ISSUER). Stamped from the
	// operator's contract→asset map at decode time.
	AssetKey string

	// Holder is the G-strkey or C-strkey owning the SAC balance.
	Holder string

	Ledger     uint32
	ObservedAt time.Time

	// Balance is the post-change SAC balance in stroops.
	Balance *big.Int

	// IsRemoval is true when the ContractData entry was Removed.
	// Asset_key is still populated (from the operator map).
	IsRemoval bool
}
