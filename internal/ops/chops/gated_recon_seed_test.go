package chops

import (
	"context"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A factory whose own genesis is at/after the re-derive's start ledger `to`
// deployed no children before `to`, so the [genesis, to) preseed window is
// empty — and inverted (genesis > to) when a whole-lake ch-reproject starts
// below a later-deploying factory's genesis, which StreamSorobanEvents
// rejects as "to < from". The guard must return before touching the store;
// passing a nil store proves it does (a walk would panic).
func TestPreseedFactoryChildrenSkipsEmptyOrInvertedWindow(t *testing.T) {
	src := reconSource{
		name:      "phoenix",
		factories: []string{"CB4SVAWJA6TSRNOJZ7W2AWFW46D5VR4ZMFZKDIKXEINZCZEGZCJZCKMI"},
		dec:       phoenix.NewDecoder(),
		genesis:   51_572_016,
	}
	// to (the re-derive -from) below the factory genesis → inverted window.
	if err := preseedFactoryChildren(context.Background(), nil, src, 50_000_000); err != nil {
		t.Fatalf("inverted window: expected skip (nil), got: %v", err)
	}
	// to == genesis → empty window, also skipped.
	if err := preseedFactoryChildren(context.Background(), nil, src, src.genesis); err != nil {
		t.Fatalf("empty window at genesis==to: expected skip (nil), got: %v", err)
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

// fakePreseedStore serves canned ledger_ingest_log gaps and records
// whether the creation-event walk ran and over which window.
type fakePreseedStore struct {
	gaps                 []timescale.LedgerGap
	gapFrom, gapTo       uint32
	streamed             bool
	streamFrom, streamTo uint32
}

func (f *fakePreseedStore) FindLedgerIngestGaps(_ context.Context, from, to uint32) ([]timescale.LedgerGap, error) {
	f.gapFrom, f.gapTo = from, to
	return f.gaps, nil
}

func (f *fakePreseedStore) StreamSorobanEvents(_ context.Context, from, to uint32, _, _, _ []string, _ func(sorobanevents.Row) error) error {
	f.streamed, f.streamFrom, f.streamTo = true, from, to
	return nil
}

func factoryPreseedSource() reconSource {
	return reconSource{
		name:        "phoenix",
		factories:   []string{"CB4SVAWJA6TSRNOJZ7W2AWFW46D5VR4ZMFZKDIKXEINZCZEGZCJZCKMI"},
		creationSym: "create",
		dec:         phoenix.NewDecoder(),
		genesis:     51_572_016,
	}
}

// RLT-383: a hole in the landing zone inside [genesis, to] hides the
// creation events of every child deployed there, so the preseed must
// refuse — naming the gap — instead of walking and reporting success.
func TestPreseedFactoryChildrenFailsClosedOnCoverageGap(t *testing.T) {
	src := factoryPreseedSource()
	const to = uint32(52_000_000)
	store := &fakePreseedStore{gaps: []timescale.LedgerGap{{Start: 51_700_000, End: 51_700_999, Size: 1000}}}

	err := preseedFactoryChildren(context.Background(), store, src, to)
	if err == nil {
		t.Fatal("a ledger_ingest_log gap inside the preseed window must fail the preseed, got nil")
	}
	if !strings.Contains(err.Error(), "51700000-51700999") || !strings.Contains(err.Error(), "phoenix") {
		t.Fatalf("error must name the source and the gap, got: %v", err)
	}
	if store.streamed {
		t.Fatal("the creation-event walk ran over a window known to be incomplete")
	}
	if store.gapFrom != src.genesis || store.gapTo != to {
		t.Fatalf("coverage checked [%d,%d], want the walk window [%d,%d]", store.gapFrom, store.gapTo, src.genesis, to)
	}
}

func TestPreseedFactoryChildrenWalksFullyCoveredWindow(t *testing.T) {
	src := factoryPreseedSource()
	const to = uint32(52_000_000)
	store := &fakePreseedStore{}

	if err := preseedFactoryChildren(context.Background(), store, src, to); err != nil {
		t.Fatalf("fully covered window: %v", err)
	}
	if !store.streamed || store.streamFrom != src.genesis || store.streamTo != to {
		t.Fatalf("walk ran=%v over [%d,%d], want [%d,%d]", store.streamed, store.streamFrom, store.streamTo, src.genesis, to)
	}
}
