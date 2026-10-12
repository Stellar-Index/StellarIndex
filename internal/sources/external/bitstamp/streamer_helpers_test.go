package bitstamp

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// symbolsFor inverts PairMap into the Bitstamp wire symbols to subscribe to; a pair missing from the map is fatal, not skipped.

func TestSymbolsFor_happyPath(t *testing.T) {
	pm, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	s := NewStreamer(pm)

	xlmusd := pm["xlmusd"]
	btcusd := pm["btcusd"]
	got, err := s.symbolsFor([]canonical.Pair{xlmusd, btcusd})
	if err != nil {
		t.Fatalf("symbolsFor: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	want := map[string]bool{"xlmusd": true, "btcusd": true}
	for _, sym := range got {
		if !want[sym] {
			t.Errorf("unexpected symbol %q", sym)
		}
	}
}

func TestSymbolsFor_unknownPairRejected(t *testing.T) {
	// A pair with no PairMap entry must error, not be silently omitted, or the venue would look quiet downstream.
	pm, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	s := NewStreamer(pm)

	// MATIC: in allow-list, intentionally not in DefaultPairs.
	matic, err := canonical.NewCryptoAsset("MATIC")
	if err != nil {
		t.Fatalf("NewCryptoAsset(MATIC): %v", err)
	}
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("NewFiatAsset(USD): %v", err)
	}
	missing, err := canonical.NewPair(matic, usd)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}

	if _, err := s.symbolsFor([]canonical.Pair{missing}); err == nil {
		t.Fatal("expected error for MATIC/USD (not in DefaultPairs), got nil")
	}
}
