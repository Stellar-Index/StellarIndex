package sushiswap_v3

import (
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
