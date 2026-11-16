package pipeline

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/sources/band"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
)

func oracleEvent() consumer.Event {
	return band.UpdateEvent{Update: canonical.OracleUpdate{Source: band.SourceName}}
}

// An abandoned on-chain event names its ledger, so the loss handed to the cursor rewind is the lowest ledger over
// trades AND events: an event from a ledger below every abandoned trade must not fall under the rewind.
func TestAbandonedEventNamesItsLedger(t *testing.T) {
	in := make(chan consumer.Event, 3)
	in <- withLedger(sdex.TradeEvent{Trade: mkTrade("sdex", 700)}, 700)
	in <- withLedger(oracleEvent(), 650)
	in <- withLedger(oracleEvent(), 720)
	close(in)

	store := &fakeTradeStore{failErr: context.DeadlineExceeded}
	ep := func(context.Context, consumer.Event, bool) error { return context.DeadlineExceeded }
	lt := &lossTracker{}
	drainBufferedEvents(in, discardLogger(), ep, store, SinkModeSkipProjected, time.Now().Add(time.Minute), lt)

	if got, want := lt.snapshot(), (ShutdownLoss{Rows: 3, MinLedger: 650}); got != want {
		t.Errorf("ShutdownLoss = %+v; want %+v", got, want)
	}
}

// An event carried from steady state into the shutdown drain keeps its ledger.
func TestCarriedEventKeepsItsLedger(t *testing.T) {
	ep := func(context.Context, consumer.Event, bool) error { return context.DeadlineExceeded }
	lt := &lossTracker{}
	persistCarried([]consumer.Event{withLedger(oracleEvent(), 900)}, discardLogger(), ep, time.Now().Add(time.Minute), lt)
	if got, want := lt.snapshot(), (ShutdownLoss{Rows: 1, MinLedger: 900}); got != want {
		t.Errorf("ShutdownLoss = %+v; want %+v", got, want)
	}
}

// Every step after receipt switches on the concrete event type, so the tag must be off before the persister or the
// trade batch sees the event: a tagged trade lands as a trade and a tagged event reaches the persister as itself.
func TestSinkTakesTheLedgerTagOff(t *testing.T) {
	in := make(chan consumer.Event, 2)
	in <- withLedger(sdex.TradeEvent{Trade: mkTrade("sdex", 700)}, 700)
	in <- withLedger(oracleEvent(), 701)
	close(in)

	store := &fakeTradeStore{}
	store.healthy.Store(true)
	var seen []consumer.Event
	ep := func(_ context.Context, ev consumer.Event, _ bool) error {
		seen = append(seen, ev)
		return nil
	}
	lt := &lossTracker{}
	drainBufferedEvents(in, discardLogger(), ep, store, SinkModeSkipProjected, time.Now().Add(time.Minute), lt)

	if n := store.landedCount(); n != 1 {
		t.Errorf("landed %d trades; want the tagged trade on the batch path", n)
	}
	if len(seen) != 1 {
		t.Fatalf("persister saw %d events; want 1", len(seen))
	}
	if _, ok := seen[0].(band.UpdateEvent); !ok {
		t.Errorf("persister saw %T; want the event as its producer made it", seen[0])
	}
	if got := lt.snapshot(); got != (ShutdownLoss{}) {
		t.Errorf("ShutdownLoss = %+v on a clean drain; want zero", got)
	}
}

// The tag is put on at the one place on-chain events enter the sink channel and taken off at every receive.
func TestLedgerTagIsAppliedAndRemovedEverywhere(t *testing.T) {
	proc, err := os.ReadFile("processor.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(proc), "events <- withLedger(ev, lcm.LedgerSequence())") {
		t.Error("ProcessLedger does not tag the events it sends with their ledger")
	}
	sink, err := os.ReadFile("sink.go")
	if err != nil {
		t.Fatal(err)
	}
	receives := strings.Count(string(sink), "ev, ok := <-in:")
	untagged := strings.Count(string(sink), "ev, ledger := eventLedger(ev)")
	if receives == 0 || receives != untagged {
		t.Errorf("sink.go receives from the events channel at %d places and takes the ledger tag off at %d", receives, untagged)
	}
}
