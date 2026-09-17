//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestStoreMonthlyUSDVWAPs_FoldsAliasSpellingsPerMonth executes the
// cohort pages' "USD then" price read against real TimescaleDB: trades in
// two months, XLM spelled BOTH ways ('native' and 'crypto:XLM') against
// two USD proxies (USDC and fiat:USD), a second classic asset, an
// XLM-quoted (non-USD) market that must not count, and a month before
// the range that must not be read.
//
// The served row per (asset, month) is Σ quote / Σ base over EVERY
// folded row — the union market's VWAP — so May's XLM price is
// (10 + 60 + 25) / (100 + 300 + 100) = 0.19, not the mean of 0.1, 0.2
// and 0.25 (0.1833…), and not either spelling's own figure.
func TestStoreMonthlyUSDVWAPs_FoldsAliasSpellingsPerMonth(t *testing.T) {
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
	usdc, err := c.NewClassicAsset("USDC", issuer)
	if err != nil {
		t.Fatal(err)
	}
	aqua, err := c.NewClassicAsset("AQUA", issuer)
	if err != nil {
		t.Fatal(err)
	}
	native, err := c.ParseAsset("native")
	if err != nil {
		t.Fatal(err)
	}
	cryptoXLM, err := c.ParseAsset("crypto:XLM")
	if err != nil {
		t.Fatal(err)
	}
	fiatUSD, err := c.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatal(err)
	}
	pair := func(base, quote c.Asset) c.Pair {
		p, err := c.NewPair(base, quote)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	apr := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	may := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	jun := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	for i, tr := range []c.Trade{
		// May, XLM in both spellings, against two USD proxies.
		mkAPITrade(1, may, pair(native, usdc), 1_000_000_000, 100_000_000),                        // 100 XLM for 10 USDC → 0.10
		mkAPITrade(2, may.Add(time.Hour), pair(cryptoXLM, usdc), 3_000_000_000, 600_000_000),      // 300 XLM for 60 USDC → 0.20
		mkAPITrade(3, may.Add(2*time.Hour), pair(cryptoXLM, fiatUSD), 1_000_000_000, 250_000_000), // 100 XLM for 25 USD → 0.25
		// May, a second classic asset.
		mkAPITrade(4, may.Add(3*time.Hour), pair(aqua, usdc), 2_000_000_000, 10_000_000), // 200 AQUA for 1 USDC → 0.005
		// May, XLM quoted in AQUA: not a USD proxy, must not count.
		mkAPITrade(5, may.Add(4*time.Hour), pair(native, aqua), 1_000_000_000, 5_000_000_000),
		// June, XLM alone.
		mkAPITrade(6, jun, pair(native, usdc), 2_000_000_000, 600_000_000), // 200 XLM for 60 USDC → 0.30
		// April, before the range: must not be read.
		mkAPITrade(7, apr, pair(native, usdc), 1_000_000_000, 1_000_000_000), // 1.00
	} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade[%d]: %v", i, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1mo', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1mo: %v", err)
	}

	from := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	rows, err := store.MonthlyUSDVWAPs(ctx, from, to)
	if err != nil {
		t.Fatalf("MonthlyUSDVWAPs: %v", err)
	}

	type key struct {
		asset string
		month string
	}
	want := map[key]string{
		{aqua.String(), "2026-05"}: "0.005",
		{"native", "2026-05"}:      "0.19",
		{"native", "2026-06"}:      "0.3",
	}
	got := map[key]string{}
	for _, r := range rows {
		got[key{r.Asset, r.Month.UTC().Format("2006-01")}] = r.VWAPUSD
		if r.Month.UTC().Day() != 1 || r.Month.UTC().Hour() != 0 {
			t.Errorf("month %s is not a month start", r.Month.UTC().Format(time.RFC3339))
		}
		if r.VolumeUSD < 0 {
			t.Errorf("%s @ %s: negative volume_usd %v", r.Asset, r.Month.UTC().Format("2006-01"), r.VolumeUSD)
		}
	}
	if len(rows) != len(want) {
		t.Errorf("rows = %d, want %d: %+v", len(rows), len(want), rows)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s @ %s: vwap_usd = %q, want %q (rows: %+v)", k.asset, k.month, got[k], v, rows)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected row %s @ %s = %q (the crypto:XLM spelling must fold into 'native'; a non-USD quote and a month before the range must not appear)", k.asset, k.month, got[k])
		}
	}
	// Ascending by (month, asset) — the loader's insert order.
	for i := 1; i < len(rows); i++ {
		a, b := rows[i-1], rows[i]
		if a.Month.After(b.Month) || (a.Month.Equal(b.Month) && a.Asset > b.Asset) {
			t.Errorf("rows not ascending by (month, asset) at %d: %+v then %+v", i, a, b)
		}
	}
}
