//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
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
	//
	// trades.base_amount / quote_amount are the exception: 0191 drops
	// 0001's `> 0` CHECKs so a one-side-zero SDEX fill can be stored.
	// At full-up the row invariant (>= 0, not both zero) is Go-only —
	// canonical.Trade.Validate on every writer — and there is no DB
	// CHECK rejecting a negative leg. Asserting absence here keeps a
	// future migration from quietly re-adding a CHECK the writers and
	// the 0187 priceable filter no longer expect.
	assertTradesAmountChecksAbsent(t, db, ctx)
	assertInsertAccepted(t, db, ctx, "zero base_amount (one-side-zero fill)", `
        INSERT INTO trades
            (source, ledger, tx_hash, op_index, ts,
             base_asset, quote_asset, base_amount, quote_amount)
        VALUES ('t', 1, 'ab', 0, now(), 'native', 'native', 0, 1)`)
	assertInsertAccepted(t, db, ctx, "zero quote_amount (one-side-zero fill)", `
        INSERT INTO trades
            (source, ledger, tx_hash, op_index, ts,
             base_asset, quote_asset, base_amount, quote_amount)
        VALUES ('t', 1, 'ac', 0, now(), 'native', 'native', 1, 0)`)
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
	// trades compresses through 0205's custom job, which the jobs view
	// attaches to no hypertable; the built-in policy it replaced is gone.
	var tradesCompressionJobs int
	if err := db.QueryRowContext(ctx, `
        SELECT count(*) FROM timescaledb_information.jobs
        WHERE proc_name = 'trades_compression_policy' AND scheduled
          AND config->>'compress_after' IS NOT NULL`).Scan(&tradesCompressionJobs); err != nil {
		t.Fatalf("check trades_compression_policy job: %v", err)
	}
	if tradesCompressionJobs != 1 {
		t.Errorf("expected one scheduled trades_compression_policy job, got %d", tradesCompressionJobs)
	}
	assertPolicyAbsent(t, db, ctx, "trades", "policy_compression")
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
	assertColumnType(t, db, ctx, "defindex_admin_events", "derive_generation", "bigint")

	// 0193 — fx_fixings, the vendor-time FX series: hypertable, the
	// binding index, compression, and no retention policy.
	assertHypertableExists(t, db, ctx, "fx_fixings")
	assertIndexExists(t, db, ctx, "fx_fixings", "fx_fixings_ticker_bar_end_idx")
	assertCompressionEnabled(t, db, ctx, "fx_fixings", true)
	assertPolicyAttached(t, db, ctx, "fx_fixings", "policy_compression")
	assertPolicyAbsent(t, db, ctx, "fx_fixings", "policy_retention")
	assertColumnType(t, db, ctx, "fx_fixings", "generation", "bigint")

	// 0196 — trades.tx_index, the post-insert apply-order tag.
	assertColumnType(t, db, ctx, "trades", "tx_index", "integer")

	// 0203 — sushiswap_v3_position_events: hypertable, compression, no
	// retention, bigint generation from creation.
	assertHypertableExists(t, db, ctx, "sushiswap_v3_position_events")
	assertCompressionEnabled(t, db, ctx, "sushiswap_v3_position_events", true)
	assertPolicyAttached(t, db, ctx, "sushiswap_v3_position_events", "policy_compression")
	assertPolicyAbsent(t, db, ctx, "sushiswap_v3_position_events", "policy_retention")
	assertColumnType(t, db, ctx, "sushiswap_v3_position_events", "derive_generation", "bigint")

	// 0210 — spectra_events: hypertable, compression, no retention, bigint
	// generation; spectra_markets is a plain table.
	assertHypertableExists(t, db, ctx, "spectra_events")
	assertCompressionEnabled(t, db, ctx, "spectra_events", true)
	assertPolicyAttached(t, db, ctx, "spectra_events", "policy_compression")
	assertPolicyAbsent(t, db, ctx, "spectra_events", "policy_retention")
	assertColumnType(t, db, ctx, "spectra_events", "derive_generation", "bigint")
	assertColumnType(t, db, ctx, "spectra_markets", "duration_s", "bigint")

	// ─── Down: roll everything back ─────────────────────────────
	// 0191's down refuses (LOUD) while any trades row has a zero leg;
	// the two probe rows accepted above must go first.
	if _, err := db.ExecContext(ctx, `DELETE FROM trades WHERE base_amount = 0 OR quote_amount = 0`); err != nil {
		t.Fatalf("delete zero-leg probe rows: %v", err)
	}
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
	assertTableAbsent(t, db, ctx, "fx_fixings")
	assertTableAbsent(t, db, ctx, "sushiswap_v3_pools")
	assertTableAbsent(t, db, ctx, "spectra_events")
	assertTableAbsent(t, db, ctx, "spectra_markets")
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

// TestEventIndexDownsRefuseWithDuplicates pins that every down which
// narrows a primary key (0053-0060, 0112) refuses while two rows differ
// only in the column its up added, instead of failing mid-down on ADD
// PRIMARY KEY or collapsing them. One container walks the versions in
// ascending order; each case cleans its rows before the next up.
func TestEventIndexDownsRefuseWithDuplicates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	const (
		h  = `repeat('a', 64)`
		ts = `'2026-01-01T00:00:00Z'`
	)
	cases := []struct {
		version uint
		file    string
		table   string
		insert  string
	}{
		{53, "0053_blend_pk_granularity.down.sql", "blend_positions", `INSERT INTO blend_positions
			(pool, ledger, tx_hash, op_index, ledger_close_time, event_kind, asset, user_address, token_amount, b_or_d_amount)
			VALUES ('P', 1, ` + h + `, 0, ` + ts + `, 'supply', 'A1', 'U', 1, 1),
			       ('P', 1, ` + h + `, 0, ` + ts + `, 'supply', 'A2', 'U', 1, 1)`},
		{53, "0053_blend_pk_granularity.down.sql", "blend_emissions", `INSERT INTO blend_emissions
			(pool, ledger, tx_hash, op_index, ledger_close_time, event_kind, event_index)
			VALUES ('P', 1, ` + h + `, 0, ` + ts + `, 'gulp', 0), ('P', 1, ` + h + `, 0, ` + ts + `, 'gulp', 1)`},
		{53, "0053_blend_pk_granularity.down.sql", "blend_admin", `INSERT INTO blend_admin
			(contract_id, ledger, tx_hash, op_index, ledger_close_time, event_kind, event_index)
			VALUES ('C', 1, ` + h + `, 0, ` + ts + `, 'set_admin', 0), ('C', 1, ` + h + `, 0, ` + ts + `, 'set_admin', 1)`},
		{54, "0054_blend_positions_event_index.down.sql", "blend_positions", `INSERT INTO blend_positions
			(pool, ledger, tx_hash, op_index, ledger_close_time, event_kind, asset, user_address, token_amount, b_or_d_amount, event_index)
			VALUES ('P', 1, ` + h + `, 0, ` + ts + `, 'supply', 'A', 'U', 1, 1, 0),
			       ('P', 1, ` + h + `, 0, ` + ts + `, 'supply', 'A', 'U', 1, 1, 1)`},
		{55, "0055_defindex_flows_event_index.down.sql", "defindex_flows", `INSERT INTO defindex_flows
			(ledger, ledger_close_time, tx_hash, op_index, contract_id, layer, direction, actor, event_index)
			VALUES (1, ` + ts + `, 'h', 0, 'C', 'vault', 'deposit', 'X', 0),
			       (1, ` + ts + `, 'h', 0, 'C', 'vault', 'deposit', 'X', 1)`},
		{56, "0056_soroswap_router_swaps_call_sig.down.sql", "soroswap_router_swaps", `INSERT INTO soroswap_router_swaps
			(ledger, ledger_close_time, tx_hash, op_index, contract_id, function_name, recipient, path, amount_in, amount_out, call_sig)
			VALUES (1, ` + ts + `, 'h', 0, 'C', 'swap_exact_tokens_for_tokens', 'R', ARRAY['a','b'], 1, 1, 'sig0'),
			       (1, ` + ts + `, 'h', 0, 'C', 'swap_exact_tokens_for_tokens', 'R', ARRAY['a','b'], 1, 1, 'sig1')`},
		{57, "0057_sep41_supply_events_event_index.down.sql", "sep41_supply_events", `INSERT INTO sep41_supply_events
			(contract_id, ledger, tx_hash, op_index, observed_at, event_kind, amount, event_index)
			VALUES ('C', 1, ` + h + `, 0, ` + ts + `, 'mint', 1, 0), ('C', 1, ` + h + `, 0, ` + ts + `, 'mint', 1, 1)`},
		{58, "0058_blend_auctions_event_index.down.sql", "blend_auctions", `INSERT INTO blend_auctions
			(pool, auction_type, user_address, ledger, tx_hash, op_index, ts, event_kind, event_index)
			VALUES ('P', 0, 'U', 1, ` + h + `, 0, ` + ts + `, 'new', 0), ('P', 0, 'U', 1, ` + h + `, 0, ` + ts + `, 'new', 1)`},
		{59, "0059_comet_liquidity_event_index.down.sql", "comet_liquidity", `INSERT INTO comet_liquidity
			(contract_id, ledger, ledger_close_time, tx_hash, op_index, event_kind, direction, caller, token, amount, event_index)
			VALUES ('C', 1, ` + ts + `, ` + h + `, 0, 'deposit', 'add', 'X', 'T', 1, 0),
			       ('C', 1, ` + ts + `, ` + h + `, 0, 'deposit', 'add', 'X', 'T', 1, 1)`},
		{60, "0060_phoenix_event_index.down.sql", "phoenix_liquidity", `INSERT INTO phoenix_liquidity
			(pool, ledger, ledger_close_time, tx_hash, op_index, action, sender, amount_a, amount_b, event_index)
			VALUES ('P', 1, ` + ts + `, 'h', 0, 'provide_liquidity', 'S', 1, 1, 0),
			       ('P', 1, ` + ts + `, 'h', 0, 'provide_liquidity', 'S', 1, 1, 1)`},
		{60, "0060_phoenix_event_index.down.sql", "phoenix_stake_events", `INSERT INTO phoenix_stake_events
			(stake_contract, ledger, ledger_close_time, tx_hash, op_index, action, user_addr, lp_token, amount, event_index)
			VALUES ('C', 1, ` + ts + `, 'h', 0, 'bond', 'U', 'L', 1, 0),
			       ('C', 1, ` + ts + `, 'h', 0, 'bond', 'U', 'L', 1, 1)`},
		{112, "0112_cctp_rozo_event_index.down.sql", "cctp_events", `INSERT INTO cctp_events
			(contract_id, ledger, tx_hash, op_index, ts, event_type, event_index)
			VALUES ('C', 1, ` + h + `, 0, ` + ts + `, 'deposit_for_burn', 0), ('C', 1, ` + h + `, 0, ` + ts + `, 'deposit_for_burn', 1)`},
		{112, "0112_cctp_rozo_event_index.down.sql", "rozo_events", `INSERT INTO rozo_events
			(contract_id, ledger, tx_hash, op_index, ts, event_type, amount, destination, event_index)
			VALUES ('C', 1, ` + h + `, 0, ` + ts + `, 'payment', 1, 'D', 0), ('C', 1, ` + h + `, 0, ` + ts + `, 'payment', 1, 'D', 1)`},
	}
	var applied uint
	for _, c := range cases {
		t.Run(strings.TrimSuffix(c.file, ".down.sql")+"/"+c.table, func(t *testing.T) {
			if c.version != applied { // Migrate to the current version returns ErrNoChange
				applyMigrationsUpTo(t, dsn, c.version)
				applied = c.version
			}
			if _, err := db.ExecContext(ctx, c.insert); err != nil {
				t.Fatalf("insert duplicate pair into %s: %v", c.table, err)
			}
			assertDownRefusedWithRows(t, ctx, db, dsn, c.version, c.file, c.table, 2)
			if _, err := db.ExecContext(ctx, `DELETE FROM `+c.table); err != nil {
				t.Fatalf("clean %s: %v", c.table, err)
			}
		})
	}
}

// assertDownRefusedWithRows migrates `from` -> from-1, requires the named
// down file's guard to refuse, requires `table` to still hold `rows`
// rows, then forces the version back to `from`.
func assertDownRefusedWithRows(t *testing.T, ctx context.Context, db *sql.DB, dsn string, from uint, file, table string, rows int) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
	m, err := migrate.New("file://"+migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	err = m.Migrate(from - 1)
	_, _ = m.Close()
	if err == nil || !strings.Contains(err.Error(), file) || !strings.Contains(err.Error(), "LOUD") {
		t.Fatalf("migrate %d -> %d with duplicate %s rows: err = %v, want the %s guard to refuse", from, from-1, table, err, file)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("count %s rows: %v", table, err)
	}
	if n != rows {
		t.Fatalf("%s rows after refused down = %d, want %d", table, n, rows)
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

// TestMigration0112DownOnCompressedChunks pins that 0112's down, which has
// no decompress prelude, still succeeds on compressed chunks (r1 compresses
// both tables) and keeps the rows (#1172 item 8).
func TestMigration0112DownOnCompressedChunks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 112)
	for _, q := range []string{
		`INSERT INTO cctp_events (contract_id, ledger, tx_hash, op_index, ts, event_type)
		 VALUES ('C', 1, repeat('a', 64), 0, '2026-01-01T00:00:00Z', 'deposit_for_burn')`,
		`INSERT INTO rozo_events (contract_id, ledger, tx_hash, op_index, ts, event_type, amount, destination)
		 VALUES ('C', 1, repeat('a', 64), 0, '2026-01-01T00:00:00Z', 'payment', 1, 'D')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	for _, tbl := range []string{"cctp_events", "rozo_events"} {
		compressAllChunks(t, ctx, db, tbl, 1)
	}

	if err := applyMigrationsUpToErr(dsn, 111); err != nil {
		t.Fatalf("migrate 112 -> 111 with compressed chunks: %v", err)
	}
	for _, tbl := range []string{"cctp_events", "rozo_events"} {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+tbl).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		if n != 1 {
			t.Fatalf("%s rows after down = %d, want 1", tbl, n)
		}
	}
}

// TestMigration0004DownRestoresCompression pins GH-1162: 0004's down
// disabled compression on `trades` and named the compression policy
// (0001) as the recovery — but that policy only SCHEDULES a job against
// a table with timescaledb.compress enabled, and it can never recompress
// a table 0004's own down just disabled it on. Migrates up through 0004,
// then rolls back exactly that one migration and asserts compression is
// still enabled, i.e. the down's terminal state matches 0001's, not a
// permanently decompressed table.
func TestMigration0004DownRestoresCompression(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

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

	if err := migrator.Migrate(4); err != nil {
		t.Fatalf("migrate to version 4: %v", err)
	}
	// 0002 registers a CAGG refresh policy on trades' derived views;
	// quiesce it so it can't race the DDL below (same 55P03 class as
	// applyMigrations in storage_test.go).
	quiesceCAGGRefreshPolicies(t, ctx, db)

	assertCompressionEnabled(t, db, ctx, "trades", true)

	// Roll back exactly 0004 (version 4 -> 3).
	if err := migrator.Steps(-1); err != nil {
		t.Fatalf("migrate down one step (0004): %v", err)
	}

	assertCompressionEnabled(t, db, ctx, "trades", true)
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

// assertCompressionEnabled checks timescaledb_information.hypertables'
// compression_enabled column — the reloption an `ALTER TABLE ... SET
// (timescaledb.compress = …)` flips, distinct from whether a
// policy_compression JOB is scheduled (assertPolicyAttached): the job
// can be attached and still fail on every run against a hypertable
// this is false on (GH-1162).
func assertCompressionEnabled(t *testing.T, db *sql.DB, ctx context.Context, hypertable string, want bool) {
	t.Helper()
	var got bool
	err := db.QueryRowContext(ctx, `
        SELECT compression_enabled FROM timescaledb_information.hypertables
        WHERE hypertable_name = $1`, hypertable).Scan(&got)
	if err != nil {
		t.Fatalf("check compression_enabled on %q: %v", hypertable, err)
	}
	if got != want {
		t.Errorf("hypertable %q compression_enabled = %v, want %v", hypertable, got, want)
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

// assertTradesAmountChecksAbsent asserts neither of 0001's inline `> 0`
// amount CHECKs on trades survives at full-up (0191 dropped them).
func assertTradesAmountChecksAbsent(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `
        SELECT count(*) FROM pg_constraint
        WHERE conrelid = 'trades'::regclass
          AND conname IN ('trades_base_amount_check', 'trades_quote_amount_check')`).Scan(&n); err != nil {
		t.Fatalf("count trades amount CHECKs: %v", err)
	}
	if n != 0 {
		t.Fatalf("trades amount CHECKs present = %d, want 0 (0191 drops both; a later migration re-added one)", n)
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
	query := configAssertionSQL(t, "CAGGS_WITHOUT_REFRESH_POLICY_SQL", "continuous_aggregates")
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

// TestTradesCompressionScheduledAssertionSQL executes config-assertions.sh's
// trades_compression_policy_scheduled query against a migrated TimescaleDB:
// a scheduled policy passes, a paused one fails, and a paused one passes
// again only while a session holds the restamp run's advisory lock — the
// lock a killed run's connection drops.
func TestTradesCompressionScheduledAssertionSQL(t *testing.T) {
	query := configAssertionSQL(t, "TRADES_COMPRESSION_SCHEDULED_SQL", "hashtext('"+timescale.USDVolumeRestampLockName+"')")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ok := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, query).Scan(&n); err != nil {
			t.Fatalf("run trades_compression_policy_scheduled SQL: %v", err)
		}
		return n
	}
	p, err := store.TradesCompressionPolicy(ctx)
	if err != nil {
		t.Fatalf("resolve trades policy: %v", err)
	}
	if got := ok(); got != 1 {
		t.Fatalf("scheduled policy: check = %d, want 1", got)
	}
	if err := store.SetJobScheduled(ctx, p.JobID, false); err != nil {
		t.Fatal(err)
	}
	if got := ok(); got != 0 {
		t.Errorf("paused policy, no run holding the lock: check = %d, want 0", got)
	}
	release, err := store.TryUSDVolumeRestampLock(ctx)
	if err != nil {
		t.Fatalf("take the restamp lock: %v", err)
	}
	if got := ok(); got != 1 {
		t.Errorf("paused policy under a live run's lock: check = %d, want 1", got)
	}
	if err := release(ctx); err != nil {
		t.Fatal(err)
	}
	if got := ok(); got != 0 {
		t.Errorf("paused policy after the lock is released: check = %d, want 0", got)
	}
}

// configAssertionSQL returns the SQL config-assertions.sh assigns to
// the shell variable name, so a test executes the shipped bytes rather
// than a copy. marker is a fragment the query must contain.
func configAssertionSQL(t *testing.T, name, marker string) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "scripts", "ops", "config-assertions.sh")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	_, rest, ok := strings.Cut(string(src), name+`="`)
	if !ok {
		t.Fatalf("%s defines no %s", path, name)
	}
	query, _, ok := strings.Cut(rest, `"`)
	if !ok || !strings.Contains(query, marker) {
		t.Fatalf("%s in %s is unterminated or lacks %q: %q", name, path, marker, query)
	}
	return query
}

// compressedDownCase describes one down migration over a hypertable that
// holds compressed chunks. Rows are inserted and compressed right after the
// table's creating migration (stageA) and again just before the migration
// under test (stageB); `reject` must be refused by the restored schema.
type compressedDownCase struct {
	version uint
	table   string
	stageA  uint
	rowA    map[string]string
	rowB    map[string]string
	reject  map[string]string // row the post-down schema must refuse
	check   string            // constraint that must refuse reject (SQLSTATE 23514)
	verify  func(t *testing.T, ctx context.Context, db *sql.DB)
}

// insertGenericRow inserts one row into table, filling every NOT NULL
// column that has no default from its type; over wins. seq keeps rows
// distinct on ledger / tx_hash / numeric key columns.
func insertGenericRow(t *testing.T, ctx context.Context, db *sql.DB, table string, seq int, ts string, over map[string]string) {
	t.Helper()
	if err := tryInsertGenericRow(ctx, db, table, seq, ts, over); err != nil {
		t.Fatalf("insert into %s: %v", table, err)
	}
}

func tryInsertGenericRow(ctx context.Context, db *sql.DB, table string, seq int, ts string, over map[string]string) error {
	rows, err := db.QueryContext(ctx, `SELECT column_name, data_type, is_nullable, column_default IS NOT NULL
		FROM information_schema.columns WHERE table_schema = 'public' AND table_name = $1
		  AND is_generated = 'NEVER' ORDER BY ordinal_position`, table)
	if err != nil {
		return fmt.Errorf("columns of %s: %w", table, err)
	}
	var cols, vals []string
	for rows.Next() {
		var name, typ, nullable string
		var hasDefault bool
		if err := rows.Scan(&name, &typ, &nullable, &hasDefault); err != nil {
			rows.Close()
			return fmt.Errorf("scan: %w", err)
		}
		v, ok := over[name]
		if !ok {
			if nullable == "YES" || hasDefault {
				continue
			}
			switch {
			case strings.Contains(typ, "timestamp"):
				v = "'" + ts + "'"
			case name == "tx_hash":
				v = fmt.Sprintf("repeat('%d', 64)", seq%10)
			case typ == "text" || strings.HasPrefix(typ, "character"):
				v = "'x'"
			case typ == "boolean":
				v = "false"
			case typ == "jsonb" || typ == "json":
				v = "'{}'"
			default: // integer family and numeric
				v = fmt.Sprint(seq)
			}
		}
		cols = append(cols, name)
		vals = append(vals, v)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("columns of %s: %w", table, err)
	}
	rows.Close()
	_, err = db.ExecContext(ctx, `INSERT INTO `+table+` (`+strings.Join(cols, ",")+`) VALUES (`+strings.Join(vals, ",")+`)`)
	return err
}

// compressAllChunks compresses every chunk of table and requires at least
// min. It counts from compress_chunk itself: reading
// timescaledb_information.chunks before a DDL hides the TimescaleDB crash
// these tests exist to catch.
func compressAllChunks(t *testing.T, ctx context.Context, db *sql.DB, table string, min int) {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM (
		SELECT compress_chunk(c, true) FROM show_chunks('`+table+`') c) s`).Scan(&n); err != nil {
		t.Fatalf("compress %s: %v", table, err)
	}
	if n < min {
		t.Fatalf("%s has %d compressed chunks, want >= %d; the test would not exercise compressed chunks", table, n, min)
	}
}

func assertColumnNullable(t *testing.T, ctx context.Context, db *sql.DB, table, col, want string) {
	t.Helper()
	var got string
	if err := db.QueryRowContext(ctx, `SELECT is_nullable FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2`, table, col).Scan(&got); err != nil {
		t.Fatalf("nullable %s.%s: %v", table, col, err)
	}
	if got != want {
		t.Fatalf("%s.%s is_nullable = %s, want %s", table, col, got, want)
	}
}

// TestDownsOnCompressedChunks pins INV-2684: a down on a hypertable with
// compressed chunks restores the schema without losing or changing rows;
// any error, including a Timescale internal one, fails the case.
//
// Most CHECK columns here are segmentby; only freeze_events.reason and
// defindex_flows.direction are compressed columns. No case seeds a row that
// the restored CHECK would refuse, so a refusal is never a pass.
//
// The compressed-chunk state is never read from
// timescaledb_information.chunks before a DDL: that read masks the crash
// TestCompressedAlterControl0174 proves this harness can see.
func TestDownsOnCompressedChunks(t *testing.T) {
	const tsA, tsA2, tsB, tsC, tsD = "2020-01-01T00:00:00Z", "2020-01-01T01:00:00Z", "2026-01-01T00:00:00Z", "2027-06-01T00:00:00Z", "2028-01-01T00:00:00Z"
	enum := func(col, v string) map[string]string { return map[string]string{col: "'" + v + "'"} }
	with := func(m map[string]string, kv ...string) map[string]string {
		out := map[string]string{}
		for k, v := range m {
			out[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			out[kv[i]] = kv[i+1]
		}
		return out
	}
	phoenixBond := map[string]string{"action": "'bond'", "user_addr": "'U'", "amount": "1", "lp_token": "'L'"}
	cases := []compressedDownCase{
		{version: 70, table: "cctp_events", stageA: 38, rowA: enum("event_type", "deposit_for_burn"), rowB: enum("event_type", "deposit_for_burn"), reject: enum("event_type", "mint_and_forward"), check: "cctp_events_event_type_check"},
		{version: 92, table: "cctp_events", stageA: 38, rowA: enum("event_type", "deposit_for_burn"), rowB: enum("event_type", "deposit_for_burn"), reject: enum("event_type", "token_pair_linked"), check: "cctp_events_event_type_check"},
		{version: 94, table: "cctp_events", stageA: 38, rowA: enum("event_type", "deposit_for_burn"), rowB: enum("event_type", "deposit_for_burn"), reject: enum("event_type", "pauser_changed"), check: "cctp_events_event_type_check"},
		{version: 95, table: "blend_backstop_events", stageA: 63, rowA: enum("event_kind", "deposit"), rowB: enum("event_kind", "deposit"), reject: enum("event_kind", "rw_zone"), check: "blend_backstop_events_event_kind_check"},
		{version: 97, table: "blend_emissions", stageA: 45, rowA: enum("event_kind", "gulp"), rowB: enum("event_kind", "gulp"), reject: enum("event_kind", "update_emissions"), check: "blend_emissions_event_kind_check"},
		{version: 97, table: "blend_admin", stageA: 45, rowA: enum("event_kind", "set_admin"), rowB: enum("event_kind", "set_admin"), reject: enum("event_kind", "new_liquidation_auction"), check: "blend_admin_event_kind_check"},
		{
			version: 98, table: "phoenix_stake_events", stageA: 44, rowA: phoenixBond, rowB: phoenixBond, reject: with(phoenixBond, "action", "'withdraw_rewards'"), check: "phoenix_stake_events_action_check",
			verify: func(t *testing.T, ctx context.Context, db *sql.DB) {
				assertColumnNullable(t, ctx, db, "phoenix_stake_events", "user_addr", "NO")
				assertColumnNullable(t, ctx, db, "phoenix_stake_events", "amount", "NO")
			},
		},
		{version: 124, table: "freeze_events", stageA: 18, rowA: enum("reason", "single_source"), rowB: enum("reason", "single_source"), reject: enum("reason", "other"), check: "freeze_events_reason_check"},
		{version: 138, table: "defindex_flows", stageA: 50, rowA: map[string]string{"direction": "'deposit'", "layer": "'vault'"}, rowB: map[string]string{"direction": "'deposit'", "layer": "'vault'"}, reject: map[string]string{"direction": "'harvest'", "layer": "'vault'"}, check: "defindex_flows_direction_check"},
		{version: 145, table: "credit_events", stageA: 90, rowA: enum("event_type", "withdrawal"), rowB: enum("event_type", "withdrawal"), reject: enum("event_type", "treasury_updated"), check: "credit_events_event_type_check"},
		{
			version: 195, table: "phoenix_stake_events", stageA: 44, rowA: phoenixBond, rowB: phoenixBond, reject: with(phoenixBond, "action", "'migration_started'"), check: "phoenix_stake_events_action_check",
			verify: func(t *testing.T, ctx context.Context, db *sql.DB) {
				assertColumnNullable(t, ctx, db, "phoenix_stake_events", "lp_token", "NO")
			},
		},
		{
			version: 195, table: "phoenix_admin_events", stageA: 132, rowA: enum("admin_action", "undo"), rowB: enum("admin_action", "undo"), reject: enum("admin_action", "blend_set_delegate"), check: "phoenix_admin_events_admin_action_check",
			verify: func(t *testing.T, ctx context.Context, db *sql.DB) {
				var n int
				if err := db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns
					WHERE table_name = 'phoenix_admin_events' AND column_name = 'value'`).Scan(&n); err != nil || n != 0 {
					t.Fatalf("phoenix_admin_events.value after down: n=%d err=%v, want column dropped", n, err)
				}
			},
		},
		{
			version: 207, table: "price_source_contributions", stageA: 169, rowA: map[string]string{"window_seconds": "300", "weight": "0.5"}, rowB: map[string]string{"window_seconds": "300", "weight": "0.5"},
			verify: func(t *testing.T, ctx context.Context, db *sql.DB) {
				assertColumnNullable(t, ctx, db, "price_source_contributions", "window_seconds", "YES")
				var n int
				if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_constraint
					WHERE conrelid = 'price_source_contributions'::regclass AND contype = 'p'`).Scan(&n); err != nil || n != 1 {
					t.Fatalf("primary key after down: n=%d err=%v, want 1", n, err)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%04d/%s", c.version, c.table), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			dsn := startTimescale(t, ctx)
			db, err := sql.Open("pgx", dsn)
			if err != nil {
				t.Fatalf("sql.Open: %v", err)
			}
			defer db.Close()

			applyMigrationsUpTo(t, dsn, c.stageA)
			insertGenericRow(t, ctx, db, c.table, 1, tsA, c.rowA)
			compressAllChunks(t, ctx, db, c.table, 1)
			applyMigrationsUpTo(t, dsn, c.version-1)
			var defBefore string
			if c.check != "" {
				defBefore = constraintDef(t, ctx, db, c.table, c.check)
			}
			applyMigrationsUpTo(t, dsn, c.version)
			insertGenericRow(t, ctx, db, c.table, 2, tsB, c.rowB)
			compressAllChunks(t, ctx, db, c.table, 2)
			// A new uncompressed chunk, and a row inside an already compressed one.
			insertGenericRow(t, ctx, db, c.table, 4, tsC, c.rowB)
			insertGenericRow(t, ctx, db, c.table, 5, tsA2, c.rowA)
			var before int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+c.table).Scan(&before); err != nil {
				t.Fatalf("count: %v", err)
			}
			snap := "zz_snap_" + c.table
			if _, err := db.ExecContext(ctx, `CREATE TABLE `+snap+` AS SELECT * FROM `+c.table); err != nil {
				t.Fatalf("snapshot: %v", err)
			}
			colsBefore := tableColumns(t, ctx, db, c.table)

			if err := applyMigrationsUpToErr(dsn, c.version-1); err != nil {
				t.Fatalf("down %d on compressed %s: %v", c.version, c.table, err)
			}
			var after int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+c.table).Scan(&after); err != nil {
				t.Fatalf("count after: %v", err)
			}
			if after != before {
				t.Fatalf("%s rows after down = %d, want %d", c.table, after, before)
			}
			common := commonColumns(colsBefore, tableColumns(t, ctx, db, c.table))
			if hb, ha := rowsDigest(t, ctx, db, snap, common), rowsDigest(t, ctx, db, c.table, common); hb != ha {
				t.Fatalf("%s row digest changed across the down: %s -> %s", c.table, hb, ha)
			}
			var compressed int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM _timescaledb_catalog.chunk ch
				JOIN _timescaledb_catalog.hypertable h ON h.id = ch.hypertable_id
				WHERE h.table_name = $1 AND ch.compressed_chunk_id IS NOT NULL`, c.table).Scan(&compressed); err != nil {
				t.Fatalf("read catalog compression state: %v", err)
			}
			if compressed < 1 {
				t.Fatalf("%s has %d compressed chunks after the down, want >= 1", c.table, compressed)
			}
			if c.check != "" {
				if got := constraintDef(t, ctx, db, c.table, c.check); got != defBefore {
					t.Fatalf("%s definition after down = %s, want %s", c.check, got, defBefore)
				}
			}
			if c.reject != nil {
				err := tryInsertGenericRow(ctx, db, c.table, 3, tsD, c.reject)
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != c.check {
					t.Fatalf("insert %v after down: err = %v, want SQLSTATE 23514 on %s", c.reject, err, c.check)
				}
			}
			if c.verify != nil {
				c.verify(t, ctx, db)
			}
		})
	}
}

func constraintDef(t *testing.T, ctx context.Context, db *sql.DB, table, name string) string {
	t.Helper()
	var def string
	if err := db.QueryRowContext(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = $1::regclass AND conname = $2`, table, name).Scan(&def); err != nil {
		t.Fatalf("constraint %s on %s: %v", name, table, err)
	}
	return def
}

func tableColumns(t *testing.T, ctx context.Context, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1 ORDER BY column_name`, table)
	if err != nil {
		t.Fatalf("columns of %s: %v", table, err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("columns of %s: %v", table, err)
	}
	return cols
}

func commonColumns(a, b []string) []string {
	in := map[string]bool{}
	for _, c := range b {
		in[c] = true
	}
	var out []string
	for _, c := range a {
		if in[c] {
			out = append(out, c)
		}
	}
	return out
}

// rowsDigest hashes the listed columns of every row, ordered by the row's own
// text so the digest does not depend on a primary key the down may change.
func rowsDigest(t *testing.T, ctx context.Context, db *sql.DB, table string, cols []string) string {
	t.Helper()
	q := `SELECT coalesce(md5(string_agg(r, E'\n' ORDER BY r)), '') FROM (SELECT ROW(` + strings.Join(cols, ",") + `)::text AS r FROM ` + table + `) s`
	var d string
	if err := db.QueryRowContext(ctx, q).Scan(&d); err != nil {
		t.Fatalf("digest of %s: %v", table, err)
	}
	return d
}

// TestCompressedAlterControl0174 is the positive control for the instrument
// above: the v0.92.0 body of 0174, a bare ADD CONSTRAINT over compressed
// sep41_transfers chunks, crashes TimescaleDB (SQLSTATE 57P03, "in recovery
// mode"). If this ALTER succeeds the harness cannot see that failure.
func TestCompressedAlterControl0174(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 173)
	if err := insertSEP41TransferAmount(ctx, db, 1, "transfer", "7"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	seedCompressedSEP41TransferChunk(t, ctx, db)

	db2, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open (second connection): %v", err)
	}
	defer db2.Close()
	_, err = db2.ExecContext(ctx, `ALTER TABLE sep41_transfers ADD CONSTRAINT sep41_transfers_amount_check
		CHECK ((amount IS NULL OR amount >= 0)
		       AND (event_kind NOT IN ('transfer', 'approve') OR amount IS NOT NULL))`)
	if err == nil {
		t.Fatal("bare 0174 ALTER over compressed chunks succeeded; the control no longer reproduces the crash")
	}
	t.Logf("control ALTER failed as required: %v", err)
}
