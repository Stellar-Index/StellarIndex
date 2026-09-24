//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestMigrationsRoundTrip spins up a throwaway TimescaleDB,
// runs every migration up, asserts the expected hypertable +
// continuous-aggregate shape exists, then rolls them all back and
// asserts a clean slate. This ties together ADR-0006 (storage
// choice) + the SQL files + the migrate binary's underlying library
// + the compose-stack's TimescaleDB image choice.
//
// Runs under `-tags=integration` only. Nominal runtime: ~30s on a
// warm Docker cache, ~2min on a cold one (TimescaleDB image pull).
func TestMigrationsRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// The shared bootstrap: pinned image, zero background workers, the
	// extension pre-created, and a readiness gate that waits for the
	// mapped port and not just the log line.
	dsn := startTimescale(t, ctx)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// Resolve the absolute path to the migrations directory from
	// the test file's own location so the test works regardless of
	// cwd.
	_, thisFile, _, _ := runtime.Caller(0)
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")

	migrator, err := migrate.New("file://"+migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	t.Cleanup(func() {
		if srcErr, dbErr := migrator.Close(); srcErr != nil || dbErr != nil {
			t.Logf("migrator close: src=%v db=%v", srcErr, dbErr)
		}
	})

	// ─── Up: apply all migrations ───────────────────────────────
	if err := migrator.Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	// This test drives its own migrator (not applyMigrations), so it
	// quiesces the freshly-created CAGG refresh policies itself before
	// the manual refresh below — same 55P03 race as everywhere else.
	// alter_job(scheduled=>false) keeps the job ROW, so the has-a-
	// refresh-policy assertions below still hold.
	quiesceCAGGRefreshPolicies(t, ctx, db)

	// Verify trades hypertable exists.
	assertHypertableExists(t, db, ctx, "trades")

	// Verify the expected indexes exist on trades.
	for _, idx := range []string{
		"trades_base_ts_idx",
		"trades_quote_ts_idx",
		"trades_pair_ts_idx",
		"trades_source_ledger_idx",
	} {
		assertIndexExists(t, db, ctx, "trades", idx)
	}

	// Verify ingestion_cursors table exists (non-hyper).
	assertTableExists(t, db, ctx, "ingestion_cursors")

	// Verify oracle_updates hypertable + its indexes (0003).
	assertHypertableExists(t, db, ctx, "oracle_updates")
	for _, idx := range []string{
		"oracle_updates_asset_ts_idx",
		"oracle_updates_pair_ts_idx",
		"oracle_updates_source_ledger_idx",
	} {
		assertIndexExists(t, db, ctx, "oracle_updates", idx)
	}

	// Verify every continuous aggregate is present and a CAGG
	// (not a plain view) + has a refresh policy attached.
	for _, caggName := range []string{
		"prices_1m", "prices_15m",
		"prices_1h", "prices_4h", "prices_1d",
		"prices_1w", "prices_1mo",
		// TWAP hierarchical CAGGs over prices_1m (migration 0081).
		"twap_1h", "twap_1d",
	} {
		assertContinuousAggregateExists(t, db, ctx, caggName)
	}

	// Spot-check: insert a trade, query the view before the refresh
	// policy fires, and make sure the pre-compute pipeline works
	// when manually refreshed.
	insertSampleTrade(t, db, ctx)
	if _, err := db.ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`,
	); err != nil {
		t.Fatalf("manual refresh prices_1m: %v", err)
	}
	assertPrices1mHasRow(t, db, ctx)

	// ─── CHECK constraints round-trip ──────────────────────────
	// Verify the invariants baked into migration 0001 / 0003 are
	// enforced. If someone accidentally drops a CHECK in a future
	// migration, this test fails loudly. The canonical.Validate
	// functions are a first line of defense — the DB CHECKs are
	// the last. Both matter.
	assertInsertRejected(t, db, ctx, "negative base_amount", `
        INSERT INTO trades
            (source, ledger, tx_hash, op_index, ts,
             base_asset, quote_asset, base_amount, quote_amount)
        VALUES ('t', 1, 'aa', 0, now(), 'native', 'native', -1, 1)`)
	// ledger=0 is ACCEPTED as of migration 0004 — off-chain
	// sources (Binance / Kraken / Bitstamp / Coinbase / FX pollers
	// / aggregators) deliberately stamp 0 and use (source, tx_hash,
	// op_index) for uniqueness. Matches oracle_updates which has
	// always allowed ledger >= 0.
	assertInsertAccepted(t, db, ctx, "zero ledger allowed for off-chain", `
        INSERT INTO trades
            (source, ledger, tx_hash, op_index, ts,
             base_asset, quote_asset, base_amount, quote_amount)
        VALUES ('binance', 0, 'dead0000000000000000000000000000000000000000000000000000000000be', 0, now(), 'crypto:XLM', 'crypto:USDT', 1, 1)`)
	assertInsertRejected(t, db, ctx, "negative op_index", `
        INSERT INTO trades
            (source, ledger, tx_hash, op_index, ts,
             base_asset, quote_asset, base_amount, quote_amount)
        VALUES ('t', 1, 'aa', -1, now(), 'native', 'native', 1, 1)`)
	assertInsertRejected(t, db, ctx, "oracle decimals > 38", `
        INSERT INTO oracle_updates
            (source, ledger, tx_hash, op_index, ts,
             asset, quote, price, decimals)
        VALUES ('o', 1, 'aa', 0, now(), 'native', 'native', 1, 39)`)
	assertInsertRejected(t, db, ctx, "oracle confidence > 1", `
        INSERT INTO oracle_updates
            (source, ledger, tx_hash, op_index, ts,
             asset, quote, price, decimals, confidence)
        VALUES ('o', 1, 'aa', 0, now(), 'native', 'native', 1, 14, 1.5)`)
	assertInsertRejected(t, db, ctx, "oracle negative price", `
        INSERT INTO oracle_updates
            (source, ledger, tx_hash, op_index, ts,
             asset, quote, price, decimals)
        VALUES ('o', 1, 'aa', 0, now(), 'native', 'native', -1, 14)`)

	// ─── Compression attached; retention deliberately ABSENT ───
	// Migrations add compression (ADR-0006). Retention was REMOVED for
	// raw trades (migration 0031) and oracle_updates (0040): ADR-0034
	// invariant 8 keeps the certified raw history forever, so a
	// drop_after retention policy on these tables is DRIFT (a rogue 90d
	// policy was in fact removed as drift on 2026-06-10). Assert
	// compression is present and retention is absent — F-1334 flipped
	// these from the old (now-invalid) assert-attached.
	assertPolicyAttached(t, db, ctx, "trades", "policy_compression")
	assertPolicyAbsent(t, db, ctx, "trades", "policy_retention")
	assertPolicyAttached(t, db, ctx, "oracle_updates", "policy_compression")
	assertPolicyAbsent(t, db, ctx, "oracle_updates", "policy_retention")

	// 0041 — soroban_events raw-event landing zone (ADR-0029).
	// Hypertable + indexes + compression policy (no retention —
	// granular coverage is the mission).
	assertHypertableExists(t, db, ctx, "soroban_events")
	for _, idx := range []string{
		"soroban_events_contract_ts_idx",
		"soroban_events_topic_sym_ts_idx",
		"soroban_events_contract_topic_idx",
	} {
		assertIndexExists(t, db, ctx, "soroban_events", idx)
	}
	assertPolicyAttached(t, db, ctx, "soroban_events", "policy_compression")

	// 0105 created the classic_movements hypertable; 0113 drops it
	// (audit C2-18 / DAT-03 — superseded by ADR-0048 D2, the archive
	// moved to ClickHouse-native stellar.account_movements and this
	// Postgres table had no live writer/reader). After the FULL up
	// stack, therefore, it must NOT exist — this asserts the cleanup
	// migration actually removed it (and guards against a future
	// re-add without a matching drop).
	assertTableAbsent(t, db, ctx, "classic_movements")

	// 0142 — the six protocol tables 0127-0132 typed derive_generation
	// as int4 while every other derived-value table (0109/0110 core +
	// protocol, 0141 fx_quotes) uses bigint (audit W1-migrations-3). The
	// column carries time.Now().Unix(), so an int4 column overflows on
	// 2038-01-19 and rejects EVERY projector write to these tables.
	// Migration 0142 widens all six to bigint; assert the applied type
	// so a future re-add of one of these tables (or a reverted 0142)
	// can't silently reintroduce the 2038 cliff.
	for _, table := range []string{
		"soroswap_liquidity",
		"aquarius_reserves_sync",
		"aquarius_protocol_fee",
		"aquarius_kill_switches",
		"phoenix_initialize",
		"phoenix_admin_events",
	} {
		assertColumnType(t, db, ctx, table, "derive_generation", "bigint")
	}

	// 0146 (defindex_fees) types derive_generation bigint NATIVELY —
	// the 0142 lesson applied at creation time. Asserted so a future
	// re-add of the table can't reintroduce the 2038 int4 cliff.
	assertColumnType(t, db, ctx, "defindex_fees", "derive_generation", "bigint")

	// ─── Down: roll everything back ─────────────────────────────
	if err := migrator.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate down: %v", err)
	}

	// After a full rollback, none of our tables / CAGGs should remain.
	// Symmetric with the up-path assertions: every object the
	// migrations created must be gone. If a future migration's down
	// script forgets to drop one CAGG, retention+compression policies
	// on the survivor will keep firing against a non-existent trades
	// hypertable, flooding logs with errors — we'd rather the test
	// fail loudly.
	assertTableAbsent(t, db, ctx, "trades")
	assertTableAbsent(t, db, ctx, "ingestion_cursors")
	assertTableAbsent(t, db, ctx, "oracle_updates")
	assertTableAbsent(t, db, ctx, "soroban_events")
	for _, cagg := range []string{
		"prices_1m", "prices_15m", "prices_1h",
		"prices_4h", "prices_1d", "prices_1w", "prices_1mo",
		"twap_1h", "twap_1d",
	} {
		assertContinuousAggregateAbsent(t, db, ctx, cagg)
	}
}

// TestMEVKindDownsRefuseWithData pins that the 0074 and 0067 downs,
// which narrow mev_events_kind_check, refuse while a row of the kind
// they remove exists, and leave that row in place — instead of
// deleting every detected oracle_sandwich / arbitrage event silently.
func TestMEVKindDownsRefuseWithData(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 74)
	insertMEVEvent(t, ctx, db, "oracle_sandwich")
	assertDownRefused(t, ctx, db, dsn, 74, 73, "0074_mev_new_kinds.down.sql", "oracle_sandwich")

	// Explicit deletion is the documented way through.
	if _, err := db.ExecContext(ctx, `DELETE FROM mev_events WHERE kind = 'oracle_sandwich'`); err != nil {
		t.Fatalf("delete oracle_sandwich: %v", err)
	}
	applyMigrationsUpTo(t, dsn, 73)

	insertMEVEvent(t, ctx, db, "arbitrage")
	assertDownRefused(t, ctx, db, dsn, 73, 66, "0067_mev_arbitrage_dedup.down.sql", "arbitrage")
}

func insertMEVEvent(t *testing.T, ctx context.Context, db *sql.DB, kind string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `INSERT INTO mev_events
		(detected_at, detected_at_ledger, kind, tx_hashes, detail)
		VALUES (now(), 1, $1, ARRAY['ab'], '{}'::jsonb)`, kind); err != nil {
		t.Fatalf("insert %s mev_event: %v", kind, err)
	}
}

// assertDownRefused migrates from `from` down to `to`, requires the
// failure to come from the named down file, and requires the row of
// `kind` to have survived. It then forces the version back to `from`
// so the schema_migrations row is clean for the next step.
func assertDownRefused(t *testing.T, ctx context.Context, db *sql.DB, dsn string, from, to uint, file, kind string) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
	m, err := migrate.New("file://"+migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	err = m.Migrate(to)
	_, _ = m.Close()
	if err == nil || !strings.Contains(err.Error(), file) || !strings.Contains(err.Error(), "LOUD") {
		t.Fatalf("migrate %d -> %d with a %s row: err = %v, want the %s guard to refuse", from, to, kind, err, file)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM mev_events WHERE kind = $1`, kind).Scan(&n); err != nil {
		t.Fatalf("count %s rows: %v", kind, err)
	}
	if n != 1 {
		t.Fatalf("%s rows after refused down = %d, want 1 (the down deleted data)", kind, n)
	}
	f, err := migrate.New("file://"+migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = f.Close() }()
	if err := f.Force(int(from)); err != nil {
		t.Fatalf("force %d: %v", from, err)
	}
}

// ─── helpers ──────────────────────────────────────────────────────

func assertTableExists(t *testing.T, db *sql.DB, ctx context.Context, name string) {
	t.Helper()
	var exists bool
	err := db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`,
		name,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("check table %q: %v", name, err)
	}
	if !exists {
		t.Errorf("expected table %q to exist", name)
	}
}

func assertTableAbsent(t *testing.T, db *sql.DB, ctx context.Context, name string) {
	t.Helper()
	var exists bool
	err := db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`,
		name,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("check absent %q: %v", name, err)
	}
	if exists {
		// Used both post-rollback (every migration's down ran) and in
		// the up-path for a table a later migration drops (0113 →
		// classic_movements), so the message stays context-neutral.
		t.Errorf("expected table %q to be absent", name)
	}
}

// assertColumnType checks that a column's SQL data type matches want
// (information_schema.columns.data_type — e.g. "bigint", "integer").
// Used to guard the derive_generation int4->bigint parity (0142): a
// column silently left as int4 overflows time.Now().Unix() in 2038.
func assertColumnType(t *testing.T, db *sql.DB, ctx context.Context, table, column, want string) {
	t.Helper()
	var got string
	err := db.QueryRowContext(ctx, `
        SELECT data_type FROM information_schema.columns
        WHERE table_name = $1 AND column_name = $2`,
		table, column,
	).Scan(&got)
	if err != nil {
		t.Fatalf("read type of %s.%s: %v", table, column, err)
	}
	if got != want {
		t.Errorf("%s.%s is %q, want %q (derive_generation must be bigint — an int4 overflows the unix-epoch generation stamp in 2038; audit W1-migrations-3 / migration 0142)",
			table, column, got, want)
	}
}

func assertHypertableExists(t *testing.T, db *sql.DB, ctx context.Context, name string) {
	t.Helper()
	var exists bool
	err := db.QueryRowContext(ctx, `
        SELECT EXISTS (
            SELECT 1 FROM timescaledb_information.hypertables
            WHERE hypertable_name = $1
        )`, name).Scan(&exists)
	if err != nil {
		t.Fatalf("check hypertable %q: %v", name, err)
	}
	if !exists {
		t.Errorf("expected hypertable %q to exist", name)
	}
}

func assertIndexExists(t *testing.T, db *sql.DB, ctx context.Context, table, idx string) {
	t.Helper()
	var exists bool
	err := db.QueryRowContext(ctx, `
        SELECT EXISTS (
            SELECT 1 FROM pg_indexes
            WHERE tablename = $1 AND indexname = $2
        )`, table, idx).Scan(&exists)
	if err != nil {
		t.Fatalf("check index %q on %q: %v", idx, table, err)
	}
	if !exists {
		t.Errorf("expected index %q on %q", idx, table)
	}
}

func assertContinuousAggregateExists(t *testing.T, db *sql.DB, ctx context.Context, name string) {
	t.Helper()
	var exists bool
	err := db.QueryRowContext(ctx, `
        SELECT EXISTS (
            SELECT 1 FROM timescaledb_information.continuous_aggregates
            WHERE view_name = $1
        )`, name).Scan(&exists)
	if err != nil {
		t.Fatalf("check cagg %q: %v", name, err)
	}
	if !exists {
		t.Errorf("expected continuous aggregate %q to exist", name)
		return
	}

	// Also assert it has a refresh policy — a CAGG without a refresh
	// policy is a silent bug per migrations/README.md.
	var policyCount int
	err = db.QueryRowContext(ctx, `
        SELECT count(*) FROM timescaledb_information.jobs j
        JOIN timescaledb_information.continuous_aggregates c
          -- TimescaleDB 2.26 (r1's version) sets jobs.hypertable_name to the
          -- cagg VIEW name for refresh jobs; older 2.x used the materialization
          -- hypertable name. Match either so the assertion is version-robust.
          ON j.hypertable_name IN (c.view_name, c.materialization_hypertable_name)
        WHERE c.view_name = $1
          AND j.proc_name = 'policy_refresh_continuous_aggregate'`,
		name,
	).Scan(&policyCount)
	if err != nil {
		t.Fatalf("check refresh policy for %q: %v", name, err)
	}
	if policyCount < 1 {
		t.Errorf("cagg %q has no refresh policy", name)
	}
}

func assertContinuousAggregateAbsent(t *testing.T, db *sql.DB, ctx context.Context, name string) {
	t.Helper()
	var exists bool
	err := db.QueryRowContext(ctx, `
        SELECT EXISTS (
            SELECT 1 FROM timescaledb_information.continuous_aggregates
            WHERE view_name = $1
        )`, name).Scan(&exists)
	if err != nil {
		t.Fatalf("check cagg absent %q: %v", name, err)
	}
	if exists {
		t.Errorf("expected cagg %q to be absent after rollback", name)
	}
}

func insertSampleTrade(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	_, err := db.ExecContext(ctx, `
        INSERT INTO trades
            (source, ledger, tx_hash, op_index, ts,
             base_asset, quote_asset,
             base_amount, quote_amount, usd_volume,
             maker, taker)
        VALUES (
            'sdex', 52430001,
            'cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe',
            0, now(),
            'native', 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
            1000000000, 12420000, 12.42,
            'maker-acc', 'taker-acc'
        )
    `)
	if err != nil {
		t.Fatalf("insert sample trade: %v", err)
	}
}

// assertPolicyAttached checks that a TimescaleDB background job
// with the given proc_name is registered against the hypertable.
// Covers `add_compression_policy` and `add_retention_policy` calls
// in the migrations — each creates a row in
// timescaledb_information.jobs keyed on (proc_name, hypertable).
func assertPolicyAttached(t *testing.T, db *sql.DB, ctx context.Context, hypertable, procName string) {
	t.Helper()
	var count int
	err := db.QueryRowContext(ctx, `
        SELECT count(*) FROM timescaledb_information.jobs
        WHERE hypertable_name = $1 AND proc_name = $2`,
		hypertable, procName,
	).Scan(&count)
	if err != nil {
		t.Fatalf("check policy %s on %s: %v", procName, hypertable, err)
	}
	if count < 1 {
		t.Errorf("expected %s policy on hypertable %q, got %d jobs", procName, hypertable, count)
	}
}

// assertPolicyAbsent is the inverse of assertPolicyAttached: it fails if
// the named policy IS attached. Used for retention policies that the
// migration chain deliberately removed (trades/oracle_updates per
// migrations 0031/0040 + ADR-0034 invariant 8: raw history is kept
// forever; a re-appeared drop_after policy is drift).
func assertPolicyAbsent(t *testing.T, db *sql.DB, ctx context.Context, hypertable, procName string) {
	t.Helper()
	var count int
	err := db.QueryRowContext(ctx, `
        SELECT count(*) FROM timescaledb_information.jobs
        WHERE hypertable_name = $1 AND proc_name = $2`,
		hypertable, procName,
	).Scan(&count)
	if err != nil {
		t.Fatalf("check policy %s on %s: %v", procName, hypertable, err)
	}
	if count != 0 {
		t.Errorf("expected NO %s policy on hypertable %q (invariant 8: raw history kept forever), got %d jobs",
			procName, hypertable, count)
	}
}

// assertInsertRejected runs `stmt` and expects Postgres to refuse
// it with a CHECK-constraint violation (SQLSTATE 23514). Passing
// statements are a test failure — they mean a constraint was
// silently dropped or weakened.
func assertInsertRejected(t *testing.T, db *sql.DB, ctx context.Context, name, stmt string) {
	t.Helper()
	_, err := db.ExecContext(ctx, stmt)
	if err == nil {
		t.Errorf("%s: expected CHECK constraint rejection, got nil error", name)
		return
	}
	// Postgres 23514 = check_violation. The driver surfaces it as a
	// string inside the error message; accept either the SQLSTATE
	// or the "check constraint" substring.
	msg := err.Error()
	if !strings.Contains(msg, "23514") && !strings.Contains(msg, "check constraint") &&
		!strings.Contains(msg, "violates check") {
		t.Errorf("%s: error %v is not a check-constraint violation", name, err)
	}
}

// assertInsertAccepted is the complement to assertInsertRejected —
// verifies a statement is accepted. Used for invariants that were
// tightened in one migration and relaxed in a later one, like the
// trades.ledger CHECK (0001 required >0; 0004 relaxed to >=0 so
// off-chain sources could emit).
func assertInsertAccepted(t *testing.T, db *sql.DB, ctx context.Context, name, stmt string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		t.Errorf("%s: expected insert to succeed, got %v", name, err)
	}
}

func assertPrices1mHasRow(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM prices_1m`).Scan(&count); err != nil {
		t.Fatalf("count prices_1m: %v", err)
	}
	if count < 1 {
		t.Errorf("expected prices_1m to contain at least 1 row after refresh, got %d", count)
	}
}

// TestCAGGRefreshPolicyAssertionSQL executes config-assertions.sh's
// caggs_have_refresh_policy query, read byte-for-byte from the script,
// against a fully migrated TimescaleDB: every CAGG the migrations create
// must be matched to its refresh job (0 uncovered), and dropping one
// policy must be counted (1 uncovered). The timescale-jobs probe keys on
// the job, so this query is the only thing that sees the dropped policy.
func TestCAGGRefreshPolicyAssertionSQL(t *testing.T) {
	query := caggRefreshPolicyAssertionSQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	var caggs, viewKeyed int
	var version string
	if err := db.QueryRowContext(ctx, `
		SELECT (SELECT count(*) FROM timescaledb_information.continuous_aggregates),
		       (SELECT count(*) FROM timescaledb_information.jobs j
		          JOIN timescaledb_information.continuous_aggregates ca
		            ON j.hypertable_schema = ca.view_schema
		           AND j.hypertable_name = ca.view_name
		         WHERE j.proc_name = 'policy_refresh_continuous_aggregate'),
		       (SELECT extversion FROM pg_extension WHERE extname = 'timescaledb')`,
	).Scan(&caggs, &viewKeyed, &version); err != nil {
		t.Fatalf("census: %v", err)
	}
	if caggs < 9 {
		t.Fatalf("migrated database has %d continuous aggregates, want at least the 9 price/TWAP views", caggs)
	}
	t.Logf("timescaledb %s: %d continuous aggregates, %d refresh jobs keyed on the view's schema and name", version, caggs, viewKeyed)

	uncovered := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, query).Scan(&n); err != nil {
			t.Fatalf("run caggs_have_refresh_policy SQL: %v", err)
		}
		return n
	}
	if got := uncovered(); got != 0 {
		t.Fatalf("caggs_have_refresh_policy counts %d uncovered aggregates on a fully migrated database, want 0", got)
	}
	if _, err := db.ExecContext(ctx, `SELECT remove_continuous_aggregate_policy('prices_1d')`); err != nil {
		t.Fatalf("remove prices_1d refresh policy: %v", err)
	}
	if got := uncovered(); got != 1 {
		t.Errorf("after dropping prices_1d's refresh policy the check counts %d uncovered aggregates, want 1", got)
	}
}

// caggRefreshPolicyAssertionSQL returns the SQL config-assertions.sh
// runs for caggs_have_refresh_policy, so the test executes the shipped
// bytes rather than a copy.
func caggRefreshPolicyAssertionSQL(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "scripts", "ops", "config-assertions.sh")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	const open = `CAGGS_WITHOUT_REFRESH_POLICY_SQL="`
	_, rest, ok := strings.Cut(string(src), open)
	if !ok {
		t.Fatalf("%s defines no CAGGS_WITHOUT_REFRESH_POLICY_SQL", path)
	}
	query, _, ok := strings.Cut(rest, `"`)
	if !ok || !strings.Contains(query, "continuous_aggregates") {
		t.Fatalf("CAGGS_WITHOUT_REFRESH_POLICY_SQL in %s is unterminated or not the aggregate census: %q", path, query)
	}
	return query
}
