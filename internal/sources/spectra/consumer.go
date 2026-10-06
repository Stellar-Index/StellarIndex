package spectra

import (
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// Event is one decoded Spectra event, discriminated by Kind.
//
//	wrap    Caller, Receiver, Shares, VaultShares
//	unwrap  Caller, Receiver, Owner, Shares, VaultShares
//
// Amounts are raw i128 (NUMERIC downstream); the market's decimals live in
// [MainnetContracts], never assumed uniform.
type Event struct {
	ContractID string

	Ledger     uint32
	ObservedAt time.Time
	TxHash     string
	OpIndex    uint32
	EventIndex uint32

	Kind string

	Caller   string
	Receiver string
	Owner    string

	// Shares is the wrapper-share amount; VaultShares the wrapped vault's
	// share amount.
	Shares      canonical.Amount
	VaultShares canonical.Amount
}

// EventKind implements [consumer.Event].
func (Event) EventKind() string { return EventKind }

// Source implements [consumer.Event].
func (Event) Source() string { return SourceName }

var _ consumer.Event = Event{}
