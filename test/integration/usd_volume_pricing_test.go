//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestUsdVolumePricingStats seeds priced, unpriced and same-issuer classic
// trades inside the window, plus rows outside it and from a source not asked
// for, and pins the exact counts. A requested source with no rows (kraken) still gets a zero row.
func TestUsdVolumePricingStats(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	issuerA := "G" + strings.Repeat("A", 55)
	issuerB := "G" + strings.Repeat("B", 55)
	from := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	in := from.Add(time.Hour)

	n := 0
	seed := func(source, base, quote string, ts time.Time, usd any) {
		t.Helper()
		n++
		_, err := store.DB().ExecContext(ctx, `
			INSERT INTO trades (source, ledger, tx_hash, op_index, ts, base_asset, quote_asset, base_amount, quote_amount, usd_volume)
			VALUES ($1, $2, $3, 0, $4::timestamptz, $5, $6, 1, 1, $7)`,
			source, 1000+n, strings.Repeat("0", 63)+string(rune('a'+n%26)), ts, base, quote, usd)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// external: 2 priced, 1 unpriced.
	seed("binance", "crypto:BTC", "fiat:USD", in, "10")
	seed("binance", "crypto:ETH", "fiat:USD", in.Add(time.Minute), "20")
	seed("binance", "crypto:XYZ", "fiat:ZZZ", in.Add(2*time.Minute), nil)
	// on-chain: 1 priced, 1 unpriced routable, 2 unroutable (one issuer), 1 mixed-issuer unpriced.
	seed("sdex", "native", "USDC-"+issuerA, in, "5")
	seed("sdex", "native", "FOO-"+issuerA, in.Add(time.Minute), nil)
	seed("sdex", "FOO-"+issuerA, "BAR-"+issuerA, in.Add(2*time.Minute), nil)
	seed("sdex", "BAR-"+issuerA, "FOO-"+issuerA, in.Add(3*time.Minute), nil)
	seed("sdex", "FOO-"+issuerA, "BAR-"+issuerB, in.Add(4*time.Minute), nil)
	// outside the window (both edges: end is exclusive) and an unrequested source.
	seed("binance", "crypto:BTC", "fiat:USD", from.Add(-time.Second), "1")
	seed("binance", "crypto:BTC", "fiat:USD", to, nil)
	seed("soroswap_router", "native", "FOO-"+issuerA, in, nil)

	got, err := store.UsdVolumePricingStats(ctx, from, to, []string{"binance", "sdex", "kraken"})
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	want := []timescale.UsdVolumePricingRow{
		{Source: "binance", Trades: 3, Priced: 2, Unpriced: 1, Unroutable: 0},
		{Source: "kraken"},
		{Source: "sdex", Trades: 5, Priced: 1, Unpriced: 2, Unroutable: 2},
	}
	if len(got) != len(want) {
		t.Fatalf("rows = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
