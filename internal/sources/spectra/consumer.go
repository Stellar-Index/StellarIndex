package spectra

import (
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// Event is one decoded Spectra event, discriminated by Kind; its fields are spectra_events' columns (migration 0210) and each kind sets only its own. Amounts are raw i128 (NUMERIC downstream), decimals live in [MainnetContracts] and are never assumed uniform; an amount is a pointer so a kind that should carry it but left it unset is refused, not written as 0.
type Event struct {
	ContractID string

	Ledger     uint32
	ObservedAt time.Time
	TxHash     string
	OpIndex    uint32
	EventIndex uint32

	Kind string
	Role Role

	MarketPT string

	Caller   string
	Receiver string
	Owner    string
	Maker    string

	// OrderID is the order's bytes32 id as 64 lowercase hex characters.
	OrderID         string
	IBT             string
	YT              string
	DurationSeconds uint64

	Shares      *canonical.Amount
	VaultShares *canonical.Amount
	Assets      *canonical.Amount
	Amount      *canonical.Amount
	YieldInIBT  *canonical.Amount
}

// EventKind implements [consumer.Event].
func (Event) EventKind() string { return EventKind }

// Source implements [consumer.Event].
func (Event) Source() string { return SourceName }

var _ consumer.Event = Event{}
