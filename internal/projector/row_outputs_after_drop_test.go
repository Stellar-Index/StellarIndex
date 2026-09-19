package projector

import (
	"context"
	"errors"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
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

// TestCycle_DroppedOutputDoesNotAbortTheRowsOtherOutputs pins the regression
// the first RLT-132 attempt introduced. Once a dropped trade is REPORTED
// (non-nil) instead of folded into nil, a loop that stops at the first sink
// error never offers the row's remaining outputs to the sink — and because a
// permanent fault is SKIPPED, the cursor advances past the row, so those valid
// outputs are lost from the served tier under one sink_permanent count.
//
// Corrected: a permanently dropped output is counted and the loop goes on, so
// [poison trade, valid output] sinks the valid output (ok=1), counts the drop
// (sink_permanent=1) and advances the cursor.
func TestCycle_DroppedOutputDoesNotAbortTheRowsOtherOutputs(t *testing.T) {
	const source = "rlt132-sibling-after-drop"
	okBefore := decodedCount(t, source, "ok")
	permBefore := decodedCount(t, source, "sink_permanent")
	retryBefore := decodedCount(t, source, "sink_retry")

	h := newWedgeHarness(t, source, []sorobanevents.Row{lakeRow(101, 1)}, 105, nil)
	h.src.Decoder = &scriptedDecoder{build: []func(events.Event) consumer.Event{poisonTrade, echoOutput}}
	siblingSunk := 0
	h.proj.sink = productionTradeSink(func(consumer.Event) error { siblingSunk++; return nil })

	h.cycle()

	if siblingSunk != 1 {
		t.Errorf("valid sibling output sunk %d times, want 1 — a permanently dropped output aborted the row and its remaining valid output was never offered to the sink", siblingSunk)
	}
	if got := decodedCount(t, source, "ok") - okBefore; got != 1 {
		t.Errorf("outcome=ok delta = %v, want 1 (the valid sibling output, and only it)", got)
	}
	if got := decodedCount(t, source, "sink_permanent") - permBefore; got != 1 {
		t.Errorf("outcome=sink_permanent delta = %v, want 1 (the dropped trade)", got)
	}
	if got := decodedCount(t, source, "sink_retry") - retryBefore; got != 0 {
		t.Errorf("outcome=sink_retry delta = %v, want 0 — nothing here is retryable", got)
	}
	if got := h.store.cursor(); got != 105 {
		t.Fatalf("cursor = %d, want 105 — a deterministic drop must not hold the source", got)
	}
}

// TestCycle_PermanentDropsAreCountedPerOutput — sink_permanent is a per-OUTPUT
// count, like outcome=ok. One row decoding to two poison trades around a valid
// output is two drops, not "one bad row".
func TestCycle_PermanentDropsAreCountedPerOutput(t *testing.T) {
	const source = "rlt132-per-output-drop-count"
	okBefore := decodedCount(t, source, "ok")
	permBefore := decodedCount(t, source, "sink_permanent")

	h := newWedgeHarness(t, source, []sorobanevents.Row{lakeRow(101, 1)}, 105, nil)
	h.src.Decoder = &scriptedDecoder{build: []func(events.Event) consumer.Event{poisonTrade, echoOutput, poisonTrade}}
	h.proj.sink = productionTradeSink(func(consumer.Event) error { return nil })

	h.cycle()

	if got := decodedCount(t, source, "sink_permanent") - permBefore; got != 2 {
		t.Errorf("outcome=sink_permanent delta = %v, want 2 — one per dropped output, not one per row", got)
	}
	if got := decodedCount(t, source, "ok") - okBefore; got != 1 {
		t.Errorf("outcome=ok delta = %v, want 1", got)
	}
	if got := h.store.cursor(); got != 105 {
		t.Fatalf("cursor = %d, want 105", got)
	}
}

// TestCycle_RetryableFaultAfterADropStillHoldsTheRow — continuing past a
// permanent drop must not weaken C2-1: a transient fault on a LATER output of
// the same row still stops the row and holds the cursor below its ledger, and
// the output after the transient fault is NOT offered (the whole row is
// re-read next cycle; the idempotent sinks absorb the repeats).
func TestCycle_RetryableFaultAfterADropStillHoldsTheRow(t *testing.T) {
	const source = "rlt132-drop-then-transient"
	permBefore := decodedCount(t, source, "sink_permanent")
	retryBefore := decodedCount(t, source, "sink_retry")

	h := newWedgeHarness(t, source, []sorobanevents.Row{lakeRow(101, 1)}, 105, nil)
	h.src.Decoder = &scriptedDecoder{build: []func(events.Event) consumer.Event{poisonTrade, echoOutput, echoOutput}}
	offered := 0
	h.proj.sink = productionTradeSink(func(consumer.Event) error {
		offered++
		return context.DeadlineExceeded // positively transient
	})

	h.cycle()

	if offered != 1 {
		t.Errorf("outputs offered after the drop = %d, want 1 — a retryable fault must still stop the row at the failing output", offered)
	}
	if got := decodedCount(t, source, "sink_retry") - retryBefore; got != 1 {
		t.Errorf("outcome=sink_retry delta = %v, want 1", got)
	}
	if got := decodedCount(t, source, "sink_permanent") - permBefore; got != 1 {
		t.Errorf("outcome=sink_permanent delta = %v, want 1 — the drop happened and is counted even though the row is then held", got)
	}
	if got := h.store.cursor(); got != 100 {
		t.Fatalf("cursor = %d, want 100 (held) — a transient fault at the window's first ledger must not advance it", got)
	}
}

// TestProcessEventSafely_ContinuesPastPermanentStopsAtRetryable pins the loop
// contract at the unit: permanent drops are collected and skipped over, the
// first retryable/unclassified fault stops the row, and both are reachable
// from the returned error.
func TestProcessEventSafely_ContinuesPastPermanentStopsAtRetryable(t *testing.T) {
	deadlock := errors.New("deadlock detected") // unclassified → holds
	outs := []consumer.Event{fakeEvent{}, fakeEvent{}, fakeEvent{}, fakeEvent{}, fakeEvent{}}
	d := &fakeDecoder{matches: true, outs: outs}
	calls := 0
	emitted, decodeFail, sinkErr := processEventSafely(Source{Name: "x", Decoder: d}, events.Event{Ledger: 9},
		func(consumer.Event) error {
			calls++
			switch calls {
			case 1:
				return canonical.ErrInvalidTrade // permanent → skip, continue
			case 2:
				return nil
			case 3:
				return canonical.ErrInvalidAmount // permanent → skip, continue
			case 4:
				return deadlock // stops the row
			default:
				return nil // never reached
			}
		}, discardLog())
	if decodeFail {
		t.Error("a sink fault is not a decode failure")
	}
	if calls != 4 {
		t.Errorf("sink called %d times, want 4 (continue past 2 drops, stop at the unclassified fault)", calls)
	}
	if emitted != 1 {
		t.Errorf("emitted = %d, want 1 — only the output that landed", emitted)
	}
	// Asserted through the standard multi-error shape rather than the concrete
	// type, so this test compiles — and fails on the VALUES — against a loop
	// that still stops at the first error.
	multi, ok := sinkErr.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("sinkErr = %v (%T), want an error carrying every fault of the row (Unwrap() []error)", sinkErr, sinkErr)
	}
	if got := len(multi.Unwrap()); got != 3 {
		t.Errorf("sinkErr carries %d faults, want 3 (two permanent drops + the fault that stopped the row)", got)
	}
	if !errors.Is(sinkErr, deadlock) || !errors.Is(sinkErr, canonical.ErrInvalidTrade) {
		t.Errorf("sinkErr must unwrap to every fault it carries; got %v", sinkErr)
	}
}
