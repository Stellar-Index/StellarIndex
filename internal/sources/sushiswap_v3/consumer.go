package sushiswap_v3

import (
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// TradeEvent is the [consumer.Event] shape the Decoder emits for each
// price-forming pool `swap`. The pipeline sink type-switches on it and
// writes the trade to the trades hypertable.
type TradeEvent struct {
	Trade canonical.Trade
}

// EventKind implements [consumer.Event].
func (TradeEvent) EventKind() string { return "sushiswap_v3.trade" }

// Source implements [consumer.Event] — matches [SourceName].
func (TradeEvent) Source() string { return SourceName }

// Compile-time check that TradeEvent satisfies consumer.Event.
var _ consumer.Event = TradeEvent{}

// PositionEvent is the [consumer.Event] shape the Decoder emits for a pool
// `mint`, `burn` or `collect` — the concentrated-liquidity position
// lifecycle. Backs a sushiswap_v3_position_events row (migration 0203).
//
// Token0 / Token1 come from the pool registry, not the event body.
// Liquidity is the mint / burn liquidity delta and is unset for a collect;
// Sender is set on a mint only and Recipient on a collect only.
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

// EventKind implements [consumer.Event]. One kind for all three actions;
// the row's action column discriminates them.
func (PositionEvent) EventKind() string { return "sushiswap_v3.position" }

// Source implements [consumer.Event] — matches [SourceName].
func (PositionEvent) Source() string { return SourceName }

// Compile-time check that PositionEvent satisfies consumer.Event.
var _ consumer.Event = PositionEvent{}
