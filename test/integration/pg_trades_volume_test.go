//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/canonical/discovery"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/ingest"
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

// TestBespokeDEX24hVolumeCarriesXLMLeg proves the DEX block's 24h USD
// figures are trade-time only against a real TimescaleDB.
//
// source_volume_1h stores sum_usd_priced plus the XLM legs of trades left
// unpriced. Valuing those legs at today's XLM/USD would make a historical
// window move with spot, so the reader sums sum_usd_priced and names the
// excluded XLM as a lower bound. It must agree with the other reader of
// the CAGG (GetSourceVolumeHistory24h), rendered on the same source page.
//
// Fixture (one closed minute ~2h back, all on soroswap):
//
//	XLM/USDC   100 XLM for 50 USDC → vwap 0.5, usd_volume = 50  (priced)
//	token/USDC 7 USDC quote                    usd_volume =  7  (priced)
//	token/XLM  20 XLM on the QUOTE side        usd_volume NULL → sum_xlm_quote
//	XLM/token  10 XLM on the BASE side         usd_volume NULL → sum_xlm_base
//
// priced             = 50 + 7  = 57  (the 24h USD volume)
// unpriced XLM       = 20 + 10 = 30 XLM, excluded and named in the hint
func TestBespokeDEX24hVolumeCarriesXLMLeg(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Classic USDC is the USD peg, so the two USDC-quoted legs land a
	// non-null usd_volume and the XLM legs stay NULL (the CAGG's
	// sum_xlm_base/sum_xlm_quote filters are `usd_volume IS NULL`).
	spec, err := timescale.NewUSDVolumeQuoteSpec(
		[]string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"}, nil)
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	store.SetUSDVolumeQuoteSpec(spec)

	xlm := c.NativeAsset()
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	token, err := c.NewSorobanAsset("CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN")
	if err != nil {
		t.Fatal(err)
	}
	xlmUSDC, _ := c.NewPair(xlm, usdc)
	tokenUSDC, _ := c.NewPair(token, usdc)
	tokenXLM, _ := c.NewPair(token, xlm)
	xlmToken, _ := c.NewPair(xlm, token)

	ts := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	for _, tr := range []c.Trade{
		mkIntegrationTrade("soroswap", 1, ts, xlmUSDC, 1_000_000_000, 500_000_000),
		mkIntegrationTrade("soroswap", 2, ts, tokenUSDC, 900, 70_000_000),
		mkIntegrationTrade("soroswap", 3, ts, tokenXLM, 500, 200_000_000),
		mkIntegrationTrade("soroswap", 4, ts, xlmToken, 100_000_000, 300),
	} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %d: %v", tr.Ledger, err)
		}
	}

	// prices_1m carries the XLM/USD vwap the read-time multiply anchors
	// on; source_volume_1h carries the pre-aggregated inputs.
	for _, cagg := range []string{"prices_1m", "source_volume_1h"} {
		if _, err := store.DB().ExecContext(ctx,
			`CALL refresh_continuous_aggregate($1, NULL, NULL)`, cagg); err != nil {
			// CALL does not accept a bind param for the view name on
			// every version — fall back to the literal form.
			if _, err2 := store.DB().ExecContext(ctx,
				`CALL refresh_continuous_aggregate('`+cagg+`', NULL, NULL)`); err2 != nil {
				t.Fatalf("refresh %s: %v / %v", cagg, err, err2)
			}
		}
	}

	// Sanity: the fixture really does park volume in the XLM legs — if it
	// did not, the assertion below would pass on the unfixed reader too.
	var priced, xlmBase, xlmQuote string
	if err := store.DB().QueryRowContext(ctx, `
		SELECT COALESCE(sum(sum_usd_priced),0)::text,
		       COALESCE(sum(sum_xlm_base),0)::text,
		       COALESCE(sum(sum_xlm_quote),0)::text
		  FROM source_volume_1h
		 WHERE source = 'soroswap' AND bucket > now() - INTERVAL '1 day'`,
	).Scan(&priced, &xlmBase, &xlmQuote); err != nil {
		t.Fatalf("read CAGG inputs: %v", err)
	}
	if mustFloat(t, priced) < 56.99 || mustFloat(t, priced) > 57.01 {
		t.Fatalf("fixture: sum_usd_priced = %s, want 57", priced)
	}
	if mustFloat(t, xlmBase) != 100_000_000 || mustFloat(t, xlmQuote) != 200_000_000 {
		t.Fatalf("fixture: XLM legs = base %s / quote %s, want 100000000 / 200000000", xlmBase, xlmQuote)
	}

	blk, err := store.BuildProtocolBespoke(ctx, "soroswap", "dex", 1)
	if err != nil {
		t.Fatalf("BuildProtocolBespoke: %v", err)
	}
	if blk == nil {
		t.Fatal("BuildProtocolBespoke returned no block for a source with 4 trades in the window")
	}

	var volKPI *timescale.BespokeKPI
	for i := range blk.KPIs {
		if blk.KPIs[i].Label == "USD volume (1d)" {
			volKPI = &blk.KPIs[i]
		}
	}
	if volKPI == nil {
		t.Fatal(`no "USD volume (1d)" KPI on the 24h DEX block`)
	}
	if volKPI.Value != "57.00" {
		t.Errorf("24h USD volume KPI = %s, want 57.00 (trade-time priced only)", volKPI.Value)
	}
	if !strings.Contains(volKPI.Hint, "LOWER BOUND") || !strings.Contains(volKPI.Hint, "excludes 30.0000000 XLM") {
		t.Errorf("24h USD volume KPI hint must be a lower bound naming the 30 XLM excluded, got %q", volKPI.Hint)
	}

	// The hourly series carries the same derivation (all four trades sit
	// in one hour bucket, so its single point is the whole window).
	var series *timescale.BespokeSeries
	for i := range blk.Series {
		if blk.Series[i].Name == "USD volume" {
			series = &blk.Series[i]
		}
	}
	if series == nil || len(series.Points) == 0 {
		t.Fatal("no hourly USD volume series on the 24h DEX block")
	}
	var total float64
	for _, p := range series.Points {
		total += mustFloat(t, p.Value)
	}
	if total < 56.99 || total > 57.01 {
		t.Errorf("hourly USD volume series totals %.4f, want ~57 (priced only)", total)
	}

	joined := strings.Join(blk.Notes, "\n")
	if !strings.Contains(joined, "30.0000000 XLM of XLM-denominated legs") || !strings.Contains(joined, "lower bounds") {
		t.Errorf("24h block note must name the excluded XLM as a lower bound, got %q", joined)
	}

	// Cross-surface parity — the whole point of the finding: the other
	// reader of this CAGG (the source page's own 24h chart, /v1/sources)
	// must report the SAME 24h volume for the same source.
	hist, err := store.GetSourceVolumeHistory24h(ctx)
	if err != nil {
		t.Fatalf("GetSourceVolumeHistory24h: %v", err)
	}
	var histTotal float64
	for _, b := range hist {
		if b.Source == "soroswap" {
			histTotal += mustFloat(t, b.VolumeUSD)
		}
	}
	if histTotal < 56.99 || histTotal > 57.01 {
		t.Fatalf("fixture check: GetSourceVolumeHistory24h totals %.4f, want ~57", histTotal)
	}
	if diff := histTotal - total; diff > 0.01 || diff < -0.01 {
		t.Errorf("the two readers of source_volume_1h disagree on soroswap's 24h volume: bespoke block %.4f vs source chart %.4f", total, histTotal)
	}
}

// TestBespokeDEX24hXLMAnchorOutageIsLowerBound — with no native/USD row in
// prices_1m the unpriced XLM legs are still excluded; the block serves the
// priced leg as a named lower bound.
//
// Fixture: the sibling test's minus its XLM/USDC trade (the only anchor):
//
//	token/USDC 7 USDC quote       usd_volume =  7  (priced)
//	token/XLM  20 XLM quote side  usd_volume NULL → sum_xlm_quote
//	XLM/token  10 XLM base side   usd_volume NULL → sum_xlm_base
func TestBespokeDEX24hXLMAnchorOutageIsLowerBound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedXLMAnchorOutage(t, ctx, store)

	blk, err := store.BuildProtocolBespoke(ctx, "soroswap", "dex", 1)
	if err != nil {
		t.Fatalf("BuildProtocolBespoke: %v", err)
	}
	if blk == nil {
		t.Fatal("BuildProtocolBespoke returned no block for a source with 3 trades in the window")
	}
	vol := bespokeKPI(blk, "USD volume (1d)")
	if vol == nil {
		t.Fatal(`no "USD volume (1d)" KPI on the 24h DEX block`)
	}
	if vol.Value != "7.00" {
		t.Errorf("anchor-outage USD volume = %s, want 7.00 (priced leg only)", vol.Value)
	}
	if !strings.Contains(vol.Hint, "LOWER BOUND") || !strings.Contains(vol.Hint, "excludes 30.0000000 XLM") {
		t.Errorf("anchor-outage USD volume hint must be a lower bound naming the 30 XLM excluded, got %q", vol.Hint)
	}
	joined := strings.Join(blk.Notes, "\n")
	if strings.Contains(joined, "additionally value") || !strings.Contains(joined, "30.0000000 XLM of XLM-denominated legs") {
		t.Errorf("anchor-outage note must disclose the unvalued XLM legs, got %q", joined)
	}
	avg := bespokeKPI(blk, "Avg trade size (1d)")
	if avg == nil || avg.Value != "7.00" {
		t.Fatalf("avg trade size KPI = %+v, want 7.00 (7 USD / 1 priced trade)", avg)
	}
	if strings.Contains(avg.Hint, "window USD volume") {
		t.Errorf("avg hint must not present the usd_volume-only average as the volume KPI ÷ trades, got %q", avg.Hint)
	}

	// Parity: the source page's chart reads the same CAGG and must agree.
	hist, err := store.GetSourceVolumeHistory24h(ctx)
	if err != nil {
		t.Fatalf("GetSourceVolumeHistory24h: %v", err)
	}
	var histTotal float64
	for _, b := range hist {
		if b.Source == "soroswap" {
			histTotal += mustFloat(t, b.VolumeUSD)
		}
	}
	if histTotal < 6.99 || histTotal > 7.01 {
		t.Errorf("GetSourceVolumeHistory24h totals %.4f on an anchor outage, want ~7 (parity with the block)", histTotal)
	}
}

// seedXLMAnchorOutage writes the outage fixture and asserts it really parks
// 30 XLM in the legs with no XLM/USD anchor — otherwise the test is vacuous.
func seedXLMAnchorOutage(t *testing.T, ctx context.Context, store *timescale.Store) {
	t.Helper()
	spec, err := timescale.NewUSDVolumeQuoteSpec(
		[]string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"}, nil)
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	store.SetUSDVolumeQuoteSpec(spec)
	xlm := c.NativeAsset()
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	token, err := c.NewSorobanAsset("CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN")
	if err != nil {
		t.Fatal(err)
	}
	tokenUSDC, _ := c.NewPair(token, usdc)
	tokenXLM, _ := c.NewPair(token, xlm)
	xlmToken, _ := c.NewPair(xlm, token)
	ts := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	for _, tr := range []c.Trade{
		mkIntegrationTrade("soroswap", 2, ts, tokenUSDC, 900, 70_000_000),
		mkIntegrationTrade("soroswap", 3, ts, tokenXLM, 500, 200_000_000),
		mkIntegrationTrade("soroswap", 4, ts, xlmToken, 100_000_000, 300),
	} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %d: %v", tr.Ledger, err)
		}
	}
	for _, cagg := range []string{"prices_1m", "source_volume_1h"} {
		if _, err := store.DB().ExecContext(ctx,
			`CALL refresh_continuous_aggregate('`+cagg+`', NULL, NULL)`); err != nil {
			t.Fatalf("refresh %s: %v", cagg, err)
		}
	}
	var anchors int
	var priced, xlmLegs string
	if err := store.DB().QueryRowContext(ctx, `
		SELECT (SELECT count(*) FROM prices_1m WHERE base_asset = 'native'
		           AND quote_asset IN ('USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN', 'fiat:USD')),
		       COALESCE(sum(sum_usd_priced),0)::text,
		       (COALESCE(sum(sum_xlm_base),0) + COALESCE(sum(sum_xlm_quote),0))::text
		  FROM source_volume_1h WHERE source = 'soroswap'`,
	).Scan(&anchors, &priced, &xlmLegs); err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if anchors != 0 || mustFloat(t, priced) != 7 || mustFloat(t, xlmLegs) != 300_000_000 {
		t.Fatalf("fixture: anchors=%d priced=%s xlm legs=%s, want 0 / 7 / 300000000", anchors, priced, xlmLegs)
	}
}

func bespokeKPI(blk *timescale.BespokeBlock, label string) *timescale.BespokeKPI {
	for i := range blk.KPIs {
		if blk.KPIs[i].Label == label {
			return &blk.KPIs[i]
		}
	}
	return nil
}

// TestBespokeDEX24hBoundaryHourExcludedFromBothReaders pins that a
// trade in the hour bucket that floors to exactly `now() - 24h` must be
// excluded by BOTH readers of source_volume_1h identically. Before the
// fix, GetSourceVolumeHistory24h floored its window with
// `date_trunc('hour', NOW() - 24h)` (a >=, 25-bucket window) and included
// this boundary bucket, while the bespoke 24h KPI/series (a strict `>`,
// 24-bucket window) excluded it — the same trade counted by one 24h
// reader of the source page and not the other.
func TestBespokeDEX24hBoundaryHourExcludedFromBothReaders(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	spec, err := timescale.NewUSDVolumeQuoteSpec(
		[]string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"}, nil)
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	store.SetUSDVolumeQuoteSpec(spec)

	xlm := c.NativeAsset()
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	xlmUSDC, _ := c.NewPair(xlm, usdc)

	// Land squarely in the hour bucket the buggy date_trunc() floor used
	// to pull in as an extra 25th bucket.
	boundaryHour := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Hour)
	ts := boundaryHour.Add(1 * time.Minute)
	if err := store.InsertTrade(ctx, mkIntegrationTrade("soroswap", 1, ts, xlmUSDC, 1_000_000_000, 500_000_000)); err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('source_volume_1h', NULL, NULL)`); err != nil {
		t.Fatalf("refresh source_volume_1h: %v", err)
	}

	// Sanity: the fixture really landed in the boundary bucket.
	var bucketCount int
	if err := store.DB().QueryRowContext(ctx, `
		SELECT count(*) FROM source_volume_1h
		 WHERE source = 'soroswap' AND bucket = date_trunc('hour', now() - INTERVAL '24 hours')`,
	).Scan(&bucketCount); err != nil {
		t.Fatalf("read boundary bucket: %v", err)
	}
	if bucketCount != 1 {
		t.Fatalf("fixture: boundary bucket has %d source_volume_1h row(s), want 1", bucketCount)
	}

	blk, err := store.BuildProtocolBespoke(ctx, "soroswap", "dex", 1)
	if err != nil {
		t.Fatalf("BuildProtocolBespoke: %v", err)
	}
	if blk != nil {
		if vol := bespokeKPI(blk, "USD volume (1d)"); vol != nil && vol.Value != "0.00" {
			t.Fatalf("fixture: bespoke 24h KPI = %s, want 0.00 (boundary-hour trade excluded)", vol.Value)
		}
	}

	hist, err := store.GetSourceVolumeHistory24h(ctx)
	if err != nil {
		t.Fatalf("GetSourceVolumeHistory24h: %v", err)
	}
	for _, b := range hist {
		if b.Source == "soroswap" {
			t.Errorf("GetSourceVolumeHistory24h must exclude the boundary-hour bucket like the bespoke 24h reader does, got %+v", b)
		}
	}
}

// TestBespokeDEX7dCoversSevenCompleteDays proves every "(7d)" figure in the
// DEX block covers the same 7 complete UTC days (D-7..D-1) against a real
// TimescaleDB, CAGG-backed and raw-trade-backed alike.
//
// Fixture: one priced soroswap trade at 12:00 UTC on each of D-8..D-1 plus
// one just after 00:00 today, with USD volumes that are distinct powers of
// two so every window mis-cut produces a different sum:
//
//	D-8 256 (outside)   D-7..D-1  1,2,4,8,16,32,64   D (today) 128 (partial)
//
// Want: USD volume 127.00, 7 trades, avg 127/7 = 18.14, top trade 64.00.
// A rolling `bucket > now() - '7 days'` cut drops the D-7 bucket (126),
// and a rolling `ts > now() - '7 days'` cut on raw trades takes today's 128.
func TestBespokeDEX7dCoversSevenCompleteDays(t *testing.T) {
	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	if now.Sub(today) < 10*time.Minute || today.Add(24*time.Hour).Sub(now) < 10*time.Minute {
		t.Skip("within 10 minutes of a UTC day boundary: the Go and DB clocks may straddle it")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedCompleteDaysFixture(t, ctx, store, today)

	blk, err := store.BuildProtocolBespoke(ctx, "soroswap", "dex", 7)
	if err != nil {
		t.Fatalf("BuildProtocolBespoke: %v", err)
	}
	if blk == nil {
		t.Fatal("BuildProtocolBespoke returned no block for a source with trades in the window")
	}

	kpis := map[string]string{}
	for _, k := range blk.KPIs {
		kpis[k.Label] = k.Value
	}
	for label, want := range map[string]string{
		"USD volume (7d)":     "127.00",
		"Trades (7d)":         "7",
		"Avg trade size (7d)": "18.14",
	} {
		if got := kpis[label]; got != want {
			t.Errorf("%s = %q, want %q (the 7 complete UTC days D-7..D-1)", label, got, want)
		}
	}

	var largest *timescale.BespokeTable
	for i := range blk.Tables {
		if blk.Tables[i].Title == "Largest trades" {
			largest = &blk.Tables[i]
		}
	}
	if largest == nil || len(largest.Rows) != 7 || largest.Rows[0][3] != "64.00" {
		t.Errorf("largest trades must list the 7 window trades topped by 64.00 (not today's 128), got %+v", largest)
	}

	var vol *timescale.BespokeSeries
	for i := range blk.Series {
		if blk.Series[i].Name == "USD volume" {
			vol = &blk.Series[i]
		}
	}
	if vol == nil || len(vol.Points) != 7 {
		t.Fatalf("daily USD volume series must carry one point per complete day D-7..D-1, got %+v", vol)
	}
	if first, want := vol.Points[0].Date, today.AddDate(0, 0, -7).Format("2006-01-02"); first != want {
		t.Errorf("daily series starts %s, want %s", first, want)
	}

	if !strings.Contains(strings.Join(blk.Notes, "\n"), "7 complete UTC days before today") {
		t.Errorf("7d block must disclose its complete-day window, notes: %q", blk.Notes)
	}
}

// seedCompleteDaysFixture inserts the D-8..D trades described above and
// materializes the daily pair CAGG over them.
func seedCompleteDaysFixture(t *testing.T, ctx context.Context, store *timescale.Store, today time.Time) {
	t.Helper()
	spec, err := timescale.NewUSDVolumeQuoteSpec(
		[]string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"}, nil)
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	store.SetUSDVolumeQuoteSpec(spec)
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	sorobanContract, err := c.NewSorobanAsset("CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN")
	if err != nil {
		t.Fatal(err)
	}
	pair, _ := c.NewPair(sorobanContract, usdc)

	const usd = 10_000_000 // USDC quote stroops per USD
	var trades []c.Trade
	for daysAgo := 8; daysAgo >= 1; daysAgo-- {
		ts := today.AddDate(0, 0, -daysAgo).Add(12 * time.Hour)
		vol := int64(256)
		if daysAgo <= 7 {
			vol = int64(1) << (7 - daysAgo) // D-7 → 1 … D-1 → 64
		}
		trades = append(trades, mkIntegrationTrade("soroswap", daysAgo, ts, pair, 1_000, vol*usd))
	}
	trades = append(trades, mkIntegrationTrade("soroswap", 20, today.Add(time.Minute), pair, 1_000, 128*usd))
	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %d: %v", tr.Ledger, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('dex_volume_by_pair_1d', NULL, NULL)`); err != nil {
		t.Fatalf("refresh dex_volume_by_pair_1d: %v", err)
	}
}

// TestStoreDailyMarketDays_ReadsWholeLastDay executes DailyMarketDays
// against real TimescaleDB with `to` at the start of the last day, as
// the /v1/rwa/premium caller passes it. Trades at 00:30 and 15:30 on
// that day must BOTH count: two hours, a 15-hour span, and the whole
// day's VWAP. A trade on the following day must not.
//
// Day D-1 VWAP = (10 + 30) / (100 + 100) = 0.2; the 00:30 hour alone
// would read 0.1 with Hours = 1.
func TestStoreDailyMarketDays_ReadsWholeLastDay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const issuerAccount = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdc, err := c.NewClassicAsset("USDC", issuerAccount)
	if err != nil {
		t.Fatal(err)
	}
	ustry, err := c.NewClassicAsset("USTRY", issuerAccount)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := c.NewPair(ustry, usdc)
	if err != nil {
		t.Fatal(err)
	}

	lastDay := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	for i, tr := range []c.Trade{
		mkAPITrade(1, lastDay.Add(30*time.Minute), pair, 1_000_000_000, 100_000_000),              // 100 for 10 → 0.10
		mkAPITrade(2, lastDay.Add(15*time.Hour+30*time.Minute), pair, 1_000_000_000, 300_000_000), // 100 for 30 → 0.30
		mkAPITrade(3, lastDay.Add(24*time.Hour+2*time.Hour), pair, 1_000_000_000, 1_000_000_000),  // next day: must not be read
		mkAPITrade(4, lastDay.Add(-24*time.Hour+3*time.Hour), pair, 1_000_000_000, 500_000_000),   // day before: 0.50
	} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade[%d]: %v", i, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1h', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1h: %v", err)
	}

	days, err := store.DailyMarketDays(ctx,
		[]c.Asset{ustry}, []c.Asset{usdc}, lastDay.Add(-24*time.Hour), lastDay)
	if err != nil {
		t.Fatalf("DailyMarketDays: %v", err)
	}
	if len(days) != 2 {
		t.Fatalf("days = %d, want 2 (the day before and the whole last day, not the day after): %+v", len(days), days)
	}
	if !days[0].Day.Equal(lastDay.Add(-24 * time.Hour)) {
		t.Errorf("first day = %s, want %s", days[0].Day, lastDay.Add(-24*time.Hour))
	}
	d := days[1]
	if !d.Day.Equal(lastDay) {
		t.Fatalf("last day = %s, want %s", d.Day, lastDay)
	}
	if d.Hours != 2 {
		t.Errorf("last day Hours = %d, want 2 (00:30 and 15:30 hours)", d.Hours)
	}
	if d.SpanSeconds != 15*3600 {
		t.Errorf("last day SpanSeconds = %d, want %d", d.SpanSeconds, 15*3600)
	}
	if d.Trades != 2 {
		t.Errorf("last day Trades = %d, want 2", d.Trades)
	}
	got, ok := new(big.Rat).SetString(d.VWAP)
	if !ok || got.Cmp(big.NewRat(1, 5)) != 0 {
		t.Errorf("last day VWAP = %s, want 0.2 (the whole day, not its 00:00 hour)", d.VWAP)
	}
}

// TestStoreDailyMarketDays_FoldsSACSpellingOfBase pins CA2-A07-correct-2:
// a token with a configured SAC wrapper trades under both spellings, and
// the live substance gate counts both. The historical reader must fold
// the SAC-form rows onto the requested classic asset, and measure hours
// and span over the UNION: the 00:00 hour traded under both spellings
// counts once.
//
// Classic: 00:10 (100 for 10), 05:30 (100 for 20). SAC: 00:40 (100 for
// 30), 20:30 (100 for 40). Union: hours {00,05,20} = 3, span 20h, 4
// trades, VWAP = 100/400 = 0.25. Classic alone reads 2 hours, 5h, 0.15.
func TestStoreDailyMarketDays_FoldsSACSpellingOfBase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const (
		issuerAccount   = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		sorobanContract = "CAAVZKRT2BJ5A5Y5SAGQNA3IC5EDAODEWBHO5CYR5ASLFDZYOQN6NEO3"
	)
	reg, err := c.NewAliasRegistry(c.PubnetPassphrase, map[string]string{sorobanContract: "USTRY:" + issuerAccount})
	if err != nil {
		t.Fatalf("NewAliasRegistry: %v", err)
	}
	c.InstallAliasRegistry(reg)
	t.Cleanup(func() { c.InstallAliasRegistry(nil) })

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := c.NewClassicAsset("USDC", issuerAccount)
	if err != nil {
		t.Fatal(err)
	}
	ustry, err := c.NewClassicAsset("USTRY", issuerAccount)
	if err != nil {
		t.Fatal(err)
	}
	ustrySAC, err := c.NewSorobanAsset(sorobanContract)
	if err != nil {
		t.Fatal(err)
	}
	classicPair, err := c.NewPair(ustry, usdc)
	if err != nil {
		t.Fatal(err)
	}
	sacPair, err := c.NewPair(ustrySAC, usdc)
	if err != nil {
		t.Fatal(err)
	}

	day := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	for i, tr := range []c.Trade{
		mkAPITrade(11, day.Add(10*time.Minute), classicPair, 1_000_000_000, 100_000_000),
		mkAPITrade(12, day.Add(5*time.Hour+30*time.Minute), classicPair, 1_000_000_000, 200_000_000),
		mkAPITrade(13, day.Add(40*time.Minute), sacPair, 1_000_000_000, 300_000_000),
		mkAPITrade(14, day.Add(20*time.Hour+30*time.Minute), sacPair, 1_000_000_000, 400_000_000),
	} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade[%d]: %v", i, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1h', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1h: %v", err)
	}

	days, err := store.DailyMarketDays(ctx, []c.Asset{ustry}, []c.Asset{usdc}, day, day)
	if err != nil {
		t.Fatalf("DailyMarketDays: %v", err)
	}
	if len(days) != 1 {
		t.Fatalf("days = %d, want 1 row folded onto the classic member: %+v", len(days), days)
	}
	d := days[0]
	if d.AssetID != ustry.String() {
		t.Errorf("AssetID = %q, want the requested classic %q", d.AssetID, ustry.String())
	}
	if d.Hours != 3 {
		t.Errorf("Hours = %d, want 3 (00, 05, 20; the 00 hour traded on both spellings counts once)", d.Hours)
	}
	if d.SpanSeconds != 20*3600 {
		t.Errorf("SpanSeconds = %d, want %d (min/max over the union)", d.SpanSeconds, 20*3600)
	}
	if d.Trades != 4 {
		t.Errorf("Trades = %d, want 4", d.Trades)
	}
	got, ok := new(big.Rat).SetString(d.VWAP)
	if !ok || got.Cmp(big.NewRat(1, 4)) != 0 {
		t.Errorf("VWAP = %s, want 0.25 (sum(quote)/sum(base) over both spellings)", d.VWAP)
	}
}

// TestHistoryPointsDirectionUnion executes the two CAGG series reads
// behind /v1/history/since-inception and /v1/chart against a real
// TimescaleDB after they were rewritten from a both-directions OR
// disjunction into a UNION ALL of two single-direction branches with a
// sargable closed-bucket bound.
//
// The shape itself is pinned without a database by the scanning guard in
// internal/storage/timescale/prices_1m_direction_union_test.go. What
// only a live database can prove is that the rewritten statements still
// PARSE, still bind their parameters in the right order once the
// optional from/to/limit clauses move inside the branches, and still
// serve the same numbers: both stored orientations folded into the
// requested one, chronological order, the bucket LIMIT counting BUCKETS
// (not rows) across the two branches, the in-progress bucket excluded,
// and an unknown pair answered with an empty series rather than an
// error.
func TestHistoryPointsDirectionUnion(t *testing.T) {
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
	xlmUSDC, _ := c.NewPair(c.NativeAsset(), usdc) // requested orientation
	usdcXLM, _ := c.NewPair(usdc, c.NativeAsset()) // the flipped storage direction

	// Three closed 1-minute buckets ~2h back. The middle one is stored
	// ONLY in the flipped orientation — a one-direction read drops it,
	// and a UNION ALL that lost a branch would drop it too.
	t0 := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	trades := []c.Trade{
		mkAPITrade(41, t0, xlmUSDC, 1_000_000, 500_000),                      // 0.5
		mkAPITrade(42, t0.Add(2*time.Minute), usdcXLM, 1_000_000, 2_000_000), // 2.0 → 0.5 inverted
		mkAPITrade(43, t0.Add(4*time.Minute), xlmUSDC, 1_000_000, 500_000),   // 0.5
	}
	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
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

	wantBuckets := []time.Time{t0, t0.Add(2 * time.Minute), t0.Add(4 * time.Minute)}

	assertSeries := func(t *testing.T, what string, pts []timescale.HistoryPoint, want []time.Time) {
		t.Helper()
		if len(pts) != len(want) {
			t.Fatalf("%s returned %d buckets, want %d: %+v", what, len(pts), len(want), pts)
		}
		for i, p := range pts {
			if !p.Bucket.UTC().Equal(want[i]) {
				t.Errorf("%s[%d].Bucket = %s, want %s (chronological, oldest first)",
					what, i, p.Bucket.UTC(), want[i])
			}
			// Every bucket is the same market at 0.5 USDC per XLM; the
			// flipped-only bucket must arrive INVERTED from its stored 2.0.
			if px := mustFloat(t, p.VWAP); px < 0.49 || px > 0.51 {
				t.Errorf("%s[%d].VWAP = %s, want ~0.5 (both directions folded into the requested orientation)",
					what, i, p.VWAP)
			}
		}
	}

	// ── HistoryPoints: the since-inception read (no lower bound) ────
	pts, err := store.HistoryPoints(ctx, xlmUSDC, timescale.Granularity1m, 0)
	if err != nil {
		t.Fatalf("HistoryPoints: %v", err)
	}
	assertSeries(t, "HistoryPoints", pts, wantBuckets)

	// The bucket LIMIT counts BUCKETS across both branches: with the cap
	// now applied per branch AND on the union, the first n buckets of the
	// merged series must still be complete and in order.
	for n := 1; n <= 3; n++ {
		capped, err := store.HistoryPoints(ctx, xlmUSDC, timescale.Granularity1m, n)
		if err != nil {
			t.Fatalf("HistoryPoints(limit=%d): %v", n, err)
		}
		assertSeries(t, "HistoryPoints(limit)", capped, wantBuckets[:n])
	}

	// Requesting the market the other way round serves the reciprocal —
	// the same rows, folded into the flipped orientation (1/0.5 = 2.0).
	rev, err := store.HistoryPoints(ctx, usdcXLM, timescale.Granularity1m, 0)
	if err != nil {
		t.Fatalf("HistoryPoints(reversed): %v", err)
	}
	if len(rev) != 3 {
		t.Fatalf("HistoryPoints(reversed) returned %d buckets, want 3", len(rev))
	}
	for i, p := range rev {
		if px := mustFloat(t, p.VWAP); px < 1.99 || px > 2.01 {
			t.Errorf("HistoryPoints(reversed)[%d].VWAP = %s, want ~2.0", i, p.VWAP)
		}
	}

	// An unknown-but-well-formed pair is the DoS scenario this rewrite
	// exists for: it must answer an empty series, not an error.
	eur, err := c.NewFiatAsset("EUR")
	if err != nil {
		t.Fatal(err)
	}
	emptyPair, _ := c.NewPair(c.NativeAsset(), eur)
	empty, err := store.HistoryPoints(ctx, emptyPair, timescale.Granularity1m, 0)
	if err != nil {
		t.Fatalf("HistoryPoints(empty pair): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("HistoryPoints(empty pair) returned %d buckets, want 0", len(empty))
	}

	// ── Closed-bucket guard, in its rewritten sargable spelling ─────
	// A 1-day bucket is served only once it has CLOSED (ADR-0015:
	// `bucket <= now() - INTERVAL '1 day'`). The seed sits ~2h back, so
	// which side of that line it falls on depends on the clock: for the
	// first ~2h of a UTC day the trades land in YESTERDAY's bucket, which
	// is closed and must be served; for the rest of the day they land in
	// today's, which is open and must not be. Assert whichever is true —
	// a fixed "want 0" fails every run between 00:00 and ~02:05 UTC.
	var dayRows int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM prices_1d
		  WHERE (base_asset = $1 AND quote_asset = $2)
		     OR (base_asset = $2 AND quote_asset = $1)`,
		xlmUSDC.Base.String(), xlmUSDC.Quote.String(),
	).Scan(&dayRows); err != nil {
		t.Fatalf("count prices_1d: %v", err)
	}
	if dayRows == 0 {
		t.Fatal("prices_1d holds no row for the seeded pair — the closed-bucket " +
			"assertion below would pass vacuously")
	}
	closedDays := func(now time.Time) map[time.Time]bool {
		out := map[time.Time]bool{}
		for _, tr := range trades {
			day := tr.Timestamp.UTC().Truncate(24 * time.Hour)
			if !day.Add(24 * time.Hour).After(now) {
				out[day] = true
			}
		}
		return out
	}
	closedBefore := closedDays(time.Now().UTC())
	dayPts, err := store.HistoryPoints(ctx, xlmUSDC, timescale.Granularity1d, 0)
	if err != nil {
		t.Fatalf("HistoryPoints(1d): %v", err)
	}
	// Read the clock on both sides of the query: a run that straddles
	// midnight may legitimately see either answer.
	closedAfter := closedDays(time.Now().UTC())
	if len(dayPts) != len(closedBefore) && len(dayPts) != len(closedAfter) {
		t.Errorf("HistoryPoints(1d) returned %d buckets, want %d — only a CLOSED day "+
			"bucket is served (ADR-0015, `bucket <= now() - INTERVAL '1 day'`): %+v",
			len(dayPts), len(closedAfter), dayPts)
	}
	for _, p := range dayPts {
		if !closedAfter[p.Bucket.UTC()] {
			t.Errorf("HistoryPoints(1d) served %s, a day bucket that is still open",
				p.Bucket.UTC().Format(time.RFC3339))
		}
	}

	// ── HistoryPointsInRange: /v1/chart's windowed read ─────────────
	from := t0.Add(-time.Minute)
	to := t0.Add(5 * time.Minute)
	ranged, err := store.HistoryPointsInRange(ctx, xlmUSDC, timescale.Granularity1m, from, to, 0)
	if err != nil {
		t.Fatalf("HistoryPointsInRange: %v", err)
	}
	assertSeries(t, "HistoryPointsInRange", ranged, wantBuckets)

	// The window must still bite once the bounds live inside the
	// branches: [t0+1m, t0+3m) keeps only the flipped-only bucket.
	narrow, err := store.HistoryPointsInRange(ctx, xlmUSDC, timescale.Granularity1m,
		t0.Add(time.Minute), t0.Add(3*time.Minute), 0)
	if err != nil {
		t.Fatalf("HistoryPointsInRange(narrow): %v", err)
	}
	assertSeries(t, "HistoryPointsInRange(narrow)", narrow, wantBuckets[1:2])

	// Optional bounds: zero `from` means since-inception, zero `to`
	// means open-ended — both branches must still carry the rest of the
	// predicate list and the placeholders must stay in step.
	noFrom, err := store.HistoryPointsInRange(ctx, xlmUSDC, timescale.Granularity1m,
		time.Time{}, to, 0)
	if err != nil {
		t.Fatalf("HistoryPointsInRange(no from): %v", err)
	}
	assertSeries(t, "HistoryPointsInRange(no from)", noFrom, wantBuckets)

	noTo, err := store.HistoryPointsInRange(ctx, xlmUSDC, timescale.Granularity1m,
		from, time.Time{}, 1)
	if err != nil {
		t.Fatalf("HistoryPointsInRange(no to, limit 1): %v", err)
	}
	assertSeries(t, "HistoryPointsInRange(no to, limit 1)", noTo, wantBuckets[:1])

	// Every optional clause at once — the maximal-placeholder path
	// ($1..$5), which is what /v1/chart issues. The bounds now live
	// inside both branches and the LIMIT is bound last, so an
	// off-by-one in the placeholder arithmetic surfaces here as a
	// bind error or as the wrong window.
	all, err := store.HistoryPointsInRange(ctx, xlmUSDC, timescale.Granularity1m,
		from, to, 2)
	if err != nil {
		t.Fatalf("HistoryPointsInRange(from, to, limit 2): %v", err)
	}
	assertSeries(t, "HistoryPointsInRange(from, to, limit 2)", all, wantBuckets[:2])
}

// TestHistoryPointsUnboundedPopulatedReadStreams measures the one shape
// the unknown-pair cost test does not: /v1/history/since-inception with
// limit=0 over a pair that HAS a long history. That statement carries no
// LIMIT and an outer `ORDER BY bucket ASC, base_asset` over a UNION ALL
// whose branches are index-ordered on bucket alone, so the question is
// whether the outer sort streams or materialises the pair's whole series.
//
// Under the serving pool's plan_cache_mode = force_custom_plan (see
// OpenServing) it streams: a Merge Append on bucket feeding an
// Incremental Sort whose groups are one bucket (at most two rows). No
// full Sort node may appear. Measured on this fixture (3,120 hourly
// buckets over ~14 chunks): 3.2 ms, 29 kB peak sort memory. Adding
// base_asset to each branch's ORDER BY changes nothing — within a branch
// base_asset is bound to a parameter, so the planner drops it as a
// redundant sort key. A GENERIC plan (not used by the serving pool) is
// logged for the record: it chose Parallel Append + a full Sort, 17 ms.
func TestHistoryPointsUnboundedPopulatedReadStreams(t *testing.T) {
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
	// Prepared statements and SET are per-session: one backend throughout.
	db.SetMaxOpenConns(1)
	seedCostFixture(t, ctx, db)
	pairs := newCostPairs(t)

	pts, err := store.HistoryPoints(ctx, pairs.traded, timescale.Granularity1h, 0)
	if err != nil {
		t.Fatalf("HistoryPoints(limit=0): %v", err)
	}
	if want := costFixtureDays * 24; len(pts) != want {
		t.Fatalf("HistoryPoints(limit=0) returned %d buckets, want %d — not the populated since-inception read", len(pts), want)
	}
	stmt := capturePreparedStatement(t, ctx, db, nil, "FROM prices_1h", "ORDER BY bucket ASC")
	args := []string{pairs.traded.Base.String(), pairs.traded.Quote.String()}

	// Instrument check first: with incremental sort disabled the same
	// statement has no way to stream, so the walker must see a full Sort.
	mustExecPlan(t, ctx, db, `SET enable_incremental_sort = off`)
	control := planNodeTypes(t, ctx, db, "force_custom_plan", stmt, args)
	mustExecPlan(t, ctx, db, `RESET enable_incremental_sort`)
	if !slices.Contains(control, "Sort") {
		t.Fatalf("instrument check failed: with incremental sort off the plan %v has no Sort node — "+
			"the walker is not reading the plan", control)
	}

	served := planNodeTypes(t, ctx, db, "force_custom_plan", stmt, args)
	t.Logf("force_custom_plan: %v", served)
	if slices.Contains(served, "Sort") {
		t.Errorf("serving-mode plan materialises the whole series in a full Sort: %v", served)
	}
	if !slices.Contains(served, "Merge Append") {
		t.Errorf("serving-mode plan does not merge the two index-ordered branches: %v", served)
	}
	t.Logf("force_generic_plan (not the serving pool's mode): %v",
		planNodeTypes(t, ctx, db, "force_generic_plan", stmt, args))
}

func mustExecPlan(t *testing.T, ctx context.Context, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// planNodeTypes prepares stmt under planMode and returns every node type
// of its EXPLAIN ANALYZE plan, depth-first. args are string literals.
func planNodeTypes(t *testing.T, ctx context.Context, db *sql.DB, planMode, stmt string, args []string) []string {
	t.Helper()
	const name = "unbounded_plan_probe"
	mustExecPlan(t, ctx, db, `SET plan_cache_mode = `+planMode)
	mustExecPlan(t, ctx, db, `PREPARE `+name+` AS `+stmt)
	defer mustExecPlan(t, ctx, db, `DEALLOCATE `+name)
	lits := ""
	for i, a := range args {
		if i > 0 {
			lits += ", "
		}
		lits += "'" + a + "'"
	}
	var raw string
	if err := db.QueryRowContext(ctx,
		`EXPLAIN (ANALYZE, FORMAT JSON) EXECUTE `+name+`(`+lits+`)`).Scan(&raw); err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	var doc []struct {
		Plan explainNode `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil || len(doc) != 1 {
		t.Fatalf("parse EXPLAIN json (%v): %s", err, raw)
	}
	var out []string
	var walk func(n explainNode)
	walk = func(n explainNode) {
		out = append(out, n.NodeType)
		for _, ch := range n.Plans {
			walk(ch)
		}
	}
	walk(doc[0].Plan)
	return out
}

// The since-inception cost fixture. The measured CAGGs' materialisation
// hypertables are re-chunked to 10 days for the fixture (they inherit
// 70 days from `trades`' 7-day interval, migration 0062 — MORE chunks
// is the adverse case for these reads, and 10 days keeps the seed
// small), so costFixtureDays of hourly trades spreads each over ~14
// chunks, and costFixturePairs markets trading every hour put ~9,600
// rows (a multi-level pair index of well over a hundred leaf pages)
// into each of them. That density is the point: an index PROBE and an
// every-row WALK are indistinguishable on a chunk whose whole index is
// one page.
const (
	costFixturePairs  = 40
	costFixtureDays   = 130
	costFixtureIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	// historyMaxPoints in internal/api/v1/history.go — the bucket limit
	// the anonymous series routes actually pass.
	costFixtureLimit = 50_000
	// Ceiling on shared buffers per (chunk, direction) for proving a
	// pair absent: one b-tree descent. Measured at 2.4–3.0 on this
	// fixture; 6 leaves slack for a taller tree or a right-link step
	// without admitting anything that scales with the chunk's rows (one
	// market's rows in one chunk are ~240 heap fetches here, the whole
	// chunk ~9,600).
	maxBuffersPerChunkProbe = 6
)

var costPlanModes = []string{"force_custom_plan", "force_generic_plan"}

// costReader is one anonymous-reachable series read that can be driven
// with NO lower time bound.
type costReader struct {
	name string
	view string // the CAGG it reads; also the capture needle
	call func(ctx context.Context, s *timescale.Store, p c.Pair) (int, error)
}

var costReaders = []costReader{
	{
		name: "HistoryPoints", view: "prices_1h",
		call: func(ctx context.Context, s *timescale.Store, p c.Pair) (int, error) {
			pts, err := s.HistoryPoints(ctx, p, timescale.Granularity1h, costFixtureLimit)
			return len(pts), err
		},
	},
	{
		name: "HistoryPointsInRange(no from/to)", view: "prices_1h",
		call: func(ctx context.Context, s *timescale.Store, p c.Pair) (int, error) {
			pts, err := s.HistoryPointsInRange(ctx, p, timescale.Granularity1h,
				time.Time{}, time.Time{}, costFixtureLimit)
			return len(pts), err
		},
	},
	{
		name: "TWAPPointsInRange(no from/to)", view: "twap_1h",
		call: func(ctx context.Context, s *timescale.Store, p c.Pair) (int, error) {
			pts, err := s.TWAPPointsInRange(ctx, p, timescale.Granularity1h,
				time.Time{}, time.Time{}, costFixtureLimit)
			return len(pts), err
		},
	},
}

// TestSeriesReadsUnknownPairCostIsBounded MEASURES the database work an
// anonymous caller buys by asking /v1/history/since-inception (or
// /v1/chart with no window) for a well-formed pair that has never
// traded (the "no literal lower bound" case).
//
// These reads have no lower time bound — that is what "since inception"
// means — so TimescaleDB cannot exclude a single chunk and every chunk
// of the CAGG appears in the plan. The question is whether that is a
// DB-burn lever. It is one only if the per-chunk work scales with the
// chunk's CONTENTS. This test pins that it does not: under the UNION ALL
// of single-direction branches each chunk is answered by an index probe
// that finds no entry — no filtered row, two or three index pages — so
// the whole read costs O(chunks), independent of how much history the
// chunks hold. Measured here: 68–84 shared buffers and under 1 ms for
// 14 chunks x 2 directions, against 5,597 buffers and all 124,800 rows
// fetched-then-discarded for the retired OR disjunction.
//
// What this test does NOT show is a saving from any gate in front of
// the read. There is none, and a gate that itself probes the hypertable
// buys nothing: the probe IS the per-chunk index descent measured here.
// Going below O(chunks) needs a lookup that does not touch the
// hypertable at all (a plain-table pair registry / first-seen bucket),
// which is a migration, not a change to this query.
//
// Two unknown pairs, because "unknown" is not one shape:
//
//   - neither asset exists anywhere (native/fiat:EUR here). The planner
//     can prove this empty from ANY index, so it is the easy case;
//   - both assets are heavily traded but never with EACH OTHER. This is
//     the adversarial one: a plan that drives a single-column
//     (base_asset, bucket) or (quote_asset, bucket) index and tests the
//     other column as a filter walks every row of a real market in
//     every chunk before it can answer "none".
//
// And two plan modes, because pgx prepares the statement and Postgres
// may switch a prepared statement to a GENERIC plan after five
// executions: a property that holds only under the custom plan holds
// for the first five requests per connection. (The retired OR form is
// exactly that case — cheap under a custom plan, a full walk under the
// generic one.)
//
// The statements measured are the PRODUCTION ones, not copies: the pool
// is pinned to a single connection, the Store method runs, and the text
// pgx prepared for it is read back out of pg_prepared_statements on that
// same session. A rewrite of a reader is therefore measured as written,
// and a copy in this file cannot drift from it.
func TestSeriesReadsUnknownPairCostIsBounded(t *testing.T) {
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
	// One connection: pgx's prepared-statement cache,
	// pg_prepared_statements and `SET plan_cache_mode` are all
	// per-session, so everything below must run on the same backend.
	db.SetMaxOpenConns(1)

	seedCostFixture(t, ctx, db)
	pairs := newCostPairs(t)
	assertRetiredORFormWalks(t, ctx, db, pairs)

	var captured []string
	for _, rd := range costReaders {
		chunks := caggChunkCount(t, ctx, db, rd.view)
		if chunks < 10 {
			t.Fatalf("%s materialised into %d chunks, want >= 10 — a bounded-cost claim about "+
				"a multi-chunk walk needs a multi-chunk fixture", rd.view, chunks)
		}
		// The production call, exactly as the route makes it.
		for _, p := range []c.Pair{pairs.neverSeen, pairs.neverPaired} {
			n, err := rd.call(ctx, store, p)
			if err != nil {
				t.Fatalf("%s(%s): %v", rd.name, p, err)
			}
			if n != 0 {
				t.Fatalf("%s(%s) returned %d buckets, want 0 — the fixture trades this pair", rd.name, p, n)
			}
		}
		// Needles name the read, not its current shape: a reader rewritten
		// into some other form must still be captured and MEASURED, not
		// slip past as "no such statement".
		stmt := capturePreparedStatement(t, ctx, db, captured, "FROM "+rd.view, "ORDER BY bucket ASC")
		captured = append(captured, stmt)

		for _, tc := range pairs.unknown() {
			for _, mode := range costPlanModes {
				got := explainPrepared(t, ctx, db, mode, stmt, costArgs(tc.pair))
				t.Logf("%s | %s | %s: %s", rd.name, tc.name, mode, got)
				assertProbeCost(t, rd.name+" / "+tc.name+" / "+mode, got, chunks)
			}
		}

		// Instrument check, buffers: the same statement over a pair that
		// DOES trade must cost far more than the probe ceiling allows.
		// If it does not, the walker is not reading buffers out of the
		// plan or the fixture is too thin to tell a probe from a read of
		// the rows, and the PASSes above mean nothing.
		control := explainPrepared(t, ctx, db, "force_custom_plan", stmt, costArgs(pairs.traded))
		t.Logf("%s | control: traded pair, full series: %s", rd.name, control)
		if ceiling := int64(2*chunks) * maxBuffersPerChunkProbe; control.buffers < 5*ceiling {
			t.Fatalf("%s: instrument check failed — reading a traded pair's whole series cost %d "+
				"buffers, under 5x the %d-buffer probe ceiling; the fixture no longer separates an "+
				"index probe from a read of the rows", rd.name, control.buffers, ceiling)
		}
	}
}

// assertRetiredORFormWalks is the instrument check for the
// rows-removed-by-filter detector, run on the known-bad case before any
// PASS is believed: the retired `(A AND B) OR (B AND A)` disjunction
// form of these readers. Under the generic
// plan a prepared statement settles into, it must show the walk this
// test exists to rule out — every row of the CAGG fetched and discarded
// — and [assertProbeCost]'s own bounds must reject it. The custom-plan
// runs are logged beside it for the record and not asserted: the
// planner rescues them with a BitmapOr, which is why the defect hid.
func assertRetiredORFormWalks(t *testing.T, ctx context.Context, db *sql.DB, pairs costPairs) {
	t.Helper()
	const orForm = `
		SELECT bucket, base_asset, vwap::text, COALESCE(volume, 0)::text, volume_usd::text
		  FROM prices_1h
		 WHERE ((base_asset = $1 AND quote_asset = $2)
		     OR (base_asset = $2 AND quote_asset = $1))
		   AND bucket <= now() - INTERVAL '1 hour'
		 ORDER BY bucket ASC
		 LIMIT $3`
	var fixtureRows int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM prices_1h`).Scan(&fixtureRows); err != nil {
		t.Fatalf("count prices_1h: %v", err)
	}
	chunks := caggChunkCount(t, ctx, db, "prices_1h")
	t.Logf("fixture: %d prices_1h rows over %d chunks", fixtureRows, chunks)
	for _, tc := range pairs.unknown() {
		for _, mode := range costPlanModes {
			got := explainPrepared(t, ctx, db, mode, orForm, costArgs(tc.pair))
			t.Logf("retired OR disjunction | %s | %s: %s", tc.name, mode, got)
			if mode != "force_generic_plan" {
				continue
			}
			ceiling := int64(got.chunkScans) * maxBuffersPerChunkProbe
			if got.rowsRemovedByFilter < fixtureRows || got.buffers <= ceiling {
				t.Fatalf("instrument check failed: the retired OR disjunction (%s, generic plan) "+
					"filtered %d of %d rows over %d buffers (probe ceiling %d) — it no longer shows "+
					"the every-row walk, so this fixture cannot tell a bounded read from an unbounded one",
					tc.name, got.rowsRemovedByFilter, fixtureRows, got.buffers, ceiling)
			}
		}
	}
}

// assertProbeCost pins the bounded-cost property for one measured run.
func assertProbeCost(t *testing.T, what string, got planCost, chunks int) {
	t.Helper()
	if got.chunkScans == 0 {
		t.Fatalf("%s: EXPLAIN shows no executed chunk scan — the plan walker is not seeing the plan", what)
	}
	if got.chunkScans > 2*chunks {
		t.Errorf("%s: %d chunk scans for %d chunks x 2 directions — a chunk is being scanned twice",
			what, got.chunkScans, chunks)
	}
	if got.seqScans != 0 {
		t.Errorf("%s: %d sequential chunk scan(s) (%v) — proving a pair absent must be an index "+
			"probe per chunk", what, got.seqScans, got.scanTypes)
	}
	if got.rowsRemovedByFilter != 0 {
		t.Errorf("%s: %d rows were fetched and then discarded by a filter — the read is walking "+
			"another market's rows, so its cost scales with the chunks' contents",
			what, got.rowsRemovedByFilter)
	}
	if ceiling := int64(got.chunkScans) * maxBuffersPerChunkProbe; got.buffers > ceiling {
		t.Errorf("%s: %d shared buffers over %d chunk scans, want <= %d (%d per probe: one "+
			"b-tree descent) — the per-chunk cost is no longer O(1)",
			what, got.buffers, got.chunkScans, ceiling, maxBuffersPerChunkProbe)
	}
}

type costPairs struct {
	neverSeen   c.Pair // neither asset appears in the fixture
	neverPaired c.Pair // both assets heavily traded, never against each other
	traded      c.Pair // a real fixture market
}

type namedCostPair struct {
	name string
	pair c.Pair
}

func (p costPairs) unknown() []namedCostPair {
	return []namedCostPair{
		{"neither asset exists", p.neverSeen},
		{"both assets traded, never together", p.neverPaired},
	}
}

func newCostPairs(t *testing.T) costPairs {
	t.Helper()
	eur, err := c.NewFiatAsset("EUR")
	if err != nil {
		t.Fatal(err)
	}
	// FILL01 is the base of a USDC-quoted market; native is the quote of
	// markets FILL21..FILL40. Both are everywhere; FILL01/native is not.
	fill01, err := c.NewClassicAsset("FILL01", costFixtureIssuer)
	if err != nil {
		t.Fatal(err)
	}
	usdc, err := c.NewClassicAsset("USDC", costFixtureIssuer)
	if err != nil {
		t.Fatal(err)
	}
	var out costPairs
	if out.neverSeen, err = c.NewPair(c.NativeAsset(), eur); err != nil {
		t.Fatal(err)
	}
	if out.neverPaired, err = c.NewPair(fill01, c.NativeAsset()); err != nil {
		t.Fatal(err)
	}
	if out.traded, err = c.NewPair(fill01, usdc); err != nil {
		t.Fatal(err)
	}
	return out
}

// costArgs is the argument list every measured reader binds: the pair,
// then the 2n+1 row cap its bucket limit becomes (bucketRowCap).
func costArgs(p c.Pair) []any {
	return []any{p.Base.String(), p.Quote.String(), 2*costFixtureLimit + 1}
}

// seedCostFixture writes one trade per hour per market for
// costFixtureDays, ending 10 days back so every bucket is closed, and
// materialises the measured CAGGs over it. Markets FILL01..FILL20 quote
// in USDC and FILL21..FILL40 in native, so every asset the adversarial
// unknown pair names is heavily traded — just never against the other.
func seedCostFixture(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, view := range []string{"prices_1h", "twap_1h"} {
		var matTable string
		if err := db.QueryRowContext(ctx, `
			SELECT format('%I.%I', materialization_hypertable_schema, materialization_hypertable_name)
			  FROM timescaledb_information.continuous_aggregates
			 WHERE view_name = $1`, view).Scan(&matTable); err != nil {
			t.Fatalf("resolve %s materialisation hypertable: %v", view, err)
		}
		if _, err := db.ExecContext(ctx,
			`SELECT set_chunk_time_interval($1::regclass, INTERVAL '10 days')`, matTable); err != nil {
			t.Fatalf("re-chunk %s: %v", matTable, err)
		}
	}
	end := time.Now().UTC().Add(-10 * 24 * time.Hour).Truncate(time.Hour)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO trades
		    (source, ledger, tx_hash, op_index, ts,
		     base_asset, quote_asset, base_amount, quote_amount, usd_volume)
		SELECT 'sdex',
		       70000000 + h,
		       lpad(to_hex(h::bigint * 1000 + p), 64, '0'),
		       0,
		       $1::timestamptz - make_interval(hours => h),
		       'FILL' || lpad(p::text, 2, '0') || '-' || $2::text,
		       CASE WHEN p <= $4::int / 2 THEN 'USDC-' || $2::text ELSE 'native' END,
		       1::numeric, 2::numeric, 2::numeric
		  FROM generate_series(1, $3::int) AS h,
		       generate_series(1, $4::int) AS p`,
		end, costFixtureIssuer, costFixtureDays*24, costFixturePairs,
	); err != nil {
		t.Fatalf("seed cost fixture: %v", err)
	}
	// twap_1h is hierarchical over prices_1m, so that refreshes first.
	for _, view := range []string{"prices_1m", "prices_1h", "twap_1h"} {
		if _, err := db.ExecContext(ctx,
			`CALL refresh_continuous_aggregate($1::regclass, NULL, NULL)`, view); err != nil {
			t.Fatalf("refresh %s: %v", view, err)
		}
	}
	// Planner statistics for the freshly materialised chunks: without
	// them the plans below are chosen on default estimates, which is
	// not the state any served database is in.
	if _, err := db.ExecContext(ctx, `ANALYZE`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

func caggChunkCount(t *testing.T, ctx context.Context, db *sql.DB, view string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*)
		  FROM timescaledb_information.chunks ch
		  JOIN timescaledb_information.continuous_aggregates ca
		    ON ca.materialization_hypertable_schema = ch.hypertable_schema
		   AND ca.materialization_hypertable_name   = ch.hypertable_name
		 WHERE ca.view_name = $1`, view).Scan(&n); err != nil {
		t.Fatalf("count %s chunks: %v", view, err)
	}
	return n
}

// capturePreparedStatement returns the text of the one statement this
// session has prepared that mentions every needle and is not already in
// `seen`. database/sql via pgx prepares each distinct query once per
// connection, and pg_prepared_statements keeps the full text
// (pg_stat_activity would truncate it at track_activity_query_size).
func capturePreparedStatement(t *testing.T, ctx context.Context, db *sql.DB, seen []string, needles ...string) string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT statement FROM pg_prepared_statements`)
	if err != nil {
		t.Fatalf("read pg_prepared_statements: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var found []string
	for rows.Next() {
		var stmt string
		if err := rows.Scan(&stmt); err != nil {
			t.Fatalf("scan pg_prepared_statements: %v", err)
		}
		// Never this file's own probes: a `PREPARE … AS <stmt>` or an
		// EXPLAIN wrapper carries the production text inside it.
		head := strings.ToUpper(strings.TrimSpace(stmt))
		match := !strings.HasPrefix(head, "PREPARE") && !strings.HasPrefix(head, "EXPLAIN") &&
			!strings.Contains(stmt, "pg_prepared_statements") && !slices.Contains(seen, stmt)
		for _, n := range needles {
			match = match && strings.Contains(stmt, n)
		}
		if match {
			found = append(found, stmt)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("pg_prepared_statements rows: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("captured %d new prepared statements matching %v, want exactly 1 — the production "+
			"statement was not prepared on this session, so there is nothing honest to measure: %q",
			len(found), needles, found)
	}
	return found[0]
}

// planCost is what one EXPLAIN (ANALYZE, BUFFERS) run cost, reduced to
// the properties the bounded-cost claim is made of.
type planCost struct {
	buffers             int64 // shared hit + read, whole statement (the root node is inclusive)
	chunkScans          int   // executed scan nodes over a _hyper_*_chunk relation
	seqScans            int   // … of which are sequential
	rowsRemovedByFilter int64
	planningMS          float64
	executionMS         float64
	scanTypes           map[string]int
	indexes             map[string]int // index name with the per-chunk prefix stripped
}

func (p planCost) String() string {
	b, _ := json.Marshal(map[string]any{
		"shared_buffers": p.buffers, "chunk_scans": p.chunkScans, "seq_scans": p.seqScans,
		"rows_removed_by_filter": p.rowsRemovedByFilter, "scan_types": p.scanTypes,
		"indexes": p.indexes, "planning_ms": p.planningMS, "execution_ms": p.executionMS,
	})
	return string(b)
}

type explainNode struct {
	NodeType            string        `json:"Node Type"`
	RelationName        string        `json:"Relation Name"`
	IndexName           string        `json:"Index Name"`
	ActualLoops         float64       `json:"Actual Loops"`
	SharedHitBlocks     int64         `json:"Shared Hit Blocks"`
	SharedReadBlocks    int64         `json:"Shared Read Blocks"`
	RowsRemovedByFilter int64         `json:"Rows Removed by Filter"`
	Plans               []explainNode `json:"Plans"`
}

// explainPrepared runs stmt through PREPARE / EXPLAIN (ANALYZE, BUFFERS)
// EXECUTE under the given plan_cache_mode, twice — the first run pays
// for loading the chunks' catalog and relcache entries, which is not
// the statement's own cost — and returns the second.
//
// PREPARE + EXECUTE rather than a parameterised EXPLAIN because only a
// prepared statement has a generic plan to force. EXECUTE is a utility
// statement and takes no bind parameters, so the arguments are
// rendered as literals; they are this test's own asset ids and ints.
func explainPrepared(t *testing.T, ctx context.Context, db *sql.DB, planMode, stmt string, args []any) planCost {
	t.Helper()
	const name = "k008_cost_probe"
	mustExec := func(q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`SET plan_cache_mode = ` + planMode)
	mustExec(`PREPARE ` + name + ` AS ` + stmt)
	defer mustExec(`DEALLOCATE ` + name)

	lits := make([]string, len(args))
	for i, a := range args {
		switch v := a.(type) {
		case string:
			lits[i] = "'" + strings.ReplaceAll(v, "'", "''") + "'"
		case int:
			lits[i] = fmt.Sprintf("%d", v)
		default:
			t.Fatalf("explainPrepared: unsupported arg type %T", a)
		}
	}
	q := `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE ` + name + `(` + strings.Join(lits, ", ") + `)`
	var raw string
	for range 2 {
		if err := db.QueryRowContext(ctx, q).Scan(&raw); err != nil {
			t.Fatalf("EXPLAIN ANALYZE: %v", err)
		}
	}
	return parsePlanCost(t, raw)
}

func parsePlanCost(t *testing.T, raw string) planCost {
	t.Helper()
	var doc []struct {
		Plan          explainNode `json:"Plan"`
		PlanningTime  float64     `json:"Planning Time"`
		ExecutionTime float64     `json:"Execution Time"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil || len(doc) != 1 {
		t.Fatalf("parse EXPLAIN json (%v): %s", err, raw)
	}
	out := planCost{
		buffers:     doc[0].Plan.SharedHitBlocks + doc[0].Plan.SharedReadBlocks,
		planningMS:  doc[0].PlanningTime,
		executionMS: doc[0].ExecutionTime,
		scanTypes:   map[string]int{},
		indexes:     map[string]int{},
	}
	var walk func(n explainNode)
	walk = func(n explainNode) {
		if strings.Contains(n.RelationName, "_hyper_") && n.ActualLoops > 0 {
			out.chunkScans++
			out.scanTypes[n.NodeType]++
			if n.NodeType == "Seq Scan" {
				out.seqScans++
			}
		}
		if n.IndexName != "" && n.ActualLoops > 0 {
			// _hyper_<h>_<c>_chunk_<index> → <index>, so the per-chunk
			// copies of one index count together.
			name := n.IndexName
			if i := strings.Index(name, "_chunk_"); i >= 0 {
				name = name[i+len("_chunk_"):]
			}
			out.indexes[name]++
		}
		out.rowsRemovedByFilter += n.RowsRemovedByFilter
		for _, ch := range n.Plans {
			walk(ch)
		}
	}
	walk(doc[0].Plan)
	return out
}

// TestResumeStalled_ArmsUSDVolumeResolution is the
// proven-red test. The resume-stalled recovery tool marches stalled cursors
// through the SAME runBackfillChunk trade-write path as the main `backfill`
// subcommand, so the store it opens must be armed for a trade-writing
// re-derive — a POSITIVE derive generation (so a corrected re-derive wins the
// writers' ON CONFLICT guard) AND the USD-volume resolvers (so on-chain DEX
// trades resolve a real usd_volume instead of NULL). Opened raw, the store
// skips this wiring, so a re-derived on-chain DEX trade lands with
// usd_volume=NULL at gen 0 and the reDeriveNullVolumeGuard is inert (it only
// fires once the generation is positive).
//
// This exercises the exact seam resume-stalled now runs after timescale.Open
// — [ingest.ArmTradeWriteStore] — then inserts an on-chain DEX (sdex) trade
// quoted in a USD-pegged classic asset and asserts the tier-1 usd_volume lands
// non-NULL with the correct value.
//
// To reproduce the red state: revert the body of [ingest.ArmTradeWriteStore]
// to `return nil` (the broken behaviour: no SetDeriveGeneration, no
// InstallUSDVolumeResolution). InsertTrade then computes usd_volume=NULL and
// the non-NULL assertion goes red.
func TestResumeStalled_ArmsUSDVolumeResolution(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const usdcIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdc, err := c.NewClassicAsset("USDC", usdcIssuer)
	if err != nil {
		t.Fatal(err)
	}
	xlm, err := c.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatal(err)
	}
	xlmUSDC, _ := c.NewPair(xlm, usdc)

	// Config exactly as resume-stalled receives it from
	// parseResumeStalledFlags: USDC declared as a USD-pegged classic asset
	// so tier-1 prices a DEX trade quoted in it with no FX/CAGG lookup.
	var cfg config.Config
	cfg.Trades.USDPeggedClassicAssets = []string{"USDC-" + usdcIssuer}

	// The wiring under test — the single call resume-stalled now makes
	// right after timescale.Open.
	if err := ingest.ArmTradeWriteStore(store, cfg); err != nil {
		t.Fatalf("ArmTradeWriteStore: %v", err)
	}

	// An on-chain DEX trade: 100 XLM for 108.5 USDC (classic 7-dec).
	// tier-1 usd_volume = 1_085_000_000 / 1e7 = 108.5.
	ts := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	trade := mkIntegrationTrade("sdex", 1, ts, xlmUSDC, 1_000_000_000, 1_085_000_000)
	if err := store.InsertTrade(ctx, trade); err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}

	var uv sql.NullString
	const q = `SELECT usd_volume::text FROM trades WHERE source = $1 AND ledger = $2`
	if err := store.DB().QueryRowContext(ctx, q, "sdex", trade.Ledger).Scan(&uv); err != nil {
		t.Fatalf("read usd_volume: %v", err)
	}
	if !uv.Valid {
		t.Fatal("usd_volume = NULL — resume-stalled wrote a DEX trade without " +
			"USD-volume resolution installed (CWR-1 regression)")
	}
	if uv.String != "108.50000000" && uv.String != "108.5" {
		t.Errorf("usd_volume = %q, want 108.5 (tier-1 quote = 1_085_000_000 / 1e7)", uv.String)
	}
}

// TestSorobanVolume24hUSD_XLMAnchored proves the XLM-anchored Soroban volume end-to-end: a
// pure-Soroban SEP-41 token whose liquidity is quoted in XLM gets a REAL
// trailing-24h USD volume from Store.SorobanVolume24hUSDForAsset, while
// the plain Store.Volume24hUSDForAsset (which only sees the insert-time
// usd_volume) reports just the USD-pegged leg.
//
// Fixture (all in one closed 1-minute bucket ~2h back):
//   - native/USDC  vwap 0.5   → the XLM→USD anchor (1 XLM = 0.5 USD)
//   - token/XLM    20 XLM quoted → XLM-quote leg  → 20 * 0.5 = 10 USD
//   - XLM/token    10 XLM based  → XLM-base leg   → 10 * 0.5 =  5 USD
//   - token/USDC    7 USDC       → USD-pegged leg → usd_volume = 7 USD
//
// SorobanVolume24hUSDForAsset(token) = 10 + 5 + 7 = 22
// Volume24hUSDForAsset(token)        =            7   (USD-pegged leg only)
func TestSorobanVolume24hUSD_XLMAnchored(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Recognise classic USDC as a USD peg so the token/USDC leg lands a
	// non-null usd_volume, which the anchored reader takes as-is.
	spec, err := timescale.NewUSDVolumeQuoteSpec(
		[]string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"}, nil)
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	store.SetUSDVolumeQuoteSpec(spec)

	xlm := c.NativeAsset()
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	token, err := c.NewSorobanAsset("CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN")
	if err != nil {
		t.Fatal(err)
	}

	xlmUSDC, _ := c.NewPair(xlm, usdc)   // anchor: vwap = 0.5
	tokenXLM, _ := c.NewPair(token, xlm) // XLM-quote leg
	xlmToken, _ := c.NewPair(xlm, token) // XLM-base leg
	tokenUSDC, _ := c.NewPair(token, usdc)

	ts := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)

	trades := []c.Trade{
		// Anchor: 100 XLM (stroops) traded for 50 USDC (stroops) → vwap 0.5.
		mkIntegrationTrade("soroswap", 1, ts, xlmUSDC, 1_000_000_000, 500_000_000),
		// token/XLM: 20 XLM on the quote side (usd_volume NULL — XLM quote).
		mkIntegrationTrade("soroswap", 2, ts, tokenXLM, 500, 200_000_000),
		// XLM/token: 10 XLM on the base side (usd_volume NULL — XLM base).
		mkIntegrationTrade("soroswap", 3, ts, xlmToken, 100_000_000, 300),
		// token/USDC: 7 USDC quote → usd_volume = 70_000_000 / 1e7 = 7.
		mkIntegrationTrade("soroswap", 4, ts, tokenUSDC, 900, 70_000_000),
	}
	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %d: %v", tr.Ledger, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	// Plain reader: only the USD-pegged token/USDC leg contributes.
	plain, _, err := store.Volume24hUSDForAsset(ctx, token.String())
	if err != nil {
		t.Fatalf("Volume24hUSDForAsset: %v", err)
	}
	if got := mustFloat(t, plain); got < 6.99 || got > 7.01 {
		t.Errorf("plain Volume24hUSDForAsset = %s (%.4f), want ~7 (USD-pegged leg only)", plain, got)
	}

	// XLM-anchored reader: USD-pegged leg (7) + XLM legs (10 + 5) = 22.
	anchored, _, err := store.SorobanVolume24hUSDForAsset(ctx, token.String())
	if err != nil {
		t.Fatalf("SorobanVolume24hUSDForAsset: %v", err)
	}
	if got := mustFloat(t, anchored); got < 21.99 || got > 22.01 {
		t.Errorf("SorobanVolume24hUSDForAsset = %s (%.4f), want ~22 (7 pegged + 10 + 5 XLM-anchored)", anchored, got)
	}
}

// TestSorobanVolume24hUSD_EmptyReturnsZero — an asset with no trades in the
// window returns "0" (not an error, not null) — same contract as the plain
// reader, so the asset-detail path can present a definite figure.
func TestSorobanVolume24hUSD_EmptyReturnsZero(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	token, err := c.NewSorobanAsset("CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN")
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := store.SorobanVolume24hUSDForAsset(ctx, token.String())
	if err != nil {
		t.Fatalf("SorobanVolume24hUSDForAsset: %v", err)
	}
	if got != "0" {
		t.Errorf("empty asset volume = %q, want \"0\"", got)
	}
}

// TestSorobanVolume24hUSD_PartlyValuedBucket pins per-trade valuation: a
// prices_1m bucket where one trade carries an insert-time usd_volume and a
// same-minute trade on the same pair does not (its FX lookup failed at
// insert) must value the second trade through the XLM leg, not report the
// first trade's figure for the whole bucket.
//
// Fixture (one closed 1-minute bucket ~2h back, anchor 1 XLM = 0.5 USD):
//   - token/XLM 10 XLM quoted, usd_volume = 5 (valued at insert)
//   - token/XLM 20 XLM quoted, usd_volume NULL → 20 * 0.5 = 10
//
// SorobanVolume24hUSDForAsset(token) = 5 + 10 = 15
func TestSorobanVolume24hUSD_PartlyValuedBucket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	xlm := c.NativeAsset()
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	token, err := c.NewSorobanAsset("CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN")
	if err != nil {
		t.Fatal(err)
	}
	xlmUSDC, _ := c.NewPair(xlm, usdc)
	tokenXLM, _ := c.NewPair(token, xlm)

	ts := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	valued := mkIntegrationTrade("aquarius", 2, ts, tokenXLM, 400, 100_000_000)
	for _, tr := range []c.Trade{
		mkIntegrationTrade("soroswap", 1, ts, xlmUSDC, 1_000_000_000, 500_000_000), // anchor vwap 0.5
		valued,
		mkIntegrationTrade("soroswap", 3, ts.Add(20*time.Second), tokenXLM, 800, 200_000_000),
	} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %d: %v", tr.Ledger, err)
		}
	}
	// No FX resolver is wired, so every row lands NULL; value the one row
	// the way a successful insert-time tier would have.
	res, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 5 WHERE source = $1 AND ledger = $2`, valued.Source, valued.Ledger)
	if err != nil {
		t.Fatalf("value one trade: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("valued %d rows, want 1", n)
	}
	// The anchor reads only minutes with USD volume behind them.
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 50 WHERE base_asset = 'native' AND quote_asset LIKE 'USDC-%'`); err != nil {
		t.Fatalf("value the anchor trade: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	got, _, err := store.SorobanVolume24hUSDForAsset(ctx, token.String())
	if err != nil {
		t.Fatalf("SorobanVolume24hUSDForAsset: %v", err)
	}
	if f := mustFloat(t, got); f < 14.99 || f > 15.01 {
		t.Errorf("SorobanVolume24hUSDForAsset = %s (%.4f), want 15 (5 valued + 20 XLM * 0.5)", got, f)
	}
}

// TestXLMSacAsBase_PriceableThroughEveryPath reproduces the r1
// firing of stellarindex_assets_popular_priceless=2
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
// ZDIR's two directions disagree and the inverted row is fresher. The
// headline price (listing, detail) takes the newest traded minute in
// either direction, so the inverted row wins there; the price-history
// series still prefer a bucket's base-side row.
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
	// says 1.5 XLM. The headline price is the newer minute → 0.60 USD.
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

	t.Run("TransitiveUSDPriceCandidates", func(t *testing.T) {
		// CBIJ: hop is the XLM SAC itself (its only XLM market is
		// SAC-as-base), which must resolve to xlm_usd. CAUP7 has no USD
		// route of its own, so it is not a candidate.
		tps, err := store.TransitiveUSDPriceCandidates(ctx, cbijID)
		if err != nil {
			t.Fatalf("TransitiveUSDPriceCandidates(CBIJ): %v", err)
		}
		if len(tps) != 1 {
			t.Errorf("TransitiveUSDPriceCandidates(CBIJ) = %+v, want one route: 0.10 via hop %s", tps, c.XLMSacContractID)
		} else {
			tp := tps[0]
			if tp.Hop != c.XLMSacContractID {
				t.Errorf("CBIJ hop = %s, want the XLM SAC %s", tp.Hop, c.XLMSacContractID)
			}
			if !ratEq(tp.PriceUSD, "0.10") {
				t.Errorf("CBIJ transitive price = %s, want 0.10", tp.PriceUSD)
			}
		}
		// CAUP7: hop CBIJ, whose own XLM market is SAC-as-base.
		tps, err = store.TransitiveUSDPriceCandidates(ctx, caup7ID)
		if err != nil {
			t.Fatalf("TransitiveUSDPriceCandidates(CAUP7): %v", err)
		}
		if len(tps) != 1 {
			t.Errorf("TransitiveUSDPriceCandidates(CAUP7) = %+v, want one route: 0.20 via hop CBIJ", tps)
		} else {
			tp := tps[0]
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

	t.Run("AssetIsPriced", func(t *testing.T) {
		// The single-asset probe must answer exactly as the sweep does:
		// the assets the sweep no longer reports are priced, an id nothing
		// ever traded is not.
		for _, id := range []string{cbijID, caup7ID} {
			priced, err := store.AssetIsPriced(ctx, id)
			if err != nil {
				t.Fatalf("AssetIsPriced(%s): %v", id, err)
			}
			if !priced {
				t.Errorf("AssetIsPriced(%s) = false; the sweep prices it through the XLM SAC", id)
			}
		}
		priced, err := store.AssetIsPriced(ctx, "NEVER-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
		if err != nil {
			t.Fatalf("AssetIsPriced(never): %v", err)
		}
		if priced {
			t.Error("AssetIsPriced(never) = true for an asset with no market")
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
			{zdir.String(), "0.6000000000"},
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
	// XLM leg in EITHER stored direction must carry a point. The four series CTEs must not read the XLM leg base-side
	// only, or CBIJ and ZINV had a headline price but an EMPTY series.
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
			{zdir.String(), "0.6000000000"},
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
