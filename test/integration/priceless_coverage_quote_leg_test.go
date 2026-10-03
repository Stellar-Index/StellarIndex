//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestPricelessCoverage_QuoteLegOnlyAsset: an asset that only ever appears
// as the QUOTE of a stored trade (swap-direction sources) must still be a
// candidate, with the trade's volume, while a proxy quote never is.
func TestPricelessCoverage_QuoteLegOnlyAsset(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const issuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	base, err := c.NewClassicAsset("BASEONLY", issuer)
	if err != nil {
		t.Fatal(err)
	}
	quoteOnly, err := c.NewClassicAsset("QUOTEONLY", issuer)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := c.NewPair(base, quoteOnly)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Minute)
	for i := 1; i <= 5; i++ {
		tr := mkIntegrationTrade("aquarius", i, now.Add(-time.Duration(i)*time.Minute), pair, 1_000_000_000, 1_000_000_000)
		tr.Taker = "GAQUOTELEGTAKERAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %d: %v", i, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE trades SET usd_volume = 2000`); err != nil {
		t.Fatalf("stamp usd_volume: %v", err)
	}

	sigs, err := store.PopularPricelessCandidates(ctx)
	if err != nil {
		t.Fatalf("PopularPricelessCandidates: %v", err)
	}
	byID := make(map[string]timescale.AssetCoverageSignals, len(sigs))
	for _, s := range sigs {
		byID[s.AssetID] = s
	}
	for _, a := range []c.Asset{base, quoteOnly} {
		sig, ok := byID[a.String()]
		if !ok {
			t.Fatalf("%s missing from the candidate set", a)
		}
		if sig.Volume7dUSD != 10_000 || sig.Trades7d != 5 {
			t.Errorf("%s: vol_7d=%v trades_7d=%d, want 10000 / 5", a, sig.Volume7dUSD, sig.Trades7d)
		}
	}
}
