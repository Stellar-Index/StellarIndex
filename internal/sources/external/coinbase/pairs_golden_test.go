package coinbase

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestDefaultPairs_GoldenSet pins the EXACT Coinbase product set, mirroring
// binance's TestDefaultPairs_GoldenSet.
//
// Why this exists (review 2026-08-27): coinbase had only
// TestDefaultPairList_matchesDefaultPairs, a cardinality tautology that
// compares the projection against its own source — deleting a product from
// DefaultPairs left the whole suite green. Binance had a real golden guard and
// coinbase did not, so a venue-coverage change to coinbase was effectively
// untested while the identical change to binance was pinned.
//
// This matters beyond drift: Coinbase subscribes to every product_id in ONE
// frame and a rejected id is fatal to the connection (streamer.go OnDisconnect
// -> "coinbase subscription rejected"), so an id that is wrong or delisted
// takes down XLM-USD, BTC-USD, ETH-USD and every major with it. The set is
// worth pinning deliberately.
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
	usd, eur, gbp := fiat("USD"), fiat("EUR"), fiat("GBP")

	type bq struct{ base, quote canonical.Asset }
	golden := map[string]bq{
		"XLM-USD": {crypto("XLM"), usd},
		// XLM-EUR verified online on the Coinbase products API before adding
		// (2026-08-27): XLM/EUR was being served from only two venues, so one
		// going quiet dropped source_count to 1 and tripped the phase-2 freeze.
		"XLM-EUR": {crypto("XLM"), eur},
		"BTC-USD": {crypto("BTC"), usd},
		"BTC-EUR": {crypto("BTC"), eur},
		"BTC-GBP": {crypto("BTC"), gbp},
		"ETH-USD": {crypto("ETH"), usd},
		"ETH-EUR": {crypto("ETH"), eur},
		"ETH-GBP": {crypto("ETH"), gbp},
	}

	got, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}

	for sym, want := range golden {
		g, ok := got[sym]
		if !ok {
			t.Errorf("missing product %s", sym)
			continue
		}
		if g.Base != want.base || g.Quote != want.quote {
			t.Errorf("%s = %s/%s, want %s/%s",
				sym, g.Base, g.Quote, want.base, want.quote)
		}
	}
	// The majors are appended programmatically; assert only that every golden
	// entry is present and that the explicitly-pinned fiat crosses did not
	// silently change shape. A bare count assert would fight the majors loop.
	for sym := range golden {
		if _, ok := got[sym]; !ok {
			t.Errorf("golden product %s vanished from DefaultPairs", sym)
		}
	}
}
