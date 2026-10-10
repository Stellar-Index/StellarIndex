//go:build integration

package integration_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/canonical/discovery"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Public Soroban contract ids used as fixture assets, one per scenario
// in seedArmRecencyFixture.
const (
	armStaleDirectContract   = "CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32"
	armFlippedUSDCContract   = "CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN"
	armTwoSidedContract      = "CD25MNVTZDL4Y3XBCPCJXGXATV5WUHHOWMYFF4YBEGU5FCPGMYTVG5JY"
	armFlippedXLMContract    = "CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7"
	armSameMinuteTieContract = "CAB6MICC2WKRT372U3FRPKGGVB5R3FDJSMWSLPF2UJNJPYMBZ76RQVYE"
)

// seedArmRecencyFixture writes one market shape per way the headline
// USD price could pick an older or one-sided observation over a newer
// or two-sided one. XLM/USDC is a constant 0.40; every token has 7
// decimals, so each stored vwap is the price itself.
//
//	staleDirect   USDC print 5 days ago at $0.10; XLM market now at
//	              0.175 XLM = $0.07                        -> 0.07
//	flippedUSDC   (token, USDC-SAC) 6 days ago at $1.00; USDC-SAC -> token
//	              buys now at $2.00, stored (USDC-SAC, token) -> 2.00
//	twoSided      one minute, both directions against USDC: 100 tokens
//	              sold for 100 USDC and 100 bought for 300 -> 400/200 = 2.00
//	flippedXLM    (token, XLM-SAC) 6 days ago at 1 XLM; XLM-SAC -> token
//	              buys now at 2 XLM, and 24 h ago at 1 XLM  -> 0.80, +100 %
//	sameMinuteTie USDC $1.00 and XLM $1.20 in the same minute -> 1.00
func seedArmRecencyFixture(t *testing.T, ctx context.Context, store *timescale.Store) {
	t.Helper()

	const usdcIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	const usdcSAC = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
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
	xlmSAC, usdcSACAsset := soroban(c.XLMSacContractID), soroban(usdcSAC)
	staleDirect, flippedUSDC := soroban(armStaleDirectContract), soroban(armFlippedUSDCContract)
	twoSided, flippedXLM := soroban(armTwoSidedContract), soroban(armFlippedXLMContract)
	tie := soroban(armSameMinuteTieContract)

	now := time.Now().UTC().Truncate(time.Minute)
	for _, id := range []string{
		armStaleDirectContract, armFlippedUSDCContract, armTwoSidedContract,
		armFlippedXLMContract, armSameMinuteTieContract,
	} {
		if err := store.RecordDiscovered(ctx, discovery.Hit{
			ContractID:        id,
			Kind:              discovery.KindSEP41,
			EventType:         discovery.EventTransfer,
			Ledger:            50_000_000,
			ObservedAtRFC3339: now.Add(-7 * 24 * time.Hour).Format(time.RFC3339),
		}); err != nil {
			t.Fatalf("RecordDiscovered %s: %v", id, err)
		}
	}

	nonce := 0
	add := func(source string, ts time.Time, p c.Pair, base, quote int64) {
		t.Helper()
		nonce++
		if err := store.InsertTrade(ctx, mkIntegrationTrade(source, nonce, ts, p, base, quote)); err != nil {
			t.Fatalf("InsertTrade %s #%d: %v", source, nonce, err)
		}
	}
	const unit = 10_000_000 // one whole 7-decimals token
	fresh, dayAgo := now.Add(-5*time.Minute), now.Add(-24*time.Hour)
	fiveDays, sixDays := now.Add(-5*24*time.Hour), now.Add(-6*24*time.Hour)

	for _, ts := range []time.Time{fresh, dayAgo} {
		add("sdex", ts, pair(c.NativeAsset(), usdc), 10*unit, 4*unit)
	}

	add("soroswap", fiveDays, pair(staleDirect, usdc), 100*unit, 10*unit)
	add("soroswap", fresh, pair(staleDirect, xlmSAC), 100*unit, 175*unit/10)

	add("aquarius", sixDays, pair(flippedUSDC, usdcSACAsset), 100*unit, 100*unit)
	add("aquarius", fresh, pair(usdcSACAsset, flippedUSDC), 200*unit, 100*unit)

	add("soroswap", fresh, pair(twoSided, usdc), 100*unit, 100*unit)
	add("aquarius", fresh, pair(usdc, twoSided), 300*unit, 100*unit)

	add("aquarius", sixDays, pair(flippedXLM, xlmSAC), 100*unit, 100*unit)
	add("aquarius", dayAgo, pair(xlmSAC, flippedXLM), 100*unit, 100*unit)
	add("aquarius", fresh, pair(xlmSAC, flippedXLM), 200*unit, 100*unit)

	add("soroswap", fresh, pair(tie, usdc), 100*unit, 100*unit)
	add("soroswap", fresh, pair(tie, xlmSAC), 100*unit, 300*unit)

	// The Soroban-venue legs carry no insert-time usd_volume, and the
	// listing spine admits a discovered contract only once it has an
	// asset_volume_24h row. This test is about the PRICE.
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 100 WHERE usd_volume IS NULL`); err != nil {
		t.Fatalf("stamp usd_volume: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}
}

// TestAssetPrice_NewestObservationAcrossArmsAndDirections pins the
// headline USD price on BOTH surfaces that derive it — the listing
// rollup (asset_price_snapshot) and the detail row (GetAssetBySlug). The price
// must come from the NEWEST observation across arms and directions: the
// direct-USD arm must not win merely because it has ANY row in 7 days, and a
// fresher flipped direction must beat the stored base-side one, including for
// flippedXLM's change_24h_pct.
func TestAssetPrice_NewestObservationAcrossArmsAndDirections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedArmRecencyFixture(t, ctx, store)

	if err := store.RefreshAssetListingRollups(ctx); err != nil {
		t.Fatalf("RefreshAssetListingRollups: %v", err)
	}

	want := map[string]string{
		armStaleDirectContract:   "0.0700000000",
		armFlippedUSDCContract:   "2.0000000000",
		armTwoSidedContract:      "2.0000000000",
		armFlippedXLMContract:    "0.8000000000",
		armSameMinuteTieContract: "1.0000000000",
	}
	check := func(t *testing.T, surface string, row timescale.AssetRow, id string) {
		t.Helper()
		if row.PriceUSD == nil || *row.PriceUSD != want[id] {
			t.Errorf("%s: %s price_usd = %s, want %s", surface, id, derefOrNil(row.PriceUSD), want[id])
		}
		if id == armFlippedXLMContract {
			// Both legs of the change come from the arm that priced it.
			if ch := row.Change24hPct; ch == nil || *ch != "100.00" {
				t.Errorf("%s: %s change_24h_pct = %s, want 100.00", surface, id, derefOrNil(ch))
			}
		}
	}

	t.Run("listing", func(t *testing.T) {
		got, err := store.ListAssetsExt(ctx, timescale.ListAssetsOptions{Limit: 50, Type: "soroban"})
		if err != nil {
			t.Fatalf("ListAssetsExt: %v", err)
		}
		byID := make(map[string]timescale.AssetRow, len(got))
		for _, r := range got {
			byID[r.AssetID] = r
		}
		for id := range want {
			row, ok := byID[id]
			if !ok {
				t.Errorf("listing: row %s missing", id)
				continue
			}
			check(t, "listing", row, id)
		}
		// Both venues that traded twoSided's minute back its price.
		if sc := byID[armTwoSidedContract].SourceCount; sc == nil || *sc != 2 {
			got := "nil"
			if sc != nil {
				got = strconv.Itoa(*sc)
			}
			t.Errorf("listing: twoSided source_count = %s, want 2", got)
		}
	})

	t.Run("detail", func(t *testing.T) {
		for id := range want {
			row, err := store.GetAssetBySlug(ctx, id)
			if err != nil {
				t.Errorf("GetAssetBySlug %s: %v", id, err)
				continue
			}
			check(t, "detail", row, id)
		}
	})
}

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

// TestAssetPriceSnapshot_NormalisesNonstandardDecimals is the
// regression for the /v1/assets LISTING price. asset_price_snapshot's
// writer stored the RAW prices_1m ratio, so every reader of the rollup
// — the listing spine and ContractCatalogueRows, the latter feeding the
// RWA contract listing's market cap — published a price off by
// 10^(7 - decimals) for a confirmed non-7-decimals token. Before the
// a raw-ratio writer the three flagged rows below read 0.0250000000, 0.0000000001 and
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

// TestAssetCatalogue_RoundsAfterDecimalsCorrection is the regression
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
		// 18 dp, k = 11: 21 places. Raw rounding gave 0.0000000001.
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

// TestAssetPriceUSDCQuotedOnly proves the stablecoin-proxy + SAC-alias
// bridges in every asset_catalogue price path, via three synthetic
// assets that each have EXACTLY ONE kind of market:
//
//   - ZAUD: quoted only in classic USDC (GA5Z…)  → direct USD price
//   - ZUSC: quoted only in the USDC SAC (CCW67T…) → direct USD price
//   - ZSAC: quoted only in the XLM SAC (CAS3J7…)  → vwap × xlm_usd
//
// None has a fiat:USD or plain-'native' pair, so without the XLM bridge every
// one of them prices NULL: the direct_usd* CTEs accept only
// quote_asset = 'fiat:USD' and the asset_vs_xlm* CTEs only 'native'.
// Regression guard: priceless /v1/assets rows (AUDD with $344k 24h volume,
// EURC, the *allow variants, and the Soroban-venue majority quoted in SAC
// ids).
func TestAssetPriceUSDCQuotedOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Circle's canonical USDC issuer + the two well-known SAC
	// contract ids — must match the literals in asset_catalogue.go's
	// stablecoin-proxy / XLM-leg IN lists (XLM SAC =
	// canonical.XLMSacContractID).
	const (
		usdcIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		usdcSAC    = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
		xlmSAC     = c.XLMSacContractID
	)
	// Real CRC-valid issuer strkeys (AQUA's + the issuers-test third
	// issuer) so canonical.NewClassicAsset's checksum passes.
	const (
		zaudIssuer = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
		zsacIssuer = "GDM4RQUQQUVSKQA7S6EM7XBZP3FCGH4Q7CL6TABQ7B2BEJ5ERARM2M5M"
	)

	mustClassic := func(code, issuer string) c.Asset {
		t.Helper()
		a, err := c.NewClassicAsset(code, issuer)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	mustSoroban := func(contractID string) c.Asset {
		t.Helper()
		a, err := c.NewSorobanAsset(contractID)
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

	usdc := mustClassic("USDC", usdcIssuer)
	zaud := mustClassic("ZAUD", zaudIssuer)
	zusc := mustClassic("ZUSC", usdcIssuer)
	zsac := mustClassic("ZSAC", zsacIssuer)

	zaudID, zuscID, zsacID := zaud.String(), zusc.String(), zsac.String()

	seedIssuers(t, ctx, store, []seedIssuer{
		{g: zaudIssuer, homeDomain: ""},
		{g: zsacIssuer, homeDomain: ""},
		{g: usdcIssuer, homeDomain: ""},
	})
	seedClassicAssets(t, ctx, store, []seedAsset{
		{assetID: zaudID, code: "ZAUD", issuer: zaudIssuer, slug: "ZAUD", obs: 3_000},
		{assetID: zuscID, code: "ZUSC", issuer: usdcIssuer, slug: "ZUSC", obs: 2_000},
		{assetID: zsacID, code: "ZSAC", issuer: zsacIssuer, slug: "ZSAC", obs: 1_000},
	})

	// Two trades per pair, 20 min apart, all inside every query's
	// "now" window. The freshest bucket must win each direct pick.
	// XLM/USDC (classic) at a constant 0.40 seeds the xlm_usd CTEs so
	// the ZSAC triangulation has a USD leg — NO plain-'native' and NO
	// fiat:USD rows exist for any of the three assets under test.
	now := time.Now().UTC().Truncate(time.Minute)
	early, late := now.Add(-30*time.Minute), now.Add(-10*time.Minute)
	trades := []c.Trade{
		// ZAUD/USDC-classic: 0.60 → 0.65.
		mkIntegrationTrade("sdex", 1, early, mustPair(zaud, usdc), 1_000_000_000, 600_000_000),
		mkIntegrationTrade("sdex", 2, late, mustPair(zaud, usdc), 1_000_000_000, 650_000_000),
		// ZUSC/USDC-SAC: 0.98 → 0.99.
		mkIntegrationTrade("soroswap", 3, early, mustPair(zusc, mustSoroban(usdcSAC)), 1_000_000_000, 980_000_000),
		mkIntegrationTrade("soroswap", 4, late, mustPair(zusc, mustSoroban(usdcSAC)), 1_000_000_000, 990_000_000),
		// ZSAC/XLM-SAC: 2.0 → 1.5 (in XLM).
		mkIntegrationTrade("soroswap", 5, early, mustPair(zsac, mustSoroban(xlmSAC)), 1_000_000_000, 2_000_000_000),
		mkIntegrationTrade("soroswap", 6, late, mustPair(zsac, mustSoroban(xlmSAC)), 1_000_000_000, 1_500_000_000),
		// XLM/USDC-classic: constant 0.40 (the xlm_usd USD leg).
		mkIntegrationTrade("sdex", 7, early, mustPair(c.NativeAsset(), usdc), 1_000_000_000, 400_000_000),
		mkIntegrationTrade("sdex", 8, late, mustPair(c.NativeAsset(), usdc), 1_000_000_000, 400_000_000),
	}
	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	// The XLM/USD anchor drops minutes under its volume_usd floor.
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 40 WHERE base_asset = 'native' AND quote_asset = $1`, usdc.String()); err != nil {
		t.Fatalf("stamp usd_volume: %v", err)
	}
	// Force the cagg refresh — the 30s policy won't fire inside the
	// test window (mirrors trades_range_test.go).
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}
	// The LISTING's price column now comes from asset_price_snapshot
	// (migration 0154) rather than from twelve per-request
	// prices_1m CTEs, so refreshing the cagg alone leaves the listing
	// arm of this test unpriced. The single-asset arm still reads
	// prices_1m live and is unaffected — which is exactly the split
	// this refresh keeps honest.
	if err := store.RefreshAssetListingRollups(ctx); err != nil {
		t.Fatalf("RefreshAssetListingRollups: %v", err)
	}

	// Expected price_usd per asset: {latest, earlier} — trades may
	// straddle an hour/day boundary depending on when the test runs,
	// so series checks accept either seeded print but require the
	// LATEST non-null point to be the fresh one.
	cases := []struct {
		name, assetID, slug, wantLatest, wantEarly string
	}{
		{"classic-USDC quote", zaudID, "ZAUD", "0.6500000000", "0.6000000000"},
		{"USDC-SAC quote", zuscID, "ZUSC", "0.9900000000", "0.9800000000"},
		{"XLM-SAC quote triangulated", zsacID, "ZSAC", "0.6000000000", "0.8000000000"}, // 1.5×0.40, 2.0×0.40
	}

	// Errorf (not Fatalf) so each of the three quote-form cases
	// reports independently — a regression in one bridge must not
	// mask the others.
	checkPrice := func(t *testing.T, label string, got *string, want string) {
		t.Helper()
		if got == nil {
			t.Errorf("%s price_usd = nil, want %s", label, want)
			return
		}
		if *got != want {
			t.Errorf("%s price_usd = %s, want %s", label, *got, want)
		}
	}
	checkSeries := func(t *testing.T, label string, pts []timescale.AssetPricePoint, wantLatest, wantEarly string) {
		t.Helper()
		var lastNonNil string
		for _, pt := range pts {
			if pt.P == nil {
				continue
			}
			if *pt.P != wantLatest && *pt.P != wantEarly {
				t.Errorf("%s: unexpected series price %s at %s", label, *pt.P, pt.T)
			}
			lastNonNil = *pt.P
		}
		if lastNonNil == "" {
			t.Errorf("%s: series has no non-null points, want latest %s", label, wantLatest)
			return
		}
		if lastNonNil != wantLatest {
			t.Errorf("%s: latest series price = %s, want %s", label, lastNonNil, wantLatest)
		}
	}

	t.Run("ListAssets", func(t *testing.T) {
		got, err := store.ListAssets(ctx, 10, "", "")
		if err != nil {
			t.Fatalf("ListAssets: %v", err)
		}
		byID := make(map[string]timescale.AssetRow, len(got))
		for _, r := range got {
			byID[r.AssetID] = r
		}
		for _, tc := range cases {
			row, ok := byID[tc.assetID]
			if !ok {
				t.Fatalf("%s: row %s missing from listing", tc.name, tc.assetID)
			}
			checkPrice(t, "listing "+tc.name, row.PriceUSD, tc.wantLatest)
		}
	})

	t.Run("GetAssetBySlug", func(t *testing.T) {
		for _, tc := range cases {
			row, err := store.GetAssetBySlug(ctx, tc.slug)
			if err != nil {
				t.Fatalf("GetAssetBySlug(%s): %v", tc.slug, err)
			}
			checkPrice(t, "detail "+tc.name, row.PriceUSD, tc.wantLatest)
		}
	})

	t.Run("PriceHistory24h", func(t *testing.T) {
		for _, tc := range cases {
			pts, err := store.GetAssetPriceHistory24h(ctx, tc.assetID)
			if err != nil {
				t.Fatalf("GetAssetPriceHistory24h(%s): %v", tc.assetID, err)
			}
			checkSeries(t, tc.name, pts, tc.wantLatest, tc.wantEarly)
		}
	})

	t.Run("PriceHistory7d", func(t *testing.T) {
		for _, tc := range cases {
			pts, err := store.GetAssetPriceHistory7d(ctx, tc.assetID)
			if err != nil {
				t.Fatalf("GetAssetPriceHistory7d(%s): %v", tc.assetID, err)
			}
			checkSeries(t, tc.name, pts, tc.wantLatest, tc.wantEarly)
		}
	})

	t.Run("PriceHistory24hBatch", func(t *testing.T) {
		got, err := store.GetAssetsPriceHistory24hBatch(ctx, []string{zaudID, zuscID, zsacID})
		if err != nil {
			t.Fatalf("GetAssetsPriceHistory24hBatch: %v", err)
		}
		for _, tc := range cases {
			checkSeries(t, tc.name, got[tc.assetID], tc.wantLatest, tc.wantEarly)
		}
	})

	t.Run("PriceHistory7dBatch", func(t *testing.T) {
		got, err := store.GetAssetsPriceHistory7dBatch(ctx, []string{zaudID, zuscID, zsacID})
		if err != nil {
			t.Fatalf("GetAssetsPriceHistory7dBatch: %v", err)
		}
		for _, tc := range cases {
			checkSeries(t, tc.name, got[tc.assetID], tc.wantLatest, tc.wantEarly)
		}
	})
}
