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
// USD price used to pick an older or one-sided observation over a newer
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
// rollup (asset_price_snapshot) and the detail row (GetAssetBySlug). It
// used to pick the direct-USD arm whenever that arm had ANY row in 7
// days, and inside each arm to prefer the stored base-side direction
// over a fresher flipped one; the direct arm never read the flipped
// direction at all. Before the fix the five rows read 0.10, 1.00, 1.00,
// 0.40 and 1.00, and flippedXLM's change_24h_pct read 0.00.
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
