package spectra

import (
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// Event is one decoded Spectra event, discriminated by Kind. Its fields
// are spectra_events' columns (migration 0210); a kind sets exactly these:
//
//	pt_deployed       MarketPT, Caller (deployer), IBT, DurationSeconds
//	yt_deployed       MarketPT (the emitting PT), YT
//	pt_added          MarketPT
//	pt_minted         MarketPT, Caller, Receiver, Shares
//	redeem            MarketPT, Owner, Receiver, Shares
//	yield_updated     MarketPT, Owner (the user), YieldInIBT
//	transfer          MarketPT, Caller (from), Receiver (to), Amount
//	wrap              Caller, Receiver, Shares, VaultShares
//	unwrap            Caller, Receiver, Owner, Shares, VaultShares
//	deposit/withdraw  Caller, Receiver, Owner, Assets, Shares
//	order_registered  Maker, OrderID, Amount (making_amount)
//	order_filled      OrderID, Amount (actual_making)
//	order_cancelled   Maker, OrderID
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

	Shares      canonical.Amount
	VaultShares canonical.Amount
	Assets      canonical.Amount
	Amount      canonical.Amount
	YieldInIBT  canonical.Amount
}

// EventKind implements [consumer.Event].
func (Event) EventKind() string { return EventKind }

// Source implements [consumer.Event].
func (Event) Source() string { return SourceName }

var _ consumer.Event = Event{}
