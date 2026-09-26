//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestDistinctPairsRanksCryptoXLMAsQuote is GH-1100: quoteRankSQL used
// to recognise only "native" and the SAC address as XLM, so a market
// recorded in the crypto:XLM off-chain spelling ranked as an ordinary
// token (rank 1) instead of XLM (rank 2). Stored as (base=crypto:XRP,
// quote=crypto:XLM) — the correct canonical orientation keeps that
// order (XLM outranks XRP), but the pre-fix rank tie broke on string
// comparison ("crypto:XRP" > "crypto:XLM") and flipped it, swapping
// which leg the listing reports as base and which as quote.
func TestDistinctPairsRanksCryptoXLMAsQuote(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	xrp := c.Asset{Type: c.AssetCrypto, Code: "XRP"}
	xlm := c.Asset{Type: c.AssetCrypto, Code: "XLM"}
	pair, err := c.NewPair(xrp, xlm)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Minute)
	tr := mkAPITrade(1, now.Add(-5*time.Minute), pair, 1_000_000, 500_000)
	if err := store.InsertTrade(ctx, tr); err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	rows, _, err := store.DistinctPairs(ctx, "", 50)
	if err != nil {
		t.Fatalf("DistinctPairs: %v", err)
	}
	// The listing folds XLM spellings (GH-1098), so the quote may come back
	// as any of XLM's alias forms; what this test pins is the orientation.
	xlmForms := map[string]bool{}
	for _, a := range c.AssetAliases(xlm) {
		xlmForms[a.String()] = true
	}
	var found bool
	for _, m := range rows {
		if m.Pair.Base.String() != xrp.String() && m.Pair.Quote.String() != xrp.String() {
			continue
		}
		found = true
		if m.Pair.Base.String() != xrp.String() || !xlmForms[m.Pair.Quote.String()] {
			t.Errorf("canonical pair = (%s, %s), want (%s, an XLM form of %s) — XLM must rank as quote",
				m.Pair.Base.String(), m.Pair.Quote.String(), xrp.String(), xlm.String())
		}
	}
	if !found {
		t.Fatalf("DistinctPairs did not return the seeded XRP/XLM market: %+v", rows)
	}
}
