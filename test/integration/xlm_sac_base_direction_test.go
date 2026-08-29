//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"math/big"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/canonical/discovery"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestXLMSacAsBase_PriceableThroughEveryPath reproduces the r1
// 2026-08-28 17:42Z firing of stellarindex_assets_popular_priceless=2
// (CBIJ… $730k/7d, CAUP7… only trades against CBIJ) and pins the fix.
//
// The aquarius decoder writes SWAP direction (base = token_in) without
// canonical.Orient, so an asset bought with XLM lands in prices_1m as
// (XLM-SAC, asset) — the XLM SAC as BASE. Every price path read the XLM
// leg base-side only (`base_asset = X AND quote_asset IN (native, SAC)`),
// so that market was invisible to:
//
//   - the catalogue listing/detail asset_vs_xlm CTEs (price NULL);
//   - the transitive resolver's hop_usd (a hop whose own XLM market is
//     SAC-as-base priced NULL, and the XLM SAC itself never resolved
//     because XLM/USD is keyed base_asset='native');
//   - the coverage tripwire's priced_direct (so one_hop could not route
//     through the XLM SAC either).
//
// Meanwhile the volume path already handled both directions
// (soroban_volume.go), which is why the asset had $730k of volume and no
// price — the tripwire's exact definition of a coverage gap.
//
// Fixture (all trades inside the trailing 24h, closed buckets):
//
//	XLM/USDC  (native base)   vwap 0.40            → xlm_usd = 0.40
//	SAC/CBIJ  (SAC as BASE)   vwap 4  (25 buckets) → CBIJ = 0.25 XLM = 0.10 USD
//	CBIJ/CAUP7 + CAUP7/CBIJ   vwap 0.5 / 2         → CAUP7 = 2 CBIJ = 0.20 USD
//	native/ZINV (native base) vwap 0.5             → ZINV = 2 XLM = 0.80 USD
//	ZDIR/native + native/ZDIR (both directions, inverted row fresher)
//
// ZDIR is the byte-identity guard: an asset that ALREADY priced through a
// base-side row must keep that exact value after the inverted arm lands
// (the inverted row is fresher and disagrees — it must not win).
func TestXLMSacAsBase_PriceableThroughEveryPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		usdcIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		cbijID     = "CBIJBDNZNF4X35BJ4FFZWCDBSCKOP5NB4PLG4SNENRMLAPYG4P5FM6VN"
		caup7ID    = "CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J"
		zdirIssuer = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
		zinvIssuer = "GDM4RQUQQUVSKQA7S6EM7XBZP3FCGH4Q7CL6TABQ7B2BEJ5ERARM2M5M"
	)

	// Classic USDC recognised as a USD peg so XLM/USDC carries usd_volume
	// (the asset_volume_24h rollup + the tripwire's vol7d read it).
	spec, err := timescale.NewUSDVolumeQuoteSpec([]string{"USDC-" + usdcIssuer}, nil)
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	store.SetUSDVolumeQuoteSpec(spec)

	mustClassic := func(code, issuer string) c.Asset {
		t.Helper()
		a, err := c.NewClassicAsset(code, issuer)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	mustSoroban := func(id string) c.Asset {
		t.Helper()
		a, err := c.NewSorobanAsset(id)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	mustPair := func(base, quote c.Asset) c.Pair {
		t.Helper()
		p, err := c.NewPair(base, quote)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	xlm := c.NativeAsset()
	xlmSAC := mustSoroban(c.XLMSacContractID)
	usdc := mustClassic("USDC", usdcIssuer)
	cbij := mustSoroban(cbijID)
	caup7 := mustSoroban(caup7ID)
	zdir := mustClassic("ZDIR", zdirIssuer)
	zinv := mustClassic("ZINV", zinvIssuer)

	seedIssuers(t, ctx, store, []seedIssuer{
		{g: zdirIssuer}, {g: zinvIssuer}, {g: usdcIssuer},
	})
	seedClassicAssets(t, ctx, store, []seedAsset{
		{assetID: zdir.String(), code: "ZDIR", issuer: zdirIssuer, slug: "ZDIR", obs: 10},
		{assetID: zinv.String(), code: "ZINV", issuer: zinvIssuer, slug: "ZINV", obs: 10},
	})
	// Soroban-native contracts live in discovered_assets only (no
	// classic_assets row is possible — issuer_g_strkey NOT NULL).
	now := time.Now().UTC().Truncate(time.Minute)
	for _, id := range []string{cbijID, caup7ID} {
		if err := store.RecordDiscovered(ctx, discovery.Hit{
			ContractID:        id,
			Kind:              discovery.KindSEP41,
			EventType:         discovery.EventTransfer,
			Ledger:            50_000_000,
			ObservedAtRFC3339: now.Add(-24 * time.Hour).Format(time.RFC3339),
		}); err != nil {
			t.Fatalf("RecordDiscovered %s: %v", id, err)
		}
	}

	var trades []c.Trade
	nonce := 0
	add := func(source string, ts time.Time, pair c.Pair, base, quote int64) {
		nonce++
		trades = append(trades, mkIntegrationTrade(source, nonce, ts, pair, base, quote))
	}

	// xlm_usd anchor: 100 XLM → 40 USDC, twice.
	add("sdex", now.Add(-30*time.Minute), mustPair(xlm, usdc), 1_000_000_000, 400_000_000)
	add("sdex", now.Add(-10*time.Minute), mustPair(xlm, usdc), 1_000_000_000, 400_000_000)

	// 25 distinct minute-buckets spanning 8h (clears the one-hop floors:
	// >= 20 buckets, >= 6h span; usd_volume set below clears >= $1000).
	for i := 0; i < 25; i++ {
		ts := now.Add(-9 * time.Hour).Add(time.Duration(i) * 20 * time.Minute)
		// xlm_usd anchor in every hour the CBIJ market trades, so the
		// price-history series (which triangulate per bucket) has an
		// XLM/USD leg wherever it has an XLM leg. Same 0.40 as above.
		add("sdex", ts, mustPair(xlm, usdc), 1_000_000_000, 400_000_000)
		// XLM SAC as BASE: 10 XLM → 40 CBIJ (vwap 4 → CBIJ = 0.25 XLM).
		add("aquarius", ts, mustPair(xlmSAC, cbij), 100_000_000, 400_000_000)
		// CBIJ/CAUP7 in BOTH stored directions, one consistent price
		// (CAUP7 = 2 CBIJ).
		if i%2 == 0 {
			add("aquarius", ts, mustPair(cbij, caup7), 20_000_000, 10_000_000)
		} else {
			add("aquarius", ts, mustPair(caup7, cbij), 10_000_000, 20_000_000)
		}
	}

	// ZDIR: base-side row at -40m (2.0 XLM), inverted row at -10m that
	// says 1.5 XLM. Base-side must keep winning → 0.80 USD, not 0.60.
	add("sdex", now.Add(-40*time.Minute), mustPair(zdir, xlm), 1_000_000_000, 2_000_000_000)
	add("aquarius", now.Add(-10*time.Minute), mustPair(xlm, zdir), 1_500_000_000, 1_000_000_000)
	// ZINV: ONLY the native-as-base direction. 100 XLM → 50 ZINV
	// (vwap 0.5 → ZINV = 2 XLM = 0.80 USD).
	add("aquarius", now.Add(-20*time.Minute), mustPair(xlm, zinv), 1_000_000_000, 500_000_000)

	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %s/%d: %v", tr.Source, tr.Ledger, err)
		}
	}
	// The aquarius legs have no USD-pegged quote, so insert-time
	// usd_volume is NULL; in production the usd-volume resolver values
	// them. Stamp $100 per trade directly — this test is about the READ
	// paths, and the tripwire / rollup only need the volume to exist.
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 100 WHERE source = 'aquarius'`); err != nil {
		t.Fatalf("stamp usd_volume: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}
	if err := store.RefreshAssetVolume24h(ctx); err != nil {
		t.Fatalf("RefreshAssetVolume24h: %v", err)
	}

	wantPrice := func(t *testing.T, label string, got *string, want string) {
		t.Helper()
		if got == nil {
			t.Errorf("%s price_usd = nil, want %s", label, want)
			return
		}
		if *got != want {
			t.Errorf("%s price_usd = %s, want %s", label, *got, want)
		}
	}
	ratEq := func(s, want string) bool {
		a, ok1 := new(big.Rat).SetString(s)
		b, ok2 := new(big.Rat).SetString(want)
		return ok1 && ok2 && a.Cmp(b) == 0
	}

	t.Run("TransitiveUSDPrice", func(t *testing.T) {
		// CBIJ: hop is the XLM SAC itself (its only XLM market is
		// SAC-as-base), which must resolve to xlm_usd.
		tp, ok, err := store.TransitiveUSDPrice(ctx, cbijID)
		if err != nil {
			t.Fatalf("TransitiveUSDPrice(CBIJ): %v", err)
		}
		if !ok {
			t.Errorf("TransitiveUSDPrice(CBIJ): no route, want 0.10 via hop %s", c.XLMSacContractID)
		} else {
			if tp.Hop != c.XLMSacContractID {
				t.Errorf("CBIJ hop = %s, want the XLM SAC %s", tp.Hop, c.XLMSacContractID)
			}
			if !ratEq(tp.PriceUSD, "0.10") {
				t.Errorf("CBIJ transitive price = %s, want 0.10", tp.PriceUSD)
			}
		}
		// CAUP7: hop CBIJ, whose own XLM market is SAC-as-base.
		tp, ok, err = store.TransitiveUSDPrice(ctx, caup7ID)
		if err != nil {
			t.Fatalf("TransitiveUSDPrice(CAUP7): %v", err)
		}
		if !ok {
			t.Errorf("TransitiveUSDPrice(CAUP7): no route, want 0.20 via hop CBIJ")
		} else {
			if tp.Hop != cbijID {
				t.Errorf("CAUP7 hop = %s, want CBIJ", tp.Hop)
			}
			if !ratEq(tp.PriceUSD, "0.20") {
				t.Errorf("CAUP7 transitive price = %s, want 0.20", tp.PriceUSD)
			}
		}
	})

	t.Run("PopularPricelessCandidates", func(t *testing.T) {
		got, err := store.PopularPricelessCandidates(ctx)
		if err != nil {
			t.Fatalf("PopularPricelessCandidates: %v", err)
		}
		for _, sig := range got {
			switch sig.AssetID {
			case cbijID, caup7ID, c.XLMSacContractID:
				t.Errorf("tripwire still reports %s as priceless (vol_7d=%.0f trades_7d=%d) — "+
					"it is priceable through the XLM SAC", sig.AssetID, sig.Volume7dUSD, sig.Trades7d)
			}
		}
	})

	t.Run("ListAssets", func(t *testing.T) {
		rows, err := store.ListAssets(ctx, 50, "", "")
		if err != nil {
			t.Fatalf("ListAssets: %v", err)
		}
		byID := make(map[string]timescale.AssetRow, len(rows))
		for _, r := range rows {
			byID[r.AssetID] = r
		}
		for _, tc := range []struct{ id, want string }{
			{cbijID, "0.1000000000"},
			{zinv.String(), "0.8000000000"},
			{zdir.String(), "0.8000000000"},
		} {
			row, ok := byID[tc.id]
			if !ok {
				t.Errorf("listing: %s missing", tc.id)
				continue
			}
			wantPrice(t, "listing "+tc.id, row.PriceUSD, tc.want)
		}
	})

	// Price-history series (the sparklines): every bucket that has an
	// XLM leg in EITHER stored direction must carry a point. Pre-fix
	// (#254 follow-up) the four series CTEs read the XLM leg base-side
	// only, so CBIJ and ZINV had a headline price but an EMPTY series.
	//
	// ZDIR is the per-bucket byte-identity guard: the bucket holding its
	// base-side row (-40m, 2.0 XLM) must price 0.80 even though a
	// fresher inverted row (-10m, 1.5 XLM) exists — and when that
	// fresher row falls in a LATER bucket of its own, that bucket is
	// filled from the inverted arm (0.60), which is the fix working.
	const bucketFmt = "2006-01-02T15:04:05Z"
	checkSeries := func(t *testing.T, label string, pts []timescale.AssetPricePoint, want string) {
		t.Helper()
		nonNil := 0
		for _, pt := range pts {
			if pt.P == nil {
				continue
			}
			nonNil++
			if *pt.P != want {
				t.Errorf("%s: series point %s at %s, want %s", label, *pt.P, pt.T, want)
			}
		}
		if nonNil == 0 {
			t.Errorf("%s: series has no non-null points, want %s — "+
				"the XLM leg is stored SAC-as-base and the series CTE is base-side only", label, want)
		}
	}
	checkZDIR := func(t *testing.T, label string, pts []timescale.AssetPricePoint, trunc time.Duration) {
		t.Helper()
		baseT := now.Add(-40 * time.Minute).Truncate(trunc).Format(bucketFmt)
		invT := now.Add(-10 * time.Minute).Truncate(trunc).Format(bucketFmt)
		byT := make(map[string]*string, len(pts))
		for _, pt := range pts {
			byT[pt.T] = pt.P
		}
		wantPrice(t, label+" bucket "+baseT+" (base-side row present)", byT[baseT], "0.8000000000")
		if invT != baseT {
			wantPrice(t, label+" bucket "+invT+" (inverted row only)", byT[invT], "0.6000000000")
		}
	}
	seriesCases := []struct{ id, want string }{
		{cbijID, "0.1000000000"},
		{zinv.String(), "0.8000000000"},
	}

	t.Run("PriceHistory24h", func(t *testing.T) {
		for _, tc := range seriesCases {
			pts, err := store.GetAssetPriceHistory24h(ctx, tc.id)
			if err != nil {
				t.Fatalf("GetAssetPriceHistory24h(%s): %v", tc.id, err)
			}
			checkSeries(t, "24h "+tc.id, pts, tc.want)
		}
		pts, err := store.GetAssetPriceHistory24h(ctx, zdir.String())
		if err != nil {
			t.Fatalf("GetAssetPriceHistory24h(ZDIR): %v", err)
		}
		checkZDIR(t, "24h ZDIR", pts, time.Hour)
	})

	t.Run("PriceHistory7d", func(t *testing.T) {
		for _, tc := range seriesCases {
			pts, err := store.GetAssetPriceHistory7d(ctx, tc.id)
			if err != nil {
				t.Fatalf("GetAssetPriceHistory7d(%s): %v", tc.id, err)
			}
			checkSeries(t, "7d "+tc.id, pts, tc.want)
		}
		pts, err := store.GetAssetPriceHistory7d(ctx, zdir.String())
		if err != nil {
			t.Fatalf("GetAssetPriceHistory7d(ZDIR): %v", err)
		}
		checkZDIR(t, "7d ZDIR", pts, 24*time.Hour)
	})

	batchIDs := []string{cbijID, zinv.String(), zdir.String()}

	t.Run("PriceHistory24hBatch", func(t *testing.T) {
		got, err := store.GetAssetsPriceHistory24hBatch(ctx, batchIDs)
		if err != nil {
			t.Fatalf("GetAssetsPriceHistory24hBatch: %v", err)
		}
		for _, tc := range seriesCases {
			checkSeries(t, "24h batch "+tc.id, got[tc.id], tc.want)
		}
		checkZDIR(t, "24h batch ZDIR", got[zdir.String()], time.Hour)
	})

	t.Run("PriceHistory7dBatch", func(t *testing.T) {
		got, err := store.GetAssetsPriceHistory7dBatch(ctx, batchIDs)
		if err != nil {
			t.Fatalf("GetAssetsPriceHistory7dBatch: %v", err)
		}
		for _, tc := range seriesCases {
			checkSeries(t, "7d batch "+tc.id, got[tc.id], tc.want)
		}
		checkZDIR(t, "7d batch ZDIR", got[zdir.String()], 24*time.Hour)
	})

	t.Run("GetAssetBySlug", func(t *testing.T) {
		for _, tc := range []struct{ id, want string }{
			{cbijID, "0.1000000000"},
			{zinv.String(), "0.8000000000"},
			{zdir.String(), "0.8000000000"},
		} {
			row, err := store.GetAssetBySlug(ctx, tc.id)
			if errors.Is(err, sql.ErrNoRows) {
				t.Errorf("detail %s: sql.ErrNoRows — the detail spine cannot see this asset", tc.id)
				continue
			}
			if err != nil {
				t.Fatalf("GetAssetBySlug(%s): %v", tc.id, err)
			}
			wantPrice(t, "detail "+tc.id, row.PriceUSD, tc.want)
		}
		// CAUP7 has no XLM/USD market of its own, so the SQL row prices
		// nil (the API fills it transitively) — but the ROW must exist,
		// or the detail handler's catalogue arm never runs for it.
		row, err := store.GetAssetBySlug(ctx, caup7ID)
		if err != nil {
			t.Errorf("GetAssetBySlug(CAUP7) = %v, want a discovered_assets-backed row", err)
		} else if row.AssetID != caup7ID {
			t.Errorf("GetAssetBySlug(CAUP7).AssetID = %s", row.AssetID)
		}
	})
}
