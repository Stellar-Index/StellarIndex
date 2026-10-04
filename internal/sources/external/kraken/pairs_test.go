package kraken

import "testing"

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
