package external

import "testing"

// TestRegistry_Credit pins the vendor credit on every licensed off-chain feed (E-7: a lone vendor value is served only
// with its credit) and its absence on on-chain venues, so a new aggregator or FX row cannot land uncredited.
func TestRegistry_Credit(t *testing.T) {
	want := map[string]string{
		"coingecko":        "CoinGecko",
		"coingecko_daily":  "CoinGecko",
		"coinmarketcap":    "CoinMarketCap",
		"cryptocompare":    "CCData (CryptoCompare)",
		"ecb":              "European Central Bank euro foreign exchange reference rates",
		"massive":          "Massive",
		"exchangeratesapi": "exchangeratesapi.io",
		"tiingo":           "Tiingo",
	}
	for name, credit := range want {
		if got := Credit(name); got != credit {
			t.Errorf("Credit(%q) = %q, want %q", name, got, credit)
		}
	}
	for name, md := range Registry {
		switch {
		case IsOnChain(name) && md.Credit != "":
			t.Errorf("%s is on-chain but carries credit %q", name, md.Credit)
		case (md.Class == ClassAggregator || md.Subclass == SubclassFX) && md.Credit == "":
			t.Errorf("%s is an aggregator or FX vendor with no credit", name)
		}
	}
	if got := Credit("chainlink"); got != "" {
		t.Errorf("Credit(chainlink) = %q, want none (an on-chain oracle network)", got)
	}
	if got := Credit("no-such-source"); got != "" {
		t.Errorf("Credit(unregistered) = %q, want empty", got)
	}
}
