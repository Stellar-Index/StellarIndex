package kraken

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

func TestStreamer_Name(t *testing.T) {
	s := NewStreamer(map[string]canonical.Pair{})
	if got := s.Name(); got != SourceName {
		t.Errorf("Name() = %q, want %q", got, SourceName)
	}
}

func TestStreamer_Class(t *testing.T) {
	s := NewStreamer(map[string]canonical.Pair{})
	if got := s.Class(); got != external.ClassExchange {
		t.Errorf("Class() = %q, want %q", got, external.ClassExchange)
	}
}

func TestDefaultPairList_matchesDefaultPairs(t *testing.T) {
	m, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	list, err := DefaultPairList()
	if err != nil {
		t.Fatalf("DefaultPairList: %v", err)
	}
	if len(list) != len(m) {
		t.Errorf("list len = %d, want %d", len(list), len(m))
	}
	if len(list) == 0 {
		t.Error("DefaultPairList returned empty slice")
	}
}

// krakenXLMWsnames is the set of XLM pairs Kraken's public AssetPairs
// endpoint lists by wsname. A symbol outside it is rejected at subscribe
// time with "Currency pair not supported" and never delivers a trade.
var krakenXLMWsnames = map[string]bool{
	"XLM/EUR": true,
	"XLM/GBP": true,
	"XLM/USD": true,
	"XLM/XBT": true,
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
