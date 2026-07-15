package coinbase

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// productsFor inverts the operator-supplied PairMap so the streamer
// can render canonical pairs as Coinbase product IDs ("XLM-USD") on
// the wire. Pairs missing from the map are fatal — silently dropping
// would starve a market downstream.

func TestProductsFor_happyPath(t *testing.T) {
	pm, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	s := NewStreamer(pm)

	got, err := s.productsFor([]canonical.Pair{pm["XLM-USD"], pm["BTC-USD"]})
	if err != nil {
		t.Fatalf("productsFor: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	want := map[string]bool{"XLM-USD": true, "BTC-USD": true}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected product %q", p)
		}
	}
}

func TestProductsFor_unknownPairRejected(t *testing.T) {
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

	if _, err := s.productsFor([]canonical.Pair{missing}); err == nil {
		t.Fatal("expected error for MATIC/USD (not in DefaultPairs), got nil")
	}
}
