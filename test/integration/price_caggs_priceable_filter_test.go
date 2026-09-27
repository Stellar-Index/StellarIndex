//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
)

// Migration 0187 (migrations/0187_price_caggs_priceable_filter.up.sql)
// restricts every price expression in the trades-derived CAGGs to rows with
// both legs > 0 and adds volume_quote / volume_priced. `trades` still
// CHECKs both legs > 0, so the test drops those CHECKs to store the
// zero-leg rows a later migration will admit.

const priceableIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// The operator recipe from 0187's headers
// (0187_price_caggs_priceable_filter.up.sql and
// 0187_price_caggs_priceable_filter.down.sql), run verbatim.
var priceableRefreshRecipe = []string{
	"CALL refresh_continuous_aggregate('prices_1m', now() - INTERVAL '7 days', now(), force => true)",
	"CALL refresh_continuous_aggregate('prices_15m', now() - INTERVAL '7 days', now(), force => true)",
	"CALL refresh_continuous_aggregate('prices_1h', now() - INTERVAL '7 days', now(), force => true)",
	"CALL refresh_continuous_aggregate('prices_4h', now() - INTERVAL '7 days', now(), force => true)",
	"CALL refresh_continuous_aggregate('prices_1d', now() - INTERVAL '7 days', now(), force => true)",
	"CALL refresh_continuous_aggregate('prices_1w', now() - INTERVAL '8 weeks', now(), force => true)",
	"CALL refresh_continuous_aggregate('prices_1mo', now() - INTERVAL '6 months', now(), force => true)",
	"CALL refresh_continuous_aggregate('twap_1h', now() - INTERVAL '7 days', now(), force => true)",
	"CALL refresh_continuous_aggregate('twap_1d', now() - INTERVAL '7 days', now(), force => true)",
	"CALL refresh_continuous_aggregate('pools_per_source_1h', now() - INTERVAL '7 days', now(), force => true)",
	// The recipe's windows cover no closed week or month bucket for data
	// two days old; materialise the open ones so those views are asserted too.
	"CALL refresh_continuous_aggregate('prices_1w', NULL, NULL)",
	"CALL refresh_continuous_aggregate('prices_1mo', NULL, NULL)",
}

const poolsRealtimeRestore = "ALTER MATERIALIZED VIEW pools_per_source_1h SET (timescaledb.materialized_only = false)"

var priceableGrains = []string{"1m", "15m", "1h", "4h", "1d", "1w", "1mo"}

func TestPriceCAGGsPriceableFilter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	assertPriceableSchema(t, ctx, db, true)

	for _, stmt := range []string{
		`ALTER TABLE trades DROP CONSTRAINT trades_base_amount_check`,
		`ALTER TABLE trades DROP CONSTRAINT trades_quote_amount_check`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	// One minute inside a closed UTC day inside the recipe's 7-day window.
	t0 := time.Now().UTC().Add(-48 * time.Hour).Truncate(24 * time.Hour).Add(time.Hour)
	mixed := ohlcDustPair{base: "PRMX-" + priceableIssuer, quote: "native"}
	allZero := ohlcDustPair{base: "PRZZ-" + priceableIssuer, quote: "native"}

	// Real fills at 5 and 6 bracketed by a zero-base row first and a
	// zero-quote row last, both above the notional floor, so an unfiltered
	// expression would divide by zero, open at infinity or close at 0.
	seed(t, db, ctx, mixed, []seedTrade{
		{off: 5 * time.Second, base: "0", quote: "7000", usd: "5"},
		{off: 10 * time.Second, base: "10000000", quote: "50000000", usd: "1000"},
		{off: 20 * time.Second, base: "20000000", quote: "120000000", usd: "1000"},
		{off: 50 * time.Second, base: "3000000", quote: "0", usd: "5"},
	}, t0)
	seed(t, db, ctx, allZero, []seedTrade{
		{off: 5 * time.Second, base: "0", quote: "100", usd: "1"},
		{off: 10 * time.Second, base: "100", quote: "0", usd: ""},
	}, t0)

	refreshAll := func(t *testing.T) {
		t.Helper()
		for _, stmt := range priceableRefreshRecipe {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
	}
	refreshAll(t)

	for _, grain := range priceableGrains {
		t.Run("mixed/"+grain, func(t *testing.T) {
			got := readPriceableRow(t, ctx, db, grain, mixed, t0)
			assertNumeric(t, "vwap", got.vwap, "5.666666666666666667")
			assertNumeric(t, "first_price", got.first, "5")
			assertNumeric(t, "last_price", got.last, "6")
			assertNumeric(t, "high_price", got.high, "6")
			assertNumeric(t, "low_price", got.low, "5")
			assertNumeric(t, "twap", got.twap, "5.5")
			assertExact(t, "volume", got.volume, "33000000")
			assertExact(t, "volume_quote", got.volumeQuote, "170007000")
			assertExact(t, "volume_priced", got.volumePriced, "30000000")
			if got.tradeCount != 4 {
				t.Errorf("trade_count = %d, want 4 — zero-leg rows still count as trades", got.tradeCount)
			}
		})
		t.Run("all-zero-leg/"+grain, func(t *testing.T) {
			got := readPriceableRow(t, ctx, db, grain, allZero, t0)
			for col, v := range map[string]sql.NullString{
				"vwap": got.vwap, "twap": got.twap, "first_price": got.first, "last_price": got.last,
				"high_price": got.high, "low_price": got.low, "volume_priced": got.volumePriced,
			} {
				if v.Valid {
					t.Errorf("%s = %s, want NULL for a bucket with no priceable trade", col, v.String)
				}
			}
			assertExact(t, "volume_quote", got.volumeQuote, "100")
			assertExact(t, "volume", got.volume, "100")
		})
	}

	var notional int64
	if err := db.QueryRowContext(ctx, `SELECT notional_trade_count FROM prices_1m
		 WHERE base_asset = $1 AND quote_asset = $2 AND bucket = $3`,
		mixed.base, mixed.quote, t0).Scan(&notional); err != nil {
		t.Fatalf("read notional_trade_count: %v", err)
	}
	if notional != 2 {
		t.Errorf("prices_1m.notional_trade_count = %d, want 2 (zero-leg rows are not priceable)", notional)
	}

	for _, view := range []string{"twap_1h", "twap_1d"} {
		t.Run(view, func(t *testing.T) {
			got := readTWAPRow(t, db, ctx, view, mixed, t0)
			assertNumeric(t, view+".twap", got.twap, "5.5")
			if got.sampleCount != 1 {
				t.Errorf("sample_count = %d, want 1", got.sampleCount)
			}
			got = readTWAPRow(t, db, ctx, view, allZero, t0)
			if got.twap.Valid || got.sampleCount != 0 {
				t.Errorf("all-zero-leg: twap = %v, sample_count = %d; want NULL, 0", got.twap, got.sampleCount)
			}
		})
	}

	t.Run("pools_per_source_1h", func(t *testing.T) {
		assertNumeric(t, "bucket_last_price", readPoolsLastPrice(t, ctx, db, mixed, t0), "6")
		if got := readPoolsLastPrice(t, ctx, db, allZero, t0); got.Valid {
			t.Errorf("all-zero-leg bucket_last_price = %s, want NULL", got.String)
		}
		// The header's real-time restore, run once the view is whole.
		if _, err := db.ExecContext(ctx, poolsRealtimeRestore); err != nil {
			t.Fatalf("%s: %v", poolsRealtimeRestore, err)
		}
		assertNumeric(t, "real-time bucket_last_price", readPoolsLastPrice(t, ctx, db, mixed, t0), "6")
	})

	// Down then up again. The down restores the unfiltered views, which
	// cannot be refreshed over zero-leg rows, so they are removed before its
	// recipe runs and stored again before the second up is refreshed.
	_, thisFile, _, _ := runtime.Caller(0)
	m, err := migrate.New("file://"+filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations"), dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Close() })
	if err := m.Steps(-1); err != nil {
		t.Fatalf("migrate down 0187: %v", err)
	}
	quiesceCAGGRefreshPolicies(t, ctx, db)
	assertPriceableSchema(t, ctx, db, false)
	if _, err := db.ExecContext(ctx,
		`DELETE FROM trades WHERE base_amount = 0 OR quote_amount = 0`); err != nil {
		t.Fatalf("delete zero-leg rows: %v", err)
	}
	refreshAll(t)
	if err := m.Up(); err != nil {
		t.Fatalf("migrate up 0187 again: %v", err)
	}
	quiesceCAGGRefreshPolicies(t, ctx, db)
	assertPriceableSchema(t, ctx, db, true)
	seed(t, db, ctx, mixed, []seedTrade{
		{off: 5 * time.Second, base: "0", quote: "7000", usd: "5"},
		{off: 50 * time.Second, base: "3000000", quote: "0", usd: "5"},
	}, t0)
	refreshAll(t)
	got := readPriceableRow(t, ctx, db, "1m", mixed, t0)
	assertExact(t, "volume_quote after up/down/up", got.volumeQuote, "170007000")
	assertNumeric(t, "vwap after up/down/up", got.vwap, "5.666666666666666667")
}

type priceableRow struct {
	vwap, twap, first, last, high, low sql.NullString
	volume, volumeQuote, volumePriced  sql.NullString
	tradeCount                         int64
}

func readPriceableRow(t *testing.T, ctx context.Context, db *sql.DB, grain string, p ohlcDustPair, t0 time.Time) priceableRow {
	t.Helper()
	var r priceableRow
	if err := db.QueryRowContext(ctx, `
		SELECT vwap::text, twap::text, first_price::text, last_price::text,
		       high_price::text, low_price::text,
		       volume::text, volume_quote::text, volume_priced::text, trade_count
		  FROM prices_`+grain+`
		 WHERE base_asset = $1 AND quote_asset = $2 AND bucket <= $3
		 ORDER BY bucket DESC LIMIT 1`,
		p.base, p.quote, t0,
	).Scan(&r.vwap, &r.twap, &r.first, &r.last, &r.high, &r.low,
		&r.volume, &r.volumeQuote, &r.volumePriced, &r.tradeCount); err != nil {
		t.Fatalf("read prices_%s for %s: %v", grain, p.base, err)
	}
	return r
}

func readPoolsLastPrice(t *testing.T, ctx context.Context, db *sql.DB, p ohlcDustPair, t0 time.Time) sql.NullString {
	t.Helper()
	var got sql.NullString
	if err := db.QueryRowContext(ctx, `
		SELECT bucket_last_price::text FROM pools_per_source_1h
		 WHERE source = 'sdex' AND base_asset = $1 AND quote_asset = $2 AND bucket <= $3
		 ORDER BY bucket DESC LIMIT 1`,
		p.base, p.quote, t0).Scan(&got); err != nil {
		t.Fatalf("read pools_per_source_1h for %s: %v", p.base, err)
	}
	return got
}

// assertExact compares volumes as exact NUMERIC strings: Σquote has to
// reconstruct to the stroop, not to a float tolerance.
func assertExact(t *testing.T, col string, got sql.NullString, want string) {
	t.Helper()
	if !got.Valid || got.String != want {
		t.Errorf("%s = %v, want exactly %s", col, got, want)
	}
}

// assertPriceableSchema checks the 0187 shape (up) or the restored
// pre-0187 shape (down): the new columns, pools_per_source_1h's
// real-time flag, and 0156's retention policy re-attached disarmed.
func assertPriceableSchema(t *testing.T, ctx context.Context, db *sql.DB, up bool) {
	t.Helper()
	for _, grain := range priceableGrains {
		var n int
		if err := db.QueryRowContext(ctx, `
			SELECT count(*) FROM information_schema.columns
			 WHERE table_name = $1 AND column_name IN ('volume_quote', 'volume_priced')`,
			"prices_"+grain).Scan(&n); err != nil {
			t.Fatalf("columns of prices_%s: %v", grain, err)
		}
		if want := map[bool]int{true: 2, false: 0}[up]; n != want {
			t.Errorf("prices_%s has %d of volume_quote/volume_priced, want %d (up=%v)", grain, n, want, up)
		}
	}
	mo := caggMaterializedOnly(t, ctx, db)
	for _, v := range []string{"prices_1m", "prices_1mo", "twap_1h", "twap_1d"} {
		if !mo[v] {
			t.Errorf("%s: materialized_only = false (up=%v)", v, up)
		}
	}
	if mo["pools_per_source_1h"] != up {
		t.Errorf("pools_per_source_1h materialized_only = %v, want %v", mo["pools_per_source_1h"], up)
	}
	var total, active int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*), count(*) FILTER (WHERE scheduled)
		  FROM timescaledb_information.jobs
		 WHERE proc_name = 'policy_retention' AND hypertable_name = 'prices_1m'`).Scan(&total, &active); err != nil {
		t.Fatalf("read prices_1m retention job: %v", err)
	}
	if total != 1 || active != 0 {
		t.Errorf("prices_1m retention jobs = %d (scheduled %d), want 1 disarmed (up=%v)", total, active, up)
	}
}
