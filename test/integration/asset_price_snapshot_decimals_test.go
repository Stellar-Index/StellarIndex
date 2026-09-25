//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/canonical/discovery"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Public Soroban contract ids used as fixtures. Four tokens, one per
// decimals scale the correction has to get right:
//
//	decimalsNineContract     9 dp  — the scale of the token confirmed on
//	                                 mainnet; factor 10^2
//	decimalsEighteenContract 18 dp — factor 10^11, where rounding the RAW
//	                                 ratio first destroys the value
//	decimalsFiveContract     5 dp  — scales DOWN (factor 10^-2); the RWA
//	                                 population holds 5-decimals funds
//	decimalsSevenContract    7 dp  — never flagged; must not move at all
const (
	decimalsNineContract     = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"
	decimalsEighteenContract = "CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J"
	decimalsFiveContract     = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	decimalsSevenContract    = "CBIJBDNZNF4X35BJ4FFZWCDBSCKOP5NB4PLG4SNENRMLAPYG4P5FM6VN"
)

// seedDecimalsFixture fills a migrated store with one market per
// fixture token, each through a DIFFERENT arm of the catalogue's price
// chain, so a factor that is right for one arm and wrong for another
// cannot pass:
//
//	nine      direct, USDC-quoted            true price  2.50 USD
//	eighteen  XLM-SAC-quoted, base side      true price 14.00 USD
//	five      XLM-SAC-quoted, INVERTED       true price  2.00 USD
//	seven     direct, USDC-quoted (control)  true price  0.65 USD
//
// XLM/USDC is a constant 0.40. Every amount is a smallest-unit integer
// at the token's REAL scale, which is what makes the stored prices_1m
// ratio raw: nine trades 1000 tokens (1e12 units at 9 dp) for 2500 USDC
// (2.5e10 units at 7 dp), so its vwap is 0.025 — a hundredth of 2.50.
//
// The three non-7 tokens are confirmed in nonstandard_decimals_assets
// before the rollups run; the refresh itself is left to the caller.
func seedDecimalsFixture(t *testing.T, ctx context.Context, store *timescale.Store) {
	t.Helper()

	const usdcIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	soroban := func(id string) c.Asset {
		t.Helper()
		a, err := c.NewSorobanAsset(id)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	pair := func(base, quote c.Asset) c.Pair {
		t.Helper()
		p, err := c.NewPair(base, quote)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	usdc, err := c.NewClassicAsset("USDC", usdcIssuer)
	if err != nil {
		t.Fatal(err)
	}
	xlmSAC := soroban(c.XLMSacContractID)
	nine, eighteen := soroban(decimalsNineContract), soroban(decimalsEighteenContract)
	five, seven := soroban(decimalsFiveContract), soroban(decimalsSevenContract)

	now := time.Now().UTC().Truncate(time.Minute)
	for _, id := range []string{
		decimalsNineContract, decimalsEighteenContract, decimalsFiveContract, decimalsSevenContract,
	} {
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

	early, late := now.Add(-30*time.Minute), now.Add(-10*time.Minute)
	nonce := 0
	add := func(source string, ts time.Time, p c.Pair, base, quote int64) {
		t.Helper()
		nonce++
		if err := store.InsertTrade(ctx, mkIntegrationTrade(source, nonce, ts, p, base, quote)); err != nil {
			t.Fatalf("InsertTrade %s #%d: %v", source, nonce, err)
		}
	}
	for _, ts := range []time.Time{early, late} {
		// XLM/USDC 0.40 — the xlm_usd leg of both triangulated tokens.
		add("sdex", ts, pair(c.NativeAsset(), usdc), 1_000_000_000, 400_000_000)
		// nine: 1000 tokens (9 dp) for 2500 USDC. Raw vwap 0.025.
		add("soroswap", ts, pair(nine, usdc), 1_000_000_000_000, 25_000_000_000)
		// eighteen: 2 tokens (18 dp) for 70 XLM. Raw vwap 3.5e-10, so the
		// raw USD figure is 1.4e-10 — which ROUND(…, 10) cuts to 1e-10.
		add("soroswap", ts, pair(eighteen, xlmSAC), 2_000_000_000_000_000_000, 700_000_000)
		// five: stored with XLM as BASE (the swap-direction sources).
		// 50 XLM for 10 tokens (5 dp). Raw vwap 0.002, inverted 500.
		add("aquarius", ts, pair(xlmSAC, five), 500_000_000, 1_000_000)
		// seven: 100 tokens for 65 USDC. Raw vwap 0.65 IS the price.
		add("soroswap", ts, pair(seven, usdc), 1_000_000_000, 650_000_000)
	}
	// nine an hour ago at HALF the price, inside the change_1h window
	// (55–65 min). Both legs of the change are raw, so the scale cancels
	// and the percentage must read +100.00 whether or not the price
	// itself is corrected.
	add("soroswap", now.Add(-60*time.Minute), pair(nine, usdc), 1_000_000_000_000, 12_500_000_000)

	// The Soroban-venue legs carry no insert-time usd_volume; the listing
	// spine and GetAssetBySlug admit a discovered contract only once it
	// has an asset_volume_24h row. This test is about the PRICE.
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 100 WHERE usd_volume IS NULL`); err != nil {
		t.Fatalf("stamp usd_volume: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}
	for id, dec := range map[string]uint32{
		decimalsNineContract: 9, decimalsEighteenContract: 18, decimalsFiveContract: 5,
	} {
		if err := store.UpsertNonstandardDecimalsAsset(ctx, id, dec, "soroswap"); err != nil {
			t.Fatalf("UpsertNonstandardDecimalsAsset %s: %v", id, err)
		}
	}
}

// TestAssetPriceSnapshot_NormalisesNonstandardDecimals is the F017
// regression for the /v1/assets LISTING price. asset_price_snapshot's
// writer stored the RAW prices_1m ratio, so every reader of the rollup
// — the listing spine and ContractCatalogueRows, the latter feeding the
// RWA contract listing's market cap — published a price off by
// 10^(7 - decimals) for a confirmed non-7-decimals token. Before the
// fix the three flagged rows below read 0.0250000000, 0.0000000001 and
// 200.0000000000.
func TestAssetPriceSnapshot_NormalisesNonstandardDecimals(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedDecimalsFixture(t, ctx, store)

	if err := store.RefreshAssetListingRollups(ctx); err != nil {
		t.Fatalf("RefreshAssetListingRollups: %v", err)
	}

	want := map[string]string{
		decimalsNineContract:     "2.5000000000",
		decimalsEighteenContract: "14.0000000000", // NOT 10: rounded after the correction
		decimalsFiveContract:     "2.0000000000",
		decimalsSevenContract:    "0.6500000000", // unflagged: byte-identical
	}
	check := func(t *testing.T, surface string, rows map[string]timescale.AssetRow) {
		t.Helper()
		for id, wantPrice := range want {
			row, ok := rows[id]
			if !ok {
				t.Errorf("%s: row %s missing", surface, id)
				continue
			}
			if row.PriceUSD == nil {
				t.Errorf("%s: %s price_usd = nil, want %s", surface, id, wantPrice)
				continue
			}
			if *row.PriceUSD != wantPrice {
				t.Errorf("%s: %s price_usd = %s, want %s", surface, id, *row.PriceUSD, wantPrice)
			}
		}
	}

	t.Run("listing spine", func(t *testing.T) {
		got, err := store.ListAssetsExt(ctx, timescale.ListAssetsOptions{Limit: 50, Type: "soroban"})
		if err != nil {
			t.Fatalf("ListAssetsExt: %v", err)
		}
		byID := make(map[string]timescale.AssetRow, len(got))
		for _, r := range got {
			byID[r.AssetID] = r
		}
		check(t, "listing", byID)
		// The scale cancels in a change ratio; correcting the price must
		// not have touched it.
		if ch := byID[decimalsNineContract].Change1hPct; ch == nil || *ch != "100.00" {
			t.Errorf("listing: nine change_1h_pct = %s, want 100.00", derefOrNil(ch))
		}
	})

	t.Run("ContractCatalogueRows", func(t *testing.T) {
		got, err := store.ContractCatalogueRows(ctx, []string{
			decimalsNineContract, decimalsEighteenContract, decimalsFiveContract, decimalsSevenContract,
		})
		if err != nil {
			t.Fatalf("ContractCatalogueRows: %v", err)
		}
		check(t, "contract catalogue", got)
	})

	// The stored column is exact, not merely right to ten places: the
	// correction multiplies an unrounded NUMERIC by an exact power of ten.
	t.Run("stored value is exact", func(t *testing.T) {
		for id, exact := range map[string]string{
			decimalsNineContract: "2.5", decimalsEighteenContract: "14", decimalsFiveContract: "2",
		} {
			var equal bool
			if err := store.DB().QueryRowContext(ctx,
				`SELECT price_usd = $2::numeric FROM asset_price_snapshot WHERE asset_id = $1`,
				id, exact).Scan(&equal); err != nil {
				t.Fatalf("read snapshot %s: %v", id, err)
			}
			if !equal {
				t.Errorf("snapshot %s price_usd is not exactly %s", id, exact)
			}
		}
	})

	// The reason the WRITER may normalise here when /v1/changes may not:
	// this rollup keeps no extreme across passes. Withdraw a confirmation
	// and the very next pass stores the raw ratio again; restore it and
	// the corrected price is back. Nothing from the other scale survives.
	t.Run("no residue across a flag change", func(t *testing.T) {
		stored := func() string {
			t.Helper()
			var p string
			if err := store.DB().QueryRowContext(ctx,
				`SELECT ROUND(price_usd, 10)::text FROM asset_price_snapshot WHERE asset_id = $1`,
				decimalsNineContract).Scan(&p); err != nil {
				t.Fatalf("read snapshot: %v", err)
			}
			return p
		}
		if err := store.DeleteNonstandardDecimalsAsset(ctx, decimalsNineContract); err != nil {
			t.Fatalf("DeleteNonstandardDecimalsAsset: %v", err)
		}
		if err := store.RefreshAssetListingRollups(ctx); err != nil {
			t.Fatalf("RefreshAssetListingRollups: %v", err)
		}
		if got := stored(); got != "0.0250000000" {
			t.Errorf("unflagged pass stored %s, want the raw 0.0250000000", got)
		}
		if err := store.UpsertNonstandardDecimalsAsset(ctx, decimalsNineContract, 9, "soroswap"); err != nil {
			t.Fatalf("UpsertNonstandardDecimalsAsset: %v", err)
		}
		if err := store.RefreshAssetListingRollups(ctx); err != nil {
			t.Fatalf("RefreshAssetListingRollups: %v", err)
		}
		if got := stored(); got != "2.5000000000" {
			t.Errorf("re-flagged pass stored %s, want 2.5000000000", got)
		}
	})
}

func derefOrNil(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// TestAssetCatalogue_RoundsAfterDecimalsCorrection is the F017 regression
// for the catalogue reads that stay RAW and are corrected by the API: the
// per-asset row's price_usd and the four price-history series. They
// rounded the raw ratio to 10 places BEFORE the correction could run, so
// an 18-decimals token worth 14 USD (raw 1.4e-10) came back as
// 0.0000000001 — exactly 10 USD once scaled — and the API could only
// withhold it.
//
// The read now rounds a confirmed token's raw ratio to 10 + k places for
// a 10^k correction, which is the corrected price rounded to 10 places.
// The strings asserted here are the ones the API's unit tests feed
// normalizeCatalogueReadUSD ("0.000000000140000000000" -> 14.0000000000),
// so the two halves are pinned to each other by value.
func TestAssetCatalogue_RoundsAfterDecimalsCorrection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedDecimalsFixture(t, ctx, store)
	// GetAssetBySlug admits a discovered contract only with a volume row.
	if err := store.RefreshAssetListingRollups(ctx); err != nil {
		t.Fatalf("RefreshAssetListingRollups: %v", err)
	}

	// RAW ratios, by design — only the number of places moves.
	cases := []struct {
		name, id, latest, earlier string
	}{
		// 9 dp, k = 2: 12 places.
		{"nine", decimalsNineContract, "0.025000000000", "0.012500000000"},
		// 18 dp, k = 11: 21 places. Was 0.0000000001.
		{"eighteen", decimalsEighteenContract, "0.000000000140000000000", ""},
		// 5 dp scales DOWN: k floors at 0, the 10 places it always had.
		{"five", decimalsFiveContract, "200.0000000000", ""},
		// Unflagged: byte-identical.
		{"seven", decimalsSevenContract, "0.6500000000", ""},
	}
	checkSeries := func(t *testing.T, label string, pts []timescale.AssetPricePoint, latest, earlier string) {
		t.Helper()
		last := ""
		for _, pt := range pts {
			if pt.P == nil {
				continue
			}
			if *pt.P != latest && (earlier == "" || *pt.P != earlier) {
				t.Errorf("%s: unexpected point %s at %s, want %s", label, *pt.P, pt.T, latest)
			}
			last = *pt.P
		}
		if last != latest {
			t.Errorf("%s: latest priced point = %q, want %s", label, last, latest)
		}
	}

	t.Run("GetAssetBySlug", func(t *testing.T) {
		for _, tc := range cases {
			row, err := store.GetAssetBySlug(ctx, tc.id)
			if err != nil {
				t.Fatalf("GetAssetBySlug(%s): %v", tc.name, err)
			}
			if row.PriceUSD == nil || *row.PriceUSD != tc.latest {
				t.Errorf("%s: price_usd = %s, want %s", tc.name, derefOrNil(row.PriceUSD), tc.latest)
			}
		}
	})
	t.Run("PriceHistory24h", func(t *testing.T) {
		for _, tc := range cases {
			pts, err := store.GetAssetPriceHistory24h(ctx, tc.id)
			if err != nil {
				t.Fatalf("GetAssetPriceHistory24h(%s): %v", tc.name, err)
			}
			checkSeries(t, "24h "+tc.name, pts, tc.latest, tc.earlier)
		}
	})
	t.Run("PriceHistory7d", func(t *testing.T) {
		for _, tc := range cases {
			pts, err := store.GetAssetPriceHistory7d(ctx, tc.id)
			if err != nil {
				t.Fatalf("GetAssetPriceHistory7d(%s): %v", tc.name, err)
			}
			checkSeries(t, "7d "+tc.name, pts, tc.latest, tc.earlier)
		}
	})

	ids := make([]string, 0, len(cases))
	for _, tc := range cases {
		ids = append(ids, tc.id)
	}
	t.Run("PriceHistory24hBatch", func(t *testing.T) {
		got, err := store.GetAssetsPriceHistory24hBatch(ctx, ids)
		if err != nil {
			t.Fatalf("GetAssetsPriceHistory24hBatch: %v", err)
		}
		for _, tc := range cases {
			checkSeries(t, "24h batch "+tc.name, got[tc.id], tc.latest, tc.earlier)
		}
	})
	t.Run("PriceHistory7dBatch", func(t *testing.T) {
		got, err := store.GetAssetsPriceHistory7dBatch(ctx, ids)
		if err != nil {
			t.Fatalf("GetAssetsPriceHistory7dBatch: %v", err)
		}
		for _, tc := range cases {
			checkSeries(t, "7d batch "+tc.name, got[tc.id], tc.latest, tc.earlier)
		}
	})
}
