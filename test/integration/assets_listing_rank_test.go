//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// /v1/assets ranking + keyset pagination against a real Timescale (#356).
//
// The defect: the scam gate withheld a directory-flagged issuer's price
// and market cap but the ORDERING never followed, so JFKBANK2 —
// `malicious`/`unsafe`, no price, no market cap, no %-changes — sat at
// #12 on the live /assets page above USDV, MJQ and BRAVO purely on
// $62.32K of 24h volume. These tests pin the two halves of the fix that
// only a real database can prove:
//
//  1. rank_tier really demotes (SQL EXISTS over account_directory.tags,
//     lowercased on both sides), and demotes a flagged asset even when it
//     IS priced and has the highest volume in the set; and
//  2. the keyset cursor still walks the whole set exactly once now that
//     the ORDER BY has a new LEADING key — the failure mode the file's own
//     comment warns about ("the keyset cursor must encode the same value
//     the ORDER BY ranks on, or pagination skips or repeats rows").

// rankAsset is one seeded listing row.
type rankAsset struct {
	code     string
	issuer   string
	volUSD   string // raw asset_volume_24h.vol_usd
	priceUSD string // "" → seed no trade, so price_usd comes back NULL
	obsCount int64
	tags     []string // account_directory tags for the issuer ("" set → no row)
}

// seedRankAsset inserts the classic_assets spine row with an explicit
// observation_count (the seedDirectoryAsset helper hardcodes 10, which
// gives the observation-count order nothing to rank on).
func seedRankAsset(t *testing.T, ctx context.Context, db *sql.DB, assetID, code, issuer string, obsCount int64) {
	t.Helper()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO classic_assets
		 (asset_id, code, issuer_g_strkey, slug, first_seen_at, first_seen_ledger,
		  last_seen_at, last_seen_ledger, observation_count)
		 VALUES ($1, $2, $3, $1, now(), 1, now(), 2, $4)`,
		assetID, code, issuer, obsCount,
	); err != nil {
		t.Fatalf("seed classic_assets %s: %v", assetID, err)
	}
}

func seedDirectoryTags(t *testing.T, ctx context.Context, db *sql.DB, address, name string, tags []string) {
	t.Helper()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO account_directory (address, name, domain, tags, source, synced_at)
		 VALUES ($1, $2, '', $3, 'stellar-expert', now())`,
		address, name, tags,
	); err != nil {
		t.Fatalf("seed account_directory %s: %v", address, err)
	}
}

// seedUSDPrice gives the asset a direct fiat:USD market so the listing's
// price_usd is non-NULL. base_amount 1 / quote_amount P makes the CAGG's
// volume-weighted quote/base ratio exactly P.
func seedUSDPrice(t *testing.T, ctx context.Context, db *sql.DB, nonce int, assetID, price string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO trades
		    (source, ledger, tx_hash, op_index, ts,
		     base_asset, quote_asset, base_amount, quote_amount, usd_volume)
		VALUES ('sdex', $1, $2, 0, now() - INTERVAL '1 hour',
		        $3, 'fiat:USD', 1::numeric, $4::numeric, 1::numeric)`,
		60_000_000+nonce, fmt.Sprintf("%064x", 900_000+nonce), assetID, price,
	); err != nil {
		t.Fatalf("seed USD trade for %s: %v", assetID, err)
	}
}

// seedRankFixture materialises the shared scenario and returns the
// canonical asset_id per code.
func seedRankFixture(t *testing.T, ctx context.Context, db *sql.DB, assets []rankAsset) map[string]string {
	t.Helper()
	ids := make(map[string]string, len(assets))
	for i, a := range assets {
		id := mustClassicID(t, a.code, a.issuer)
		ids[a.code] = id
		seedRankAsset(t, ctx, db, id, a.code, a.issuer, a.obsCount)
		seedRawVolume(t, ctx, db, id, a.volUSD)
		// Every asset is an honest `market` so the §4-B concentration
		// demote is a no-op here and rank_tier is the only thing moving
		// rows — otherwise a passing test could be crediting the wrong
		// mechanism.
		seedCharacter(t, ctx, db, id, timescale.VolumeCharacterMarket, 0.10)
		if len(a.tags) > 0 {
			seedDirectoryTags(t, ctx, db, a.issuer, a.code+" issuer", a.tags)
		}
		if a.priceUSD != "" {
			seedUSDPrice(t, ctx, db, i, id, a.priceUSD)
		}
	}
	if _, err := db.ExecContext(ctx,
		"CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)"); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}
	return ids
}

// rankFixture is the scenario both tests share.
//
// Raw 24h volume orders the set FLAGA > NOPRC > GOODA > GOODB > FLAGB >
// GOODC, which is exactly the pre-fix ranking. FLAGA is deliberately
// BOTH flagged and priced and the highest-volume row in the set: if the
// test only used an unpriced flagged asset, the unpriced tier alone would
// carry it and the scam demotion would go unproven.
var rankFixture = []rankAsset{
	{
		code: "FLAGA", issuer: audIssuer, volUSD: "500000", priceUSD: "2.5", obsCount: 6000,
		tags: []string{"malicious", "unsafe"},
	},
	{
		code: "FLAGB", issuer: auddIssuer, volUSD: "9000", obsCount: 5000,
		tags: []string{"UNSAFE"},
	}, // upper-case: the SQL lowercases like the Go predicate
	{
		code: "GOODA", issuer: goodIssuer, volUSD: "100000", priceUSD: "1.0", obsCount: 4000,
		tags: []string{"anchor", "issuer"},
	}, // benign tags must NOT demote
	{code: "GOODB", issuer: realIssuer, volUSD: "50000", priceUSD: "0.5", obsCount: 3000},
	{code: "GOODC", issuer: audrIssuer, volUSD: "1000", priceUSD: "0.25", obsCount: 2000},
	{code: "NOPRC", issuer: washIssuer, volUSD: "200000", obsCount: 1000},
}

func listRankOrder(t *testing.T, ctx context.Context, store *timescale.Store, order timescale.AssetsOrder, limit int) []timescale.AssetRow {
	t.Helper()
	rows, err := store.ListAssetsExt(ctx, timescale.ListAssetsOptions{Order: order, Limit: limit})
	if err != nil {
		t.Fatalf("ListAssetsExt: %v", err)
	}
	return rows
}

func codesOf(rows []timescale.AssetRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Code)
	}
	return out
}

// TestAssetsListing_FlaggedAndUnpricedDemotion is the #356 red test: a
// directory-flagged asset must not outrank ANY unflagged one, and an
// unpriced asset must not outrank a priced one, under the default
// volume-desc listing sort.
func TestAssetsListing_FlaggedAndUnpricedDemotion(t *testing.T) {
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

	seedRankFixture(t, ctx, db, rankFixture)

	rows := listRankOrder(t, ctx, store, timescale.AssetsOrderVolume24hUSDDesc, 100)
	got := codesOf(rows)

	// Tier 0 (unflagged + priced) by adjusted volume desc, then tier 1
	// (unflagged, no price), then tier 2 (flagged) by volume desc.
	want := []string{"GOODA", "GOODB", "GOODC", "NOPRC", "FLAGA", "FLAGB"}
	if len(got) != len(want) {
		t.Fatalf("listing returned %v, want all %d seeded rows %v (annotate + demote never HIDES a row)", got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("listing order = %v, want %v", got, want)
		}
	}

	byCode := map[string]timescale.AssetRow{}
	for _, r := range rows {
		byCode[r.Code] = r
	}
	// The headline of #356: the flagged asset with the HIGHEST volume in
	// the set sits below the lowest-volume unflagged priced asset.
	if pos(got, "FLAGA") < pos(got, "GOODC") {
		t.Errorf("FLAGA ($500k, malicious/unsafe) at %d must rank BELOW GOODC ($1k, unflagged, priced) at %d",
			pos(got, "FLAGA"), pos(got, "GOODC"))
	}
	// ...and it is demoted for being FLAGGED, not for being unpriced:
	// FLAGA carries a price of its own in the row.
	if byCode["FLAGA"].PriceUSD == nil {
		t.Error("FLAGA must be PRICED in the row — otherwise this test only proves the unpriced tier, not the scam demotion")
	}
	if byCode["FLAGA"].RankTier == nil || *byCode["FLAGA"].RankTier != 2 {
		t.Errorf("FLAGA rank_tier = %v, want 2 (directory-flagged)", byCode["FLAGA"].RankTier)
	}
	// Case-insensitive tag match, matching pricingguard.IsDirectoryScamFlagged.
	if byCode["FLAGB"].RankTier == nil || *byCode["FLAGB"].RankTier != 2 {
		t.Errorf("FLAGB rank_tier = %v, want 2 (tag 'UNSAFE' must match case-insensitively)", byCode["FLAGB"].RankTier)
	}
	// A benign directory label (anchor/issuer) must not demote anything.
	if byCode["GOODA"].RankTier == nil || *byCode["GOODA"].RankTier != 0 {
		t.Errorf("GOODA rank_tier = %v, want 0 — benign directory tags (anchor, issuer) must never demote", byCode["GOODA"].RankTier)
	}
	// An unpriced asset does not outrank a priced one, however big its volume.
	if pos(got, "NOPRC") < pos(got, "GOODC") {
		t.Errorf("NOPRC ($200k, no USD price) at %d must rank BELOW GOODC ($1k, priced) at %d",
			pos(got, "NOPRC"), pos(got, "GOODC"))
	}
	// The raw chain fact is untouched — we demote the RANK, never the data.
	if byCode["FLAGA"].Volume24hUSD == nil || *byCode["FLAGA"].Volume24hUSD != "500000" {
		t.Errorf("FLAGA volume_24h_usd = %v, want the unaltered raw 500000", byCode["FLAGA"].Volume24hUSD)
	}
	if byCode["FLAGA"].PriceUSD == nil || *byCode["FLAGA"].PriceUSD != "2.5000000000" {
		t.Errorf("FLAGA price_usd = %v, want the unaltered 2.5 (the API layer, not the query, withholds it)", byCode["FLAGA"].PriceUSD)
	}

	// The observation-count order demotes flagged rows too ("whatever the
	// active sort key"), while leaving unpriced rows ranked on activity —
	// that order's contract is activity, not market cap.
	obs := codesOf(listRankOrder(t, ctx, store, timescale.AssetsOrderObservationCountDesc, 100))
	wantObs := []string{"GOODA", "GOODB", "GOODC", "NOPRC", "FLAGA", "FLAGB"}
	for i := range wantObs {
		if obs[i] != wantObs[i] {
			t.Fatalf("observation-count order = %v, want %v (flagged last, unpriced still ranked on activity)", obs, wantObs)
		}
	}
}

// TestAssetsListing_KeysetPaginationCoversEveryRow is the risky half:
// the ORDER BY grew a new LEADING key, so the keyset WHERE must compare
// that key first and the cursor must encode it. Walking the listing two
// rows at a time — exactly as the handler does, via the store's own
// EncodeAssetsCursor — must reproduce the single-page order with no
// skipped and no repeated row, for BOTH orders.
func TestAssetsListing_KeysetPaginationCoversEveryRow(t *testing.T) {
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

	seedRankFixture(t, ctx, db, rankFixture)

	for _, tc := range []struct {
		name  string
		order timescale.AssetsOrder
	}{
		{"volume_desc", timescale.AssetsOrderVolume24hUSDDesc},
		{"observation_count_desc", timescale.AssetsOrderObservationCountDesc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			whole := codesOf(listRankOrder(t, ctx, store, tc.order, 100))

			const pageSize = 2
			var walked []string
			cursor := ""
			for page := 0; page < 20; page++ {
				rows, err := store.ListAssetsExt(ctx, timescale.ListAssetsOptions{
					Order:  tc.order,
					Cursor: cursor,
					// Overfetch-by-one, exactly like the handler.
					Limit: pageSize + 1,
				})
				if err != nil {
					t.Fatalf("page %d: %v", page, err)
				}
				hasMore := len(rows) > pageSize
				if hasMore {
					rows = rows[:pageSize]
				}
				if len(rows) == 0 {
					break
				}
				// A cursor that does not round-trip through the validator is
				// a 400 at the handler boundary — pin it here too.
				if err := timescale.ValidateAssetsCursor(cursor, tc.order); err != nil {
					t.Fatalf("page %d: emitted cursor %q failed ValidateAssetsCursor: %v", page, cursor, err)
				}
				walked = append(walked, codesOf(rows)...)
				if !hasMore {
					cursor = ""
					break
				}
				cursor = timescale.EncodeAssetsCursor(rows[len(rows)-1], tc.order)
			}
			if cursor != "" {
				t.Fatalf("pagination did not terminate within 20 pages; walked %v", walked)
			}
			if len(walked) != len(whole) {
				t.Fatalf("paginated walk returned %d rows %v, want the %d of the single page %v (skips or dupes)",
					len(walked), walked, len(whole), whole)
			}
			for i := range whole {
				if walked[i] != whole[i] {
					t.Fatalf("paginated walk = %v, want the single-page order %v", walked, whole)
				}
			}
			seen := map[string]int{}
			for _, code := range walked {
				seen[code]++
			}
			for code, n := range seen {
				if n != 1 {
					t.Errorf("%s appeared %d times across pages, want exactly once", code, n)
				}
			}
		})
	}
}

func pos(codes []string, want string) int {
	for i, c := range codes {
		if c == want {
			return i
		}
	}
	return -1
}
