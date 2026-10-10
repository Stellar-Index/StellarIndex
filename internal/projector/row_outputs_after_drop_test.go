package projector

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
)

// poisonTrade is a soroswap trade the store can never hold: the zero-value
// canonical.Trade fails Trade.Validate inside Store.InsertTrade before any SQL
// runs, so pipeline.HandleEvent reports it as a *pipeline.TradeDroppedError.
func poisonTrade(ev events.Event) consumer.Event {
	return soroswap.TradeEvent{Trade: canonical.Trade{Source: "soroswap", Ledger: ev.Ledger, TxHash: ev.TxHash}}
}

// scriptedDecoder matches every row and decodes it to a fixed list of outputs
// built from the row — ONE lake row, SEVERAL outputs. That is a production
// shape, not a contrivance: soroswap's emitCompleted emits one TradeEvent per
// completed swap+sync pair absorbed from a single event, and phoenix's
// decodeSwapEvent emits the rescued evicted trades plus the completed one.
type scriptedDecoder struct {
	build []func(events.Event) consumer.Event
}

func (*scriptedDecoder) Name() string              { return "scripted" }
func (*scriptedDecoder) Matches(events.Event) bool { return true }
func (d *scriptedDecoder) Decode(ev events.Event) ([]consumer.Event, error) {
	outs := make([]consumer.Event, 0, len(d.build))
	for _, b := range d.build {
		outs = append(outs, b(ev))
	}
	return outs, nil
}

func echoOutput(ev events.Event) consumer.Event { return ledgerEvent{ledger: ev.Ledger} }

// productionTradeSink routes trades through pipeline.HandleEvent — the sink
// cmd/stellarindex-indexer binds — and hands every other output to other. A
// nil store is safe for the poison trade: Validate rejects it before the
// store is touched.
func productionTradeSink(other func(consumer.Event) error) SinkFunc {
	return func(ctx context.Context, ev consumer.Event) error {
		if _, ok := ev.(soroswap.TradeEvent); ok {
			return pipeline.HandleEvent(ctx, discardLog(), nil, ev)
		}
		return other(ev)
	}
}
