package chops

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A factory whose own genesis is at/after the re-derive's start ledger `to`
// deployed no children before `to`, so the [genesis, to) preseed window is
// empty — and inverted (genesis > to) when a whole-lake ch-reproject starts
// below a later-deploying factory's genesis. The guard must return before
// touching the streamer; passing a nil streamer proves it does (a walk would
// panic).
func TestPreseedFactoryChildrenSkipsEmptyOrInvertedWindow(t *testing.T) {
	src := reconSource{
		name:      "phoenix",
		factories: []string{"CB4SVAWJA6TSRNOJZ7W2AWFW46D5VR4ZMFZKDIKXEINZCZEGZCJZCKMI"},
		dec:       phoenix.NewDecoder(),
		genesis:   51_572_016,
	}
	// to (the re-derive -from) below the factory genesis → inverted window.
	if _, err := preseedFactoryChildren(context.Background(), nil, src, 50_000_000); err != nil {
		t.Fatalf("inverted window: expected skip (nil), got: %v", err)
	}
	// to == genesis → empty window, also skipped.
	if _, err := preseedFactoryChildren(context.Background(), nil, src, src.genesis); err != nil {
		t.Fatalf("empty window at genesis==to: expected skip (nil), got: %v", err)
	}
}

// A -ch reconcile over a window that starts after a child's deploy must seed
// that child from the lake it re-derives from. The streamer is the only event
// source handed in, so a preseed still reading Postgres would count nothing.
func TestExpectedProjectionPreseedsFactoryChildFromTheLake(t *testing.T) {
	const lo, hi = uint32(100), uint32(200)
	const preLoChild = "CPRELOCHILDAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	lake := countingEventStreamer{evs: []events.Event{
		mockCreate(50, prefFactory, preLoChild, 0), // deployed before the window
		mockBiz(150, preLoChild, 1),
	}}
	src := reconSource{
		name:        "mockgated",
		genesis:     1,
		dec:         newMockGatedDecoder(),
		factories:   []string{prefFactory},
		creationSym: "create",
	}
	expectedFor, blind, err := expectedProjection(context.Background(), lake, "", src, lo, hi)
	if err != nil {
		t.Fatalf("expectedProjection: %v", err)
	}
	if blind.Any() {
		t.Fatalf("unexpected blind spots: %s", blind.Detail())
	}
	if got := expectedFor(reconTarget{kinds: []string{"mock.biz"}})[150]; got != 1 {
		t.Fatalf("expected[150] = %d, want 1: the pre-window child was not seeded from the lake", got)
	}
}

// RLT-395: a preseed walk over a real, non-empty window that finds zero
// factory children must not go silent — an empty gate registry then makes
// every pre-existing pool's events look like a real projection gap for the
// rest of the re-derive instead of the decoder-blind spot it actually is.
func TestPreseedResultMessageWarnsOnZeroSeeded(t *testing.T) {
	msg := preseedResultMessage("blend", 0)
	if !strings.Contains(msg, "WARNING") || !strings.Contains(msg, "blend") {
		t.Fatalf("expected a visible WARNING naming the source for a zero-seed result, got: %q", msg)
	}

	seededMsg := preseedResultMessage("blend", 3)
	if strings.Contains(seededMsg, "WARNING") {
		t.Fatalf("a successful seed must not read as a warning, got: %q", seededMsg)
	}
}

// The preseed must read the ClickHouse lake, not the Postgres soroban_events
// landing zone, so emptying that table cannot silently empty every gate.
func TestPreseedFactoryChildrenTakesNoTimescaleStore(t *testing.T) {
	fn := reflect.TypeOf(preseedFactoryChildren)
	storeT := reflect.TypeOf((*timescale.Store)(nil))
	for i := 0; i < fn.NumIn(); i++ {
		if fn.In(i) == storeT {
			t.Fatalf("preseedFactoryChildren parameter %d is %s; it must stream the lake instead", i, storeT)
		}
	}
}

// A non-empty preseed window that seeds no child must fail the caller: an
// empty gate registry otherwise reports every pre-existing child's rows as
// missing, indistinguishable from real loss.
func TestPreseedFactoryChildrenErrorsOnZeroSeeded(t *testing.T) {
	src := reconSource{
		name:        "mock",
		factories:   []string{prefFactory},
		creationSym: "create",
		dec:         newMockGatedDecoder(),
		genesis:     100,
	}
	_, err := preseedFactoryChildren(context.Background(), countingEventStreamer{}, src, 200)
	if err == nil || !strings.Contains(err.Error(), "mock") {
		t.Fatalf("zero seeded children over [100,200]: want an error naming the source, got %v", err)
	}

	dec := newMockGatedDecoder()
	src.dec = dec
	streamer := countingEventStreamer{evs: []events.Event{
		{Ledger: 150, ContractID: prefFactory, Topic: []string{"create"}, Value: prefInWin},
	}}
	if _, err := preseedFactoryChildren(context.Background(), streamer, src, 200); err != nil {
		t.Fatalf("one seeded child: want nil, got %v", err)
	}
	if !dec.reg.Has(prefInWin) {
		t.Fatal("the announced child was not registered in the gate")
	}
}
