package sushiswap_v3

import (
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// TradeEvent is the [consumer.Event] the Decoder emits for each price-forming pool `swap`, written by the sink to the trades hypertable.
type TradeEvent struct {
	Trade canonical.Trade
}

// EventKind implements [consumer.Event].
func (TradeEvent) EventKind() string { return "sushiswap_v3.trade" }

// Source implements [consumer.Event] — matches [SourceName].
func (TradeEvent) Source() string { return SourceName }

// Compile-time check that TradeEvent satisfies consumer.Event.
var _ consumer.Event = TradeEvent{}

// PositionEvent is the [consumer.Event] for a pool `mint`, `burn` or `collect`; Token0 / Token1 come from the pool registry, Liquidity is the mint / burn delta (unset for collect), Sender is set on mint only and Recipient on collect only.
type PositionEvent struct {
	ContractID string // pool contract C-strkey (emitter)
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	ObservedAt time.Time
	Action     string // EventMint | EventBurn | EventCollect
	Owner      string
	Sender     string
	Recipient  string
	Token0     string // canonical asset_id from the pool registry
	Token1     string
	TickLower  int32
	TickUpper  int32
	Liquidity  canonical.Amount
	Amount0    canonical.Amount
	Amount1    canonical.Amount
}

// EventKind implements [consumer.Event]: one kind for all three actions, the row's action column discriminates.
func (PositionEvent) EventKind() string { return "sushiswap_v3.position" }

// Source implements [consumer.Event] — matches [SourceName].
func (PositionEvent) Source() string { return SourceName }

// Compile-time check that PositionEvent satisfies consumer.Event.
var _ consumer.Event = PositionEvent{}
