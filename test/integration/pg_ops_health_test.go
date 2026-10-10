//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestDataFreshnessSurvivesADeadFeed executes the data-freshness
// watchdog's per-domain SQL — the SHIPPED bytes of
// configs/ansible/roles/archival-node/files/data-freshness.sh — against
// a real database in which feeds have been dead long enough to fall out
// of the windows the queries enumerate them from.
//
// The defect being pinned: the window that decided WHICH sources exist
// was the same window that decided whether they were healthy. A source
// dead beyond it dropped out of its own GROUP BY, so
// `stellarindex_data_freshness_stale{source=X}` went ABSENT instead of
// going to 1, Prometheus aged the series out, and `== 1` alerts
// RESOLVED — the watchdog fell silent the worse the outage got. The
// oracle and fx legs auto-resolved at 30 days, the supply leg at 7, and
// a per-asset gauge reported a literal healthy 0 once every
// watched asset had been frozen longer than 30 days.
func TestDataFreshnessSurvivesADeadFeed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()

	now := time.Now().UTC()
	longDead := now.AddDate(0, 0, -40) // past the 30-day oracle/fx windows
	frozen := now.AddDate(0, 0, -40)   // past the 7-day supply window

	// One oracle alive, one dead for 40 days (the coingecko-quota shape).
	seedOracleUpdate(t, ctx, db, 1, "reflector", now.Add(-2*time.Minute))
	seedOracleUpdate(t, ctx, db, 2, "coingecko", longDead)
	// One FX source, dead for 40 days.
	seedFXQuote(t, ctx, db, "massive", "EUR", longDead)
	// A backfill provenance label: historical rows that never advance.
	seedFXQuote(t, ctx, db, "frankfurter-historical", "GBP", now.AddDate(0, 0, -150))
	// Every watched supply asset frozen 40 days ago.
	for i, asset := range []string{"USDC-GA5Z", "EURC-GB3Q", "BENJI-GBHN"} {
		seedSupplyRow(t, ctx, db, asset, frozen.Add(time.Duration(i)*time.Minute))
	}
	// issuers exists but no SEP-1 payload was ever resolved.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO issuers (g_strkey) VALUES ('GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN')`,
	); err != nil {
		t.Fatalf("seed issuer: %v", err)
	}

	samples := runFreshnessDomainQueries(t, ctx, db)

	t.Run("a long-dead source still reports stale", func(t *testing.T) {
		for _, want := range []struct{ key, why string }{
			{
				`stellarindex_data_freshness_stale{domain="oracle",source="coingecko"}`,
				"an oracle dead 40 days is past the 30-day enumeration window",
			},
			{
				`stellarindex_data_freshness_stale{domain="fx",source="massive"}`,
				"an FX source dead 40 days is past the 30-day enumeration window",
			},
			{
				`stellarindex_data_freshness_stale{domain="supply",source="asset_supply_history"}`,
				"supply frozen 40 days is past the 7-day enumeration window",
			},
		} {
			v, ok := samples[want.key]
			if !ok {
				t.Errorf("%s is ABSENT — %s, so the series that should alert disappeared instead of reading 1", want.key, want.why)
				continue
			}
			if v != "1" {
				t.Errorf("%s = %s, want 1 (%s)", want.key, v, want.why)
			}
		}
	})

	t.Run("a domain that never observed anything is stale, not silent", func(t *testing.T) {
		key := `stellarindex_data_freshness_stale{domain="sep1",source="issuers"}`
		v, ok := samples[key]
		if !ok {
			t.Errorf("%s is ABSENT — a sep1 refresh that has never succeeded leaves max(sep1_payload_fetched_at) NULL, and an unknown age must not read as no alarm", key)
		} else if v != "1" {
			t.Errorf("%s = %s, want 1 — nothing was ever resolved, which is the most stale this domain can be", key, v)
		}
	})

	t.Run("a backfill provenance label is not a feed", func(t *testing.T) {
		for _, key := range []string{
			`stellarindex_data_freshness_stale{domain="fx",source="frankfurter-historical"}`,
			`stellarindex_data_freshness_age_seconds{domain="fx",source="frankfurter-historical"}`,
		} {
			if v, ok := samples[key]; ok {
				t.Errorf("%s = %s, want no series — the fx-history-backfill label never advances, so any series for it is a permanent false alarm", key, v)
			}
		}
	})

	t.Run("a live source still reports healthy", func(t *testing.T) {
		key := `stellarindex_data_freshness_stale{domain="oracle",source="reflector"}`
		v, ok := samples[key]
		if !ok || v != "0" {
			t.Errorf("%s = %q (present=%v), want 0 — an oracle that printed two minutes ago is not stale", key, v, ok)
		}
		age, ok := samples[`stellarindex_data_freshness_age_seconds{domain="oracle",source="reflector"}`]
		if !ok {
			t.Fatalf("no age sample for the live oracle")
		}
		if n, err := strconv.ParseFloat(age, 64); err != nil || n > 600 {
			t.Errorf("live oracle age = %s seconds, want a couple of minutes", age)
		}
	})

	t.Run("the per-asset supply gauge counts a whole-domain freeze", func(t *testing.T) {
		v, ok := samples["stellarindex_supply_assets_stale"]
		if !ok {
			t.Fatal("stellarindex_supply_assets_stale is absent")
		}
		if v != "3" {
			t.Errorf("stellarindex_supply_assets_stale = %s, want 3 — every watched asset froze 40 days ago, and a count of 0 there is the CS-102 shape reported as health", v)
		}
		worst, ok := samples["stellarindex_supply_asset_max_age_seconds"]
		if !ok {
			t.Fatal("stellarindex_supply_asset_max_age_seconds is absent")
		}
		if n, err := strconv.ParseFloat(worst, 64); err != nil || n < 30*24*3600 {
			t.Errorf("worst per-asset supply age = %s seconds, want >= 30 days", worst)
		}
	})
}

// runFreshnessDomainQueries executes the watchdog's per-domain and
// per-asset blocks and returns every exposition sample it rendered,
// keyed by `name{labels}`.
func runFreshnessDomainQueries(t *testing.T, ctx context.Context, db *sql.DB) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, needle := range []string{
		"stellarindex_data_freshness_stale",
		"stellarindex_supply_assets_stale",
	} {
		for _, stmt := range freshnessSQLBlocks(t, needle) {
			rows, err := db.QueryContext(ctx, stmt)
			if err != nil {
				t.Fatalf("execute watchdog block: %v\n--- SQL ---\n%s", err, stmt)
			}
			for rows.Next() {
				var line sql.NullString
				if err := rows.Scan(&line); err != nil {
					t.Fatalf("scan exposition line: %v", err)
				}
				// A NULL rendering is the failure mode the publication
				// validator withholds — record it as such rather than
				// silently dropping it, so an assertion can see it.
				if !line.Valid {
					continue
				}
				key, value, found := strings.Cut(strings.TrimSpace(line.String), " ")
				if !found {
					t.Fatalf("un-parseable exposition line %q", line.String)
				}
				out[key] = value
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("watchdog block rows: %v", err)
			}
			_ = rows.Close()
		}
	}
	return out
}

func seedOracleUpdate(t *testing.T, ctx context.Context, db *sql.DB, nonce int, source string, at time.Time) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO oracle_updates
		    (source, ledger, tx_hash, op_index, ts, asset, quote, price, decimals, ingested_at)
		VALUES ($1, $2, $3, 0, $4, 'native', 'fiat:USD', 0.12::numeric, 14, $4)`,
		source, 90_000_000+nonce, fmt.Sprintf("%064x", 900_000+nonce), at,
	); err != nil {
		t.Fatalf("seed oracle_updates %s: %v", source, err)
	}
}

func seedFXQuote(t *testing.T, ctx context.Context, db *sql.DB, source, ticker string, at time.Time) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO fx_quotes (bucket, ticker, rate_usd, inverse_usd, source, observed_at)
		VALUES ($1, $2, 1.08::numeric, 0.925::numeric, $3, $1)`,
		at, ticker, source,
	); err != nil {
		t.Fatalf("seed fx_quotes %s: %v", source, err)
	}
}

func seedSupplyRow(t *testing.T, ctx context.Context, db *sql.DB, assetKey string, at time.Time) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO asset_supply_history
		    (time, asset_key, total_supply, circulating_supply, basis, ledger_sequence)
		VALUES ($1, $2, 1000::numeric, 900::numeric, 'classic', 50000000)`,
		at, assetKey,
	); err != nil {
		t.Fatalf("seed asset_supply_history %s: %v", assetKey, err)
	}
}

// TestDivergenceGroupedReads executes the pair-grouped board and the
// per-reference series SQL: the board's limit counts pairs (a pair is never
// cut between its references) and orders pairs by their widest gap;
// firing-only keeps a pair with any firing reference, with all of its
// references; the series returns one cell per (bucket, reference).
func TestDivergenceGroupedReads(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sink := timescale.NewDivergenceSink(store)

	btc, err := canonical.ParsePair("crypto:BTC/fiat:USD")
	if err != nil {
		t.Fatalf("ParsePair: %v", err)
	}
	eth, err := canonical.ParsePair("crypto:ETH/fiat:USD")
	if err != nil {
		t.Fatalf("ParsePair: %v", err)
	}
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	for _, rec := range []domain.DivergenceObservationRecord{
		{Pair: btc, Reference: "chainlink", OurPrice: "100", RefPrice: "99.5", DeltaPct: "0.5", ObservedAt: at},
		{Pair: btc, Reference: "coingecko", OurPrice: "100", RefPrice: "99", DeltaPct: "1.0101", Firing: true, ObservedAt: at},
		{Pair: eth, Reference: "band", OurPrice: "10", RefPrice: "9", DeltaPct: "11.11", ObservedAt: at},
		{Pair: eth, Reference: "coingecko", OurPrice: "10", RefPrice: "10", DeltaPct: "0", ObservedAt: at},
	} {
		if err := sink.RecordObservation(ctx, rec); err != nil {
			t.Fatalf("RecordObservation %s/%s: %v", rec.Pair, rec.Reference, err)
		}
	}

	rows, err := store.ListDivergenceLatest(ctx, 7, false, 1)
	if err != nil {
		t.Fatalf("ListDivergenceLatest: %v", err)
	}
	if len(rows) != 2 || rows[0].AssetID != "crypto:ETH" || rows[1].AssetID != "crypto:ETH" {
		t.Fatalf("limit=1 rows = %+v, want both ETH references (widest pair, never split)", rows)
	}
	if rows[0].Reference != "band" {
		t.Errorf("first reference = %s, want band (widest |delta_pct| first within the pair)", rows[0].Reference)
	}

	all, err := store.ListDivergenceLatest(ctx, 7, false, 100)
	if err != nil {
		t.Fatalf("ListDivergenceLatest: %v", err)
	}
	if len(all) != 4 || all[2].AssetID != "crypto:BTC" {
		t.Fatalf("rows = %+v, want ETH's two references then BTC's two", all)
	}

	firing, err := store.ListDivergenceLatest(ctx, 7, true, 100)
	if err != nil {
		t.Fatalf("ListDivergenceLatest firing: %v", err)
	}
	if len(firing) != 2 || firing[0].AssetID != "crypto:BTC" || firing[1].AssetID != "crypto:BTC" {
		t.Fatalf("firing rows = %+v, want BTC's two references only (ETH has none firing)", firing)
	}
	if firing[0].Reference != "coingecko" || firing[0].Status != "firing" || firing[1].Status != "clear" {
		t.Errorf("firing rows = %+v, want firing coingecko then clear chainlink", firing)
	}

	points, err := store.ListDivergenceSeries(ctx, "crypto:BTC", "fiat:USD", 7)
	if err != nil {
		t.Fatalf("ListDivergenceSeries: %v", err)
	}
	if len(points) != 2 || points[0].Reference != "chainlink" || points[1].Reference != "coingecko" ||
		!points[0].Bucket.Equal(points[1].Bucket) {
		t.Fatalf("series = %+v, want chainlink + coingecko in one bucket", points)
	}
	if points[1].OurPrice != "100" || points[1].RefPrice != "99" || !points[1].LastAt.Equal(at) {
		t.Errorf("coingecko cell = %+v, want our 100 vs ref 99 at %v", points[1], at)
	}
}

// TestMigration0186_DivergenceRefObservedAt runs 0186 against a compressed
// divergence_observations chunk holding a row the previous binary wrote, then
// round-trips the reference time through the sink and the /v1/divergence
// reader: the new row returns exactly what was recorded, the old row
// and a record with no reference time return nil, and down drops the column.
func TestMigration0186_DivergenceRefObservedAt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 185)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	comparedAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	seedCompressedPre0186Row(t, ctx, db, comparedAt)

	applyMigrationsUpTo(t, dsn, 186)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sink := timescale.NewDivergenceSink(store)

	btcUSD, err := canonical.ParsePair("crypto:BTC/fiat:USD")
	if err != nil {
		t.Fatalf("ParsePair: %v", err)
	}
	refAt := comparedAt.Add(-47*time.Minute - 123456*time.Microsecond)
	for _, rec := range []domain.DivergenceObservationRecord{
		{
			Pair: btcUSD, Reference: "redstone", OurPrice: "100", RefPrice: "99", DeltaPct: "1.0101",
			ObservedAt: comparedAt, RefObservedAt: refAt.In(time.FixedZone("UTC-5", -5*3600)),
		},
		{
			Pair: btcUSD, Reference: "coingecko", OurPrice: "100", RefPrice: "100", DeltaPct: "0",
			ObservedAt: comparedAt,
		},
	} {
		if err := sink.RecordObservation(ctx, rec); err != nil {
			t.Fatalf("RecordObservation %s: %v", rec.Reference, err)
		}
	}

	rows, err := store.ListDivergenceLatest(ctx, 7, false, 100)
	if err != nil {
		t.Fatalf("ListDivergenceLatest: %v", err)
	}
	got := map[string]*time.Time{}
	for _, r := range rows {
		got[r.Reference] = r.RefObservedAt
	}
	if len(got) != 3 {
		t.Fatalf("rows = %+v, want redstone, coingecko and the pre-0186 chainlink row", rows)
	}
	if at := got["redstone"]; at == nil || !at.Equal(refAt) {
		t.Errorf("redstone ref_observed_at = %v, want %v", at, refAt)
	}
	if at := got["coingecko"]; at != nil {
		t.Errorf("coingecko recorded no reference time; ref_observed_at = %v, want nil", at)
	}
	if at := got["chainlink"]; at != nil {
		t.Errorf("pre-0186 row ref_observed_at = %v, want nil", at)
	}

	applyMigrationsUpTo(t, dsn, 185)
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'divergence_observations' AND column_name = 'ref_observed_at'`).Scan(&n); err != nil {
		t.Fatalf("read columns: %v", err)
	}
	if n != 0 {
		t.Errorf("0186 down left ref_observed_at in place")
	}
}

// seedCompressedPre0186Row writes a row in the previous binary's INSERT shape
// and compresses its chunk, so 0186's ADD COLUMN has to survive compressed
// data rather than an empty table.
func seedCompressedPre0186Row(t *testing.T, ctx context.Context, db *sql.DB, at time.Time) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO divergence_observations (
		    asset_id, quote_id, reference, observed_at, observed_at_ledger,
		    our_price, ref_price, delta_pct, status)
		VALUES ('crypto:BTC', 'fiat:USD', 'chainlink', $1, 0, 100, 100, 0, 'clear')`, at); err != nil {
		t.Fatalf("seed pre-0186 row: %v", err)
	}
	var compressed int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM (
			SELECT compress_chunk(c) FROM show_chunks('divergence_observations') c
		) s`).Scan(&compressed); err != nil {
		t.Fatalf("compress_chunk: %v", err)
	}
	if compressed == 0 {
		t.Fatal("no divergence_observations chunk was compressed — the ADD COLUMN-on-compressed claim would be vacuous")
	}
}

// 0019's stored comment, verbatim; 0173's down must restore exactly this.
const divergenceStatusComment0019 = `Whether |delta_pct| exceeded the per-(reference,pair) ` +
	`threshold at observation time. Distinct from the persistent ` +
	`flag returned by the API (which is "any reference firing").`

// TestDivergenceObservationsStatusComment pins the catalog comment on
// divergence_observations.status after migration 0173 up and down. The
// comment lives in pg_description, so only a real database can say what an
// operator reads through `\d+`.
func TestDivergenceObservationsStatusComment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	got := divergenceStatusComment(t, ctx, db)
	// The flag is quorum + median/agreement + debounce; a row is one
	// reference against one threshold.
	for _, want := range []string{
		"NOT the API flag",
		"single threshold",
		"min_sources_for_warning",
		"median breach",
		"WarningPersistence",
		"div:<base>/<quote>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status comment missing %q; got %q", want, got)
		}
	}
	for _, bad := range []string{"any reference firing", "per-(reference,pair)"} {
		if strings.Contains(got, bad) {
			t.Errorf("status comment still claims %q; got %q", bad, got)
		}
	}

	_, thisFile, _, _ := runtime.Caller(0)
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
	m, err := migrate.New("file://"+migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Migrate(172); err != nil {
		t.Fatalf("migrate down to 172: %v", err)
	}
	if got := divergenceStatusComment(t, ctx, db); got != divergenceStatusComment0019 {
		t.Errorf("0173 down did not restore 0019's comment verbatim:\n got %q\nwant %q", got, divergenceStatusComment0019)
	}
}

func divergenceStatusComment(t *testing.T, ctx context.Context, db *sql.DB) string {
	t.Helper()
	var c sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT col_description('divergence_observations'::regclass, a.attnum)
		   FROM pg_attribute a
		  WHERE a.attrelid = 'divergence_observations'::regclass AND a.attname = 'status'`,
	).Scan(&c); err != nil {
		t.Fatalf("read divergence_observations.status comment: %v", err)
	}
	if !c.Valid {
		t.Fatal("divergence_observations.status has no catalog comment")
	}
	return c.String
}

const mevIntegrationAccount = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

func openMEVStore(t *testing.T, ctx context.Context) *timescale.Store {
	t.Helper()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn) //nolint:contextcheck // golang-migrate takes no context
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestStorage_MEVTradeScanKeepsNewest: with more on-chain trades in the
// window than the cap, the MEV trade scan returns the NEWEST `limit`
// rows, ascending. An oldest-first LIMIT returned the same stale head
// every tick and never reached a burst's tail.
func TestStorage_MEVTradeScanKeepsNewest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	store := openMEVStore(t, ctx)

	usdc, err := canonical.NewClassicAsset("USDC", mevIntegrationAccount)
	if err != nil {
		t.Fatalf("usdc: %v", err)
	}
	pair, err := canonical.NewPair(canonical.NativeAsset(), usdc)
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	ts := time.Now().UTC().Truncate(time.Second)
	for nonce := 1; nonce <= 5; nonce++ {
		tr := mkIntegrationTrade("soroswap", nonce, ts, pair, 1_000_000_000, 25_000_000)
		tr.Taker = mevIntegrationAccount
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade(%d): %v", nonce, err)
		}
	}

	trades, usd, err := store.TradesForArbScan(ctx, ts.Add(-time.Hour), 3)
	if err != nil {
		t.Fatalf("TradesForArbScan: %v", err)
	}
	if len(trades) != 3 || len(usd) != 3 {
		t.Fatalf("got %d trades / %d usd, want 3 / 3", len(trades), len(usd))
	}
	for i, want := range []uint32{50_000_003, 50_000_004, 50_000_005} {
		if trades[i].Ledger != want {
			t.Errorf("trades[%d].Ledger = %d, want %d (newest 3, ascending)", i, trades[i].Ledger, want)
		}
	}
}

// TestStorage_MEVFillScanCarriesPositionAssets: the cascade correlator
// keys its oracle evidence on the filled position's own reserves, which
// the scan reads out of the fill's bid + lot jsonb as a distinct, sorted
// asset list (a fill with no bid/lot yields an empty list).
func TestStorage_MEVFillScanCarriesPositionAssets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	store := openMEVStore(t, ctx)

	const (
		xlmSAC  = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
		usdcSAC = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
		pool    = "CDVQVKOY2YSXS2IC7KN6MNASSHPAO7UN2UR2ON4OI2SKMFJNVAMDX6DP"
		txA     = "4444444444444444444444444444444444444444444444444444444444444444"
		txB     = "5555555555555555555555555555555555555555555555555555555555555555"
	)
	xlm, err := canonical.NewSorobanAsset(xlmSAC)
	if err != nil {
		t.Fatalf("xlm: %v", err)
	}
	usdcC, err := canonical.NewSorobanAsset(usdcSAC)
	if err != nil {
		t.Fatalf("usdc: %v", err)
	}
	ts := time.Now().UTC().Truncate(time.Second)
	amt := func(asset canonical.Asset) blend.AssetAmount {
		return blend.AssetAmount{Asset: asset, Amount: big.NewInt(1_000)}
	}

	withData := blend.FillAuctionEvent{
		Pool: pool, AuctionType: 0, User: mevIntegrationAccount, Filler: mevIntegrationAccount,
		FillPercent: big.NewInt(100),
		Data: &blend.AuctionData{
			Bid: []blend.AssetAmount{amt(usdcC)},
			Lot: []blend.AssetAmount{amt(xlm), amt(usdcC)},
		},
		Ledger: 52_500_001, TxHash: txA, Timestamp: ts,
	}
	bare := withData
	bare.Data = &blend.AuctionData{}
	bare.Ledger, bare.TxHash = 52_500_002, txB
	for _, e := range []blend.FillAuctionEvent{withData, bare} {
		if err := store.InsertBlendFillAuction(ctx, e); err != nil {
			t.Fatalf("InsertBlendFillAuction(%d): %v", e.Ledger, err)
		}
	}

	fills, err := store.BlendFillsForMEVScan(ctx, ts.Add(-time.Hour), 0)
	if err != nil {
		t.Fatalf("BlendFillsForMEVScan: %v", err)
	}
	if len(fills) != 2 {
		t.Fatalf("got %d fills, want 2: %+v", len(fills), fills)
	}
	if want := []string{xlmSAC, usdcSAC}; !reflect.DeepEqual(fills[0].Assets, want) {
		t.Errorf("fill assets = %v, want the distinct sorted bid+lot assets %v", fills[0].Assets, want)
	}
	if len(fills[1].Assets) != 0 {
		t.Errorf("bare fill assets = %v, want empty", fills[1].Assets)
	}
}

// TestStorage_MEVEventAssetColumns: a pair-scoped event lands its primary
// asset/quote in mev_events.asset_id / quote_id (the per-asset history
// index's key), a cross-asset event stores NULL there, and profit_usd
// stays NULL for both — no detector estimates attacker profit.
func TestStorage_MEVEventAssetColumns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	store := openMEVStore(t, ctx)

	const txA = "6666666666666666666666666666666666666666666666666666666666666666"
	ts := time.Now().UTC().Truncate(time.Second)
	events := []domain.MEVStoredEvent{
		{
			Kind: "sandwich", DetectedAtLedger: 52_600_001, Timestamp: ts,
			AssetID: "native", QuoteID: "fiat:USD",
			TxHashes: []string{txA}, Accounts: []string{mevIntegrationAccount},
			DedupKey: "sandwich:it:1", DetailJSON: []byte(`{"notional_usd":"1234.56"}`),
		},
		{
			Kind: "arbitrage", DetectedAtLedger: 52_600_002, Timestamp: ts.Add(time.Second),
			TxHashes: []string{txA}, Accounts: []string{mevIntegrationAccount},
			DedupKey: "arbitrage:it:2", DetailJSON: []byte(`{}`),
		},
	}
	for _, e := range events {
		if ok, err := store.InsertMEVEvent(ctx, e); err != nil || !ok {
			t.Fatalf("InsertMEVEvent(%s) = %v, %v", e.Kind, ok, err)
		}
	}

	rows, err := store.ListMEVEvents(ctx, "", 10)
	if err != nil {
		t.Fatalf("ListMEVEvents: %v", err)
	}
	got := map[string]timescale.MEVEventRow{}
	for _, r := range rows {
		got[r.Kind] = r
	}
	if s := got["sandwich"]; s.AssetID != "native" || s.QuoteID != "fiat:USD" || s.ProfitUSD != "" {
		t.Errorf("sandwich row asset/quote/profit = %q/%q/%q, want native/fiat:USD/NULL", s.AssetID, s.QuoteID, s.ProfitUSD)
	}
	if a := got["arbitrage"]; a.AssetID != "" || a.QuoteID != "" || a.ProfitUSD != "" {
		t.Errorf("arbitrage row asset/quote/profit = %q/%q/%q, want all NULL", a.AssetID, a.QuoteID, a.ProfitUSD)
	}

	var indexed int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM mev_events WHERE asset_id = 'native'`).Scan(&indexed); err != nil {
		t.Fatalf("per-asset count: %v", err)
	}
	if indexed != 1 {
		t.Errorf("per-asset lookup found %d events for native, want 1", indexed)
	}
}

// The reaper sweeps over `magic_link_tokens`,
// `login_code_lockouts` and `accounts` must not issue a single unbounded
// DELETE with no LIMIT. Every one of those tables is grown by an
// unauthenticated or attacker-influenced write path, so a sweep landing
// on a large backlog held row locks and WAL for as long as the DELETE
// ran, on the shared connection pool the request path uses.
//
// This proves the bound is real: with the batch size overridden small,
// a single Sweep call over a backlog LARGER than one call's cap
// (batchSize * sweepMaxBatchesPerCall) deletes only up to the cap and
// leaves the remainder for the next call — the unbounded
// DELETE would have removed everything in the first call.
func TestSweepExpiredMagicLinkTokens_BoundedPerCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Small batch size + the package's fixed sweepMaxBatchesPerCall (25)
	// gives a per-call cap of batchSize*25 that a modest seed count can
	// exceed without seeding tens of thousands of rows.
	const batchSize = 4
	const perCallCap = batchSize * 25 // matches postgresstore.sweepMaxBatchesPerCall
	const seedRows = perCallCap + 10  // exceeds one call's cap

	tokens := postgresstore.NewTokenStore(postgresstore.New(db)).WithSweepBatchSize(batchSize)

	for i := 0; i < seedRows; i++ {
		hash := make([]byte, 32)
		hash[0] = byte(i)
		hash[1] = byte(i >> 8)
		if _, err := db.ExecContext(ctx,
			`INSERT INTO magic_link_tokens
			     (token_hash, email, purpose, expires_at, requested_ip)
			 VALUES ($1, 'bulk-expired@example.com', 'login',
			         now() - interval '1 hour', '203.0.113.9')`,
			hash); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}

	deletedFirstCall, err := tokens.SweepExpiredMagicLinkTokens(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if deletedFirstCall != perCallCap {
		t.Errorf("first call deleted = %d, want exactly the per-call cap %d "+
			"(an unbounded DELETE would have removed all %d seeded rows in one call)",
			deletedFirstCall, perCallCap, seedRows)
	}

	var remaining int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM magic_link_tokens WHERE email = 'bulk-expired@example.com'`,
	).Scan(&remaining); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	wantRemaining := seedRows - perCallCap
	if remaining != wantRemaining {
		t.Errorf("rows remaining after first call = %d, want %d", remaining, wantRemaining)
	}

	// The next call (the reaper's next tick) finishes the backlog.
	deletedSecondCall, err := tokens.SweepExpiredMagicLinkTokens(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if deletedSecondCall != int64(wantRemaining) {
		t.Errorf("second call deleted = %d, want %d (the remainder)", deletedSecondCall, wantRemaining)
	}

	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM magic_link_tokens WHERE email = 'bulk-expired@example.com'`,
	).Scan(&remaining); err != nil {
		t.Fatalf("count final: %v", err)
	}
	if remaining != 0 {
		t.Errorf("rows remaining after second call = %d, want 0", remaining)
	}
}

// SweepEndedSessions shares deleteInBatches with the
// sweeps above, but nothing proved its own call site actually threads
// the per-call cap: a bug pinning it to defaultSweepBatchRows instead of
// r.sweepBatchRows would still pass the table-driven correctness test in
// platform_retention_reaper_test.go, since that test never seeds past
// one batch.
func TestSweepEndedSessions_BoundedPerCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	pg := postgresstore.New(db)
	accounts := postgresstore.NewAccountStore(pg)
	acct, err := accounts.Create(ctx, platform.Account{
		Name: "Bounded Sweep Co", Slug: "bounded-sweep-co",
		BillingEmail: "billing@bounded-sweep.example",
		Tier:         platform.TierFree, Status: platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	users := postgresstore.NewUserStore(pg)
	user, err := users.CreateUser(ctx, platform.User{
		AccountID: acct.ID, Email: "owner@bounded-sweep.example", Role: platform.RoleOwner,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	const batchSize = 4
	const perCallCap = batchSize * 25 // matches postgresstore.sweepMaxBatchesPerCall
	const seedRows = perCallCap + 10  // exceeds one call's cap

	for i := 0; i < seedRows; i++ {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO sessions (token_hash, user_id, expires_at, ip_first_seen, ip_last_seen, user_agent)
			 VALUES ($1, $2, now() - interval '1 hour', '203.0.113.9', '203.0.113.9', 'bulk')`,
			[]byte(fmt.Sprintf("bounded-sweep-session-%04d", i)), user.ID); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}

	sessions := users.WithSweepBatchSize(batchSize)

	deletedFirstCall, err := sessions.SweepEndedSessions(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if deletedFirstCall != perCallCap {
		t.Errorf("first call deleted = %d, want exactly the per-call cap %d", deletedFirstCall, perCallCap)
	}

	deletedSecondCall, err := sessions.SweepEndedSessions(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if wantRemaining := int64(seedRows - perCallCap); deletedSecondCall != wantRemaining {
		t.Errorf("second call deleted = %d, want %d (the remainder)", deletedSecondCall, wantRemaining)
	}

	var remaining int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sessions WHERE user_id = $1`, user.ID,
	).Scan(&remaining); err != nil {
		t.Fatalf("count final: %v", err)
	}
	if remaining != 0 {
		t.Errorf("rows remaining after second call = %d, want 0", remaining)
	}
}

// CA2-A33-harden-1 — SweepLoginCodeLockouts reports whether it stopped
// at the per-call cap. The reaper re-sweeps promptly on that signal
// instead of waiting a full Interval, so the signal must be true exactly
// when every batch came back full, and false once the backlog is gone.
func TestSweepLoginCodeLockouts_ReportsCapHit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const batchSize = 4
	const perCallCap = batchSize * 25 // matches postgresstore.sweepMaxBatchesPerCall
	const seedRows = perCallCap + 10

	tokens := postgresstore.NewTokenStore(postgresstore.New(db)).WithSweepBatchSize(batchSize)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO login_code_lockouts (email, failed_count, updated_at)
		 SELECT 'bulk-' || g || '@example.com', 1, now() - interval '72 hours'
		   FROM generate_series(1, $1::int) AS g`, seedRows); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cutoff := time.Now().UTC().Add(-48 * time.Hour)
	deleted, more, err := tokens.SweepLoginCodeLockouts(ctx, cutoff)
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if deleted != perCallCap || !more {
		t.Errorf("first call = (%d, more=%v), want (%d, more=true)", deleted, more, perCallCap)
	}

	deleted, more, err = tokens.SweepLoginCodeLockouts(ctx, cutoff)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if want := int64(seedRows - perCallCap); deleted != want || more {
		t.Errorf("second call = (%d, more=%v), want (%d, more=false)", deleted, more, want)
	}
}
