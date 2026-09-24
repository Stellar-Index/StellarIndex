package chops

import (
	"context"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
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
	if _, err := preseedFactoryChildren(context.Background(), nil, src, 50_000_000); err != nil {
		t.Fatalf("inverted window: expected skip (nil), got: %v", err)
	}
	// to == genesis → empty window, also skipped.
	if _, err := preseedFactoryChildren(context.Background(), nil, src, src.genesis); err != nil {
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
