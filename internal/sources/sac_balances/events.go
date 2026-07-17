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

	// IntraLedgerSeq is the within-ledger position of the change that
	// produced this observation, in the dispatcher's canonical meta-walk
	// order (see dispatcher.LedgerEntryChangeContext.IntraLedgerSeq). When a
	// single (contract, holder) balance changes MULTIPLE times within one
	// ledger, this is what lets the writer keep the FINAL change rather than
	// whichever out-of-order PersistEvents worker committed last — the
	// wrong-supply-component bug (audit-2026-07-16 C2-6).
	IntraLedgerSeq uint32
}
