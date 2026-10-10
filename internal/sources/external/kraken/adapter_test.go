package kraken

import (
	"testing"
)

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
