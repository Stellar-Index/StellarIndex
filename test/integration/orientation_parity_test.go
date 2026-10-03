//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"net/url"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const usdcSACForm = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"

func openOrientationStore(t *testing.T, ctx context.Context, dsn string) (*timescale.Store, *sql.DB) {
	t.Helper()
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, store.DB()
}

func refreshPrices1m(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}
}

// soleTopMarket returns the asset's only top market, failing otherwise.
func soleTopMarket(t *testing.T, ctx context.Context, store *timescale.Store, assetID string) timescale.AssetTopMarket {
	t.Helper()
	tops, err := store.GetAssetTopMarkets(ctx, assetID, 5)
	if err != nil {
		t.Fatalf("GetAssetTopMarkets(%s): %v", assetID, err)
	}
	if len(tops) != 1 {
		t.Fatalf("GetAssetTopMarkets(%s) = %+v, want exactly one market", assetID, tops)
	}
	return tops[0]
}

// A market traded through a declared USDC SAC must orient like
// native/USDC in SQL as it does in canonical.Orient: USDC is the quote.
func TestAssetTopMarkets_DeclaredStablecoinSACIsQuote(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	reg, err := canonical.NewAliasRegistry(canonical.PubnetPassphrase,
		map[string]string{usdcSACForm: "USDC:" + realIssuer})
	if err != nil {
		t.Fatalf("alias registry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, db := openOrientationStore(t, ctx, dsn)
	seedGrainTrade(t, ctx, db, 1, "soroswap", time.Now().UTC().Add(-30*time.Minute),
		canonical.XLMSacContractID, usdcSACForm, "10", "1", "1")
	refreshPrices1m(t, ctx, db)

	if _, quote, _ := canonical.Orient(canonical.XLMSacContractID, usdcSACForm); quote != usdcSACForm {
		t.Fatalf("precondition: canonical.Orient quote = %s, want the USDC SAC", quote)
	}
	m := soleTopMarket(t, ctx, store, "USDC-"+realIssuer)
	if m.Side != "quote" || m.Counterparty != canonical.XLMSacContractID {
		t.Errorf("USDC top market = %+v, want side quote against the XLM SAC", m)
	}
}

// On a database whose default collation is not byte order, a rank tie
// must still break the way canonical.Orient breaks it.
func TestAssetTopMarkets_TieBreakIsByteOrderUnderICUCollation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE icu_en_us TEMPLATE template0
		LOCALE_PROVIDER icu ICU_LOCALE 'en-US' LOCALE 'C'`); err != nil {
		t.Fatalf("create ICU database: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/icu_en_us"
	icuDSN := u.String()
	icu, err := sql.Open("pgx", icuDSN)
	if err != nil {
		t.Fatalf("open ICU database: %v", err)
	}
	t.Cleanup(func() { _ = icu.Close() })
	if _, err := icu.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS timescaledb`); err != nil {
		t.Fatalf("enable timescaledb: %v", err)
	}
	var localeGreater bool
	if err := icu.QueryRowContext(ctx, `SELECT 'yX'::text > 'ZZ'::text`).Scan(&localeGreater); err != nil {
		t.Fatalf("collation probe: %v", err)
	}
	if localeGreater {
		t.Fatalf("instrument: ICU database compares 'yX' > 'ZZ' in byte order; the tie would not be exercised")
	}

	applyMigrations(t, icuDSN)
	store, db := openOrientationStore(t, ctx, icuDSN)
	const lower = "yXLM-" + realIssuer
	const upper = "ZZZ-" + realIssuer
	seedGrainTrade(t, ctx, db, 1, "sdex", time.Now().UTC().Add(-30*time.Minute), upper, lower, "1", "1", "1")
	refreshPrices1m(t, ctx, db)

	if _, quote, _ := canonical.Orient(upper, lower); quote != lower {
		t.Fatalf("precondition: canonical.Orient quote = %s, want %s", quote, lower)
	}
	if m := soleTopMarket(t, ctx, store, lower); m.Side != "quote" {
		t.Errorf("%s top market = %+v, want side quote as canonical.Orient decides", lower, m)
	}
}

// A Soroban-native token whose own contract address carries a scam-class
// directory tag ranks in the flagged tier, like a flagged issuer's asset.
func TestListAssets_FlaggedContractTokenRanksFlaggedTier(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	canonical.InstallAliasRegistry(nil)

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, db := openOrientationStore(t, ctx, dsn)
	const flagged = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"
	const clean = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	for _, id := range []string{flagged, clean} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO discovered_assets
			    (contract_id, first_seen_at, first_seen_ledger, first_seen_event,
			     last_seen_at, last_seen_ledger, event_count)
			VALUES ($1, now(), 1, 'transfer', now(), 2, 5)`, id); err != nil {
			t.Fatalf("seed discovered_assets %s: %v", id, err)
		}
		seedRawVolume(t, ctx, db, id, "5000")
	}
	seedDirectoryTags(t, ctx, db, flagged, "Example Token", []string{"malicious"})

	tiers := map[string]int{}
	for _, r := range listRankOrder(t, ctx, store, timescale.AssetsOrderObservationCountDesc, 50) {
		if r.RankTier != nil {
			tiers[r.AssetID] = *r.RankTier
		}
	}
	if tiers[flagged] != 2 {
		t.Errorf("flagged contract token rank tier = %d (present=%v), want 2", tiers[flagged], tiers)
	}
	if tier, ok := tiers[clean]; !ok || tier != 0 {
		t.Errorf("unlisted contract token rank tier = %d (present=%v), want 0", tier, ok)
	}
}
