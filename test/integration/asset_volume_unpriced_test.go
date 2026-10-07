//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/canonical/discovery"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func unpricedTestContract(t *testing.T, b byte) c.Asset {
	t.Helper()
	var raw [32]byte
	raw[0], raw[31] = 0xA5, b
	id, err := strkey.Encode(strkey.VersionByteContract, raw[:])
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.NewSorobanAsset(id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestAssetVolume_UnpricedTradesFlagLowerBound runs migration 0212 and the
// refresh SQL end to end. prices_1m sums coalesce(usd_volume, 0), so an
// unpriced token/token trade adds 0 to vol_usd; the rollup must count it and
// every reader must serve the volume as a lower bound. A fully priced asset
// stays unflagged, and an asset whose only trades the volume sum has not
// seen gets no row, so it cannot enter the Soroban listing spine.
func TestAssetVolume_UnpricedTradesFlagLowerBound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	c.InstallAliasRegistry(nil)
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	tokA := unpricedTestContract(t, 1) // unpriced trade vs tokB → flagged
	tokB := unpricedTestContract(t, 2)
	tokC := unpricedTestContract(t, 3) // priced only → not flagged
	tokD := unpricedTestContract(t, 4) // unpriced, not yet in prices_1m → no row
	usd, _ := c.NewFiatAsset("USD")
	pairAB, _ := c.NewPair(tokA, tokB)
	pairCUSD, _ := c.NewPair(tokC, usd)
	pairDB, _ := c.NewPair(tokD, tokB)

	now := time.Now().UTC()
	for i, a := range []c.Asset{tokA, tokC, tokD} {
		if err := store.RecordDiscovered(ctx, discovery.Hit{
			ContractID:        a.String(),
			Kind:              discovery.KindSEP41,
			EventType:         discovery.EventTransfer,
			Ledger:            uint32(50_000_000 + i),
			ObservedAtRFC3339: now.Add(-24 * time.Hour).Format(time.RFC3339),
		}); err != nil {
			t.Fatalf("RecordDiscovered: %v", err)
		}
	}

	ts := now.Add(-2 * time.Hour).Truncate(time.Second)
	for _, tr := range []c.Trade{
		mkIntegrationTrade("soroswap", 1, ts, pairAB, 500, 900),
		mkIntegrationTrade("binance", 2, ts, pairCUSD, 100_000_000, 300_000_000),
		// Outside the 24h window and a week back, so the count's plan has a
		// second trades chunk to exclude.
		mkIntegrationTrade("soroswap", 3, now.Add(-8*24*time.Hour), pairAB, 500, 900),
	} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %s: %v", tr.Source, err)
		}
	}
	var nullAB, nullC int
	if err := store.DB().QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE base_asset = $1 AND usd_volume IS NULL AND ts > now() - INTERVAL '1 day'),
		       count(*) FILTER (WHERE base_asset = $2 AND usd_volume IS NULL)
		  FROM trades`, tokA.String(), tokC.String()).Scan(&nullAB, &nullC); err != nil {
		t.Fatal(err)
	}
	if nullAB != 1 || nullC != 0 {
		t.Fatalf("fixture: unpriced A/B=%d (want 1), unpriced C/USD=%d (want 0)", nullAB, nullC)
	}
	if _, err := store.DB().ExecContext(ctx, `CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}
	// D trades after prices_1m materialised: the count sees it, the volume
	// sum does not (the shape of cagg lag on a live host).
	if err := store.InsertTrade(ctx, mkIntegrationTrade("soroswap", 4, ts, pairDB, 500, 900)); err != nil {
		t.Fatalf("InsertTrade D: %v", err)
	}

	if err := store.RefreshAssetVolume24h(ctx); err != nil {
		t.Fatalf("RefreshAssetVolume24h: %v", err)
	}

	counts := map[string]int64{}
	rows, err := store.DB().QueryContext(ctx, `SELECT asset_id, unpriced_trades FROM asset_volume_24h`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			t.Fatal(err)
		}
		counts[id] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got, ok := counts[tokA.String()]; !ok || got != 1 {
		t.Errorf("unpriced_trades[A] = %d (row %v), want 1", got, ok)
	}
	if got, ok := counts[tokC.String()]; !ok || got != 0 {
		t.Errorf("unpriced_trades[C] = %d (row %v), want 0", got, ok)
	}
	if _, ok := counts[tokD.String()]; ok {
		t.Errorf("the unpriced count created an asset_volume_24h row for D; it must only annotate volume rows")
	}

	listed, err := store.ListAssetsExt(ctx, timescale.ListAssetsOptions{Type: "soroban", Limit: 50})
	if err != nil {
		t.Fatalf("ListAssetsExt: %v", err)
	}
	byID := map[string]timescale.AssetRow{}
	for _, r := range listed {
		byID[r.AssetID] = r
	}
	if r, ok := byID[tokA.String()]; !ok || !r.VolumeLowerBound {
		t.Errorf("listing A: present=%v volume_lower_bound=%v, want present and true", ok, r.VolumeLowerBound)
	}
	if r, ok := byID[tokC.String()]; !ok || r.VolumeLowerBound {
		t.Errorf("listing C: present=%v volume_lower_bound=%v, want present and false", ok, r.VolumeLowerBound)
	}
	if _, ok := byID[tokD.String()]; ok {
		t.Errorf("listing admitted D, whose only trade is unpriced and outside the volume sum")
	}

	cat, err := store.ContractCatalogueRows(ctx, []string{tokA.String(), tokC.String()})
	if err != nil {
		t.Fatalf("ContractCatalogueRows: %v", err)
	}
	if !cat[tokA.String()].VolumeLowerBound || cat[tokC.String()].VolumeLowerBound {
		t.Errorf("/v1/contracts rows: A=%v C=%v, want true/false",
			cat[tokA.String()].VolumeLowerBound, cat[tokC.String()].VolumeLowerBound)
	}
	for _, tc := range []struct {
		a    c.Asset
		want bool
	}{{tokA, true}, {tokC, false}} {
		if _, lb, err := store.Volume24hUSDForAsset(ctx, tc.a.String()); err != nil || lb != tc.want {
			t.Errorf("Volume24hUSDForAsset(%s) lowerBound=%v err=%v, want %v", tc.a, lb, err, tc.want)
		}
		row, err := store.GetAssetByAssetID(ctx, tc.a.String())
		if err != nil || row.VolumeLowerBound != tc.want {
			t.Errorf("GetAssetByAssetID(%s) volume_lower_bound=%v err=%v, want %v", tc.a, row.VolumeLowerBound, err, tc.want)
		}
	}

	// Read-only plan of the count over a 24h window with a second, older
	// chunk present: it must not scan the out-of-window chunk.
	var plan []string
	prow, err := store.DB().QueryContext(ctx, `EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, SUMMARY OFF)
		SELECT leg.asset_id, count(*)
		  FROM trades tr
		 CROSS JOIN LATERAL (VALUES (tr.base_asset), (tr.quote_asset)) AS leg(asset_id)
		 WHERE tr.ts >= now() - INTERVAL '24 hours' AND tr.ts < now() AND tr.usd_volume IS NULL
		 GROUP BY leg.asset_id`)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	for prow.Next() {
		var line string
		if err := prow.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	_ = prow.Close()
	t.Logf("unpriced count plan:\n%s", strings.Join(plan, "\n"))
	var chunks int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM timescaledb_information.chunks WHERE hypertable_name = 'trades'`).Scan(&chunks); err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, l := range plan {
		if strings.Contains(l, "_hyper_") && strings.Contains(l, "Scan") {
			scanned++
		}
	}
	if chunks < 2 || scanned >= chunks {
		t.Errorf("chunk exclusion: %d trades chunks, plan scans %d; want >=2 chunks and fewer scanned", chunks, scanned)
	}
}
