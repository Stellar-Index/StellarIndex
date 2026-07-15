package bitstamp

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// symbolsFor inverts the operator-supplied PairMap so the streamer
// can render a list of canonical pairs as the Bitstamp wire symbols
// it must subscribe to. A pair missing from the map is fatal — we
// don't want to silently subscribe to nothing and starve a market.

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
	// A pair with no matching PairMap entry must surface as an
	// error, not a silent omission — the operator's intent is to
	// see ticks for that pair, and skipping it would create a
	// confusing "venue went quiet" appearance downstream.
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
