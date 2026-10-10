//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/canonical/discovery"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Real, checksum-valid G-strkeys (harvested from the codebase) — the asset
// issuers MUST parse so canonical.ParseAsset extracts the issuer the SQL
// regex derives, or the issuer-side signal would diverge between the
// per-asset read and the rollup.
const (
	audIssuer  = "GA2PZMWITS45LSQF7KWN7SH2YREIUHLC4SNNIYX5D2LTNEML74CANJMO"
	auddIssuer = "GA2QXW7YFAIR35LGKM2TDCQQZFR33XJCWF4N6SMRLOKX3HL76JKKPA62"
	audrIssuer = "GA2XUFPBYK3GQM7RLYXP4QJVSYWADSVW6DWSFPCPTRNLJQNXR5PIGALA"
	goodIssuer = "GA2XZLXNLAL26VBCA2OESAIMXTRH5GXKLHYZMDGNCR2SYS5QZWWNBLCK"
	washIssuer = "GA3GJGKCUKPOPL6NYPMSBK7LMFYNW7SJMAJ7ZGWR3KGSHJWJHQRQZA3L"
	realIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

	// Three more issuers so the listing tests can seed rows that TIE on
	// the sort keys. Distinct issuers (not one issuer with three codes)
	// because the tie-break is on the full asset_id, and sharing an
	// issuer would leave the codes as the only varying part — a weaker
	// exercise of the keyset walk than the real long tail.
	tieIssuerA = "GBXF3MBQLVQIVLY72WFA5F6RSI3GHK365KBKPBRAHSXVLRE4KY4GDJXP"
	tieIssuerB = "GDH6SHBRFUIPGMSBALDYRMWQA4XCH7VMZUFZ3H3YAHTF2HQFNW2KKV23"
	tieIssuerC = "GDCQNKPZUBHITG6V3S25OS53K2BD4OGZHORUGF4KLEQ7BXV36YFLZ36T"
)

func mustClassicID(t *testing.T, code, issuer string) string {
	t.Helper()
	a, err := c.NewClassicAsset(code, issuer)
	if err != nil {
		t.Fatalf("NewClassicAsset(%s,%s): %v", code, issuer, err)
	}
	return a.String()
}

// charAccount is a checksum-valid G-strkey for fixture account n: the
// account-structure signals only treat a G-strkey maker as an account.
func charAccount(n byte) string {
	raw := [32]byte{0: 0xA5, 31: n}
	return strkey.MustEncode(strkey.VersionByteAccountID, raw[:])
}

// charPoolMaker is trades.maker exactly as the sdex decoder writes it for a
// classic liquidity-pool fill: the hex pool id, not an account.
func charPoolMaker(n byte) string {
	return fmt.Sprintf("%x", xdr.PoolId([32]byte{0: 0x5A, 31: n}))
}

// insertCharTrade raw-inserts one priced trade with an explicit
// maker/taker/usd_volume — the account-structure inputs the per-asset read
// and the rollup both roll over `trades`. Raw insert (not InsertTrade) gives
// exact control over the three canonical scenarios without depending on the
// usd_volume derivation tiers.
func insertCharTrade(t *testing.T, ctx context.Context, db *sql.DB, nonce int, ts time.Time, base, quote, maker, taker string, usdVol float64) {
	t.Helper()
	const q = `INSERT INTO trades
	  (source, ledger, tx_hash, op_index, ts, base_asset, quote_asset,
	   base_amount, quote_amount, usd_volume, maker, taker)
	  VALUES ('sdex', $1, $2, 0, $3, $4, $5, 1, 1, $6, $7, $8)`
	txHash := fmt.Sprintf("%064x", nonce)
	if _, err := db.ExecContext(ctx, q,
		50_000_000+nonce, txHash, ts, base, quote, usdVol, maker, taker,
	); err != nil {
		t.Fatalf("insert trade nonce=%d: %v", nonce, err)
	}
}

// TestAssetVolumeCharacterRollup_OracleMatchesPerAsset is the verification
// oracle: RefreshAssetVolumeCharacter must produce, per asset, the SAME
// signals + character the existing per-asset AssetVolumeCharacter returns —
// the rollup only MOVES the compute off the request path, it must not change
// the VALUE. Covers the scam-AUD volume-painting wash (concentrated), an
// issuer wrap-corridor (operational), and a healthy multi-account asset
// (market).
func TestAssetVolumeCharacterRollup_OracleMatchesPerAsset(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Force the XLM-only alias baseline so the rollup's canonical fold is
	// deterministic regardless of any registry a sibling test installed.
	c.InstallAliasRegistry(nil)

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()

	audID := mustClassicID(t, "AUD", audIssuer)     // market-styled wash
	auddID := mustClassicID(t, "AUDD", auddIssuer)  // wrap corridor
	audrID := mustClassicID(t, "AUDR", audrIssuer)  // its non-market sibling
	goodID := mustClassicID(t, "GOODX", goodIssuer) // healthy market asset

	// Trades close within the 14d window and are bounded to ~now (2026),
	// well below any tip other integration tests reserve.
	ts := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Second)
	nonce := 0
	next := func() int { nonce++; return nonce }

	// (1) Scam-AUD volume-painting wash: 9/10 of the AUD/USD volume is the
	// single (account 1, issuer) pair with the issuer as taker; market-styled
	// (fiat:USD counterpart) → concentrated.
	for i := 0; i < 9; i++ {
		insertCharTrade(t, ctx, db, next(), ts, audID, "fiat:USD", charAccount(1), audIssuer, 1000)
	}
	insertCharTrade(t, ctx, db, next(), ts, audID, "fiat:USD", charAccount(2), charAccount(3), 100)

	// (2) AUDD wrap/redeem corridor: issuer-side (maker == issuer) on a
	// NON-market-styled sibling pair (AUDR) → operational.
	for i := 0; i < 5; i++ {
		insertCharTrade(t, ctx, db, next(), ts, auddID, audrID, auddIssuer, charAccount(4), 2000)
	}

	// (3) Healthy market asset: six distinct account pairs on fiat:USD, no
	// pair dominant, issuer uninvolved → market.
	marketPairs := [][2]string{
		{charAccount(5), charAccount(6)},
		{charAccount(7), charAccount(8)},
		{charAccount(9), charAccount(10)},
		{charAccount(11), charAccount(12)},
		{charAccount(13), charAccount(14)},
		{charAccount(15), charAccount(16)},
	}
	for _, p := range marketPairs {
		insertCharTrade(t, ctx, db, next(), ts, goodID, "fiat:USD", p[0], p[1], 1000)
	}

	if err := store.RefreshAssetVolumeCharacter(ctx); err != nil {
		t.Fatalf("RefreshAssetVolumeCharacter: %v", err)
	}

	cases := []struct {
		name     string
		assetID  string
		wantChar string
	}{
		{"scam_AUD_concentrated", audID, timescale.VolumeCharacterConcentrated},
		{"AUDD_operational_corridor", auddID, timescale.VolumeCharacterOperational},
		{"GOODX_market", goodID, timescale.VolumeCharacterMarket},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			per, err := store.AssetVolumeCharacter(ctx, tc.assetID)
			if err != nil {
				t.Fatalf("AssetVolumeCharacter(%s): %v", tc.assetID, err)
			}
			roll, found, err := store.AssetVolumeCharacterRollup(ctx, tc.assetID)
			if err != nil {
				t.Fatalf("AssetVolumeCharacterRollup(%s): %v", tc.assetID, err)
			}
			if !found {
				t.Fatalf("rollup has no row for %s", tc.assetID)
			}

			// Sanity: the derived character is the census verdict.
			if per.Character != tc.wantChar {
				t.Errorf("per-asset character = %q, want %q", per.Character, tc.wantChar)
			}

			// The oracle: the rollup EQUALS the per-asset read. Shares are
			// compared to FULL precision (both are round4 over the SAME
			// double sums), character/counts exactly, volume as the exact
			// NUMERIC text.
			if roll.Character != per.Character {
				t.Errorf("character: rollup=%q per-asset=%q", roll.Character, per.Character)
			}
			if roll.TopAccountPairVolShare != per.TopAccountPairVolShare {
				t.Errorf("top_account_pair_vol_share: rollup=%v per-asset=%v", roll.TopAccountPairVolShare, per.TopAccountPairVolShare)
			}
			if roll.SelfCrossShare != per.SelfCrossShare {
				t.Errorf("self_cross_share: rollup=%v per-asset=%v", roll.SelfCrossShare, per.SelfCrossShare)
			}
			if roll.IssuerSideShare != per.IssuerSideShare {
				t.Errorf("issuer_side_share: rollup=%v per-asset=%v", roll.IssuerSideShare, per.IssuerSideShare)
			}
			if roll.MarketStyledShare != per.MarketStyledShare {
				t.Errorf("market_styled_share: rollup=%v per-asset=%v", roll.MarketStyledShare, per.MarketStyledShare)
			}
			if roll.IsMarketStyled != per.IsMarketStyled {
				t.Errorf("is_market_styled: rollup=%v per-asset=%v", roll.IsMarketStyled, per.IsMarketStyled)
			}
			if roll.DistinctMakers != per.DistinctMakers {
				t.Errorf("distinct_makers: rollup=%d per-asset=%d", roll.DistinctMakers, per.DistinctMakers)
			}
			if roll.DistinctTakers != per.DistinctTakers {
				t.Errorf("distinct_takers: rollup=%d per-asset=%d", roll.DistinctTakers, per.DistinctTakers)
			}
			if roll.WindowDays != per.WindowDays {
				t.Errorf("window_days: rollup=%d per-asset=%d", roll.WindowDays, per.WindowDays)
			}
			if roll.VolumeUSD != per.VolumeUSD {
				t.Errorf("volume_usd (exact NUMERIC): rollup=%s per-asset=%s", roll.VolumeUSD, per.VolumeUSD)
			}
		})
	}

	// Idempotent: a second refresh leaves the target rows unchanged.
	if err := store.RefreshAssetVolumeCharacter(ctx); err != nil {
		t.Fatalf("RefreshAssetVolumeCharacter (2nd): %v", err)
	}
	for _, id := range []string{audID, auddID, goodID} {
		if _, found, err := store.AssetVolumeCharacterRollup(ctx, id); err != nil || !found {
			t.Errorf("row for %s vanished across idempotent refresh (found=%v err=%v)", id, found, err)
		}
	}

	// Prune: a stale sentinel is dropped by the next refresh.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO asset_volume_character
		 (asset_id, window_days, volume_usd, distinct_makers, distinct_takers,
		  top_account_pair_vol_share, self_cross_share, issuer_side_share,
		  market_styled_share, is_market_styled, character, computed_at)
		 VALUES ('ZZZ-STALE', 14, 1, 0, 0, 0, 0, 0, 0, false, 'market', now() - interval '1 hour')`,
	); err != nil {
		t.Fatalf("insert stale sentinel: %v", err)
	}
	if err := store.RefreshAssetVolumeCharacter(ctx); err != nil {
		t.Fatalf("RefreshAssetVolumeCharacter (3rd): %v", err)
	}
	var stale int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM asset_volume_character WHERE asset_id = 'ZZZ-STALE'`).Scan(&stale); err != nil {
		t.Fatalf("count sentinel: %v", err)
	}
	if stale != 0 {
		t.Errorf("stale sentinel survived the prune")
	}
}

// TestAssetVolumeCharacter_ClassicPoolMakerIsNotAnAccount: a classic
// liquidity-pool fill carries the hex pool id in trades.maker. One account
// round-tripping an asset through two classic pools must read as ONE
// concentrated actor, and neither pool may count as a distinct maker — in
// the per-asset read and the rollup alike. Unfixed, the two (pool, taker)
// pairs split the volume into 0.495 halves (character market) and the two
// pools inflate distinct_makers to 3.
func TestAssetVolumeCharacter_ClassicPoolMakerIsNotAnAccount(t *testing.T) {
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
	db := store.DB()

	poolxID := mustClassicID(t, "POOLX", tieIssuerA)
	ts := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Second)
	nonce := 1000
	next := func() int { nonce++; return nonce }

	trader := charAccount(40)
	for i := 0; i < 10; i++ {
		insertCharTrade(t, ctx, db, next(), ts, poolxID, "native", charPoolMaker(byte(1+i%2)), trader, 1000)
	}
	insertCharTrade(t, ctx, db, next(), ts, poolxID, "native", charAccount(41), charAccount(42), 100)

	if err := store.RefreshAssetVolumeCharacter(ctx); err != nil {
		t.Fatalf("RefreshAssetVolumeCharacter: %v", err)
	}
	per, err := store.AssetVolumeCharacter(ctx, poolxID)
	if err != nil {
		t.Fatalf("AssetVolumeCharacter: %v", err)
	}
	roll, found, err := store.AssetVolumeCharacterRollup(ctx, poolxID)
	if err != nil || !found {
		t.Fatalf("AssetVolumeCharacterRollup: found=%v err=%v", found, err)
	}
	for name, got := range map[string]timescale.AssetVolumeCharacter{"per-asset": per, "rollup": roll} {
		if got.DistinctMakers != 1 {
			t.Errorf("%s distinct_makers = %d, want 1 (the pools are not accounts)", name, got.DistinctMakers)
		}
		if got.DistinctTakers != 2 {
			t.Errorf("%s distinct_takers = %d, want 2", name, got.DistinctTakers)
		}
		if got.TopAccountPairVolShare != 0.9901 {
			t.Errorf("%s top_account_pair_vol_share = %v, want 0.9901 (10000/10100 on the lone taker)", name, got.TopAccountPairVolShare)
		}
		if got.SelfCrossShare != 0 {
			t.Errorf("%s self_cross_share = %v, want 0", name, got.SelfCrossShare)
		}
		if got.Character != timescale.VolumeCharacterConcentrated {
			t.Errorf("%s character = %q, want %q", name, got.Character, timescale.VolumeCharacterConcentrated)
		}
	}
}

// TestAssetVolumeCharacter_MakerlessAMMKeyedOnTaker: a Soroban AMM swap
// stores no maker. One account swapping an asset back and forth through an
// AMM must read as ONE concentrated actor in both the per-asset read and the
// rollup, and a maker-less row must not disturb an order-book asset's pairs.
func TestAssetVolumeCharacter_MakerlessAMMKeyedOnTaker(t *testing.T) {
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
	db := store.DB()

	ammID := mustClassicID(t, "AMMX", tieIssuerB)
	mixID := mustClassicID(t, "MIXX", tieIssuerC)
	ts := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Second)
	nonce := 2000
	insertAMM := func(base, taker string, usdVol float64) {
		nonce++
		if _, err := db.ExecContext(ctx, `INSERT INTO trades
		  (source, ledger, tx_hash, op_index, ts, base_asset, quote_asset,
		   base_amount, quote_amount, usd_volume, maker, taker)
		  VALUES ('soroswap', $1, $2, 0, $3, $4, 'native', 1, 1, $5, NULL, $6)`,
			50_000_000+nonce, fmt.Sprintf("%064x", nonce), ts, base, usdVol, taker,
		); err != nil {
			t.Fatalf("insert amm trade nonce=%d: %v", nonce, err)
		}
	}

	trader := charAccount(50)
	for i := 0; i < 10; i++ {
		insertAMM(ammID, trader, 1000)
	}
	insertAMM(ammID, charAccount(51), 100)

	// Mixed: six equal order-book pairs plus one small maker-less AMM swap.
	for i := byte(0); i < 6; i++ {
		nonce++
		insertCharTrade(t, ctx, db, nonce, ts, mixID, "native", charAccount(60+2*i), charAccount(61+2*i), 1000)
	}
	insertAMM(mixID, charAccount(80), 500)

	if err := store.RefreshAssetVolumeCharacter(ctx); err != nil {
		t.Fatalf("RefreshAssetVolumeCharacter: %v", err)
	}
	cases := []struct {
		assetID  string
		topShare float64
		char     string
	}{
		{ammID, 0.9901, timescale.VolumeCharacterConcentrated}, // 10000/10100 on the lone taker
		{mixID, 0.1538, timescale.VolumeCharacterMarket},       // 1000/6500 per order-book pair
	}
	for _, tc := range cases {
		per, err := store.AssetVolumeCharacter(ctx, tc.assetID)
		if err != nil {
			t.Fatalf("AssetVolumeCharacter(%s): %v", tc.assetID, err)
		}
		roll, found, err := store.AssetVolumeCharacterRollup(ctx, tc.assetID)
		if err != nil || !found {
			t.Fatalf("AssetVolumeCharacterRollup(%s): found=%v err=%v", tc.assetID, found, err)
		}
		for name, got := range map[string]timescale.AssetVolumeCharacter{"per-asset": per, "rollup": roll} {
			if got.TopAccountPairVolShare != tc.topShare {
				t.Errorf("%s %s top_account_pair_vol_share = %v, want %v", tc.assetID, name, got.TopAccountPairVolShare, tc.topShare)
			}
			if got.Character != tc.char {
				t.Errorf("%s %s character = %q, want %q", tc.assetID, name, got.Character, tc.char)
			}
		}
	}
}

// TestAssetVolumeCharacterRollup_DemoteSort proves §4-B "annotate + demote":
// under the default AssetsOrderVolume24hUSDDesc sort, a high-RAW-volume
// CONCENTRATED (wash) asset ranks BELOW a lower-raw-volume MARKET asset,
// while the raw volume_24h_usd stays the real, unaltered number and the
// asset stays present in the directory.
func TestAssetVolumeCharacterRollup_DemoteSort(t *testing.T) {
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
	db := store.DB()

	washID := mustClassicID(t, "WASH", washIssuer) // $200k raw, concentrated
	realID := mustClassicID(t, "REAL", realIssuer) // $50k raw, market

	seedDirectoryAsset(t, ctx, db, washID, "WASH", washIssuer, "wash-token")
	seedDirectoryAsset(t, ctx, db, realID, "REAL", realIssuer, "real-token")

	// Raw 24h volumes: the wash asset has 4x the raw volume.
	seedRawVolume(t, ctx, db, washID, "200000")
	seedRawVolume(t, ctx, db, realID, "50000")

	// Character rollup: wash is concentrated with a 0.99 top-pair share
	// (adjusted = 200000 × 0.01 = 2000); real is market (adjusted = raw).
	seedCharacter(t, ctx, db, washID, timescale.VolumeCharacterConcentrated, 0.99)
	seedCharacter(t, ctx, db, realID, timescale.VolumeCharacterMarket, 0.10)

	rows, err := store.ListAssetsExt(ctx, timescale.ListAssetsOptions{
		Order: timescale.AssetsOrderVolume24hUSDDesc,
		Limit: 100,
	})
	if err != nil {
		t.Fatalf("ListAssetsExt: %v", err)
	}

	washPos, realPos := -1, -1
	var washRow timescale.AssetRow
	for i, r := range rows {
		switch r.AssetID {
		case washID:
			washPos, washRow = i, r
		case realID:
			realPos = i
		}
	}
	if washPos < 0 || realPos < 0 {
		t.Fatalf("both assets must be PRESENT in the directory (annotate+demote never hides): wash=%d real=%d", washPos, realPos)
	}
	// The demote: the lower-raw market asset outranks the higher-raw wash.
	if !(realPos < washPos) {
		t.Errorf("REAL ($50k market) at pos %d must rank ABOVE WASH ($200k concentrated) at pos %d under the default sort", realPos, washPos)
	}
	// The raw chain fact is untouched + visible.
	if washRow.Volume24hUSD == nil || *washRow.Volume24hUSD != "200000" {
		t.Errorf("WASH raw volume_24h_usd = %v, want the real unaltered 200000", washRow.Volume24hUSD)
	}
	// The listing carries the label (was null pre-change).
	if washRow.VolumeCharacter == nil || *washRow.VolumeCharacter != timescale.VolumeCharacterConcentrated {
		t.Errorf("WASH listing volume_character = %v, want concentrated", washRow.VolumeCharacter)
	}
}

func seedDirectoryAsset(t *testing.T, ctx context.Context, db *sql.DB, assetID, code, issuer, slug string) {
	t.Helper()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO classic_assets
		 (asset_id, code, issuer_g_strkey, slug, first_seen_at, first_seen_ledger,
		  last_seen_at, last_seen_ledger, observation_count)
		 VALUES ($1, $2, $3, $4, now(), 1, now(), 2, 10)`,
		assetID, code, issuer, slug,
	); err != nil {
		t.Fatalf("seed classic_assets %s: %v", assetID, err)
	}
}

func seedRawVolume(t *testing.T, ctx context.Context, db *sql.DB, assetID, volUSD string) {
	t.Helper()
	if _, err := db.ExecContext(ctx,
		// Upsert, not a bare insert: a fixture that runs
		// RefreshAssetListingRollups first (assets_listing_rank_test.go
		// does, because the listing's price column now comes from
		// asset_price_snapshot) already has a derived row here, and this
		// helper's job is to OVERRIDE it with the control value.
		`INSERT INTO asset_volume_24h (asset_id, vol_usd, computed_at)
		 VALUES ($1, $2, now())
		 ON CONFLICT (asset_id) DO UPDATE
		   SET vol_usd = EXCLUDED.vol_usd, computed_at = EXCLUDED.computed_at`,
		assetID, volUSD,
	); err != nil {
		t.Fatalf("seed asset_volume_24h %s: %v", assetID, err)
	}
}

func seedCharacter(t *testing.T, ctx context.Context, db *sql.DB, assetID, character string, topShare float64) {
	t.Helper()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO asset_volume_character
		 (asset_id, window_days, volume_usd, distinct_makers, distinct_takers,
		  top_account_pair_vol_share, self_cross_share, issuer_side_share,
		  market_styled_share, is_market_styled, character, computed_at)
		 VALUES ($1, 14, 1000, 2, 2, $2, 0, 0, 1, true, $3, now())`,
		assetID, topShare, character,
	); err != nil {
		t.Fatalf("seed asset_volume_character %s: %v", assetID, err)
	}
}

// liveAsset24hVolSQL is the single-asset per-request SUM the per_asset_24h_vol
// CTE inlines. The rollup must reproduce it byte-for-byte (only NUMERIC,
// ADR-0003): it moves the compute off the request path, not the value.
const liveAsset24hVolSQL = `
SELECT COALESCE(SUM(volume_usd), 0)::text
  FROM (
    SELECT volume_usd FROM prices_1m
     WHERE base_asset = $1
       AND bucket >= now() - INTERVAL '24 hours'
       AND bucket  <  now()
       AND volume_usd IS NOT NULL
    UNION ALL
    SELECT volume_usd FROM prices_1m
     WHERE quote_asset = $1
       AND bucket >= now() - INTERVAL '24 hours'
       AND bucket  <  now()
       AND volume_usd IS NOT NULL
  ) t`

// TestAssetVolumeRollup_MatchesLiveSum proves the asset-volume rollup end-to-end:
// RefreshAssetVolume24h populates asset_volume_24h with values that are
// byte-identical to the old inline per-request SUM over prices_1m, the
// refresh is idempotent, and an asset that ages out of the 24h window is
// pruned on the next pass.
func TestAssetVolumeRollup_MatchesLiveSum(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	xlm, _ := c.NewCryptoAsset("XLM")
	usd, _ := c.NewFiatAsset("USD")
	xlmUSD, _ := c.NewPair(xlm, usd)

	// binance + fiat:USD trades carry a populated usd_volume, so
	// prices_1m.volume_usd is > 0 for this pair. Two trades in the same
	// minute → one bucket whose volume_usd sums both.
	ts := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	seed := []c.Trade{
		mkIntegrationTrade("binance", 1, ts, xlmUSD, 100_000_000, 12_000_000),
		mkIntegrationTrade("binance", 2, ts, xlmUSD, 100_000_000, 34_000_000),
	}
	for _, tr := range seed {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %s: %v", tr.Source, err)
		}
	}

	// Materialize prices_1m so the rollup + the live comparison see rows.
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	if err := store.RefreshAssetVolume24h(ctx); err != nil {
		t.Fatalf("RefreshAssetVolume24h: %v", err)
	}

	// Every rollup row must equal the live single-asset SUM for that
	// asset, rendered identically (the byte-identical guarantee).
	rows, err := store.DB().QueryContext(ctx, `SELECT asset_id, vol_usd::text FROM asset_volume_24h`)
	if err != nil {
		t.Fatalf("read rollup: %v", err)
	}
	seen := 0
	for rows.Next() {
		var assetID, rollupVal string
		if err := rows.Scan(&assetID, &rollupVal); err != nil {
			t.Fatalf("scan rollup: %v", err)
		}
		var liveVal string
		if err := store.DB().QueryRowContext(ctx, liveAsset24hVolSQL, assetID).Scan(&liveVal); err != nil {
			t.Fatalf("live sum for %s: %v", assetID, err)
		}
		if rollupVal != liveVal {
			t.Errorf("asset %s: rollup vol_usd=%q, live SUM=%q (must be byte-identical)", assetID, rollupVal, liveVal)
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	// Both sides of the pair (base XLM + quote fiat:USD) should be
	// present, so the test actually exercised a populated rollup.
	if seen < 2 {
		t.Fatalf("rollup has %d rows, want >= 2 (base + quote of the seeded pair)", seen)
	}

	// Idempotent: a second refresh leaves the values unchanged.
	if err := store.RefreshAssetVolume24h(ctx); err != nil {
		t.Fatalf("RefreshAssetVolume24h (2nd): %v", err)
	}
	var after int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM asset_volume_24h`).Scan(&after); err != nil {
		t.Fatalf("count after 2nd refresh: %v", err)
	}
	if after != seen {
		t.Errorf("row count changed across idempotent refresh: %d → %d", seen, after)
	}

	// Prune: a stale sentinel row (an asset with no volume this pass)
	// must be dropped by the next refresh, while the live rows survive.
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO asset_volume_24h (asset_id, vol_usd, computed_at)
		 VALUES ('ZZZ-STALE-ASSET', 12345, now() - interval '1 hour')`); err != nil {
		t.Fatalf("insert stale sentinel: %v", err)
	}
	if err := store.RefreshAssetVolume24h(ctx); err != nil {
		t.Fatalf("RefreshAssetVolume24h (3rd): %v", err)
	}
	var stale int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM asset_volume_24h WHERE asset_id = 'ZZZ-STALE-ASSET'`).Scan(&stale); err != nil {
		t.Fatalf("count sentinel after prune: %v", err)
	}
	if stale != 0 {
		t.Errorf("stale sentinel still present after refresh, want pruned")
	}
	var live int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM asset_volume_24h`).Scan(&live); err != nil {
		t.Fatalf("count live after prune: %v", err)
	}
	if live != seen {
		t.Errorf("live rollup rows changed across prune: %d → %d", seen, live)
	}
}

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
