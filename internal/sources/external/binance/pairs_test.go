package binance

import (
	"reflect"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestDefaultPairs_GoldenSet pins the YAML-embedded pair table to the
// exact map the previous Go-literal DefaultPairs built (pre-45b
// swap). The expected side is constructed through the same canonical
// constructors the old code called, so any drift — a symbol added or
// dropped, a class flipped (fiat EUR vs crypto EUR), a base/quote
// swap — fails loudly. Editing pairs.yaml intentionally means
// updating this table in the same commit.
func TestDefaultPairs_GoldenSet(t *testing.T) {
	crypto := func(code string) canonical.Asset {
		a, err := canonical.NewCryptoAsset(code)
		if err != nil {
			t.Fatalf("crypto %s: %v", code, err)
		}
		return a
	}
	fiat := func(code string) canonical.Asset {
		a, err := canonical.NewFiatAsset(code)
		if err != nil {
			t.Fatalf("fiat %s: %v", code, err)
		}
		return a
	}
	type bq struct{ base, quote canonical.Asset }
	usdt := crypto("USDT")
	golden := map[string]bq{
		"XLMUSDT": {crypto("XLM"), usdt},
		"XLMBTC":  {crypto("XLM"), crypto("BTC")},
		"BTCUSDT": {crypto("BTC"), usdt},
		"BTCEUR":  {crypto("BTC"), fiat("EUR")},
		"BTCGBP":  {crypto("BTC"), fiat("GBP")},
		"ETHUSDT": {crypto("ETH"), usdt},
		"ETHEUR":  {crypto("ETH"), fiat("EUR")},
		"ETHGBP":  {crypto("ETH"), fiat("GBP")},
	}
	for _, code := range []string{
		"ADA", "ATOM", "AVAX", "BCH", "BNB", "DASH", "DOGE", "DOT",
		"LINK", "LTC", "NEAR", "SHIB", "SOL", "TON", "TRX", "UNI", "XRP",
	} {
		golden[code+"USDT"] = bq{crypto(code), usdt}
	}

	expected := make(map[string]canonical.Pair, len(golden))
	for sym, g := range golden {
		pair, err := canonical.NewPair(g.base, g.quote)
		if err != nil {
			t.Fatalf("golden %s: %v", sym, err)
		}
		expected[sym] = pair
	}

	got, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	if len(got) != len(expected) {
		t.Errorf("pair count = %d, want %d", len(got), len(expected))
	}
	for sym, want := range expected {
		g, ok := got[sym]
		if !ok {
			t.Errorf("missing symbol %s", sym)
			continue
		}
		if !reflect.DeepEqual(g, want) {
			t.Errorf("%s = %+v, want %+v", sym, g, want)
		}
	}
	for sym := range got {
		if _, ok := expected[sym]; !ok {
			t.Errorf("unexpected extra symbol %s", sym)
		}
	}
}

// TestDefaultPairList_MatchesMap guards the convenience projection:
// same cardinality, every pair present in the map.
func TestDefaultPairList_MatchesMap(t *testing.T) {
	m, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	l, err := DefaultPairList()
	if err != nil {
		t.Fatalf("DefaultPairList: %v", err)
	}
	if len(l) != len(m) {
		t.Fatalf("list len %d != map len %d", len(l), len(m))
	}
	inMap := make(map[canonical.Pair]bool, len(m))
	for _, p := range m {
		inMap[p] = true
	}
	for _, p := range l {
		if !inMap[p] {
			t.Errorf("list pair %+v not in map", p)
		}
	}
}
