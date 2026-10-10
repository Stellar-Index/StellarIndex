//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/canonical/discovery"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
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
	soroban := func(id string) canonical.Asset {
		t.Helper()
		a, err := canonical.NewSorobanAsset(id)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	pair := func(base, quote canonical.Asset) canonical.Pair {
		t.Helper()
		p, err := canonical.NewPair(base, quote)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	usdc, err := canonical.NewClassicAsset("USDC", usdcIssuer)
	if err != nil {
		t.Fatal(err)
	}
	xlmSAC, usdcSACAsset := soroban(canonical.XLMSacContractID), soroban(usdcSAC)
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
	add := func(source string, ts time.Time, p canonical.Pair, base, quote int64) {
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
		add("sdex", ts, pair(canonical.NativeAsset(), usdc), 10*unit, 4*unit)
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
			t.Errorf("%s: %s price_usd = %s, want %s", surface, id, derefOr(row.PriceUSD), want[id])
		}
		if id == armFlippedXLMContract {
			// Both legs of the change come from the arm that priced it.
			if ch := row.Change24hPct; ch == nil || *ch != "100.00" {
				t.Errorf("%s: %s change_24h_pct = %s, want 100.00", surface, id, derefOr(ch))
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
	soroban := func(id string) canonical.Asset {
		t.Helper()
		a, err := canonical.NewSorobanAsset(id)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	pair := func(base, quote canonical.Asset) canonical.Pair {
		t.Helper()
		p, err := canonical.NewPair(base, quote)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	usdc, err := canonical.NewClassicAsset("USDC", usdcIssuer)
	if err != nil {
		t.Fatal(err)
	}
	xlmSAC := soroban(canonical.XLMSacContractID)
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
	add := func(source string, ts time.Time, p canonical.Pair, base, quote int64) {
		t.Helper()
		nonce++
		if err := store.InsertTrade(ctx, mkIntegrationTrade(source, nonce, ts, p, base, quote)); err != nil {
			t.Fatalf("InsertTrade %s #%d: %v", source, nonce, err)
		}
	}
	for _, ts := range []time.Time{early, late} {
		// XLM/USDC 0.40 — the xlm_usd leg of both triangulated tokens.
		add("sdex", ts, pair(canonical.NativeAsset(), usdc), 1_000_000_000, 400_000_000)
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
			t.Errorf("listing: nine change_1h_pct = %s, want 100.00", derefOr(ch))
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
				t.Errorf("%s: price_usd = %s, want %s", tc.name, derefOr(row.PriceUSD), tc.latest)
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
		xlmSAC     = canonical.XLMSacContractID
	)
	// Real CRC-valid issuer strkeys (AQUA's + the issuers-test third
	// issuer) so canonical.NewClassicAsset's checksum passes.
	const (
		zaudIssuer = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
		zsacIssuer = "GDM4RQUQQUVSKQA7S6EM7XBZP3FCGH4Q7CL6TABQ7B2BEJ5ERARM2M5M"
	)

	mustClassic := func(code, issuer string) canonical.Asset {
		t.Helper()
		a, err := canonical.NewClassicAsset(code, issuer)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	mustSoroban := func(contractID string) canonical.Asset {
		t.Helper()
		a, err := canonical.NewSorobanAsset(contractID)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	mustPair := func(base, quote canonical.Asset) canonical.Pair {
		t.Helper()
		p, err := canonical.NewPair(base, quote)
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
	trades := []canonical.Trade{
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
		mkIntegrationTrade("sdex", 7, early, mustPair(canonical.NativeAsset(), usdc), 1_000_000_000, 400_000_000),
		mkIntegrationTrade("sdex", 8, late, mustPair(canonical.NativeAsset(), usdc), 1_000_000_000, 400_000_000),
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

// TestTimedVWAPsForPair1m_NotionalFloor pins that the anomaly baseline's
// series carries each minute's USD notional across both stored directions,
// and that the refresher's USD-volume bars over it on real Postgres keep
// penny-authored minutes from buying baseline points. All amounts 1e7.
//
//	m0  one $20 fill                                   → $20
//	m1  one $0.001 dust fill                           → $0.001
//	m2  one unpriced fill (usd_volume NULL)            → nil
//	m3  $0.001 dust beside a $20 fill                  → $20.001
//	m4  $0.001 dust stored; $20 fill stored flipped    → $20.001
//	m5..m64  sixty $0.01 self-trade minutes            → $0.01 each
func TestTimedVWAPsForPair1m_NotionalFloor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()

	usdcID := "USDC-" + zeroLegIssuer
	thin := ohlcDustPair{base: "THIN-" + zeroLegIssuer, quote: usdcID}
	flip := ohlcDustPair{base: usdcID, quote: thin.base}
	eur := ohlcDustPair{base: "EURT-" + zeroLegIssuer, quote: usdcID}
	pny := ohlcDustPair{base: "PNY-" + zeroLegIssuer, quote: usdcID}

	t0 := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Hour)
	m := func(i int) time.Time { return t0.Add(time.Duration(i) * time.Minute) }
	real := seedTrade{off: 10 * time.Second, base: "100000000", quote: "200000000", usd: "20"}
	dust := seedTrade{off: 20 * time.Second, base: "5", quote: "10", usd: "0.001"}
	penny := seedTrade{off: 20 * time.Second, base: "50000", quote: "100000", usd: "0.01"}
	unpriced := seedTrade{off: 10 * time.Second, base: "100000000", quote: "200000000"}

	seed(t, db, ctx, thin, []seedTrade{real}, m(0))
	seed(t, db, ctx, thin, []seedTrade{dust}, m(1))
	seed(t, db, ctx, thin, []seedTrade{unpriced}, m(2))
	seed(t, db, ctx, thin, []seedTrade{dust, real}, m(3))
	seed(t, db, ctx, thin, []seedTrade{dust}, m(4))
	seed(t, db, ctx, flip, []seedTrade{{off: 30 * time.Second, base: "200000000", quote: "100000000", usd: "20"}}, m(4))
	for i := 5; i < 65; i++ {
		seed(t, db, ctx, thin, []seedTrade{penny}, m(i))
		seed(t, db, ctx, eur, []seedTrade{unpriced}, m(i))
		seed(t, db, ctx, pny, []seedTrade{penny}, m(i))
	}
	if _, err := db.ExecContext(ctx, "CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)"); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	usdc, err := canonical.NewClassicAsset("USDC", zeroLegIssuer)
	if err != nil {
		t.Fatal(err)
	}
	pairOf := func(code string) canonical.Pair {
		a, err := canonical.NewClassicAsset(code, zeroLegIssuer)
		if err != nil {
			t.Fatal(err)
		}
		p, err := canonical.NewPair(a, usdc)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	pair := pairOf("THIN")

	pts, err := store.TimedVWAPsForPair1m(ctx, pair, t0, m(65))
	if err != nil {
		t.Fatalf("TimedVWAPsForPair1m: %v", err)
	}
	if len(pts) != 65 {
		t.Fatalf("got %d points, want 65 (one per traded minute)", len(pts))
	}
	wants := map[int]string{0: "20", 1: "1/1000", 2: "", 3: "20001/1000", 4: "20001/1000"} // "" = nil
	for i := 5; i < 65; i++ {
		wants[i] = "1/100"
	}
	for i, p := range pts {
		if !p.BucketEnd.Equal(m(i + 1)) {
			t.Errorf("point %d bucket end = %s, want %s", i, p.BucketEnd, m(i+1))
		}
		if p.VWAP < 1.99 || p.VWAP > 2.01 {
			t.Errorf("point %d vwap = %v, want ~2", i, p.VWAP)
		}
		w := wants[i]
		switch {
		case w == "" && p.USDVolume != nil:
			t.Errorf("point %d usd = %s, want nil (unpriced)", i, p.USDVolume.RatString())
		case w != "" && (p.USDVolume == nil || p.USDVolume.RatString() != w):
			t.Errorf("point %d usd = %v, want %s", i, p.USDVolume, w)
		}
	}

	// The production refresher, floor as wired on defaults, over the same
	// rows: THIN's three real minutes are three points (N=2) and its sixty
	// pennies ($0.60) never make a point; a pair of nothing but pennies has
	// too little flow for bars and keeps main's per-minute stats (59 returns,
	// under the density clamp); the all-unpriced pair keeps one under
	// ok_unvalued.
	sink := &captureBaselineSink{}
	r := baseline.NewRefresher(store, sink, baseline.DefaultWindow, nil).
		WithMinuteNotionalFloor(baseline.MinuteNotionalFloor(10_000, 24*time.Hour))
	outcome, err := r.RefreshPair(ctx, pair)
	if err != nil || outcome != baseline.OutcomeOK || sink.last.Day30 == nil || sink.last.Day30.N != 2 {
		t.Errorf("THIN refresh = (%v, %v, %+v), want (ok, nil) with Day30.N=2", outcome, err, sink.last.Day30)
	}
	outcome, err = r.RefreshPair(ctx, pairOf("PNY"))
	if err != nil || outcome != baseline.OutcomeOKPerMinuteFallback || sink.last.Day30 == nil || sink.last.Day30.N != 59 {
		t.Errorf("PNY refresh = (%v, %v, %+v), want ok_per_minute_fallback with Day30.N=59", outcome, err, sink.last.Day30)
	}
	outcome, err = r.RefreshPair(ctx, pairOf("EURT"))
	if err != nil || outcome != baseline.OutcomeOKUnvalued {
		t.Errorf("EURT refresh = (%v, %v), want (ok_unvalued, nil)", outcome, err)
	}
	if sink.calls != 3 || sink.last.Day30 == nil || sink.last.Day30.N != 59 {
		t.Errorf("sink calls=%d last=%+v, want THIN, PNY, EURT upserts, EURT Day30.N=59", sink.calls, sink.last.Day30)
	}
}

type captureBaselineSink struct {
	calls int
	last  baseline.MultiBaseline
}

func (s *captureBaselineSink) UpsertBaseline(_ context.Context, _ canonical.Pair, _, _, _ time.Time, m baseline.MultiBaseline) error {
	s.calls++
	s.last = m
	return nil
}

// TestBaselineStorageRoundTrip exercises UpsertBaseline → LatestBaseline
// against a real TimescaleDB with the volatility_baseline_1m migration
// applied (including 0008's multi-window columns). Confirms:
//
//   - Empty-table read returns ErrBaselineNotFound
//   - Upsert + LatestBaseline round-trips a full multi-window struct
//   - Partial baselines (Day1/Day7 nil) round-trip with the
//     nullable columns
//   - Re-upsert overwrites (current-state semantics)
//   - Pre-flight checks (Day30 nil, N < MinSamples, window-validity)
//     reject before touching the DB
//   - Distinct pairs are isolated
func TestBaselineStorageRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	xlm, _ := canonical.ParseAsset("native")
	usd, _ := canonical.ParseAsset("fiat:USD")
	pair, _ := canonical.NewPair(xlm, usd)

	// ─── Empty table → ErrBaselineNotFound ──────────────────────────
	if _, err := store.LatestBaseline(ctx, pair); !errors.Is(err, timescale.ErrBaselineNotFound) {
		t.Fatalf("LatestBaseline on empty table: err = %v, want ErrBaselineNotFound", err)
	}

	// ─── Full three-window upsert + read-back ───────────────────────
	t0 := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	d1 := &baseline.Baseline{Median: 0.0001, MAD: 0.0010, N: 60}
	d7 := &baseline.Baseline{Median: 0.0002, MAD: 0.0050, N: 50}
	d30 := &baseline.Baseline{Median: 0.0003, MAD: 0.0148, N: 120}
	sb := timescale.StoredBaseline{
		Pair:        pair,
		ComputedAt:  t0,
		WindowStart: t0.Add(-30 * 24 * time.Hour),
		WindowEnd:   t0,
		Multi:       baseline.MultiBaseline{Day1: d1, Day7: d7, Day30: d30},
	}
	if err := store.UpsertBaseline(ctx, sb); err != nil {
		t.Fatalf("UpsertBaseline: %v", err)
	}

	got, err := store.LatestBaseline(ctx, pair)
	if err != nil {
		t.Fatalf("LatestBaseline: %v", err)
	}
	if got.Multi.Day30 == nil {
		t.Fatal("Day30 nil after round-trip")
	}
	if got.Multi.Day30.Median != 0.0003 || got.Multi.Day30.MAD != 0.0148 || got.Multi.Day30.N != 120 {
		t.Errorf("Day30 = %+v, want {0.0003, 0.0148, 120}", got.Multi.Day30)
	}
	if got.Multi.Day7 == nil || got.Multi.Day7.N != 50 {
		t.Errorf("Day7 round-trip wrong: %+v", got.Multi.Day7)
	}
	if got.Multi.Day1 == nil || got.Multi.Day1.N != 60 {
		t.Errorf("Day1 round-trip wrong: %+v", got.Multi.Day1)
	}

	// ─── Partial baseline (Day1/Day7 nil — bootstrap) ──────────────
	other, _ := canonical.ParseAsset("fiat:EUR")
	pair2, _ := canonical.NewPair(xlm, other)
	sbPartial := timescale.StoredBaseline{
		Pair:        pair2,
		ComputedAt:  t0,
		WindowStart: t0.Add(-30 * 24 * time.Hour),
		WindowEnd:   t0,
		Multi:       baseline.MultiBaseline{Day30: d30}, // Day1 + Day7 nil
	}
	if err := store.UpsertBaseline(ctx, sbPartial); err != nil {
		t.Fatalf("UpsertBaseline (partial): %v", err)
	}
	gotPartial, err := store.LatestBaseline(ctx, pair2)
	if err != nil {
		t.Fatalf("LatestBaseline (partial): %v", err)
	}
	if gotPartial.Multi.Day30 == nil {
		t.Error("Day30 nil; expected populated")
	}
	if gotPartial.Multi.Day1 != nil {
		t.Errorf("Day1 should round-trip as nil; got %+v", gotPartial.Multi.Day1)
	}
	if gotPartial.Multi.Day7 != nil {
		t.Errorf("Day7 should round-trip as nil; got %+v", gotPartial.Multi.Day7)
	}

	// ─── Re-upsert overwrites ──────────────────────────────────────
	t1 := t0.Add(1 * time.Hour)
	sb2 := sb
	sb2.ComputedAt = t1
	sb2.WindowEnd = t1
	sb2.WindowStart = t1.Add(-30 * 24 * time.Hour)
	sb2.Multi.Day30 = &baseline.Baseline{Median: 0.0009, MAD: 0.02, N: 121}
	sb2.Multi.Day1 = nil // overwrite to bootstrap on this scale
	if err := store.UpsertBaseline(ctx, sb2); err != nil {
		t.Fatalf("UpsertBaseline (overwrite): %v", err)
	}
	got, err = store.LatestBaseline(ctx, pair)
	if err != nil {
		t.Fatalf("LatestBaseline (after overwrite): %v", err)
	}
	if got.Multi.Day30.Median != 0.0009 {
		t.Errorf("Day30.Median didn't advance; got %v, want 0.0009", got.Multi.Day30.Median)
	}
	if got.Multi.Day1 != nil {
		t.Errorf("Day1 should round-trip as nil after overwrite; got %+v", got.Multi.Day1)
	}

	// ─── Validation: Day30 nil rejected pre-flight ─────────────────
	bad := sb
	bad.Multi.Day30 = nil
	if err := store.UpsertBaseline(ctx, bad); err == nil {
		t.Error("UpsertBaseline with Day30 nil should fail; got nil")
	}

	// ─── Validation: Day30.N < MinSamples rejected ─────────────────
	bad = sb
	bad.Multi.Day30 = &baseline.Baseline{Median: 0, MAD: 0, N: 1}
	if err := store.UpsertBaseline(ctx, bad); err == nil {
		t.Error("UpsertBaseline with N=1 should fail; got nil")
	}

	// ─── Validation: window_end ≤ window_start rejected ────────────
	bad = sb
	bad.WindowStart = t1
	bad.WindowEnd = t1
	if err := store.UpsertBaseline(ctx, bad); err == nil {
		t.Error("UpsertBaseline with equal window_start/window_end should fail; got nil")
	}

	// ─── CountBaselines ────────────────────────────────────────────
	count, err := store.CountBaselines(ctx)
	if err != nil {
		t.Fatalf("CountBaselines: %v", err)
	}
	if count != 2 {
		t.Errorf("CountBaselines = %d, want 2 (pair + pair2)", count)
	}
}

// TestClosedVWAP1mAtOrBefore_ExecutesAgainstTimescale runs the statement
// behind /v1/assets/{id}'s change_24h_pct against the real schema. The
// scripted-driver tests pin its shape but never parse it; a parameter
// bound untyped beside `- INTERVAL '1 minute'` is resolved by Postgres
// as an interval, and the statement then fails with 42883 on every
// call while the caller turns the error into an absent field. An empty
// table must therefore yield sql.ErrNoRows — not a planning error.
func TestClosedVWAP1mAtOrBefore_ExecutesAgainstTimescale(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()

	pair := canonical.Pair{
		Base:  canonical.Asset{Type: canonical.AssetCrypto, Code: "XLM"},
		Quote: canonical.Asset{Type: canonical.AssetFiat, Code: "USD"},
	}
	_, err = store.ClosedVWAP1mAtOrBefore(ctx, pair, time.Now().Add(-24*time.Hour))
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ClosedVWAP1mAtOrBefore on an empty table: err = %v, want sql.ErrNoRows — the statement does not execute against the real schema", err)
	}
}

// TestTimedVWAPs1mForChangeSummary_ExcludesTheOpenBucket runs the
// /v1/changes source read against the real prices_1m CAGG.
// The worker passes its wall clock as `to`, and `bucket < to` admitted the
// minute that clock sits inside: a fat-finger print there became
// current_value and, through the upsert's GREATEST/LEAST, the stored
// ath/atl for good.
//
// Deterministic on purpose: both buckets are minutes in the past and `to`
// is placed 30 s into the later one, so the later bucket is "in progress"
// relative to the caller without racing the real clock's minute edge.
func TestTimedVWAPs1mForChangeSummary_ExcludesTheOpenBucket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()

	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := canonical.NewPair(canonical.NativeAsset(), usdc)
	if err != nil {
		t.Fatal(err)
	}

	closedBucket := time.Now().UTC().Truncate(time.Minute).Add(-10 * time.Minute)
	openBucket := closedBucket.Add(5 * time.Minute)
	// 100 XLM for 10 USDC: 0.1 — the honest closed price.
	if err := store.InsertTrade(ctx, mkIntegrationTrade("sdex", 1, closedBucket.Add(10*time.Second), pair,
		1_000_000_000, 100_000_000)); err != nil {
		t.Fatalf("InsertTrade closed: %v", err)
	}
	// 1 XLM for 1000 USDC: the fat-finger extreme in the unclosed minute.
	if err := store.InsertTrade(ctx, mkIntegrationTrade("sdex", 2, openBucket.Add(10*time.Second), pair,
		10_000_000, 10_000_000_000)); err != nil {
		t.Fatalf("InsertTrade open: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	to := openBucket.Add(30 * time.Second)
	got, err := store.TimedVWAPs1mForChangeSummary(ctx, pair, closedBucket.Add(-time.Hour), to)
	if err != nil {
		t.Fatalf("TimedVWAPs1mForChangeSummary: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("returned %d points %+v, want exactly the one closed bucket — "+
			"the bucket still open at `to` must not be served", len(got), got)
	}
	if want := closedBucket.Add(time.Minute); !got[0].At.Equal(want) {
		t.Errorf("point At = %s, want the closed bucket's end %s", got[0].At, want)
	}
	v, ok := new(big.Rat).SetString(got[0].Value)
	if !ok || v.Cmp(big.NewRat(1, 10)) != 0 {
		t.Errorf("point value = %q, want 0.1 (the closed bucket), not the open-minute extreme", got[0].Value)
	}

	// Once `to` is past it, the same bucket is closed and is served.
	got, err = store.TimedVWAPs1mForChangeSummary(ctx, pair, closedBucket.Add(-time.Hour), openBucket.Add(time.Minute))
	if err != nil {
		t.Fatalf("TimedVWAPs1mForChangeSummary (after close): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("returned %d points after the later bucket closed, want 2", len(got))
	}
}

// TestDeleteChangeSummary_ClearsRatchetedExtremes pins the repair path: the
// upsert's GREATEST/LEAST never lowers a stored ath, and deleting the row
// lets the next upsert start from the corrected values.
func TestDeleteChangeSummary_ClearsRatchetedExtremes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()

	now := time.Now().UTC().Truncate(time.Second)
	row := func(ath string) timescale.ChangeSummaryRow {
		cur := "0.11"
		low := "0.10"
		return timescale.ChangeSummaryRow{
			EntityType: "coin", EntityID: "crypto:XLM", RefreshedAt: now, CurrentValue: cur,
			ATHValue: &ath, ATHAt: &now, ATLValue: &low, ATLAt: &now,
		}
	}
	if err := store.UpsertChangeSummary(ctx, row("9000")); err != nil {
		t.Fatalf("upsert bad: %v", err)
	}
	if err := store.UpsertChangeSummary(ctx, row("0.12")); err != nil {
		t.Fatalf("upsert good: %v", err)
	}
	got, err := store.GetChangeSummary(ctx, "coin", "crypto:XLM")
	if err != nil {
		t.Fatal(err)
	}
	if got.ATHValue == nil || *got.ATHValue != "9000" {
		t.Fatalf("ratchet should have held the bad ath, got %v", got.ATHValue)
	}

	existed, err := store.DeleteChangeSummary(ctx, "coin", "crypto:XLM")
	if err != nil || !existed {
		t.Fatalf("delete = %v, %v; want true, nil", existed, err)
	}
	if existed, err = store.DeleteChangeSummary(ctx, "coin", "crypto:XLM"); err != nil || existed {
		t.Fatalf("second delete = %v, %v; want false, nil", existed, err)
	}
	if err := store.UpsertChangeSummary(ctx, row("0.12")); err != nil {
		t.Fatalf("upsert after reset: %v", err)
	}
	got, err = store.GetChangeSummary(ctx, "coin", "crypto:XLM")
	if err != nil {
		t.Fatal(err)
	}
	if got.ATHValue == nil || *got.ATHValue != "0.12" {
		t.Fatalf("ath after reset = %v, want 0.12", got.ATHValue)
	}
}

// TestClosedVWAP1mCombinedBefore_AnchorsAtTheInstant executes the
// point-in-time guard's baseline read against the real schema: it must
// return the buckets immediately BEFORE the anchor (never the anchor's own
// bucket, never newer ones), newest-first, with a flipped-only bucket
// combined into the requested orientation.
func TestClosedVWAP1mCombinedBefore_AnchorsAtTheInstant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	xlmUSDC, _ := canonical.NewPair(canonical.NativeAsset(), usdc)
	usdcXLM, _ := canonical.NewPair(usdc, canonical.NativeAsset())

	// Six one-minute buckets three hours back. Bucket 2 is stored ONLY in
	// the flipped direction (2.0 XLM/USDC = 0.5 USDC/XLM once inverted).
	t0 := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Minute)
	bucket := func(i int) time.Time { return t0.Add(time.Duration(i) * time.Minute) }
	for i := 0; i < 6; i++ {
		tr := mkAPITrade(i+1, bucket(i).Add(10*time.Second), xlmUSDC, 1_000_000, int64(100_000*(i+1)))
		if i == 2 {
			tr = mkAPITrade(i+1, bucket(i).Add(10*time.Second), usdcXLM, 1_000_000, 2_000_000)
		}
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	rows, err := store.ClosedVWAP1mCombinedBefore(ctx, xlmUSDC, bucket(4), 3)
	if err != nil {
		t.Fatalf("ClosedVWAP1mCombinedBefore: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3 (buckets 3, 2, 1): %+v", len(rows), rows)
	}
	for i, want := range []int{3, 2, 1} {
		if !rows[i].Bucket.Equal(bucket(want)) {
			t.Errorf("rows[%d].Bucket = %v, want bucket %d (%v) — the read must start strictly before the anchor",
				i, rows[i].Bucket, want, bucket(want))
		}
	}
	if got, err := strconv.ParseFloat(rows[1].VWAP, 64); err != nil || got < 0.49 || got > 0.51 {
		t.Errorf("flipped-only bucket VWAP = %q, want ~0.5 (inverted into the requested orientation)", rows[1].VWAP)
	}

	none, err := store.ClosedVWAP1mCombinedBefore(ctx, xlmUSDC, bucket(0), 3)
	if err != nil {
		t.Fatalf("ClosedVWAP1mCombinedBefore before history: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("anchor at the first bucket returned %d rows, want none: %+v", len(none), none)
	}
}

// TestLatestClosedVWAP1m_RecentExistenceGate exercises the recent-existence
// gate for the empty-alias latency case. The
// /v1/price handler reads native/fiat:USD as an alias on every XLM query,
// and that synthetic pair has ZERO rows; without the gate, each such read
// ran a max() over ~400 days of prices_1m chunks to conclude "no rows"
// (minutes cold), timing out before the fast crypto:XLM/fiat:USD alias was
// tried. The gate makes the empty/quiet case an O(recent-chunks) probe
// that returns sql.ErrNoRows, while a live pair still returns its latest
// closed bucket combining BOTH stored directions.
//
// Three cases, one fixture:
//   - empty pair (native/fiat:USD, no rows) → sql.ErrNoRows,
//   - quiet pair whose only bucket predates the gate window → sql.ErrNoRows
//     (proving the GATE, not just "no rows", bounds the horizon: the
//     ~400-day value walk WOULD have found this bucket),
//   - live pair with a recent flipped-only latest bucket → the combined,
//     inverted VWAP (both directions), NOT a miss.
func TestLatestClosedVWAP1m_RecentExistenceGate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	aqua, err := canonical.NewClassicAsset("AQUA", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatal(err)
	}
	fiatUSD, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatal(err)
	}

	xlmUSDC, _ := canonical.NewPair(canonical.NativeAsset(), usdc) // requested orientation
	usdcXLM, _ := canonical.NewPair(usdc, canonical.NativeAsset()) // the flipped storage direction
	xlmAQUA, _ := canonical.NewPair(canonical.NativeAsset(), aqua)
	xlmUSD, _ := canonical.NewPair(canonical.NativeAsset(), fiatUSD) // never traded — the incident pair

	// Live pair: two recent closed buckets ~2h back. The LATEST bucket
	// stores ONLY the flipped direction (2.0 XLM/USDC = 0.5 USDC/XLM once
	// inverted), so a one-direction read would either miss it or return the
	// older direct bucket — the combine + inversion is what makes the
	// answer correct.
	recent := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	// Quiet pair: a single closed bucket 30 days back — inside the ~400-day
	// value window but OUTSIDE the 14-day gate window.
	quiet := time.Now().UTC().Add(-30 * 24 * time.Hour).Truncate(time.Minute)

	trades := []canonical.Trade{
		mkAPITrade(1, recent, xlmUSDC, 1_000_000, 500_000),                      // 0.5 direct
		mkAPITrade(2, recent.Add(2*time.Minute), usdcXLM, 1_000_000, 2_000_000), // 2.0 flipped → 0.5
		mkAPITrade(3, quiet, xlmAQUA, 1_000_000, 250_000),                       // quiet pair, 30d old
	}
	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	// ── empty pair → ErrNoRows (no full walk) ──────────────────────────
	if _, err := store.LatestClosedVWAP1mForPair(ctx, xlmUSD); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("empty pair native/fiat:USD: err = %v, want sql.ErrNoRows", err)
	}

	// ── quiet pair (bucket older than the gate window) → ErrNoRows ─────
	// This is the load-bearing gate assertion: the bucket exists 30d back,
	// so the ~400-day value walk WOULD return it; the gate is what makes
	// the read fall through to the handler's last-trade fallback instead.
	if _, err := store.LatestClosedVWAP1mForPair(ctx, xlmAQUA); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("quiet pair native/AQUA (bucket 30d old, gate window 14d): err = %v, want sql.ErrNoRows", err)
	}

	// ── live pair → latest closed bucket, both directions combined ─────
	row, err := store.LatestClosedVWAP1mForPair(ctx, xlmUSDC)
	if err != nil {
		t.Fatalf("live pair native/USDC: unexpected err %v", err)
	}
	wantBucket := recent.Add(2 * time.Minute)
	if !row.Bucket.Equal(wantBucket) {
		t.Errorf("live pair latest bucket = %v, want %v (the flipped-only bucket — proves both directions read)",
			row.Bucket, wantBucket)
	}
	if px := mustFloat(t, row.VWAP); px < 0.49 || px > 0.51 {
		t.Errorf("live pair VWAP = %s, want ~0.5 (flipped 2.0 inverted — proves direction combine)", row.VWAP)
	}
}

// TestStoreMonthlyUSDVWAPs_FoldsAliasSpellingsPerMonth executes the
// cohort pages' "USD then" price read against real TimescaleDB: trades in
// two months, XLM spelled BOTH ways ('native' and 'crypto:XLM') against
// two USD proxies (USDC and fiat:USD), a second classic asset, an
// XLM-quoted (non-USD) market that must not count, and a month before
// the range that must not be read.
//
// The served row per (asset, month) is Σ quote / Σ base over EVERY
// folded row in WHOLE units — the union market's VWAP — so May's XLM
// price is (10 + 60 + 25) / (100 + 300 + 100) = 0.19, not the mean of
// 0.1, 0.2 and 0.25 (0.1833…), not either spelling's own figure, and not
// the 0.2098 that weighting by raw stored volume gives: the crypto:XLM
// trades are stored at the CEX feeds' 10^8, the native one in stroops.
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
	usdc, err := canonical.NewClassicAsset("USDC", issuer)
	if err != nil {
		t.Fatal(err)
	}
	aqua, err := canonical.NewClassicAsset("AQUA", issuer)
	if err != nil {
		t.Fatal(err)
	}
	native, err := canonical.ParseAsset("native")
	if err != nil {
		t.Fatal(err)
	}
	cryptoXLM, err := canonical.ParseAsset("crypto:XLM")
	if err != nil {
		t.Fatal(err)
	}
	fiatUSD, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatal(err)
	}
	// A CEX feed's crypto:XLM trade, amounts at that source's 10^8.
	cex := func(tr canonical.Trade) canonical.Trade {
		tr.Source = "coinbase"
		return tr
	}
	pair := func(base, quote canonical.Asset) canonical.Pair {
		p, err := canonical.NewPair(base, quote)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	apr := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	may := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	jun := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	for i, tr := range []canonical.Trade{
		// May, XLM in both spellings, against two USD proxies.
		mkAPITrade(1, may, pair(native, usdc), 1_000_000_000, 100_000_000),                                // 100 XLM for 10 USDC → 0.10
		cex(mkAPITrade(2, may.Add(time.Hour), pair(cryptoXLM, usdc), 30_000_000_000, 6_000_000_000)),      // 300 XLM for 60 USDC → 0.20
		cex(mkAPITrade(3, may.Add(2*time.Hour), pair(cryptoXLM, fiatUSD), 10_000_000_000, 2_500_000_000)), // 100 XLM for 25 USD → 0.25
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
	rows, err := store.MonthlyUSDVWAPs(ctx, from, to, func(src string) int { return external.Lookup(src).AmountScaleDecimals() })
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

// TestObservationIntraLedgerSeqGuard is the proven-red test for the
// 8-worker last-writer-wins bug (migration 0111): the
// PersistEvents workers (PersistWorkers=8) do NOT preserve order, and the
// `*_observations` writers upsert with pure last-writer-wins. So when a
// single (contract/holder, asset, ledger) changes MULTIPLE times within one
// ledger, whichever worker commits LAST wins — which is NOT necessarily the
// FINAL intra-ledger state. A stale intra-ledger balance could be persisted
// as the observation for that ledger → a wrong classic-supply component (the
// served supply the operator keeps re-backfilling).
//
// The fix stamps each observation with its within-ledger position
// (intra_ledger_seq) and guards the upsert
// (intra_ledger_seq <= EXCLUDED.intra_ledger_seq) so an EARLIER intra-ledger
// change delivered LATE by an out-of-order worker can never overwrite the
// FINAL change.
//
// Proven-red evidence, all on ONE live container:
//   - "unguarded_last_writer_wins_reproduces_bug" runs the EXACT unguarded SQL
//     (a plain ON CONFLICT DO UPDATE with no seq guard) and shows the STALE
//     (earlier) value wins when it commits last — the bug, reproduced.
//   - "guarded_writer_keeps_final" runs the REAL fixed writer with the SAME
//     out-of-order sequence and shows the FINAL value wins. This assertion
//     FAILS on an unguarded writer (no column / no guard → the stale late
//     write wins), so it is red-on-unfixed and green-after-fix.
func TestObservationIntraLedgerSeqGuard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// One fixed instant so both writes share the (…, ledger, observed_at)
	// primary key — a conflict on the SAME row, exactly as the live path
	// produces for two changes to one entry in one ledger (observed_at is the
	// ledger close time, identical for every change in the ledger).
	observedAt := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	const (
		assetKey   = "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		ledger     = uint32(5_000_000)
		staleValue = int64(100) // an EARLIER intra-ledger balance
		finalValue = int64(900) // the FINAL intra-ledger balance
	)

	readSAC := func(t *testing.T, contractID, holder string) (balance string, seq int64) {
		t.Helper()
		const q = `SELECT balance_stroops::text, intra_ledger_seq
		             FROM sac_balance_observations
		            WHERE contract_id = $1 AND holder = $2 AND ledger = $3`
		if err := store.DB().QueryRowContext(ctx, q, contractID, holder, int(ledger)).
			Scan(&balance, &seq); err != nil {
			t.Fatalf("read sac_balance_observations (%s,%s): %v", contractID, holder, err)
		}
		return balance, seq
	}

	writeSAC := func(t *testing.T, contractID, holder string, bal int64, seq uint32) {
		t.Helper()
		if err := store.InsertSACBalanceObservation(ctx, timescale.SACBalanceObservation{
			ContractID:     contractID,
			AssetKey:       assetKey,
			Holder:         holder,
			Ledger:         ledger,
			ObservedAt:     observedAt,
			Balance:        big.NewInt(bal),
			IntraLedgerSeq: seq,
		}); err != nil {
			t.Fatalf("InsertSACBalanceObservation(%s,%s,seq=%d): %v", contractID, holder, seq, err)
		}
	}

	// ── The unguarded behaviour, reproduced live ────────────────────────────
	// Raw unguarded last-writer-wins (the writer without the seq guard), writing the
	// FINAL change first and the EARLIER change last (an out-of-order worker):
	// the stale earlier value overwrites the final. This is the bug.
	t.Run("unguarded_last_writer_wins_reproduces_bug", func(t *testing.T) {
		const (
			contractID = "CBUNGUARDEDSACWRAPPERCONTRACTID000000000000000000000000"
			holder     = "GHOLDERUNGUARDEDAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		)
		unguardedUpsert := func(bal int64, seq uint32) {
			// Byte-for-byte the pre-0111 writer: value columns overwritten on
			// conflict with NO intra_ledger_seq guard. (We still populate the
			// column so the row is valid post-migration; the point is the
			// MISSING WHERE clause.)
			const q = `
                INSERT INTO sac_balance_observations (
                    contract_id, asset_key, holder, ledger, observed_at,
                    balance_stroops, is_removal, intra_ledger_seq
                ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
                ON CONFLICT (contract_id, holder, ledger, observed_at) DO UPDATE SET
                    asset_key        = EXCLUDED.asset_key,
                    balance_stroops  = EXCLUDED.balance_stroops,
                    is_removal       = EXCLUDED.is_removal,
                    intra_ledger_seq = EXCLUDED.intra_ledger_seq
            `
			if _, err := store.DB().ExecContext(ctx, q,
				contractID, assetKey, holder, int(ledger), observedAt,
				big.NewInt(bal).String(), false, int64(seq),
			); err != nil {
				t.Fatalf("unguarded upsert (bal=%d,seq=%d): %v", bal, seq, err)
			}
		}

		unguardedUpsert(finalValue, 5) // FINAL change lands first
		unguardedUpsert(staleValue, 2) // EARLIER change commits LAST (out of order)

		if bal, _ := readSAC(t, contractID, holder); bal != "100" {
			t.Fatalf("pre-fix reproduction: balance = %s, expected the STALE 100 to win "+
				"under unguarded last-writer-wins (the C2-6 bug)", bal)
		}
		t.Logf("pre-fix last-writer-wins persisted the STALE intra-ledger balance 100 "+
			"(final was %d) — the bug the guard fixes", finalValue)
	})

	// ── The fix ───────────────────────────────────────────────────────────
	// The REAL guarded writer, SAME out-of-order sequence: FINAL first, then
	// the EARLIER change last. The guard rejects the late earlier write, so
	// the FINAL value survives. Red-on-unfixed (no column/guard → stale wins).
	t.Run("guarded_writer_keeps_final_when_earlier_change_commits_last", func(t *testing.T) {
		const (
			contractID = "CBGUARDEDSACWRAPPERCONTRACTID000000000000000000000000000"
			holder     = "GHOLDERGUARDEDAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		)
		writeSAC(t, contractID, holder, finalValue, 5) // FINAL change lands first
		writeSAC(t, contractID, holder, staleValue, 2) // EARLIER change commits LAST

		if bal, seq := readSAC(t, contractID, holder); bal != "900" || seq != 5 {
			t.Fatalf("out-of-order: balance = %s (seq %d), want FINAL 900 (seq 5) — "+
				"the guard must reject the late-arriving earlier change (C2-6)", bal, seq)
		}
	})

	// Forward path unbroken: writing in the natural order (earlier then final)
	// must also land the FINAL value — the guard admits a HIGHER position.
	t.Run("guarded_writer_forward_order_still_lands_final", func(t *testing.T) {
		const (
			contractID = "CBFORWARDSACWRAPPERCONTRACTID000000000000000000000000000"
			holder     = "GHOLDERFORWARDAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		)
		writeSAC(t, contractID, holder, staleValue, 2) // earlier change first
		writeSAC(t, contractID, holder, finalValue, 5) // final change second

		if bal, seq := readSAC(t, contractID, holder); bal != "900" || seq != 5 {
			t.Fatalf("forward order: balance = %s (seq %d), want FINAL 900 (seq 5)", bal, seq)
		}
	})

	// The ops seed sentinel (SeedIntraLedgerSeq = MaxUint32) is the top of the
	// intra-ledger order: a live per-ledger change (a much smaller position)
	// can never overwrite a seed, and a re-seed (equal sentinel) stays
	// corrective. This is what keeps the seed authoritative AND correctable.
	t.Run("seed_sentinel_wins_over_live_and_reseed_is_corrective", func(t *testing.T) {
		const (
			contractID = "CBSEEDSACWRAPPERCONTRACTID0000000000000000000000000000000"
			holder     = "GHOLDERSEEDAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		)
		writeSAC(t, contractID, holder, 500, timescale.SeedIntraLedgerSeq) // seed: authoritative final state
		writeSAC(t, contractID, holder, 999, 7)                            // a live per-ledger change, later
		if bal, _ := readSAC(t, contractID, holder); bal != "500" {
			t.Fatalf("seed sentinel: balance = %s, want the seed's 500 to survive a "+
				"live per-ledger change (live position cannot exceed the sentinel)", bal)
		}
		writeSAC(t, contractID, holder, 501, timescale.SeedIntraLedgerSeq) // re-seed correction
		if bal, _ := readSAC(t, contractID, holder); bal != "501" {
			t.Fatalf("re-seed: balance = %s, want the corrective re-seed 501 to land "+
				"(equal sentinel is admitted by the <= guard)", bal)
		}
	})

	// ── Walk-version renumbering boundary ────────────────────────────────
	//
	// intra_ledger_seq is PERSISTED and compared ACROSS BINARY VERSIONS by
	// this guard, but it is only meaningful within one walk version
	// (dispatcher.EntryWalkVersion). Walk version 2 renumbered every ledger:
	// the v1 per-tx walk gave an account's ledger-final balance a HIGH
	// position (a fee-phase change that sorted last — the corruption); the v2
	// ledger-wide walk correctly places that same final balance LOWER.
	//
	// When both rows carry the SAME walk_version (a renumbering shipped
	// without bumping EntryWalkVersion), replaying the changes under the new
	// binary is SILENTLY DISCARDED by `legacy_seq <= corrected_seq` = false,
	// and re-running never helps because the walk is deterministic. This
	// subtest pins BOTH halves: the trap, and the repair path migration 0120
	// names (reconstruct-final-then-seed at SeedIntraLedgerSeq). A bumped
	// version is covered by the walk_version subtests below.
	t.Run("walk_version_renumbering_strands_replay_but_seed_repairs", func(t *testing.T) {
		const (
			contractID = "CBRENUMBERSACWRAPPERCONTRACTID00000000000000000000000000"
			holder     = "GHOLDERRENUMBERAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
			legacyBad  = int64(700) // v1 walk: a FEE-phase balance, stamped last
			correct    = int64(250) // v2 walk: the true ledger-final balance
		)
		// A row written by the OLD binary: wrong value, HIGH position.
		writeSAC(t, contractID, holder, legacyBad, 6)

		// The corrective re-derive under the NEW binary recomputes the same
		// entry at its correct — and LOWER — position.
		writeSAC(t, contractID, holder, correct, 3)
		if bal, seq := readSAC(t, contractID, holder); bal != "700" || seq != 6 {
			t.Fatalf("renumbering trap: balance = %s (seq %d), want the LEGACY 700 (seq 6) to "+
				"still be there — this documents that a change-replay re-derive at a lower "+
				"walk-version position is dropped by the guard", bal, seq)
		}
		// Replaying does not help: same deterministic position, same rejection.
		writeSAC(t, contractID, holder, correct, 3)
		if bal, _ := readSAC(t, contractID, holder); bal != "700" {
			t.Fatalf("replay: balance = %s, want 700 — a second identical replay must not "+
				"change the outcome (the walk is deterministic)", bal)
		}

		// THE REPAIR PATH (migration 0120): reconstruct the FINAL per-(key,
		// ledger) state from the repaired lake and write ONE row at the
		// sentinel. `<=` always admits MaxUint32, so the correction lands
		// regardless of what the legacy row carries.
		writeSAC(t, contractID, holder, correct, timescale.SeedIntraLedgerSeq)
		if bal, seq := readSAC(t, contractID, holder); bal != "250" || seq != int64(timescale.SeedIntraLedgerSeq) {
			t.Fatalf("reconstruct-final-then-seed: balance = %s (seq %d), want the CORRECTED "+
				"250 at the sentinel — this is the only repair that beats a legacy-numbered row",
				bal, seq)
		}
		// And it stays idempotent-corrective: a re-run of the repair re-lands.
		writeSAC(t, contractID, holder, 251, timescale.SeedIntraLedgerSeq)
		if bal, _ := readSAC(t, contractID, holder); bal != "251" {
			t.Fatalf("repair re-run: balance = %s, want 251 (equal sentinel is admitted)", bal)
		}
	})

	// walk_version (migration 0199) scopes the comparison: a row numbered by
	// an older walk — or by a binary that did not record one (0) — is replaced
	// by the current writer's correction even at a lower position, and the
	// within-walk guard still holds afterwards.
	t.Run("older_walk_version_row_yields_to_current_walk_correction", func(t *testing.T) {
		const (
			holder    = "GHOLDERWALKVERSIONAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
			legacyBad = int64(700)
			correct   = int64(250)
		)
		for _, tc := range []struct {
			name       string
			contractID string
			legacyWV   int
		}{
			{"unrecorded", "CBWALKVERSIONUNRECORDED0000000000000000000000000000000000", 0},
			{"previous_walk", "CBWALKVERSIONPREVIOUS000000000000000000000000000000000000", dispatcher.EntryWalkVersion - 1},
		} {
			t.Run(tc.name, func(t *testing.T) {
				const q = `
                    INSERT INTO sac_balance_observations (
                        contract_id, asset_key, holder, ledger, observed_at,
                        balance_stroops, is_removal, intra_ledger_seq, walk_version
                    ) VALUES ($1,$2,$3,$4,$5,$6,false,6,$7)`
				if _, err := store.DB().ExecContext(ctx, q,
					tc.contractID, assetKey, holder, int(ledger), observedAt,
					big.NewInt(legacyBad).String(), tc.legacyWV,
				); err != nil {
					t.Fatalf("legacy insert: %v", err)
				}

				writeSAC(t, tc.contractID, holder, correct, 3)
				if bal, seq := readSAC(t, tc.contractID, holder); bal != "250" || seq != 3 {
					t.Fatalf("balance = %s (seq %d), want the current walk's 250 (seq 3) to replace "+
						"the walk-version-%d row at seq 6", bal, seq, tc.legacyWV)
				}
				var wv int
				if err := store.DB().QueryRowContext(ctx,
					`SELECT walk_version FROM sac_balance_observations WHERE contract_id = $1 AND holder = $2 AND ledger = $3`,
					tc.contractID, holder, int(ledger)).Scan(&wv); err != nil {
					t.Fatalf("read walk_version: %v", err)
				}
				if wv != dispatcher.EntryWalkVersion {
					t.Fatalf("walk_version = %d, want dispatcher.EntryWalkVersion %d", wv, dispatcher.EntryWalkVersion)
				}

				writeSAC(t, tc.contractID, holder, 100, 2)
				if bal, _ := readSAC(t, tc.contractID, holder); bal != "250" {
					t.Fatalf("balance = %s, want 250: an earlier change in the same walk must still be rejected", bal)
				}
			})
		}
	})

	// The other four writers carry their own copy of the guard. Each seeds a
	// row the way the previous binary writes it (no walk_version → 0) at a
	// higher position, then the real writer's lower-position correction must
	// replace it.
	t.Run("every_writer_replaces_an_unrecorded_walk_row", func(t *testing.T) {
		const legacyID = "GWALKVERSIONLEGACYAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		for _, tc := range []struct {
			table, legacySQL, readSQL string
			id                        string // overrides legacyID when a table has two writer paths
			write                     func() error
		}{
			{
				table: "account_observations",
				legacySQL: `INSERT INTO account_observations (account_id, ledger, observed_at, balance_stroops, flags, seq_num, is_removal, intra_ledger_seq)
				            VALUES ($1, $2, $3, 700, 0, 0, false, 6)`,
				readSQL: `SELECT balance_stroops::text FROM account_observations WHERE account_id = $1 AND ledger = $2`,
				write: func() error {
					return store.InsertAccountObservation(ctx, domain.AccountObservation{
						AccountID: legacyID, Ledger: ledger, ObservedAt: observedAt, Balance: big.NewInt(250), IntraLedgerSeq: 3,
					})
				},
			},
			{
				table: "trustline_observations",
				legacySQL: `INSERT INTO trustline_observations (account_id, asset_key, ledger, observed_at, balance_stroops, is_removal, intra_ledger_seq)
				            VALUES ($1, '` + assetKey + `', $2, $3, 700, false, 6)`,
				readSQL: `SELECT balance_stroops::text FROM trustline_observations WHERE account_id = $1 AND ledger = $2`,
				write: func() error {
					return store.InsertTrustlineObservation(ctx, timescale.TrustlineObservation{
						AccountID: legacyID, AssetKey: assetKey, Ledger: ledger, ObservedAt: observedAt, Balance: big.NewInt(250), IntraLedgerSeq: 3,
					})
				},
			},
			{
				table: "claimable_observations",
				legacySQL: `INSERT INTO claimable_observations (claimable_id, asset_key, ledger, observed_at, balance_stroops, is_removal, intra_ledger_seq)
				            VALUES ($1, '` + assetKey + `', $2, $3, 700, false, 6)`,
				readSQL: `SELECT balance_stroops::text FROM claimable_observations WHERE claimable_id = $1 AND ledger = $2`,
				write: func() error {
					return store.InsertClaimableObservationBatch(ctx, []timescale.ClaimableObservation{{
						ClaimableID: legacyID, AssetKey: assetKey, Ledger: ledger, ObservedAt: observedAt, Balance: big.NewInt(250), IntraLedgerSeq: 3,
					}})
				},
			},
			{
				table: "claimable_observations",
				id:    legacyID + "SINGLE",
				legacySQL: `INSERT INTO claimable_observations (claimable_id, asset_key, ledger, observed_at, balance_stroops, is_removal, intra_ledger_seq)
				            VALUES ($1, '` + assetKey + `', $2, $3, 700, false, 6)`,
				readSQL: `SELECT balance_stroops::text FROM claimable_observations WHERE claimable_id = $1 AND ledger = $2`,
				write: func() error {
					return store.InsertClaimableObservation(ctx, timescale.ClaimableObservation{
						ClaimableID: legacyID + "SINGLE", AssetKey: assetKey, Ledger: ledger, ObservedAt: observedAt, Balance: big.NewInt(250), IntraLedgerSeq: 3,
					})
				},
			},
			{
				table: "lp_reserve_observations",
				legacySQL: `INSERT INTO lp_reserve_observations (pool_id, asset_key, ledger, observed_at, balance_stroops, is_removal, intra_ledger_seq)
				            VALUES ($1, '` + assetKey + `', $2, $3, 700, false, 6)`,
				readSQL: `SELECT balance_stroops::text FROM lp_reserve_observations WHERE pool_id = $1 AND ledger = $2`,
				write: func() error {
					return store.InsertLPReserveObservation(ctx, timescale.LPReserveObservation{
						PoolID: legacyID, AssetKey: assetKey, Ledger: ledger, ObservedAt: observedAt, Balance: big.NewInt(250), IntraLedgerSeq: 3,
					})
				},
			},
		} {
			t.Run(tc.table, func(t *testing.T) {
				id := legacyID
				if tc.id != "" {
					id = tc.id
				}
				if _, err := store.DB().ExecContext(ctx, tc.legacySQL, id, int(ledger), observedAt); err != nil {
					t.Fatalf("legacy insert: %v", err)
				}
				if err := tc.write(); err != nil {
					t.Fatalf("writer: %v", err)
				}
				var bal string
				if err := store.DB().QueryRowContext(ctx, tc.readSQL, id, int(ledger)).Scan(&bal); err != nil {
					t.Fatalf("read: %v", err)
				}
				if bal != "250" {
					t.Fatalf("%s balance = %s, want the stamped correction 250 to replace the unrecorded-walk 700", tc.table, bal)
				}
			})
		}
	})

	// A row from a NEWER walk than this binary's (a rolled-back binary
	// replaying a ledger) is not displaced, whatever its position.
	t.Run("newer_walk_version_row_is_not_displaced", func(t *testing.T) {
		const (
			contractID = "CBWALKVERSIONNEWER00000000000000000000000000000000000000"
			holder     = "GHOLDERWALKVERSIONNEWERAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		)
		const q = `
            INSERT INTO sac_balance_observations (
                contract_id, asset_key, holder, ledger, observed_at,
                balance_stroops, is_removal, intra_ledger_seq, walk_version
            ) VALUES ($1,$2,$3,$4,$5,'400',false,1,$6)`
		if _, err := store.DB().ExecContext(ctx, q,
			contractID, assetKey, holder, int(ledger), observedAt, dispatcher.EntryWalkVersion+1,
		); err != nil {
			t.Fatalf("newer-walk insert: %v", err)
		}
		writeSAC(t, contractID, holder, 999, 9)
		if bal, _ := readSAC(t, contractID, holder); bal != "400" {
			t.Fatalf("balance = %s, want the newer walk's 400 to survive an older walk's write", bal)
		}
	})

	// Readers order by ledger, walk_version, then position: within one
	// ledger the higher walk_version row wins even when it is older by
	// observed_at and lower by intra_ledger_seq.
	t.Run("reader_prefers_higher_walk_version_in_same_ledger", func(t *testing.T) {
		const (
			contractID = "CBWALKVERSIONREADER0000000000000000000000000000000000000"
			holder     = "GHOLDERWALKVERSIONREADERAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		)
		const q = `
            INSERT INTO sac_balance_observations (
                contract_id, asset_key, holder, ledger, observed_at,
                balance_stroops, is_removal, intra_ledger_seq, walk_version
            ) VALUES ($1,$2,$3,$4,$5,$6,false,$7,$8)`
		if _, err := store.DB().ExecContext(ctx, q, contractID, assetKey, holder, int(ledger),
			observedAt.Add(time.Second), "111", 9, 1); err != nil {
			t.Fatalf("lower-version insert: %v", err)
		}
		if _, err := store.DB().ExecContext(ctx, q, contractID, assetKey, holder, int(ledger),
			observedAt, "222", 1, 2); err != nil {
			t.Fatalf("higher-version insert: %v", err)
		}
		got, err := store.SACBalanceForContractAtOrBefore(ctx, holder, assetKey, ledger)
		if err != nil {
			t.Fatalf("reader: %v", err)
		}
		if got.String() != "222" {
			t.Fatalf("balance = %s, want the higher walk_version row 222", got)
		}
	})

	// The finding lists account_observations.go:43 as a sibling site; the
	// same guard protects the native-XLM reserve component (fee + op + op in
	// one ledger). Prove the FINAL post-state wins under out-of-order commit.
	t.Run("account_observation_guard_keeps_final", func(t *testing.T) {
		const accountID = "GACCOUNTC26AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		writeAcct := func(bal int64, seq uint32) {
			t.Helper()
			if err := store.InsertAccountObservation(ctx, domain.AccountObservation{
				AccountID:      accountID,
				Ledger:         ledger,
				ObservedAt:     observedAt,
				Balance:        big.NewInt(bal),
				IntraLedgerSeq: seq,
			}); err != nil {
				t.Fatalf("InsertAccountObservation(seq=%d): %v", seq, err)
			}
		}
		writeAcct(finalValue, 9) // FINAL post-state (last op) lands first
		writeAcct(staleValue, 4) // an earlier change (e.g. fee debit) commits last

		const q = `SELECT balance_stroops::text FROM account_observations
		            WHERE account_id = $1 AND ledger = $2`
		var bal string
		if err := store.DB().QueryRowContext(ctx, q, accountID, int(ledger)).Scan(&bal); err != nil {
			t.Fatalf("read account_observations: %v", err)
		}
		if bal != "900" {
			t.Fatalf("account out-of-order: balance = %s, want FINAL 900 (C2-6)", bal)
		}
	})
}

// Audit finding B11-F1 — dust trades set OHLC chart extremes
// (docs/operations/finding-dust-trades-set-chart-extremes.md).
//
// Without a size filter on the prices_* continuous aggregates' OHLC extremes,
// one economically-meaningless fill sets high/low for the whole bucket.
// Example: the served XLM/USD low was 0.1333333333 — the inverse of a SINGLE
// `USDC-GA5Z…/native` print of 2 stroops for 15 stroops (usd_volume
// $0.00000027, price 7.5), while the real market low that hour was 0.1822.
//
// Migration 0115 adds a notional floor ($0.01 of usd_volume) to the extremes
// inside the CAGGs — it must be there, not in the serve layer, because the
// CAGG stores only the already-collapsed extremes.
//
// These tests seed the EXACT production shape (a 2↔15-stroop crumb inside an
// otherwise healthy bucket, stored in the REVERSE direction so the serve layer
// inverts it) and assert the corrected numbers, on a real TimescaleDB.

// ohlcDustPair identifies one seeded scenario pair.
type ohlcDustPair struct {
	base  string
	quote string
}

// dustFloorGrains is every price CAGG the notional floor must cover.
var dustFloorGrains = []string{"1m", "15m", "1h", "4h", "1d", "1w", "1mo"}

const (
	// usdcIssuer is the canonical Circle USDC asset id (the pair the
	// production wick was observed on).
	usdcIssuer = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	// Scenario pairs reuse the same (valid) issuer with distinct codes so
	// each scenario gets its own row in every grain, at the SAME timestamp —
	// a single bucket per grain, uncontaminated by the other scenarios.
	dustOnlyAsset = "DSTO-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	whaleAsset    = "WHAL-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	unpricedAsset = "NUSD-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
)

// TestOHLCDustFloor_CAGGExtremes proves migration 0115's notional floor on all
// seven price CAGGs, across the four cases the fix has to get right:
//
//	A. mixed bucket   — dust must NOT set high/low/open/close; the normal
//	                    trades must; volume + trade_count keep ALL trades.
//	B. all-dust       — a bucket with only dust must still report an extreme
//	                    (the COALESCE fallback), never NULL.
//	C. legit whale    — a large trade far from VWAP is a REAL market event and
//	                    must be KEPT (we filter on SIZE, never on price
//	                    distance — see the finding's operator DECISION).
//	D. NULL usd_volume — an unpriced pair keeps today's unfiltered behaviour.
func TestOHLCDustFloor_CAGGExtremes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// 3h back, snapped to the hour: the 1h bucket is closed (bucket+1h <=
	// now()) so the serve-layer assertion below sees it, and every coarser
	// grain buckets the same instant.
	t0 := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Hour)

	// ── A. the production shape ────────────────────────────────────────
	// Stored REVERSE (base=USDC, quote=native), exactly as the SDEX decoder
	// recorded the offending trade: price = XLM per USDC ≈ 5.4875, which the
	// serve layer inverts to XLM/USD ≈ 0.1822.
	mixed := ohlcDustPair{base: usdcIssuer, quote: "native"}
	seed(t, db, ctx, mixed, []seedTrade{
		// The crumb: 2 stroops for 15 stroops → price 7.5, $0.00000027.
		// FIRST in the bucket, so it also owns `open` when unfiltered.
		{off: 1 * time.Second, base: "2", quote: "15", usd: "0.00000027"},
		{off: 10 * time.Second, base: "10000000", quote: "54875000", usd: "1000"}, // 5.4875 → open
		{off: 20 * time.Second, base: "10000000", quote: "54885000", usd: "1000"}, // 5.4885 → high
		{off: 30 * time.Second, base: "10000000", quote: "54200000", usd: "1000"}, // 5.42   → low
		{off: 40 * time.Second, base: "10000000", quote: "54500000", usd: "1000"}, // 5.45   → close
		// A second crumb on the other side, LAST in the bucket so it also
		// owns `close` when unfiltered.
		{off: 50 * time.Second, base: "10000000", quote: "10000", usd: "0.000001"}, // 0.001
	}, t0)

	// ── B. all-dust bucket (COALESCE fallback) ─────────────────────────
	dustOnly := ohlcDustPair{base: dustOnlyAsset, quote: "native"}
	seed(t, db, ctx, dustOnly, []seedTrade{
		{off: 1 * time.Second, base: "2", quote: "15", usd: "0.00000027"},          // 7.5
		{off: 30 * time.Second, base: "10000000", quote: "10000", usd: "0.000001"}, // 0.001
	}, t0)

	// ── C. legitimate large trade far from market ──────────────────────
	whale := ohlcDustPair{base: whaleAsset, quote: "native"}
	seed(t, db, ctx, whale, []seedTrade{
		{off: 10 * time.Second, base: "10000000", quote: "54875000", usd: "1000"},    // 5.4875
		{off: 20 * time.Second, base: "10000000", quote: "80000000", usd: "100000"},  // 8.0, $100k
		{off: 30 * time.Second, base: "10000000", quote: "20000000", usd: "100000"},  // 2.0, $100k
		{off: 40 * time.Second, base: "10000000", quote: "54900000", usd: "1000"},    // 5.49
		{off: 50 * time.Second, base: "2", quote: "15", usd: "0.00000027"},           // 7.5 crumb
		{off: 55 * time.Second, base: "10000000", quote: "10000", usd: "0.00000027"}, // 0.001 crumb
	}, t0)

	// ── D. unpriced pair (usd_volume NULL) ─────────────────────────────
	unpriced := ohlcDustPair{base: unpricedAsset, quote: "native"}
	seed(t, db, ctx, unpriced, []seedTrade{
		{off: 10 * time.Second, base: "10000000", quote: "54875000", usd: ""}, // NULL
		{off: 20 * time.Second, base: "2", quote: "15", usd: ""},              // NULL crumb, 7.5
	}, t0)

	refreshPriceCAGGs(t, db, ctx)

	for _, grain := range dustFloorGrains {
		t.Run("mixed/"+grain, func(t *testing.T) {
			got := readCAGG(t, db, ctx, grain, mixed, t0)
			// The crumbs (7.5 and 0.001) must be gone from EVERY extreme.
			assertNumeric(t, "high_price", got.high, "5.4885")
			assertNumeric(t, "low_price", got.low, "5.4200")
			assertNumeric(t, "first_price", got.first, "5.4875")
			assertNumeric(t, "last_price", got.last, "5.4500")
			// …but the dust still counts as volume and as a trade: the floor
			// filters the EXTREMES only, never the volume/count semantics.
			if got.tradeCount != 6 {
				t.Errorf("trade_count = %d, want 6 — the floor must not drop trades from the count", got.tradeCount)
			}
			assertNumeric(t, "volume", got.volume, "50000002")
			assertNumeric(t, "volume_usd", got.volumeUSD, "4000.00000127")
		})

		t.Run("all-dust/"+grain, func(t *testing.T) {
			got := readCAGG(t, db, ctx, grain, dustOnly, t0)
			// COALESCE fallback: a bucket with ONLY dust still reports the
			// dust extreme rather than NULL (a NULL here would blank the bar).
			assertNumeric(t, "high_price", got.high, "7.5")
			assertNumeric(t, "low_price", got.low, "0.001")
			assertNumeric(t, "first_price", got.first, "7.5")
			assertNumeric(t, "last_price", got.last, "0.001")
		})

		t.Run("whale/"+grain, func(t *testing.T) {
			got := readCAGG(t, db, ctx, grain, whale, t0)
			// A $100k fill at 8.0 (≈1.46× VWAP) is a real market event and
			// MUST show. This is what the removed 2×-VWAP serve-layer band
			// could have clipped, and what a "drop prices far from VWAP"
			// filter would wrongly suppress.
			assertNumeric(t, "high_price", got.high, "8.0")
			assertNumeric(t, "low_price", got.low, "2.0")
		})

		t.Run("unpriced/"+grain, func(t *testing.T) {
			got := readCAGG(t, db, ctx, grain, unpriced, t0)
			// usd_volume IS NULL never satisfies `>= 0.01`, so an entirely
			// unpriced bucket falls through the COALESCE to the unfiltered
			// extreme — today's behaviour, deliberately unchanged (finding
			// open question 2: unpriced pairs keep current behaviour).
			assertNumeric(t, "high_price", got.high, "7.5")
			assertNumeric(t, "low_price", got.low, "5.4875")
		})
	}
}

// TestOHLCDustFloor_ServedSeriesReproducesTheWick is the end-to-end
// reproduction of the reported symptom through the real serve query
// (Store.OHLCSeries — the non-fiat `?interval=` path): the crumb is stored in
// the REVERSE direction, so `1/high_price` becomes the served LOW.
//
// Without the floor this bar serves low = 0.1333333333 (1/7.5) and high = 1000
// (1/0.001); with it, the real market range, low 0.1822 / high 0.1845 —
// matching the CEX range (0.1822–0.1836) measured for the same bar.
func TestOHLCDustFloor_ServedSeriesReproducesTheWick(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	t0 := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Hour)
	stored := ohlcDustPair{base: usdcIssuer, quote: "native"}
	seed(t, db, ctx, stored, []seedTrade{
		{off: 1 * time.Second, base: "2", quote: "15", usd: "0.00000027"},
		{off: 10 * time.Second, base: "10000000", quote: "54875000", usd: "1000"},
		{off: 20 * time.Second, base: "10000000", quote: "54885000", usd: "1000"},
		{off: 30 * time.Second, base: "10000000", quote: "54200000", usd: "1000"},
		{off: 40 * time.Second, base: "10000000", quote: "54500000", usd: "1000"},
		{off: 50 * time.Second, base: "10000000", quote: "10000", usd: "0.000001"},
	}, t0)
	refreshPriceCAGGs(t, db, ctx)

	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	// Requested orientation XLM/USDC — the stored rows are USDC/XLM, so every
	// price is inverted and high↔low swap. This is the production path.
	xlmUSDC, err := canonical.NewPair(canonical.NativeAsset(), usdc)
	if err != nil {
		t.Fatal(err)
	}

	bars, err := store.OHLCSeries(ctx, xlmUSDC, timescale.HistoryGranularity("1h"),
		t0.Add(-time.Hour), t0.Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("OHLCSeries: %v", err)
	}
	if len(bars) != 1 {
		t.Fatalf("OHLCSeries returned %d bars, want 1 (the seeded hour)", len(bars))
	}
	b := bars[0]

	// The headline: 0.1333333333 was the served low. It must now be 0.1822.
	// Exact inverses: the serve layer divides in NUMERIC, so a rounded literal
	// would spend the tolerance instead of measuring the served value.
	const wantLow = 1 / 5.4885
	if got := mustFloat(t, b.Low); !closeTo(got, wantLow, 1e-9) {
		t.Errorf("served low = %s, want ~%.8f (1/5.4885, the real market low).\n"+
			"0.1333333333 here is the B11-F1 dust wick (1/7.5, a 2↔15-stroop crumb)", b.Low, wantLow)
	}
	const wantHigh = 1 / 5.42
	if got := mustFloat(t, b.High); !closeTo(got, wantHigh, 1e-9) {
		t.Errorf("served high = %s, want ~%.8f (1/5.42); 1000 here is the 0.001 dust print inverted", b.High, wantHigh)
	}
	const wantOpen = 1 / 5.4875
	if got := mustFloat(t, b.Open); !closeTo(got, wantOpen, 1e-9) {
		t.Errorf("served open = %s, want ~%.8f (1/5.4875) — the first NON-dust trade", b.Open, wantOpen)
	}
	const wantClose = 1 / 5.45
	if got := mustFloat(t, b.Close); !closeTo(got, wantClose, 1e-9) {
		t.Errorf("served close = %s, want ~%.8f (1/5.45) — the last NON-dust trade", b.Close, wantClose)
	}
	if b.TradeCount != 6 {
		t.Errorf("served trade_count = %d, want 6 (dust still counts as a trade)", b.TradeCount)
	}
}

// ─── helpers ──────────────────────────────────────────────────────────

// seedTrade is one row to write into `trades`: `off` from the bucket start,
// raw stroop amounts as NUMERIC text, and usd_volume ("" → SQL NULL).
type seedTrade struct {
	off   time.Duration
	base  string
	quote string
	usd   string
}

// seedNonce keeps every seeded trade's primary key distinct across pairs and
// tests within a run.
var seedNonce int

// seed inserts trades directly so usd_volume is exactly what the scenario
// needs (the resolver-driven InsertTrade path derives it instead).
func seed(t *testing.T, db *sql.DB, ctx context.Context, p ohlcDustPair, trades []seedTrade, t0 time.Time) {
	t.Helper()
	for _, tr := range trades {
		seedNonce++
		var usd any
		if tr.usd != "" {
			usd = tr.usd
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO trades
			    (source, ledger, tx_hash, op_index, ts,
			     base_asset, quote_asset, base_amount, quote_amount, usd_volume)
			VALUES ('sdex', $1, $2, 0, $3, $4, $5, $6::numeric, $7::numeric, $8::numeric)`,
			50_000_000+seedNonce,
			fmt.Sprintf("%064x", seedNonce),
			t0.Add(tr.off),
			p.base, p.quote, tr.base, tr.quote, usd,
		); err != nil {
			t.Fatalf("seed trade (%s/%s, +%s): %v", p.base, p.quote, tr.off, err)
		}
	}
}

func refreshPriceCAGGs(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	for _, grain := range dustFloorGrains {
		if _, err := db.ExecContext(ctx,
			"CALL refresh_continuous_aggregate('prices_"+grain+"', NULL, NULL)"); err != nil {
			t.Fatalf("refresh prices_%s: %v", grain, err)
		}
	}
}

// caggRow is the subset of a prices_<grain> row these tests assert on.
// Extremes are nullable so a missing COALESCE fallback surfaces as an
// explicit NULL failure rather than a scan error.
type caggRow struct {
	high, low, first, last sql.NullString
	volume, volumeUSD      sql.NullString
	tradeCount             int64
}

func readCAGG(t *testing.T, db *sql.DB, ctx context.Context, grain string, p ohlcDustPair, t0 time.Time) caggRow {
	t.Helper()
	var got caggRow
	// A single seeded instant lands in exactly one bucket per grain, so
	// bucket <= t0 < bucket + grain: select the row covering t0.
	err := db.QueryRowContext(ctx, `
		SELECT high_price::text, low_price::text, first_price::text, last_price::text,
		       volume::text, volume_usd::text, trade_count
		  FROM prices_`+grain+`
		 WHERE base_asset = $1 AND quote_asset = $2
		   AND bucket <= $3
		 ORDER BY bucket DESC
		 LIMIT 1`,
		p.base, p.quote, t0,
	).Scan(&got.high, &got.low, &got.first, &got.last, &got.volume, &got.volumeUSD, &got.tradeCount)
	if err != nil {
		t.Fatalf("read prices_%s for %s/%s: %v", grain, p.base, p.quote, err)
	}
	return got
}

// assertNumeric compares a NUMERIC::text column against an expected decimal
// literal by value (so "5.4200" == "5.42"), failing loudly on NULL.
func assertNumeric(t *testing.T, col string, got sql.NullString, want string) {
	t.Helper()
	if !got.Valid {
		t.Errorf("%s = NULL, want %s — the COALESCE fallback is missing", col, want)
		return
	}
	g := mustFloat(t, got.String)
	w := mustFloat(t, want)
	if !closeTo(g, w, 1e-9*max(1, absFloat(w))) {
		t.Errorf("%s = %s, want %s", col, got.String, want)
	}
}

func closeTo(got, want, tol float64) bool { return absFloat(got-want) <= tol }

func absFloat(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// timescaleBucketOrigin is time_bucket's default origin: Monday
// 2000-01-03 00:00 UTC. Every fixed-width bucket Timescale produces
// is origin + k×width, which is why 1w and 2w buckets start on
// Mondays and 3d buckets sit on a 3-day grid anchored there rather
// than at any particular month.
var timescaleBucketOrigin = time.Date(2000, 1, 3, 0, 0, 0, 0, time.UTC)

// timeBucket mirrors Postgres time_bucket(width, ts) for the fixed
// widths the OHLC routes fold by.
func timeBucket(width time.Duration, ts time.Time) time.Time {
	return timescaleBucketOrigin.Add(ts.Sub(timescaleBucketOrigin).Truncate(width))
}

// foldWidth parses the `N unit` INTERVAL literal a folded route
// carries, so the expectation is built from the same text the query
// interpolates.
func foldWidth(t *testing.T, lit string) time.Duration {
	t.Helper()
	f := strings.Fields(lit)
	if len(f) != 2 {
		t.Fatalf("fold literal %q is not `N unit`", lit)
	}
	n, err := strconv.Atoi(f[0])
	if err != nil {
		t.Fatalf("fold literal %q: %v", lit, err)
	}
	units := map[string]time.Duration{
		"minutes": time.Minute, "hours": time.Hour,
		"days": 24 * time.Hour, "weeks": 7 * 24 * time.Hour,
	}
	u, ok := units[f[1]]
	if !ok {
		t.Fatalf("fold literal %q: unknown unit", lit)
	}
	return time.Duration(n) * u
}

// TestOHLCFoldIntervals_ExecuteAgainstTheirSourceView runs every folded
// /v1/ohlc route ([timescale.OHLCRoutes]) through OHLCSeriesReBucketed
// against a real TimescaleDB and checks the fold's arithmetic and
// alignment against trades placed by hand. 2h, 12h, 3d and 2w never
// had no coverage before this test: their literals
// were routed but not allow-listed, so the store refused them before
// composing SQL and the API answered 500. A fold
// literal that Postgres would not accept, or a bucket that does not
// land where the API promises (`t` aligned to UTC interval
// boundaries, weeks on Monday), fails here rather than at the edge.
func TestOHLCFoldIntervals_ExecuteAgainstTheirSourceView(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, _ := canonical.NewPair(canonical.NativeAsset(), usdc)

	// The window is one 2-week bucket, anchored on a 2w boundary at
	// least six weeks back so the widest fold's bucket is closed under
	// the ADR-0015 guard (bucket + width <= now()). Every narrower
	// fold's grid nests inside it.
	const window = 14 * 24 * time.Hour
	t0 := timeBucket(window, time.Now().UTC().Add(-6*7*24*time.Hour))
	if t0.Weekday() != time.Monday {
		t.Fatalf("2w anchor %v is a %v, want Monday — origin arithmetic is wrong", t0, t0.Weekday())
	}

	// Four trades, all in the requested orientation, at prices that
	// make open/high/low/close distinguishable per fold. Base is a
	// constant 1e6 so volumes are exact multiples.
	type seed struct {
		at    time.Duration
		quote int64 // price = quote / 1e6
	}
	seeds := []seed{
		{1 * time.Hour, 1_000_000},                // 1.0
		{1*time.Hour + 30*time.Minute, 1_500_000}, // 1.5 — same 2h bucket as the first
		{3*24*time.Hour + 5*time.Hour, 2_000_000}, // 2.0 — day 4, first 12h half
		{10*24*time.Hour + 13*time.Hour, 500_000}, // 0.5 — week 2, second 12h half
	}
	for i, s := range seeds {
		if err := store.InsertTrade(ctx, mkAPITrade(i+1, t0.Add(s.at), pair, 1_000_000, s.quote)); err != nil {
			t.Fatalf("InsertTrade %d: %v", i, err)
		}
	}
	// Materialise every view a folded route reads from. Each prices_<g>
	// aggregates the trades hypertable directly (migration 0147), so
	// order does not matter.
	sources := map[timescale.HistoryGranularity]bool{}
	for _, r := range timescale.OHLCRoutes {
		if r.Folded() {
			sources[r.Source] = true
		}
	}
	for g := range sources {
		if _, err := store.DB().ExecContext(ctx,
			"CALL refresh_continuous_aggregate('prices_"+string(g)+"', NULL, NULL)"); err != nil {
			t.Fatalf("refresh prices_%s: %v", g, err)
		}
	}

	executed := 0
	for _, route := range timescale.OHLCRoutes {
		if !route.Folded() {
			continue
		}
		executed++
		t.Run(route.Interval, func(t *testing.T) {
			width := foldWidth(t, route.Fold)
			bars, err := store.OHLCSeriesReBucketed(ctx, pair, route.Source, route.Fold, t0, t0.Add(window), 0)
			if err != nil {
				t.Fatalf("OHLCSeriesReBucketed(%s, %q): %v", route.Source, route.Fold, err)
			}

			// Expectation: group the seeds by the fold's bucket, in
			// time order, and roll each group up the way the query
			// documents (open = first, close = last, high/low = extremes,
			// volumes = sums, n = count).
			type expect struct {
				open, high, low, close float64
				quote, n               int64
			}
			var order []time.Time
			want := map[time.Time]*expect{}
			for _, s := range seeds {
				b := timeBucket(width, t0.Add(s.at))
				px := float64(s.quote) / 1e6
				e, ok := want[b]
				if !ok {
					e = &expect{open: px, high: px, low: px}
					want[b] = e
					order = append(order, b)
				}
				e.close = px
				e.high = max(e.high, px)
				e.low = min(e.low, px)
				e.quote += s.quote
				e.n++
			}
			if len(bars) != len(order) {
				t.Fatalf("got %d bars, want %d (buckets %v): %+v", len(bars), len(order), order, bars)
			}
			for i, bar := range bars {
				b := order[i]
				e := want[b]
				if !bar.Bucket.Equal(b) {
					t.Errorf("bar[%d].Bucket = %v, want %v (time_bucket('%s') of the seeds)", i, bar.Bucket, b, route.Fold)
				}
				if !bar.Bucket.Equal(timeBucket(width, bar.Bucket)) {
					t.Errorf("bar[%d].Bucket = %v is not aligned to a %v grid from the Timescale origin", i, bar.Bucket, width)
				}
				if route.Source == timescale.Granularity1w && bar.Bucket.Weekday() != time.Monday {
					t.Errorf("bar[%d].Bucket = %v is a %v; a fold of prices_1w must start on its Monday", i, bar.Bucket, bar.Bucket.Weekday())
				}
				if bar.TradeCount != e.n {
					t.Errorf("bar[%d].TradeCount = %d, want %d", i, bar.TradeCount, e.n)
				}
				for name, got := range map[string]struct{ have, want float64 }{
					"Open":        {mustFloat(t, bar.Open), e.open},
					"High":        {mustFloat(t, bar.High), e.high},
					"Low":         {mustFloat(t, bar.Low), e.low},
					"Close":       {mustFloat(t, bar.Close), e.close},
					"BaseVolume":  {mustFloat(t, bar.BaseVolume), float64(e.n) * 1e6},
					"QuoteVolume": {mustFloat(t, bar.QuoteVolume), float64(e.quote)},
				} {
					if diff := got.have - got.want; diff > 1e-6 || diff < -1e-6 {
						t.Errorf("bar[%d].%s = %v, want %v", i, name, got.have, got.want)
					}
				}
			}
		})
	}
	if executed < 7 {
		t.Fatalf("executed %d folded routes, want at least the 7 the API serves", executed)
	}
	t.Logf("executed %d of %d folded routes against TimescaleDB", executed, executed)
}

// TestOHLCRebuiltVolumeIsTheIntegerSum executes both OHLC readers over a
// bucket whose volume legs have to be rebuilt from vwap·volume in both
// stored directions, with a vwap (1/3) that NUMERIC division cannot
// represent exactly. PostgreSQL stores 1e6/3e6 as 0.33333333333333333333,
// so the bare product serves 999999.99999999999999000000 where the trades
// sum to exactly 1000000. Volumes are integer smallest-unit sums; the
// served value must be the integer.
func TestOHLCRebuiltVolumeIsTheIntegerSum(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := canonical.NewPair(canonical.NativeAsset(), usdc)
	if err != nil {
		t.Fatal(err)
	}
	flipped, err := canonical.NewPair(usdc, canonical.NativeAsset())
	if err != nil {
		t.Fatal(err)
	}

	const fold = 4 * time.Hour
	t0 := timeBucket(fold, time.Now().UTC().Add(-30*time.Hour))
	// Requested direction: 3e6 XLM for 1e6 USDC (quote leg rebuilt).
	// Flipped direction: 3e6 USDC for 1e6 XLM (base leg rebuilt).
	// Either way the served bar is 4e6 XLM against 4e6 USDC.
	for i, tr := range []canonical.Trade{
		mkAPITrade(1, t0.Add(10*time.Minute), pair, 3_000_000, 1_000_000),
		mkAPITrade(2, t0.Add(20*time.Minute), flipped, 3_000_000, 1_000_000),
	} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %d: %v", i, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1h', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1h: %v", err)
	}

	native, err := store.OHLCSeries(ctx, pair, timescale.Granularity1h, t0, t0.Add(fold), 10)
	if err != nil {
		t.Fatalf("OHLCSeries: %v", err)
	}
	folded, err := store.OHLCSeriesReBucketed(ctx, pair, timescale.Granularity1h, "4 hours", t0, t0.Add(fold), 10)
	if err != nil {
		t.Fatalf("OHLCSeriesReBucketed: %v", err)
	}
	for name, bars := range map[string][]timescale.OHLCBar{"OHLCSeries": native, "OHLCSeriesReBucketed": folded} {
		if len(bars) != 1 {
			t.Fatalf("%s returned %d bars, want 1", name, len(bars))
		}
		if got := bars[0].BaseVolume; got != "4000000" {
			t.Errorf("%s base_volume = %q, want exactly 4000000 (3e6 stored + 1e6 rebuilt from the flipped row)", name, got)
		}
		if got := bars[0].QuoteVolume; got != "4000000" {
			t.Errorf("%s quote_volume = %q, want exactly 4000000 (1e6 rebuilt + 3e6 stored on the flipped row)", name, got)
		}
	}
}

// TestOHLCSeries_LimitKeepsNewestBucketsInWideWindow pins that an
// explicit window wider than `limit` intervals must serve the NEWEST
// `limit` buckets, not the oldest. The previous query ended `ORDER BY
// bucket ASC` and then applied `LIMIT`, so a wide window silently
// returned a stale slice starting at `from` and never reaching `to` —
// exactly backwards from what a chart client sizing `limit` down for a
// wide window expects. Runs against a real TimescaleDB because the
// defect is in the SQL's ORDER BY/LIMIT interaction, not reproducible
// against a scripted driver that ignores the query text.
func TestOHLCSeries_LimitKeepsNewestBucketsInWideWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := canonical.NewPair(canonical.NativeAsset(), usdc)
	if err != nil {
		t.Fatal(err)
	}

	// Five distinct, closed 1-minute buckets, each with exactly one
	// trade so open == high == low == close == that bucket's price —
	// the buckets are trivially distinguishable by value. t0 sits 20
	// minutes back so every bucket in [t0, t0+5m) is closed under the
	// ADR-0015 `bucket + interval <= now()` guard.
	t0 := time.Now().UTC().Add(-20 * time.Minute).Truncate(time.Minute)
	const nBuckets = 5
	for i := 0; i < nBuckets; i++ {
		price := int64(i + 1) // 1, 2, 3, 4, 5
		if err := store.InsertTrade(ctx, mkAPITrade(
			i+1, t0.Add(time.Duration(i)*time.Minute), pair,
			1_000_000, price*1_000_000,
		)); err != nil {
			t.Fatalf("InsertTrade %d: %v", i, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	const limit = 2
	bars, err := store.OHLCSeries(ctx, pair, timescale.Granularity1m,
		t0, t0.Add(nBuckets*time.Minute), limit)
	if err != nil {
		t.Fatalf("OHLCSeries: %v", err)
	}
	if len(bars) != limit {
		t.Fatalf("len(bars) = %d, want %d", len(bars), limit)
	}

	// Ascending order is the documented contract regardless of which
	// end got cut.
	if !bars[0].Bucket.Before(bars[1].Bucket) {
		t.Fatalf("bars not ascending: %v then %v", bars[0].Bucket, bars[1].Bucket)
	}

	wantBuckets := []time.Time{t0.Add(3 * time.Minute), t0.Add(4 * time.Minute)}
	wantOpen := []float64{4, 5}
	for i, want := range wantBuckets {
		if !bars[i].Bucket.Equal(want) {
			t.Errorf("bars[%d].Bucket = %v, want %v (the NEWEST %d buckets) — "+
				"got the OLDEST %d instead, the RLT-453 regression",
				i, bars[i].Bucket, want, limit, limit)
		}
		if got := mustFloat(t, bars[i].Open); got != wantOpen[i] {
			t.Errorf("bars[%d].Open = %v, want %v", i, got, wantOpen[i])
		}
	}
}

// TestOHLCSeriesReBucketed_LimitKeepsNewestOutBucketsInWideWindow is
// the folded-interval sibling of
// TestOHLCSeries_LimitKeepsNewestBucketsInWideWindow: OHLCSeriesReBucketed
// carries the identical ORDER BY/LIMIT defect, one layer up —
// re-bucketing prices_1h into 4-hour out-buckets.
func TestOHLCSeriesReBucketed_LimitKeepsNewestOutBucketsInWideWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := canonical.NewPair(canonical.NativeAsset(), usdc)
	if err != nil {
		t.Fatal(err)
	}

	const fold = 4 * time.Hour
	// Anchored 30h back (comfortably more than the 20h the 5 out-buckets
	// below span) so every out-bucket closes under the ADR-0015 guard.
	t0 := timeBucket(fold, time.Now().UTC().Add(-30*time.Hour))

	const nBuckets = 5
	for i := 0; i < nBuckets; i++ {
		price := int64(i + 1) // 1, 2, 3, 4, 5
		if err := store.InsertTrade(ctx, mkAPITrade(
			i+1, t0.Add(time.Duration(i)*fold), pair,
			1_000_000, price*1_000_000,
		)); err != nil {
			t.Fatalf("InsertTrade %d: %v", i, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1h', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1h: %v", err)
	}

	const limit = 2
	bars, err := store.OHLCSeriesReBucketed(ctx, pair, timescale.Granularity1h, "4 hours",
		t0, t0.Add(nBuckets*fold), limit)
	if err != nil {
		t.Fatalf("OHLCSeriesReBucketed: %v", err)
	}
	if len(bars) != limit {
		t.Fatalf("len(bars) = %d, want %d", len(bars), limit)
	}
	if !bars[0].Bucket.Before(bars[1].Bucket) {
		t.Fatalf("bars not ascending: %v then %v", bars[0].Bucket, bars[1].Bucket)
	}

	wantBuckets := []time.Time{t0.Add(3 * fold), t0.Add(4 * fold)}
	wantOpen := []float64{4, 5}
	for i, want := range wantBuckets {
		if !bars[i].Bucket.Equal(want) {
			t.Errorf("bars[%d].Bucket = %v, want %v (the NEWEST %d out-buckets) — "+
				"got the OLDEST %d instead, the RLT-453 regression",
				i, bars[i].Bucket, want, limit, limit)
		}
		if got := mustFloat(t, bars[i].Open); got != wantOpen[i] {
			t.Errorf("bars[%d].Open = %v, want %v", i, got, wantOpen[i])
		}
	}
}

// Migration 0166 (migrations/0166_twap_notional_floor.up.sql) puts 0115's
// $0.01 notional floor on the TWAP chain. TWAP is equal-weight twice over —
// per trade in prices_1m, per minute in twap_1h / twap_1d — so before 0166 a
// 2-stroop crumb at an absurd price counted as much as a $1,000 fill, and a
// minute holding only that crumb counted as much as a minute of real trading.
//
// Every scenario stores a real fill at 5 and dust at 500 (or 0.5), so the
// unfloored answers (252.5, 502.5) cannot be mistaken for the floored 5.

const (
	twapFloorIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

	real5    = "50000000" // quote for base 10000000 → price 5
	realBase = "10000000"
	realUSD  = "1000"
	dustUSD  = "0.0000003"
)

// The operator re-materialisation recipe from 0166's header, run verbatim:
// windowed, forced, prices_1m before the TWAP views built on it.
var twapFloorRefreshRecipe = []string{
	"CALL refresh_continuous_aggregate('prices_1m', now() - INTERVAL '7 days', now(), force => true)",
	"CALL refresh_continuous_aggregate('twap_1h', now() - INTERVAL '7 days', now(), force => true)",
	"CALL refresh_continuous_aggregate('twap_1d', now() - INTERVAL '7 days', now(), force => true)",
}

func TestTWAPNotionalFloor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// One hour into a fully closed UTC day inside the recipe's 7-day window,
	// so the minute, hour and day buckets all materialise and serve.
	t0 := time.Now().UTC().Add(-48 * time.Hour).Truncate(24 * time.Hour).Add(time.Hour)
	pair := func(code string) ohlcDustPair {
		return ohlcDustPair{base: code + "-" + twapFloorIssuer, quote: "native"}
	}

	// Mixed minute: a real fill and a crumb in the SAME minute.
	mixed := pair("TWMX")
	seed(t, db, ctx, mixed, []seedTrade{
		{off: 10 * time.Second, base: realBase, quote: real5, usd: realUSD},
		{off: 20 * time.Second, base: "2", quote: "1000", usd: dustUSD}, // 500
	}, t0)

	// Thin hour: one real minute, one minute holding only a crumb.
	thin := pair("TWTH")
	seed(t, db, ctx, thin, []seedTrade{
		{off: 10 * time.Second, base: realBase, quote: real5, usd: realUSD},
		{off: 2 * time.Minute, base: "2", quote: "1000", usd: dustUSD}, // 500
	}, t0)

	// All-dust hour: nothing clears the floor, so the fallback must report.
	allDust := pair("TWDU")
	seed(t, db, ctx, allDust, []seedTrade{
		{off: 10 * time.Second, base: "2", quote: "1000", usd: dustUSD}, // 500
		{off: 2 * time.Minute, base: "2", quote: "1", usd: "0.0000001"}, // 0.5
	}, t0)

	// Unpriced pair (usd_volume NULL): behaviour unchanged.
	unpriced := pair("TWNU")
	seed(t, db, ctx, unpriced, []seedTrade{
		{off: 10 * time.Second, base: realBase, quote: real5, usd: ""},
		{off: 2 * time.Minute, base: "2", quote: "1000", usd: ""},
	}, t0)

	// Two stored directions: a real forward minute, and a dust-only minute
	// stored REVERSE (native/TWDM at 0.001, i.e. 1000 once oriented).
	merged := pair("TWDM")
	seed(t, db, ctx, merged, []seedTrade{
		{off: 10 * time.Second, base: realBase, quote: real5, usd: realUSD},
	}, t0)
	seed(t, db, ctx, ohlcDustPair{base: "native", quote: merged.base}, []seedTrade{
		{off: 2 * time.Minute, base: realBase, quote: "10000", usd: "0.000001"},
	}, t0)

	for _, stmt := range twapFloorRefreshRecipe {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	t.Run("prices_1m twap ignores the crumb in a mixed minute", func(t *testing.T) {
		assertNumeric(t, "prices_1m.twap", readMinuteTWAP(t, db, ctx, mixed, t0), "5")
		var notional, trades int64
		if err := db.QueryRowContext(ctx, `
			SELECT notional_trade_count, trade_count FROM prices_1m
			 WHERE base_asset = $1 AND quote_asset = $2 AND bucket = $3`,
			mixed.base, mixed.quote, t0).Scan(&notional, &trades); err != nil {
			t.Fatalf("read prices_1m counts: %v", err)
		}
		if notional != 1 || trades != 2 {
			t.Errorf("notional_trade_count, trade_count = %d, %d; want 1, 2 — the floor "+
				"filters the twap only, never the trade count", notional, trades)
		}
	})

	for _, view := range []string{"twap_1h", "twap_1d"} {
		t.Run(view+" drops the dust-only minute", func(t *testing.T) {
			got := readTWAPRow(t, db, ctx, view, thin, t0)
			assertNumeric(t, view+".twap", got.twap, "5")
			if got.sampleCount != 1 {
				t.Errorf("sample_count = %d, want 1 — it must count exactly the minutes the twap averaged",
					got.sampleCount)
			}
			if n := readNotionalSamples(t, db, ctx, view, thin, t0); n != 1 {
				t.Errorf("notional_sample_count = %d, want 1", n)
			}
		})
		t.Run(view+" all-dust falls back rather than going NULL", func(t *testing.T) {
			got := readTWAPRow(t, db, ctx, view, allDust, t0)
			assertNumeric(t, view+".twap", got.twap, "250.25")
			if got.sampleCount != 2 {
				t.Errorf("sample_count = %d, want 2", got.sampleCount)
			}
			if n := readNotionalSamples(t, db, ctx, view, allDust, t0); n != 0 {
				t.Errorf("notional_sample_count = %d, want 0", n)
			}
		})
		t.Run(view+" unpriced pair is unchanged", func(t *testing.T) {
			got := readTWAPRow(t, db, ctx, view, unpriced, t0)
			assertNumeric(t, view+".twap", got.twap, "252.5")
			if got.sampleCount != 2 {
				t.Errorf("sample_count = %d, want 2", got.sampleCount)
			}
		})
	}

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	asset, err := canonical.NewClassicAsset("TWDM", twapFloorIssuer)
	if err != nil {
		t.Fatal(err)
	}
	p, err := canonical.NewPair(asset, canonical.NativeAsset())
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []timescale.HistoryGranularity{timescale.Granularity1h, timescale.Granularity1d} {
		t.Run("served "+string(g)+" TWAP drops the fallback direction", func(t *testing.T) {
			pts, err := store.TWAPPointsInRange(ctx, p, g, t0.Add(-2*time.Hour), t0.Add(24*time.Hour), 0)
			if err != nil {
				t.Fatalf("TWAPPointsInRange: %v", err)
			}
			if len(pts) != 1 {
				t.Fatalf("got %d points, want 1", len(pts))
			}
			assertNumeric(t, "served twap", sql.NullString{String: pts[0].VWAP, Valid: true}, "5")
		})
	}
}

type twapRow struct {
	twap        sql.NullString
	sampleCount int64
}

func readMinuteTWAP(t *testing.T, db *sql.DB, ctx context.Context, p ohlcDustPair, bucket time.Time) sql.NullString {
	t.Helper()
	var got sql.NullString
	if err := db.QueryRowContext(ctx, `
		SELECT twap::text FROM prices_1m
		 WHERE base_asset = $1 AND quote_asset = $2 AND bucket = $3`,
		p.base, p.quote, bucket).Scan(&got); err != nil {
		t.Fatalf("read prices_1m.twap for %s: %v", p.base, err)
	}
	return got
}

func readTWAPRow(t *testing.T, db *sql.DB, ctx context.Context, view string, p ohlcDustPair, t0 time.Time) twapRow {
	t.Helper()
	var got twapRow
	if err := db.QueryRowContext(ctx, `
		SELECT twap::text, sample_count FROM `+view+`
		 WHERE base_asset = $1 AND quote_asset = $2 AND bucket <= $3
		 ORDER BY bucket DESC LIMIT 1`,
		p.base, p.quote, t0).Scan(&got.twap, &got.sampleCount); err != nil {
		t.Fatalf("read %s for %s: %v", view, p.base, err)
	}
	return got
}

func readNotionalSamples(t *testing.T, db *sql.DB, ctx context.Context, view string, p ohlcDustPair, t0 time.Time) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRowContext(ctx, `
		SELECT notional_sample_count FROM `+view+`
		 WHERE base_asset = $1 AND quote_asset = $2 AND bucket <= $3
		 ORDER BY bucket DESC LIMIT 1`,
		p.base, p.quote, t0).Scan(&n); err != nil {
		t.Fatalf("read %s.notional_sample_count for %s: %v", view, p.base, err)
	}
	return n
}
