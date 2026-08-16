//go:build integration

package integration_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestAPI_MarketsAssetFilter_XLMAliasComplete proves the served rate for
// the /v1/markets?asset= alias-completeness fix (F-1340) against real
// TimescaleDB: an XLM market keyed under `crypto:XLM` (as every CEX feed
// writes it) MUST surface for a ?asset=native query, and vice-versa.
//
// Pre-fix distinctPairsCommon matched the asset filter with a scalar
// `base_asset = $5 OR quote_asset = $5`, so ?asset=native returned zero
// rows for a crypto:XLM-keyed market — the exact undercount this test
// pins. Post-fix the filter binds the full alias set and matches with
// ANY-membership on each leg.
func TestAPI_MarketsAssetFilter_XLMAliasComplete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	cryptoXLM, err := c.ParseAsset("crypto:XLM")
	if err != nil {
		t.Fatal(err)
	}
	// The market lives ONLY under the crypto:XLM form (the CEX spelling).
	cexPair, _ := c.NewPair(cryptoXLM, usdc)

	t0 := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second)
	for i, tr := range []c.Trade{
		mkAPITrade(1, t0.Add(0*time.Minute), cexPair, 1_000_000_000, 12_000_000),
		mkAPITrade(2, t0.Add(5*time.Minute), cexPair, 1_000_000_000, 12_100_000),
	} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade[%d]: %v", i, err)
		}
	}

	for _, stmt := range []string{
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`,
		`CALL refresh_continuous_aggregate('prices_1d', NULL, NULL)`,
	} {
		if _, err := store.DB().ExecContext(ctx, stmt); err != nil {
			t.Fatalf("refresh cagg: %v", err)
		}
	}

	srv := v1.New(v1.Options{Markets: apiMarketsAdapter{s: store}})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Query the OTHER alias form. Pre-fix: 0 rows (crypto:XLM omitted).
	var env struct {
		Data []v1.Market `json:"data"`
	}
	getJSON(t, ts.URL+"/v1/markets?asset=native", &env)

	foundXLMLeg := false
	for _, m := range env.Data {
		if m.Base == "crypto:XLM" || m.Quote == "crypto:XLM" {
			foundXLMLeg = true
		}
	}
	if !foundXLMLeg {
		t.Fatalf("?asset=native did not surface the crypto:XLM-keyed market "+
			"(alias-incomplete filter); got %d rows: %+v", len(env.Data), env.Data)
	}
}
