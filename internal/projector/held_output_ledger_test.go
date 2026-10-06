package projector

import (
	"errors"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

type carriedEvent struct{ ledger uint32 }

func (carriedEvent) EventKind() string     { return "fake.ledger" }
func (carriedEvent) Source() string        { return "fake" }
func (e carriedEvent) EventLedger() uint32 { return e.ledger }

// A carried output from an earlier ledger that fails to sink must hold the
// cursor below ITS ledger, not the scanned row's, or the retry never re-reads
// the buffer state that produced it.
func TestProcessEventSafely_HeldLedgerIsOutputLedger(t *testing.T) {
	d := &fakeDecoder{matches: true, outs: []consumer.Event{carriedEvent{ledger: 90}, fakeEvent{}}}
	_, _, sinkErr := processEventSafely(Source{Name: "x", Decoder: d}, events.Event{Ledger: 100},
		func(consumer.Event) error { return errors.New("deadlock detected") }, discardLog())
	f := rowFaultsOf(sinkErr)
	if f.held == nil || f.heldLedger != 90 {
		t.Fatalf("held=%v heldLedger=%d, want a held fault at ledger 90", f.held, f.heldLedger)
	}
}

func TestOutputLedger(t *testing.T) {
	cases := []struct {
		name string
		out  consumer.Event
		want uint32
	}{
		{"plain output uses the row ledger", fakeEvent{}, 100},
		{"carried earlier ledger wins", carriedEvent{ledger: 90}, 90},
		{"zero declared ledger falls back", carriedEvent{}, 100},
		{"later declared ledger never raises the hold", carriedEvent{ledger: 120}, 100},
	}
	for _, c := range cases {
		if got := outputLedger(c.out, 100); got != c.want {
			t.Errorf("%s: outputLedger = %d, want %d", c.name, got, c.want)
		}
	}
}

// carryDecoder emits, for the row at carryAt, an output that belongs to the
// ledger before it (a buffered decoder flushing earlier state); every other
// row emits its own ledger's output.
type carryDecoder struct{ carryAt uint32 }

func (*carryDecoder) Name() string              { return "carry" }
func (*carryDecoder) Matches(events.Event) bool { return true }
func (d *carryDecoder) Decode(ev events.Event) ([]consumer.Event, error) {
	if ev.Ledger == d.carryAt {
		return []consumer.Event{carriedEvent{ledger: ev.Ledger - 2}}, nil
	}
	return []consumer.Event{ledgerEvent{ledger: ev.Ledger}}, nil
}

func carryHarness(t *testing.T, source string) *wedgeHarness {
	t.Helper()
	rows := []events.Event{lakeEvent(101, 1), lakeEvent(102, 2), lakeEvent(103, 3)}
	h := newWedgeHarness(t, source, rows, 105, func(ev consumer.Event) error {
		if _, ok := ev.(carriedEvent); ok {
			return errors.New("unclassified store fault")
		}
		return nil
	})
	h.src.Decoder = &carryDecoder{carryAt: 103}
	return h
}

// The failing output belongs to ledger 101 though it was scanned at ledger
// 103: the cursor must stay below 101, not advance to 102.
func TestCycle_CarriedOutputFaultHoldsCursorBelowOutputLedger(t *testing.T) {
	h := carryHarness(t, "carry-hold")
	h.cycle()
	if got := h.store.cursor(); got > 100 {
		t.Fatalf("cursor = %d, want <= 100 (hold below the carried output's ledger 101)", got)
	}
}

// Quarantining a carried-output row must forget the history the tracker
// recorded for that row, or a later identical row inherits a spent budget.
func TestCycle_CarriedOutputQuarantineForgetsTrackerHistory(t *testing.T) {
	const source = "carry-quarantine"
	h := carryHarness(t, source)
	before := decodedCount(t, source, "sink_quarantined")
	for i := 0; i < QuarantineAfterCycles; i++ {
		h.cycle()
	}
	if got := decodedCount(t, source, "sink_quarantined") - before; got != 1 {
		t.Fatalf("sink_quarantined delta = %v, want 1", got)
	}
	if n := len(h.tracker.fails); n != 0 {
		t.Errorf("tracker still holds %d entries after quarantine: %v", n, h.tracker.fails)
	}
	if got := h.store.cursor(); got != 105 {
		t.Errorf("cursor = %d, want 105 after quarantine", got)
	}
}
