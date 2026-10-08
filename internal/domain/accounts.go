package domain

import (
	"math/big"
	"time"
)

// AccountObservation is one persisted AccountEntry delta (ADR-0021); origin
// type internal/sources/accounts.Observation adds the consumer.Event methods.
type AccountObservation struct {
	// AccountID is the G-strkey of the observed account.
	AccountID string

	// Ledger is the ledger sequence at which this delta landed.
	Ledger uint32

	// ObservedAt is the ledger close time, UTC.
	ObservedAt time.Time

	// Balance is the post-change native XLM balance in stroops (ADR-0003).
	Balance *big.Int

	// HomeDomain is the AccountEntry.HomeDomain value; empty when unset.
	HomeDomain string

	// Flags is the AccountEntry.Flags bitmask.
	Flags uint32

	// SeqNum is the AccountEntry.SeqNum after the change.
	SeqNum int64

	// IsRemoval is true when the change removed the AccountEntry.
	IsRemoval bool

	// IntraLedgerSeq is the change's position in the dispatcher's meta-walk order.
	// The writer's last-writer-wins upsert guards on it so an out-of-order worker
	// cannot overwrite a later change; the seed path stamps timescale.SeedIntraLedgerSeq.
	IntraLedgerSeq uint32
}
