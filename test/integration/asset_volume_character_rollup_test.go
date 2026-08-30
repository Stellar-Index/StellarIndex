//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
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
	// single (acct-01, issuer) pair with the issuer as taker; market-styled
	// (fiat:USD counterpart) → concentrated.
	for i := 0; i < 9; i++ {
		insertCharTrade(t, ctx, db, next(), ts, audID, "fiat:USD", "acct-01", audIssuer, 1000)
	}
	insertCharTrade(t, ctx, db, next(), ts, audID, "fiat:USD", "acct-02", "acct-03", 100)

	// (2) AUDD wrap/redeem corridor: issuer-side (maker == issuer) on a
	// NON-market-styled sibling pair (AUDR) → operational.
	for i := 0; i < 5; i++ {
		insertCharTrade(t, ctx, db, next(), ts, auddID, audrID, auddIssuer, "acct-04", 2000)
	}

	// (3) Healthy market asset: six distinct account pairs on fiat:USD, no
	// pair dominant, issuer uninvolved → market.
	marketPairs := [][2]string{
		{"acct-05", "acct-06"},
		{"acct-07", "acct-08"},
		{"acct-09", "acct-10"},
		{"acct-11", "acct-12"},
		{"acct-13", "acct-14"},
		{"acct-15", "acct-16"},
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
			// double sums), character/counts exactly, volume at the 2dp wire
			// precision.
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
			if rw, pw := fmt2(roll.VolumeUSD), fmt2(per.VolumeUSD); rw != pw {
				t.Errorf("volume_usd (2dp): rollup=%s per-asset=%s", rw, pw)
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
		`INSERT INTO asset_volume_24h (asset_id, vol_usd, computed_at) VALUES ($1, $2, now())`,
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

func fmt2(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }
