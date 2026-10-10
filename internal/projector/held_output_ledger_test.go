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
