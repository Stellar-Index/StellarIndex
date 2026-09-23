package projector

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// fakeLake records every read a cycle makes through its source's lake
// connection.
type fakeLake struct {
	wm, lakeMin uint32
	wmFrom      []uint32
	minCalls    int
	closed      bool
}

func (f *fakeLake) ContiguousWatermark(_ context.Context, from uint32) (uint32, error) {
	f.wmFrom = append(f.wmFrom, from)
	return f.wm, nil
}

func (f *fakeLake) LakeMinLedger(context.Context) (uint32, error) {
	f.minCalls++
	return f.lakeMin, nil
}

func (f *fakeLake) Close() error {
	f.closed = true
	return nil
}

// newLakeProjector points chAddr at a port nothing listens on, so any
// ClickHouse read that bypasses the source's lake connection and dials
// p.chAddr itself fails the cycle instead of reaching the fake.
func newLakeProjector(store *fakeStore) *Projector {
	return &Projector{
		store:  store,
		logger: discardLog(),
		chAddr: "127.0.0.1:1",
		sink:   func(context.Context, consumer.Event) error { return nil },
	}
}

func cycleN(p *Projector, n int, lake *sourceLake) {
	src := Source{Name: "lake-wiring", Decoder: &ledgerEchoDecoder{}}
	window := uint32(BatchLimit)
	var tracker poisonTracker
	var wedge wedgeTracker
	for range n {
		p.cycleOneSource(context.Background(), src, &window, &tracker, &wedge, lake)
	}
}

// TestCycleReadsWatermarkOnSourceConnection: the tip clamp is read on the
// source's own connection, opened once and reused every cycle — never a
// fresh dial per cycle.
func TestCycleReadsWatermarkOnSourceConnection(t *testing.T) {
	// Cursor at 100 so from=101; watermark 100 means nothing past the cursor
	// is complete, so each cycle goes idle before the lake event scan.
	p := newLakeProjector(&fakeStore{projectorCursor: 100, haveCursor: true, tipLedger: 500})
	fake := &fakeLake{wm: 100}
	opens := 0
	lake := &sourceLake{open: func(context.Context) (lakeReader, error) {
		opens++
		return fake, nil
	}}

	cycleN(p, 3, lake)

	if want := []uint32{101, 101, 101}; !slices.Equal(fake.wmFrom, want) {
		t.Fatalf("watermark reads on the source connection = %v, want %v", fake.wmFrom, want)
	}
	if opens != 1 {
		t.Fatalf("lake connection opened %d times over 3 cycles, want 1", opens)
	}
	lake.close()
	if !fake.closed {
		t.Fatal("sourceLake.close did not close the connection")
	}
}

// TestFreshSourceStartsAtLakeFloor: a source with no cursor row reads the
// lake's first ledger on its own connection and asks the watermark from
// there, never from ledger 0.
func TestFreshSourceStartsAtLakeFloor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		lakeMin  uint32
		wantFrom uint32
	}{
		{"populated lake starts at its first ledger", 2, 2},
		{"lake starting later is respected", 63_000_000, 63_000_000},
		{"empty lake starts at 1, never 0", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newLakeProjector(&fakeStore{tipLedger: 500})
			// Watermark from-1: the floor ledger itself is not yet complete,
			// so the cycle goes idle before the lake event scan.
			fake := &fakeLake{lakeMin: tc.lakeMin, wm: tc.wantFrom - 1}
			lake := &sourceLake{open: func(context.Context) (lakeReader, error) { return fake, nil }}

			cycleN(p, 1, lake)

			if fake.minCalls != 1 {
				t.Fatalf("LakeMinLedger reads on the source connection = %d, want 1", fake.minCalls)
			}
			if want := []uint32{tc.wantFrom}; !slices.Equal(fake.wmFrom, want) {
				t.Fatalf("watermark asked from %v, want %v", fake.wmFrom, want)
			}
		})
	}
}

// TestSourceLakeOpenFailureDoesNotLatch: a failed open is retried on the
// next cycle rather than cached.
func TestSourceLakeOpenFailureDoesNotLatch(t *testing.T) {
	p := newLakeProjector(&fakeStore{projectorCursor: 100, haveCursor: true, tipLedger: 500})
	fake := &fakeLake{wm: 100}
	opens := 0
	lake := &sourceLake{open: func(context.Context) (lakeReader, error) {
		opens++
		if opens == 1 {
			return nil, errors.New("dial: connection refused")
		}
		return fake, nil
	}}

	cycleN(p, 2, lake)

	if opens != 2 {
		t.Fatalf("opens = %d, want 2 (failed open retried next cycle)", opens)
	}
	if want := []uint32{101}; !slices.Equal(fake.wmFrom, want) {
		t.Fatalf("watermark reads = %v, want %v", fake.wmFrom, want)
	}
}
