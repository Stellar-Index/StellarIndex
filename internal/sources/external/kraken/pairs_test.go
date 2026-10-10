package kraken

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
)

// TestDefaultPairs_StablecoinUSD pins the USD-quoted stablecoin markets the
// API's depeg band reads; without them the band compares $1 to $1.
func TestDefaultPairs_StablecoinUSD(t *testing.T) {
	pairs, err := DefaultPairs()
	if err != nil {
		t.Fatal(err)
	}
	for _, sym := range []string{"USDT/USD", "USDC/USD"} {
		p, ok := pairs[sym]
		if !ok {
			t.Fatalf("%s missing from DefaultPairs", sym)
		}
		if p.Quote.String() != "fiat:USD" {
			t.Errorf("%s quote = %s, want fiat:USD", sym, p.Quote)
		}
	}
}

func TestDefaultPairs_XLMSymbolsAreVenueSupported(t *testing.T) {
	m, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	for symbol := range m {
		if strings.HasPrefix(symbol, "XLM/") && !krakenXLMWsnames[symbol] {
			t.Errorf("DefaultPairs subscribes %q, which Kraken does not list", symbol)
		}
	}
}

// Every shipped pair must fit both synthesised identities whole, so a
// new long symbol fails here rather than colliding in production.
func TestDefaultPairs_FitSyntheticTxHash(t *testing.T) {
	m, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	for sym := range m {
		if _, err := candleTxHash(sym, 0, scale.LegacyCandleGranularity); err != nil {
			t.Errorf("candleTxHash(%q): %v", sym, err)
		}
		if _, err := formatTxHash(sym, 0); err != nil {
			t.Errorf("formatTxHash(%q): %v", sym, err)
		}
	}
}
