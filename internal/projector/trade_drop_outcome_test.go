package projector

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
)

// invalidTradeDecoder matches every row and emits ONE soroswap trade the
// store can never hold: a zero-value canonical.Trade fails Trade.Validate
// inside Store.InsertTrade before any SQL runs — the pre-SQL twin of a
// SQLSTATE 22/23 rejection, and deterministic for the row.
type invalidTradeDecoder struct{}

func (*invalidTradeDecoder) Name() string              { return "invalid-trade" }
func (*invalidTradeDecoder) Matches(events.Event) bool { return true }
func (*invalidTradeDecoder) Decode(ev events.Event) ([]consumer.Event, error) {
	return []consumer.Event{soroswap.TradeEvent{
		Trade: canonical.Trade{Source: "soroswap", Ledger: ev.Ledger, TxHash: ev.TxHash},
	}}, nil
}

// TestCycle_DroppedTradeIsNotReportedOK pins RLT-132 end to end, through the
// PRODUCTION sink: cmd/stellarindex-indexer binds the projector's SinkFunc to
// pipeline.HandleEvent, and so does this test (a nil store is safe — Validate
// rejects the trade before the store is touched).
//
// persistTrade used to return nil for a permanently dropped trade, which is
// also what a landed trade returns; processEventSafely counted it emitted and
// the cycle published it under outcome="ok" — the label whose own comment
// promises "only events that DURABLY committed". The row is in neither the
// served tier nor any loss counter the projector owns.
//
// Corrected: the drop is labelled sink_permanent, never ok — and the cursor
// STILL advances, because a deterministic fault must not wedge a sole-writer
// source (COR-11).
func TestCycle_DroppedTradeIsNotReportedOK(t *testing.T) {
	const source = "rlt132-dropped-trade"
	rows := []sorobanevents.Row{lakeRow(101, 1)}
	okBefore := decodedCount(t, source, "ok")
	permBefore := decodedCount(t, source, "sink_permanent")
	retryBefore := decodedCount(t, source, "sink_retry")

	h := newWedgeHarness(t, source, rows, 105, nil)
	h.src.Decoder = &invalidTradeDecoder{}
	h.proj.sink = func(ctx context.Context, ev consumer.Event) error {
		return pipeline.HandleEvent(ctx, discardLog(), nil, ev)
	}

	h.cycle()

	if got := decodedCount(t, source, "ok") - okBefore; got != 0 {
		t.Errorf("outcome=ok delta = %v, want 0 — a trade the store permanently rejected was reported as durably committed", got)
	}
	if got := decodedCount(t, source, "sink_permanent") - permBefore; got != 1 {
		t.Errorf("outcome=sink_permanent delta = %v, want 1 — the dropped trade must be counted as a permanent sink fault", got)
	}
	if got := decodedCount(t, source, "sink_retry") - retryBefore; got != 0 {
		t.Errorf("outcome=sink_retry delta = %v, want 0 — a deterministic drop must be skipped, not held for retry", got)
	}
	if got := h.store.cursor(); got != 105 {
		t.Fatalf("cursor = %d, want 105 — reporting the drop must not wedge the source on a row that can never land", got)
	}
}
