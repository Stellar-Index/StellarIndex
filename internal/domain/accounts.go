package domain

import (
	"math/big"
	"time"
)

// AccountObservation is one AccountEntry-delta record captured by
// the account observer (ADR-0021). Canonical home of
// internal/sources/accounts.Observation — see doc.go. The origin
// type keeps the EventKind()/Source() methods that satisfy
// consumer.Event; this shape carries only the persisted fields.
type AccountObservation struct {
	// AccountID is the G-strkey of the observed account.
	AccountID string

	// Ledger is the ledger sequence at which this delta landed.
	Ledger uint32

	// ObservedAt is the ledger close time, UTC.
	ObservedAt time.Time

	// Balance is the post-change native XLM balance in stroops.
	// big.Int per ADR-0003.
	Balance *big.Int

	// HomeDomain is the AccountEntry.HomeDomain value (empty when
	// unset).
	HomeDomain string

	// Flags is the AccountEntry.Flags bitmask.
	Flags uint32

	// SeqNum is the AccountEntry.SeqNum after the change.
	SeqNum int64

	// IsRemoval is true when the change removed the AccountEntry.
	IsRemoval bool

	// IntraLedgerSeq is the within-ledger position of the change that
	// produced this observation, in the dispatcher's canonical meta-walk
	// order (see dispatcher.LedgerEntryChangeContext.IntraLedgerSeq). The
	// writer persists it and guards its last-writer-wins upsert on it so an
	// out-of-order PersistEvents worker can never overwrite a later
	// intra-ledger change with an earlier one (audit-2026-07-16 C2-6). The
	// ops seed path stamps timescale.SeedIntraLedgerSeq (the authoritative
	// final state for the ledger).
	IntraLedgerSeq uint32
}
