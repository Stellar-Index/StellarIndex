package external_test

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/binance"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/bitstamp"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/coinbase"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/kraken"
)

// TestShippedVenueQuotes_HaveUSDReference requires every quote asset in each venue's DefaultPairs to resolve to a USD
// reference. Otherwise a configured pair runs with the dust guard inert; a new quote leg fails here instead of only warning at
// startup.
func TestShippedVenueQuotes_HaveUSDReference(t *testing.T) {
	venues := map[string]func() (map[string]canonical.Pair, error){
		"binance":  binance.DefaultPairs,
		"bitstamp": bitstamp.DefaultPairs,
		"coinbase": coinbase.DefaultPairs,
		"kraken":   kraken.DefaultPairs,
	}
	for name, defaults := range venues {
		pairs, err := defaults()
		if err != nil {
			t.Fatalf("%s DefaultPairs: %v", name, err)
		}
		if len(pairs) == 0 {
			t.Fatalf("%s DefaultPairs is empty", name)
		}
		for sym, p := range pairs {
			if _, ok := external.QuoteUSDReferenceMicros(p.Quote); !ok {
				t.Errorf("%s %s: quote %s has no USD dust-floor reference", name, sym, p.Quote)
			}
		}
	}
}
