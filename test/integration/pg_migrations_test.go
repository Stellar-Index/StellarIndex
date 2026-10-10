//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/mev"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestHypertableChunkIntervalFloor executes 0062 + 0171 against real
// TimescaleDB: after the full migration set no public hypertable may be
// left with chunks narrower than 7 days (the chunk-count / lock-pressure
// pathology 0062 names). The textual twin is
// internal/storage/timescale/chunk_interval_floor_test.go.
func TestHypertableChunkIntervalFloor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 167)
	if w := chunkInterval(t, ctx, db, "sep41_transfers"); w != "1 day" {
		t.Fatalf("pre-0171 sep41_transfers chunk interval = %q, want \"1 day\" — the fixture no longer reproduces", w)
	}

	applyMigrations(t, dsn)
	const q = `
		SELECT hypertable_name, time_interval::text
		  FROM timescaledb_information.dimensions
		 WHERE hypertable_schema = 'public'
		   AND dimension_type = 'Time'
		   AND time_interval < INTERVAL '7 days'
		 ORDER BY hypertable_name`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query dimensions: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, width string
		if err := rows.Scan(&name, &width); err != nil {
			t.Fatalf("scan: %v", err)
		}
		t.Errorf("hypertable %s has chunk interval %s, want >= 7 days", name, width)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	for _, rel := range []string{
		"oracle_updates", "api_usage_events", "soroswap_skim_events",
		"blend_positions", "blend_emissions", "blend_admin",
		"sep41_transfers", "blend_backstop_events", "aquarius_rewards_events",
	} {
		if w := chunkInterval(t, ctx, db, rel); w != "7 days" {
			t.Errorf("%s chunk interval after 0171 = %q, want \"7 days\"", rel, w)
		}
	}
}

func chunkInterval(t *testing.T, ctx context.Context, db *sql.DB, rel string) string {
	t.Helper()
	const q = `
		SELECT time_interval::text
		  FROM timescaledb_information.dimensions
		 WHERE hypertable_schema = 'public'
		   AND hypertable_name = $1
		   AND dimension_type = 'Time'`
	var w string
	if err := db.QueryRowContext(ctx, q, rel).Scan(&w); err != nil {
		t.Fatalf("read %s chunk interval: %v", rel, err)
	}
	return w
}

const (
	mev0185Ledger   = 60_000_000
	mev0185Attacker = "GATTACKER0185"
	mev0185Victim   = "GVICTIM0185"
	mev0185USDC     = "USDC-" + mevIntegrationAccount
	mev0185XLMSAC   = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
)

type mev0185Scanner struct{ trades []c.Trade }

func (s mev0185Scanner) TradesForArbScan(context.Context, time.Time, int) ([]c.Trade, []string, error) {
	return s.trades, make([]string, len(s.trades)), nil
}

type mev0185Oracles struct{ refs []mev.OracleRef }

func (o mev0185Oracles) OracleUpdatesForMEVScan(context.Context, time.Time, int) ([]mev.OracleRef, error) {
	return o.refs, nil
}

type mev0185Order map[string]uint32

func (o mev0185Order) TxIndexes(context.Context, []string) (map[string]uint32, error) {
	return o, nil
}

func mev0185Hash(n int) string {
	return strings.Repeat("0", 62) + string("0123456789abcdef"[n>>4]) + string("0123456789abcdef"[n&0xf])
}

func mev0185Trade(t *testing.T, n int, taker string, base, quote c.Asset, ts time.Time) c.Trade {
	t.Helper()
	pair, err := c.NewPair(base, quote)
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	return c.Trade{
		Source: "sdex", Ledger: mev0185Ledger, TxHash: mev0185Hash(n), Timestamp: ts,
		Pair: pair, BaseAmount: c.NewAmount(big.NewInt(10_000_000)), QuoteAmount: c.NewAmount(big.NewInt(2_000_000)),
		Taker: taker,
	}
}

// mev0185DetectCurrent runs the CURRENT detectors through the real worker
// into the real store — the writer whose output 0185 must never delete. One
// ledger yields both kinds: the attacker trades native->USDC at tx 1 and
// USDC->native at tx 4, around a victim at tx 3 and an oracle update at tx 2.
func mev0185DetectCurrent(t *testing.T, ctx context.Context, store *timescale.Store) {
	t.Helper()
	usdc, err := c.NewClassicAsset("USDC", mevIntegrationAccount)
	if err != nil {
		t.Fatalf("usdc: %v", err)
	}
	ts := time.Now().UTC().Truncate(time.Second)
	trades := []c.Trade{
		mev0185Trade(t, 1, mev0185Attacker, c.NativeAsset(), usdc, ts),
		mev0185Trade(t, 3, mev0185Victim, c.NativeAsset(), usdc, ts),
		mev0185Trade(t, 4, mev0185Attacker, usdc, c.NativeAsset(), ts),
	}
	oracle := mev.OracleRef{Source: "reflector-dex", Ledger: mev0185Ledger, TxHash: mev0185Hash(2), Asset: mev0185USDC, Timestamp: ts}
	order := mev0185Order{mev0185Hash(1): 1, mev0185Hash(2): 2, mev0185Hash(3): 3, mev0185Hash(4): 4}
	w := mev.NewWorker(mev0185Scanner{trades: trades}, store, mev.WorkerConfig{
		Oracles: mev0185Oracles{refs: []mev.OracleRef{oracle}},
		Order:   order,
	})
	if _, _, err := w.RunOnce(ctx, ts.Add(time.Minute)); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
}

func mev0185Leg(source, base, quote, role string) string {
	return `{"source":"` + source + `","tx_hash":"` + mev0185Hash(9) + `","tx_index":1,"base":"` + base +
		`","quote":"` + quote + `","base_amount":"1","quote_amount":"1","role":"` + role + `"}`
}

func mev0185Insert(t *testing.T, ctx context.Context, store *timescale.Store, kind, key, detail string) {
	t.Helper()
	ok, err := store.InsertMEVEvent(ctx, domain.MEVStoredEvent{
		Kind: kind, Ledger: mev0185Ledger, DetectedAtLedger: mev0185Ledger,
		Timestamp: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		TxHashes:  []string{mev0185Hash(9)}, Accounts: []string{mev0185Attacker},
		DedupKey: key, DetailJSON: []byte(detail),
	})
	if err != nil || !ok {
		t.Fatalf("insert %s: ok=%v err=%v", key, ok, err)
	}
}

// mev0185InsertLegacy writes rows in the shape the legacy detectors
// persisted and returns which of them 0185 must keep.
func mev0185InsertLegacy(t *testing.T, ctx context.Context, store *timescale.Store) map[string]bool {
	t.Helper()
	sandwich := func(a, b string) string {
		return `{"pair":"x","attacker":"` + mev0185Attacker + `","legs":[` + a + `,` +
			mev0185Leg("sdex", "native", mev0185USDC, "victim") + `,` + b + `],"note":"legacy"}`
	}
	oracleSw := func(a, b string) string {
		return `{"asset":"` + mev0185USDC + `","account":"` + mev0185Attacker + `","legs":[` + a + `,` + b + `],"note":"legacy"}`
	}
	rows := []struct {
		kind, key, detail string
		keep              bool
	}{
		{"sandwich", "legacy:same-direction", sandwich(
			mev0185Leg("sdex", "native", mev0185USDC, "bracket"),
			mev0185Leg("sdex", "native", mev0185USDC, "bracket")), false},
		{"sandwich", "legacy:unknown-source", sandwich(
			mev0185Leg("sdex", "native", mev0185USDC, "bracket"),
			mev0185Leg("blend", mev0185USDC, "native", "bracket")), false},
		// Opposite once the XLM SAC is read as native; unnormalised it
		// would compare CAS3... against native and look same-direction.
		{"sandwich", "legacy:opposite-via-sac", sandwich(
			mev0185Leg("soroswap", mev0185XLMSAC, mev0185USDC, "bracket"),
			mev0185Leg("soroswap", mev0185USDC, "native", "bracket")), true},
		{"oracle_sandwich", "legacy:oracle-same-direction", oracleSw(
			mev0185Leg("soroswap", mev0185USDC, "native", "before"),
			mev0185Leg("soroswap", mev0185USDC, "native", "after")), false},
		{"oracle_sandwich", "legacy:oracle-opposite", oracleSw(
			mev0185Leg("soroswap", mev0185USDC, "native", "before"),
			mev0185Leg("sdex", mev0185USDC, "native", "after")), true},
		{"wash_trade", "legacy:other-kind", `{"variant":"self_trade","legs":[],"note":"x"}`, true},
	}
	want := map[string]bool{}
	for _, r := range rows {
		mev0185Insert(t, ctx, store, r.kind, r.key, r.detail)
		want[r.key] = r.keep
	}
	return want
}

func mev0185Keys(t *testing.T, ctx context.Context, store *timescale.Store) map[string]string {
	t.Helper()
	rows, err := store.DB().QueryContext(ctx, `SELECT dedup_key, kind FROM mev_events`)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var key, kind string
		if err := rows.Scan(&key, &kind); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[key] = kind
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// TestMigration0185_DropsOnlyUnprovenDirectionSandwiches: 0185 deletes the
// sandwich / oracle_sandwich rows whose own legs do not prove opposite
// directions, keeps every row the current detectors write, and leaves other
// kinds alone.
func TestMigration0185_DropsOnlyUnprovenDirectionSandwiches(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 184)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	defer func() { _ = store.Close() }()

	mev0185DetectCurrent(t, ctx, store)
	current := mev0185Keys(t, ctx, store)
	kinds := map[string]bool{}
	for _, kind := range current {
		kinds[kind] = true
	}
	if !kinds["sandwich"] || !kinds["oracle_sandwich"] {
		t.Fatalf("fixture: the current detectors wrote %v, want a sandwich and an oracle_sandwich", current)
	}
	want := mev0185InsertLegacy(t, ctx, store)
	for key := range current {
		want[key] = true
	}

	applyMigrations(t, dsn)

	got := mev0185Keys(t, ctx, store)
	for key, keep := range want {
		if _, present := got[key]; present != keep {
			t.Errorf("%s: present after 0185 = %v, want %v", key, present, keep)
		}
	}
}

// TestMigration0164_DeletesCompressedRowsAboveDMLDecompressCap applies 0164
// in r1's shape: cctp_events holds more compressed rows than
// timescaledb.max_tuples_decompressed_per_dml_transaction allows (148,660
// against 100,000 on r1). The unqualified DELETE must take the direct
// compressed-batch delete and succeed; the control shows the same DELETE
// fails on the cap when that path is off, so the test is not vacuous.
func TestMigration0164_DeletesCompressedRowsAboveDMLDecompressCap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 163)

	const cctpRows = 120_001
	// 40 s apart spans ~56 days: nine 7-day chunks.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO cctp_events (contract_id, ledger, tx_hash, op_index, ts, event_type)
		SELECT 'CCTPTESTCONTRACT', g, lpad(to_hex(g), 64, '0'), 0,
		       TIMESTAMPTZ '2026-01-01 00:00:00Z' + g * INTERVAL '40 seconds', 'message_sent'
		  FROM generate_series(1, $1) g`, cctpRows); err != nil {
		t.Fatalf("seed cctp_events: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO rozo_events (contract_id, ledger, tx_hash, op_index, ts, event_type, amount, destination)
		SELECT 'ROZOTESTCONTRACT', g, lpad(to_hex(g), 64, '0'), 0,
		       TIMESTAMPTZ '2026-01-01 00:00:00Z' + g * INTERVAL '10 days', 'payment', 1, 'GDEST'
		  FROM generate_series(1, 5) g`); err != nil {
		t.Fatalf("seed rozo_events: %v", err)
	}
	for _, table := range []string{"cctp_events", "rozo_events"} {
		var n int
		if err := db.QueryRowContext(ctx, `
			SELECT count(*) FROM (SELECT compress_chunk(c) FROM show_chunks($1::regclass) c) s`,
			table).Scan(&n); err != nil {
			t.Fatalf("compress %s: %v", table, err)
		}
		if n < 2 {
			t.Fatalf("compressed %d %s chunks, want >= 2", n, table)
		}
	}

	var capSetting string
	if err := db.QueryRowContext(ctx,
		`SELECT current_setting('timescaledb.max_tuples_decompressed_per_dml_transaction')`).Scan(&capSetting); err != nil {
		t.Fatalf("read decompress cap: %v", err)
	}
	if capSetting != "100000" {
		t.Fatalf("max_tuples_decompressed_per_dml_transaction = %s, want the default 100000 (r1's value)", capSetting)
	}

	// Control: without the direct batch delete the same statement decompresses
	// and trips the cap.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin control tx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL timescaledb.enable_compressed_direct_batch_delete = off`); err != nil {
		_ = tx.Rollback()
		t.Fatalf("disable direct batch delete: %v", err)
	}
	_, ctlErr := tx.ExecContext(ctx, `DELETE FROM cctp_events`)
	_ = tx.Rollback()
	if ctlErr == nil || !strings.Contains(strings.ToLower(ctlErr.Error()), "decompress") {
		t.Fatalf("control DELETE with direct batch delete off: err = %v, want the decompression-limit error", ctlErr)
	}

	applyMigrationsUpTo(t, dsn, 164)
	requireSchemaVersion(t, ctx, db, 164)
	for _, table := range []string{"cctp_events", "rozo_events"} {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("%s has %d rows after 0164, want 0", table, n)
		}
	}
}

const migration0191File = "0191_trades_drop_amount_checks.down.sql"

// TestMigration0191_DropsTradesAmountChecksOverCompressedChunks runs 0191
// against the shape production is in: two populated, compressed trades
// chunks. Up must be catalog-only (every chunk still compressed), remove
// both of 0001's inline CHECKs and admit a one-side-zero fill into a
// compressed chunk's range while prices_1m's 0187 filter keeps it out of
// every price expression. Down must refuse LOUDLY over zero-leg rows and
// over compressed chunks, and restore both CHECKs by their 0001 names once
// neither obstacle remains.
func TestMigration0191_DropsTradesAmountChecksOverCompressedChunks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 190)
	requireSchemaVersion(t, ctx, db, 190)
	requireTradesAmountChecks(t, ctx, db, true, "at 190")
	quiesceCAGGRefreshPolicies(t, ctx, db)

	pair := ohlcDustPair{base: "PRZL-" + priceableIssuer, quote: "native"}
	// Two priceable fills at t0 (two days ago) and one thirty days earlier,
	// so the table spans two 1-day chunks.
	t0 := time.Now().UTC().Add(-48 * time.Hour).Truncate(24 * time.Hour).Add(time.Hour)
	seed(t, db, ctx, pair, []seedTrade{
		{off: 0, base: "1000", quote: "5000", usd: "1"},
		{off: time.Second, base: "1000", quote: "6000", usd: "1"},
		{off: -30 * 24 * time.Hour, base: "1000", quote: "5000", usd: "1"},
	}, t0)
	compressTradesChunks(t, ctx, db, 2)
	requireCompressedTradesChunks(t, ctx, db, 2, "before 0191")

	// At 190 the CHECK is still what rejects a zero leg, for a row aimed
	// at the compressed chunk too.
	assertInsertRejected(t, db, ctx, "zero quote_amount at 190", zeroLegInsert(pair, t0, "ad", "1", "0"))

	applyMigrationsUpTo(t, dsn, 191)
	requireSchemaVersion(t, ctx, db, 191)
	requireCompressedTradesChunks(t, ctx, db, 2, "after 0191 up")
	requireTradesAmountChecks(t, ctx, db, false, "after 0191 up")

	// One zero leg each way, into the compressed chunk's range.
	seed(t, db, ctx, pair, []seedTrade{
		{off: 10 * time.Second, base: "0", quote: "7000"},
		{off: 20 * time.Second, base: "3000", quote: "0"},
	}, t0)
	if n := countZeroLegTrades(t, ctx, db); n != 2 {
		t.Fatalf("zero-leg trades stored after 0191 = %d, want 2", n)
	}
	requireCompressedTradesChunks(t, ctx, db, 2, "after zero-leg inserts")

	if _, err := db.ExecContext(ctx, priceableRefreshRecipe[0]); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}
	got := readPriceableRow(t, ctx, db, "1m", pair, t0)
	assertNumeric(t, "prices_1m.vwap", got.vwap, "5.5")
	assertNumeric(t, "prices_1m.twap", got.twap, "5.5")
	assertNumeric(t, "prices_1m.high_price", got.high, "6")
	assertNumeric(t, "prices_1m.low_price", got.low, "5")
	assertExact(t, "prices_1m.volume_priced", got.volumePriced, "2000")
	assertExact(t, "prices_1m.volume_quote", got.volumeQuote, "18000")
	assertExact(t, "prices_1m.volume", got.volume, "5000")
	if got.tradeCount != 4 {
		t.Errorf("prices_1m.trade_count = %d, want 4 (zero-leg fills are trades, just unpriceable)", got.tradeCount)
	}
	var notional int64
	if err := db.QueryRowContext(ctx, `
		SELECT notional_trade_count FROM prices_1m
		 WHERE base_asset = $1 AND quote_asset = $2 AND bucket <= $3
		 ORDER BY bucket DESC LIMIT 1`, pair.base, pair.quote, t0).Scan(&notional); err != nil {
		t.Fatalf("read prices_1m.notional_trade_count: %v", err)
	}
	if notional != 2 {
		t.Errorf("prices_1m.notional_trade_count = %d, want 2 (only the priceable fills)", notional)
	}

	// twap_1h is hierarchical over prices_1m; the zero-leg fills must not
	// reach it either.
	if _, err := db.ExecContext(ctx, "CALL refresh_continuous_aggregate('twap_1h', now() - INTERVAL '7 days', now(), force => true)"); err != nil {
		t.Fatalf("refresh twap_1h: %v", err)
	}
	var twap sql.NullString
	var samples int64
	if err := db.QueryRowContext(ctx, `
		SELECT twap, sample_count FROM twap_1h
		 WHERE base_asset = $1 AND quote_asset = $2 AND bucket <= $3
		 ORDER BY bucket DESC LIMIT 1`, pair.base, pair.quote, t0).Scan(&twap, &samples); err != nil {
		t.Fatalf("read twap_1h: %v", err)
	}
	assertNumeric(t, "twap_1h.twap", twap, "5.5")
	if samples != 1 {
		t.Errorf("twap_1h.sample_count = %d, want 1 (one priced minute; zero-leg fills add none)", samples)
	}

	// Down with zero-leg rows present: refused before any DDL.
	refuse0191Down(t, ctx, db, dsn, "zero or negative leg")
	if n := countZeroLegTrades(t, ctx, db); n != 2 {
		t.Fatalf("zero-leg trades after refused down = %d, want 2 (the down deleted data)", n)
	}

	// Rows gone, chunks still compressed: refused on the compression guard.
	if _, err := db.ExecContext(ctx, `DELETE FROM trades WHERE base_amount = 0 OR quote_amount = 0`); err != nil {
		t.Fatalf("delete zero-leg rows: %v", err)
	}
	refuse0191Down(t, ctx, db, dsn, "compressed chunks")
	requireCompressedTradesChunks(t, ctx, db, 2, "after refused downs")

	// Neither obstacle: down restores both CHECKs under their 0001 names.
	if _, err := db.ExecContext(ctx, `
		SELECT count(*) FROM (
			SELECT decompress_chunk(c, if_compressed => true) FROM show_chunks('trades') c
		) s`); err != nil {
		t.Fatalf("decompress trades chunks: %v", err)
	}
	requireCompressedTradesChunks(t, ctx, db, 0, "before the clean down")
	applyMigrationsUpTo(t, dsn, 190)
	requireSchemaVersion(t, ctx, db, 190)
	requireTradesAmountChecks(t, ctx, db, true, "after 0191 down")
	assertInsertRejected(t, db, ctx, "zero base_amount after 0191 down", zeroLegInsert(pair, t0, "ae", "0", "1"))
}

// zeroLegInsert is a literal INSERT (assertInsertRejected takes no bind
// parameters) aimed at the seeded chunk.
func zeroLegInsert(p ohlcDustPair, ts time.Time, txHash, base, quote string) string {
	return fmt.Sprintf(`
		INSERT INTO trades (source, ledger, tx_hash, op_index, ts,
		                    base_asset, quote_asset, base_amount, quote_amount)
		VALUES ('sdex', 1, '%s', 0, TIMESTAMPTZ '%s', '%s', '%s', %s, %s)`,
		txHash, ts.Format(time.RFC3339Nano), p.base, p.quote, base, quote)
}

// refuse0191Down runs the 191→190 down expecting the DO-block guard to RAISE
// with the file name and "LOUD", asserts the DDL was rolled back (no CHECK
// re-added) and that golang-migrate left schema_migrations dirty at 190 —
// the state an operator sees — then forces it back to a clean 191.
func refuse0191Down(t *testing.T, ctx context.Context, db *sql.DB, dsn, guard string) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
	m, err := migrate.New("file://"+migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	err = m.Migrate(190)
	_, _ = m.Close() // a failed Migrate keeps its advisory lock; Force needs a fresh instance
	if err == nil || !strings.Contains(err.Error(), migration0191File) ||
		!strings.Contains(err.Error(), "LOUD") || !strings.Contains(err.Error(), guard) {
		t.Fatalf("0191 down with %s: err = %v, want the %s guard to refuse", guard, err, migration0191File)
	}
	requireTradesAmountChecks(t, ctx, db, false, "after refused down ("+guard+")")

	var version int
	var dirty bool
	if err := db.QueryRowContext(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if version != 190 || !dirty {
		t.Fatalf("schema_migrations after refused down = %d dirty=%v, want 190 dirty", version, dirty)
	}
	f, err := migrate.New("file://"+migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = f.Close() }()
	if err := f.Force(191); err != nil {
		t.Fatalf("force 191: %v", err)
	}
	requireSchemaVersion(t, ctx, db, 191)
}

func requireTradesAmountChecks(t *testing.T, ctx context.Context, db *sql.DB, want bool, when string) {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM pg_constraint
		 WHERE conrelid = 'trades'::regclass
		   AND conname IN ('trades_base_amount_check', 'trades_quote_amount_check')`).Scan(&n); err != nil {
		t.Fatalf("read pg_constraint: %v", err)
	}
	wantN := 0
	if want {
		wantN = 2
	}
	if n != wantN {
		t.Fatalf("%s: trades amount CHECKs present = %d, want %d", when, n, wantN)
	}
}

// compressTradesChunks compresses every trades chunk, counted from
// compress_chunk itself rather than the information view.
func compressTradesChunks(t *testing.T, ctx context.Context, db *sql.DB, want int) {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM (
			SELECT compress_chunk(c, if_not_compressed => true) FROM show_chunks('trades') c
		) s`).Scan(&n); err != nil {
		t.Fatalf("compress trades chunks: %v", err)
	}
	if n != want {
		t.Fatalf("compress_chunk over trades touched %d chunks, want %d", n, want)
	}
}

func requireCompressedTradesChunks(t *testing.T, ctx context.Context, db *sql.DB, want int, when string) {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM timescaledb_information.chunks
		 WHERE hypertable_name = 'trades' AND is_compressed`).Scan(&n); err != nil {
		t.Fatalf("read timescaledb_information.chunks: %v", err)
	}
	if n != want {
		t.Fatalf("%s: compressed trades chunks = %d, want %d", when, n, want)
	}
}

func countZeroLegTrades(t *testing.T, ctx context.Context, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM trades WHERE base_amount = 0 OR quote_amount = 0`).Scan(&n); err != nil {
		t.Fatalf("count zero-leg trades: %v", err)
	}
	return n
}

// TestMigration0197_TradesCompressionClearsTheRollWindow pins both halves of
// the fix: the asset-character roll locks only chunks inside its 14-day
// window, and trades compress only after that window, so the two never
// contend for the same chunk.
func TestMigration0197_TradesCompressionClearsTheRollWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c.InstallAliasRegistry(nil)

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 196)
	requireSchemaVersion(t, ctx, db, 196)
	quiesceCAGGRefreshPolicies(t, ctx, db)
	if got := tradesCompressAfter(t, ctx, db); got != "7 days" {
		t.Fatalf("before 0197 trades compress_after = %q, want 7 days", got)
	}

	now := time.Now().UTC()
	for i, days := range []int{40, 2} {
		insertTxIndexTrade(t, ctx, db, "sdex", 59_000_000+i, fmt.Sprintf("rw%d", i), now.Add(-time.Duration(days)*24*time.Hour))
	}
	var oldChunk string
	if err := db.QueryRowContext(ctx, `
		SELECT c::text FROM show_chunks('trades', older_than => now() - interval '30 days') c`).Scan(&oldChunk); err != nil {
		t.Fatalf("find the 40-day-old chunk: %v", err)
	}

	applyMigrationsUpTo(t, dsn, 197)
	requireSchemaVersion(t, ctx, db, 197)
	if got := tradesCompressAfter(t, ctx, db); got != "15 days" {
		t.Fatalf("after 0197 trades compress_after = %q, want 15 days", got)
	}

	// A lock on a chunk outside the window must not stall the roll; before
	// the literal window it needed ACCESS SHARE on every chunk.
	holder, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.ExecContext(ctx, `LOCK TABLE `+oldChunk+` IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock %s: %v", oldChunk, err)
	}
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	defer func() { _ = store.Close() }()
	rctx, rcancel := context.WithTimeout(ctx, 20*time.Second)
	defer rcancel()
	if err := store.RefreshAssetVolumeCharacter(rctx); err != nil {
		t.Fatalf("roll with a chunk outside its window locked: %v", err)
	}
	_ = holder.Rollback()

	applyMigrationsUpTo(t, dsn, 196)
	requireSchemaVersion(t, ctx, db, 196)
	if got := tradesCompressAfter(t, ctx, db); got != "7 days" {
		t.Errorf("after 0197 down trades compress_after = %q, want 7 days", got)
	}
}

func tradesCompressAfter(t *testing.T, ctx context.Context, db *sql.DB) string {
	t.Helper()
	var v string
	if err := db.QueryRowContext(ctx, `
		SELECT config->>'compress_after' FROM timescaledb_information.jobs
		 WHERE proc_name = 'policy_compression' AND hypertable_name = 'trades'`).Scan(&v); err != nil {
		t.Fatalf("read trades compress_after: %v", err)
	}
	return v
}

// TestMigration0205_TradesCompressionGivesUpOnALockedChunk pins that the
// trades compression job bounds its wait for a chunk lock: behind a reader
// it fails within seconds and leaves the chunk uncompressed instead of
// queueing every later reader of the chunk behind it.
func TestMigration0205_TradesCompressionGivesUpOnALockedChunk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c.InstallAliasRegistry(nil)

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 204)
	requireSchemaVersion(t, ctx, db, 204)
	quiesceCAGGRefreshPolicies(t, ctx, db)
	before := tradesCompressionJobs(t, ctx, db)
	if before.builtin != 1 || before.bounded != 0 {
		t.Fatalf("before 0205: built-in=%d bounded=%d, want 1 and 0", before.builtin, before.bounded)
	}

	applyMigrationsUpTo(t, dsn, 205)
	requireSchemaVersion(t, ctx, db, 205)
	after := tradesCompressionJobs(t, ctx, db)
	if after.builtin != 0 || after.bounded != 1 {
		t.Fatalf("after 0205: built-in=%d bounded=%d, want 0 and 1", after.builtin, after.bounded)
	}
	if after.job != before.job {
		t.Errorf("after 0205 the job settings changed:\n got %+v\nwant %+v", after.job, before.job)
	}

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	defer func() { _ = store.Close() }()
	p, err := store.TradesCompressionPolicy(ctx)
	if err != nil {
		t.Fatalf("resolve the trades compression job: %v", err)
	}
	if p.CompressAfter != 15*24*time.Hour || !p.Scheduled {
		t.Fatalf("resolved job = %+v, want scheduled with compress_after 15 days", p)
	}
	// Run it by hand only, so the scheduler cannot race the assertions.
	if err := store.SetJobScheduled(ctx, p.JobID, false); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	insertTxIndexTrade(t, ctx, db, "sdex", 59_100_000, "lt-old", now.Add(-40*24*time.Hour))
	insertTxIndexTrade(t, ctx, db, "sdex", 59_100_001, "lt-new", now.Add(-2*24*time.Hour))
	var oldChunk string
	if err := db.QueryRowContext(ctx, `
		SELECT c::text FROM show_chunks('trades', older_than => now() - interval '30 days') c`).Scan(&oldChunk); err != nil {
		t.Fatalf("find the 40-day-old chunk: %v", err)
	}

	overdue := overdueCompressionProbe(t)
	if got := overdueTradesChunks(t, ctx, db, overdue); got != 1 {
		t.Fatalf("overdue-compression probe counts %d trades chunks before compression, want 1", got)
	}

	holder, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.ExecContext(ctx, `SELECT count(*) FROM `+oldChunk); err != nil {
		t.Fatalf("read %s: %v", oldChunk, err)
	}

	runJob := func(budget time.Duration) (time.Duration, error) {
		rctx, rcancel := context.WithTimeout(ctx, budget)
		defer rcancel()
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("conn: %v", err)
		}
		defer func() { _ = conn.Close() }()
		start := time.Now()
		_, err = conn.ExecContext(rctx, fmt.Sprintf(`CALL run_job(%d)`, p.JobID))
		elapsed := time.Since(start)
		// A failed run leaves its session lock_timeout set; keep it off the
		// pool. A connection a cancelled run broke is discarded anyway.
		_, _ = conn.ExecContext(ctx, `RESET lock_timeout`)
		return elapsed, err
	}

	elapsed, err := runJob(60 * time.Second)
	// TimescaleDB reports the chunk's lock timeout as "columnstore policy failure".
	if err == nil || !strings.Contains(err.Error(), "policy failure") || elapsed >= 30*time.Second {
		t.Fatalf("compression behind a reader: err=%v after %s, want a policy failure within 30s", err, elapsed)
	}
	if chunkIsCompressed(t, ctx, db, oldChunk) {
		t.Fatal("the failed run compressed the chunk")
	}

	_ = holder.Rollback()
	if _, err := runJob(2 * time.Minute); err != nil {
		t.Fatalf("compression with the reader gone: %v", err)
	}
	if !chunkIsCompressed(t, ctx, db, oldChunk) {
		t.Fatal("the uncontended run left the old chunk uncompressed")
	}
	if got := overdueTradesChunks(t, ctx, db, overdue); got != 0 {
		t.Errorf("overdue-compression probe counts %d trades chunks after compression, want 0", got)
	}

	if err := store.SetJobScheduled(ctx, p.JobID, true); err != nil {
		t.Fatal(err)
	}
	applyMigrationsUpTo(t, dsn, 204)
	requireSchemaVersion(t, ctx, db, 204)
	down := tradesCompressionJobs(t, ctx, db)
	if down.builtin != 1 || down.bounded != 0 {
		t.Fatalf("after 0205 down: built-in=%d bounded=%d, want 1 and 0", down.builtin, down.bounded)
	}
	if down.job != before.job {
		t.Errorf("after 0205 down the job settings changed:\n got %+v\nwant %+v", down.job, before.job)
	}
	var procs int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_proc WHERE proname = 'trades_compression_policy'`).Scan(&procs); err != nil || procs != 0 {
		t.Errorf("after 0205 down trades_compression_policy procedures = %d (err %v), want 0", procs, err)
	}
}

// TestMigration0205_RefusesWhileARestampHoldsItsLock pins that the swap
// does not delete the built-in job under a live usd-volume-restamp -write.
func TestMigration0205_RefusesWhileARestampHoldsItsLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c.InstallAliasRegistry(nil)

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 204)
	requireSchemaVersion(t, ctx, db, 204)
	quiesceCAGGRefreshPolicies(t, ctx, db)

	holder, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("holder conn: %v", err)
	}
	defer func() { _ = holder.Close() }()
	var got bool
	if err := holder.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtext($1::text))`,
		timescale.USDVolumeRestampLockName).Scan(&got); err != nil || !got {
		t.Fatalf("take the restamp lock: got=%v err=%v", got, err)
	}

	err = applyMigrationsUpToErr(dsn, 205)
	if err == nil || !strings.Contains(err.Error(), "usd-volume-restamp") {
		t.Fatalf("0205 with the restamp lock held: err=%v, want a refusal naming usd-volume-restamp", err)
	}
	if before := tradesCompressionJobs(t, ctx, db); before.builtin != 1 || before.bounded != 0 {
		t.Fatalf("after the refusal: built-in=%d bounded=%d, want 1 and 0", before.builtin, before.bounded)
	}

	// A refused step leaves schema_migrations dirty; put it back to retry.
	if _, err := db.ExecContext(ctx, `UPDATE schema_migrations SET version = 204, dirty = false`); err != nil {
		t.Fatalf("clear the dirty flag: %v", err)
	}
	var released bool
	if err := holder.QueryRowContext(ctx, `SELECT pg_advisory_unlock(hashtext($1::text))`,
		timescale.USDVolumeRestampLockName).Scan(&released); err != nil || !released {
		t.Fatalf("release the restamp lock: %v %v", released, err)
	}
	applyMigrationsUpTo(t, dsn, 205)
	requireSchemaVersion(t, ctx, db, 205)
	if after := tradesCompressionJobs(t, ctx, db); after.builtin != 0 || after.bounded != 1 {
		t.Fatalf("after 0205: built-in=%d bounded=%d, want 0 and 1", after.builtin, after.bounded)
	}
}

type tradesCompressionJobSettings struct {
	schedule, maxRuntime, retryPeriod string
	maxRetries                        int
	scheduled                         bool
	compressAfter                     string
}

type tradesCompressionJobCensus struct {
	builtin, bounded int
	job              tradesCompressionJobSettings
}

// tradesCompressionJobs counts both job shapes and reads the settings of
// whichever one exists.
func tradesCompressionJobs(t *testing.T, ctx context.Context, db *sql.DB) tradesCompressionJobCensus {
	t.Helper()
	var r tradesCompressionJobCensus
	const builtin = `j.proc_name = 'policy_compression' AND j.hypertable_schema = current_schema() AND j.hypertable_name = 'trades'`
	const bounded = `j.proc_schema = current_schema() AND j.proc_name = 'trades_compression_policy'`
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE `+builtin+`), count(*) FILTER (WHERE `+bounded+`)
		  FROM timescaledb_information.jobs j`).Scan(&r.builtin, &r.bounded); err != nil {
		t.Fatalf("count trades compression jobs: %v", err)
	}
	if r.builtin+r.bounded != 1 {
		return r
	}
	if err := db.QueryRowContext(ctx, `
		SELECT j.schedule_interval::text, j.max_runtime::text, j.retry_period::text,
		       j.max_retries, j.scheduled, j.config->>'compress_after'
		  FROM timescaledb_information.jobs j
		 WHERE (`+builtin+`) OR (`+bounded+`)`).Scan(
		&r.job.schedule, &r.job.maxRuntime, &r.job.retryPeriod,
		&r.job.maxRetries, &r.job.scheduled, &r.job.compressAfter); err != nil {
		t.Fatalf("read the trades compression job: %v", err)
	}
	return r
}

func chunkIsCompressed(t *testing.T, ctx context.Context, db *sql.DB, chunk string) bool {
	t.Helper()
	var compressed bool
	if err := db.QueryRowContext(ctx, `
		SELECT is_compressed FROM timescaledb_information.chunks
		 WHERE format('%I.%I', chunk_schema, chunk_name) = $1`, chunk).Scan(&compressed); err != nil {
		t.Fatalf("read %s compression state: %v", chunk, err)
	}
	return compressed
}

// overdueCompressionProbe is the stellarindex_timescale_chunks_overdue_compression
// query exactly as the node_exporter probe ships it.
func overdueCompressionProbe(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "configs", "ansible", "roles", "archival-node", "tasks", "10-observability.yml")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	_, rest, ok := strings.Cut(string(src), `q "SELECT COALESCE(j.hypertable_name, 'trades'),`)
	if !ok {
		t.Fatalf("%s has no overdue-compression query mapping the trades job", path)
	}
	query, _, ok := strings.Cut(rest, `"`)
	if !ok {
		t.Fatalf("unterminated overdue-compression query in %s", path)
	}
	return "SELECT COALESCE(j.hypertable_name, 'trades')," + query
}

func overdueTradesChunks(t *testing.T, ctx context.Context, db *sql.DB, query string) int {
	t.Helper()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("run the overdue-compression probe: %v", err)
	}
	defer rows.Close()
	n := -1
	for rows.Next() {
		var ht string
		var overdue int
		if err := rows.Scan(&ht, &overdue); err != nil {
			t.Fatalf("scan the overdue-compression probe: %v", err)
		}
		if ht == "trades" {
			n = overdue
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the overdue-compression probe: %v", err)
	}
	if n < 0 {
		t.Fatal("the overdue-compression probe emits no trades row")
	}
	return n
}

// TestMigration0206_ClearsOnlyEmptiedSourcesRebuildCheckpoints applies 0206
// over checkpoints for the sources 0137/0164 emptied and 0203 extended, a neighbour whose
// name shares a prefix, another projected source and the live projector's own
// cursors. Only the projected-rebuild rows for comet, cctp, rozo and
// sushiswap_v3 may go.
func TestMigration0206_ClearsOnlyEmptiedSourcesRebuildCheckpoints(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 205)

	seed := [][2]string{
		{"projected-rebuild", "comet:51499000-51548999"},
		{"projected-rebuild", "cctp:62146641-62196640"},
		{"projected-rebuild", "rozo:60829397-60879396"},
		{"projected-rebuild", "rozo:60879397-60929396"},
		{"projected-rebuild", "sushiswap_v3:61487379-61537378"},
		{"projected-rebuild", "comet_v2:51499000-51548999"},
		{"projected-rebuild", "blend_backstop:51499546-51549545"},
		{"projector", "comet"},
		{"projector", "cctp"},
		{"projector", "rozo"},
	}
	for _, r := range seed {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO ingestion_cursors (source, sub_source, last_ledger) VALUES ($1, $2, 62200000)`,
			r[0], r[1]); err != nil {
			t.Fatalf("seed %s/%s: %v", r[0], r[1], err)
		}
	}

	applyMigrationsUpTo(t, dsn, 206)

	rows, err := db.QueryContext(ctx,
		`SELECT source || '/' || sub_source FROM ingestion_cursors ORDER BY 1`)
	if err != nil {
		t.Fatalf("read cursors: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, k)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	want := []string{
		"projected-rebuild/blend_backstop:51499546-51549545",
		"projected-rebuild/comet_v2:51499000-51548999",
		"projector/cctp",
		"projector/comet",
		"projector/rozo",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("cursors after 0206 = %q, want %q", got, want)
	}
}

// TestMigration0209_OraclePublishedPrice pins the migration end to end: the column
// lands on a hypertable with a compressed chunk, pre-existing rows read as
// "not recorded", the writer round-trips the on-chain integer bit-for-bit
// beside an unchanged price, the derive_generation guard governs it like
// every other column, and the down migration drops it.
func TestMigration0209_OraclePublishedPrice(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c.InstallAliasRegistry(nil)

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 207)
	requireSchemaVersion(t, ctx, db, 207)
	quiesceCAGGRefreshPolicies(t, ctx, db)

	mxne, err := c.NewCryptoAsset("MXNe")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-60 * 24 * time.Hour).Truncate(time.Second)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO oracle_updates (source, ledger, tx_hash, op_index, ts, asset, quote, price, decimals)
		VALUES ('redstone', 60000000, $1, 0, $2, $3, 'fiat:USD', 5747126, 8)`,
		strings.Repeat("ab", 32), old, mxne.String()); err != nil {
		t.Fatalf("insert pre-0209 row: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`SELECT compress_chunk(ch) FROM show_chunks('oracle_updates') ch`); err != nil {
		t.Fatalf("compress oracle_updates chunks: %v", err)
	}

	applyMigrationsUpTo(t, dsn, 209)
	requireSchemaVersion(t, ctx, db, 209)

	live, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	defer func() { _ = live.Close() }()

	got, err := live.LatestOracleUpdateForAsset(ctx, "redstone", mxne)
	if err != nil {
		t.Fatalf("read pre-0209 row: %v", err)
	}
	if got.PublishedPrice != nil {
		t.Errorf("pre-0209 row published_price = %s, want nil (not recorded)", got.PublishedPrice)
	}

	usd, err := c.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	r := c.NewAmount(big.NewInt(1_740_000_001))
	u := c.OracleUpdate{
		Source: "redstone", Ledger: 61_000_000, TxHash: strings.Repeat("cd", 32),
		Timestamp: time.Now().UTC().Truncate(time.Second), Asset: mxne, Quote: usd,
		Price: c.NewAmount(big.NewInt(5_747_126)), PublishedPrice: &r, Decimals: 8,
	}
	if err := live.InsertOracleUpdate(ctx, u); err != nil {
		t.Fatalf("insert: %v", err)
	}
	assertPublished := func(want string) {
		t.Helper()
		got, err := live.LatestOracleUpdateForAsset(ctx, "redstone", mxne)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if got.Price.String() != "5747126" {
			t.Errorf("price = %s, want 5747126 (unchanged)", got.Price)
		}
		if got.PublishedPrice == nil || got.PublishedPrice.String() != want {
			t.Errorf("published_price = %v, want %s", got.PublishedPrice, want)
		}
	}
	assertPublished("1740000001")

	rederive, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	defer func() { _ = rederive.Close() }()
	rederive.SetDeriveGeneration(5)
	corrected := c.NewAmount(big.NewInt(1_740_000_000))
	u.PublishedPrice = &corrected
	if err := rederive.InsertOracleUpdate(ctx, u); err != nil {
		t.Fatalf("re-derive insert: %v", err)
	}
	assertPublished("1740000000")

	u.PublishedPrice = &r
	if err := live.InsertOracleUpdate(ctx, u); err != nil {
		t.Fatalf("gen-0 replay insert: %v", err)
	}
	assertPublished("1740000000") // a lower generation never reverts a correction

	applyMigrationsUpTo(t, dsn, 207)
	requireSchemaVersion(t, ctx, db, 207)
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'oracle_updates' AND column_name = 'published_price'`).Scan(&n); err != nil {
		t.Fatalf("read columns: %v", err)
	}
	if n != 0 {
		t.Errorf("after 0209 down published_price still present")
	}
}

// moneyColumnExceptions are the migrated columns whose DDL carries a
// `-- lint-money:ok <reason>` marker; the test requires both halves to agree.
var moneyColumnExceptions = map[string]bool{
	"public.sdex_offer_events.price_n":   true,
	"public.sdex_offer_events.price_d":   true,
	"public.defindex_fees.fee_index":     true,
	"public.sushiswap_v3_pools.fee_pips": true,

	"public.asset_volume_24h.unpriced_trades": true, // trade count, not an amount
}

// TestMoneyColumnsAreNumeric is the runtime half of the ADR-0003 money
// gate. scripts/ci/lint-migrations.sh greps single-line `name type` DDL,
// so a CAGG/view column typed by its expression (`sum(x)::double precision
// AS volume_usd`), a type on the next line or a float DOMAIN all pass it.
// The catalog sees the resolved type of every table, view, materialized
// view and foreign table column whatever the syntax, so this asserts no
// money-named column in the fully migrated schema is an integer, float or
// money type, nor a domain or array over one.
func TestMoneyColumnsAreNumeric(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	_, thisFile, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(thisFile), "..", "..")
	namePattern := lintMoneyNamePattern(t, repo)
	markedColumns := lintMoneyMarkedColumns(t, repo)

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	flagged := nonNumericMoneyColumns(ctx, t, db, namePattern)
	for _, col := range sortedKeys(flagged) {
		if !moneyColumnExceptions[col] {
			t.Errorf("%s is %s; a monetary column must be NUMERIC (ADR-0003) — cast the expression to numeric, or mark the DDL `-- lint-money:ok <reason>` and add it to moneyColumnExceptions",
				col, flagged[col])
		}
	}
	for _, col := range sortedKeys(moneyColumnExceptions) {
		if _, ok := flagged[col]; !ok {
			t.Errorf("moneyColumnExceptions entry %s is no longer a non-NUMERIC money column — remove it", col)
		}
		if !markedColumns[col[strings.LastIndex(col, ".")+1:]] {
			t.Errorf("moneyColumnExceptions entry %s has no `-- lint-money:ok <reason>` marker in migrations/*.up.sql", col)
		}
	}

	// The shapes the catalog query must resolve, so a blind spot in it
	// fails here rather than passing the migrated schema vacuously.
	probe := map[string]string{
		"money_gate_probe.probe_t.amounts":         "",
		"money_gate_probe.probe_t.fee_d":           "",
		"money_gate_probe.probe_t.reserves":        "",
		"money_gate_probe.probe_m.price_usd":       "",
		"money_gate_probe.probe_v.volume_usd":      "",
		"money_gate_probe.probe_ok.price_numeric":  "numeric",
		"money_gate_probe.probe_ok.amount_numeric": "numeric[]",
	}
	for _, stmt := range []string{
		`CREATE SCHEMA money_gate_probe`,
		`CREATE DOMAIN money_gate_probe.f8 AS double precision`,
		`CREATE DOMAIN money_gate_probe.f8s AS money_gate_probe.f8[]`,
		`CREATE TABLE money_gate_probe.probe_t (amounts double precision[], fee_d money_gate_probe.f8, reserves money_gate_probe.f8s)`,
		`CREATE MATERIALIZED VIEW money_gate_probe.probe_m AS SELECT 1.0::float8 AS price_usd`,
		`CREATE VIEW money_gate_probe.probe_v AS SELECT 1::bigint AS volume_usd`,
		`CREATE TABLE money_gate_probe.probe_ok (price_numeric numeric, amount_numeric numeric[])`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("probe %q: %v", stmt, err)
		}
	}
	probed := nonNumericMoneyColumns(ctx, t, db, namePattern)
	for _, col := range sortedKeys(probe) {
		_, got := probed[col]
		if want := probe[col] == ""; got != want {
			t.Errorf("catalog probe %s: flagged=%v, want %v", col, got, want)
		}
	}
}

// nonNumericMoneyColumns maps each money-named column whose type resolves,
// through domains and array elements, to an integer, float or money type
// to its declared type.
func nonNumericMoneyColumns(ctx context.Context, t *testing.T, db *sql.DB, namePattern string) map[string]string {
	t.Helper()
	// pg_catalog rather than information_schema: the latter omits
	// materialized views and reports every array as data_type ARRAY.
	rows, err := db.QueryContext(ctx, `
		WITH RECURSIVE resolved(col, declared, typ) AS (
			SELECT n.nspname || '.' || c.relname || '.' || a.attname,
			       format_type(a.atttypid, a.atttypmod), a.atttypid
			  FROM pg_attribute a
			  JOIN pg_class c ON c.oid = a.attrelid
			  JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f')
			   AND a.attnum > 0 AND NOT a.attisdropped
			   AND n.nspname !~ '^(pg_|information_schema$|_?timescaledb)'
			   AND a.attname ~* $1
			UNION ALL
			SELECT r.col, r.declared,
			       CASE WHEN t.typtype = 'd' THEN t.typbasetype ELSE t.typelem END
			  FROM resolved r
			  JOIN pg_type t ON t.oid = r.typ
			 WHERE t.typtype = 'd' OR (t.typcategory = 'A' AND t.typelem <> 0)
		)
		SELECT DISTINCT col, declared
		  FROM resolved
		 WHERE typ = ANY ('{int2,int4,int8,float4,float8,money}'::regtype[])`,
		"^("+namePattern+")$")
	if err != nil {
		t.Fatalf("query money columns: %v", err)
	}
	defer rows.Close()

	flagged := map[string]string{}
	for rows.Next() {
		var col, declared string
		if err := rows.Scan(&col, &declared); err != nil {
			t.Fatalf("scan: %v", err)
		}
		flagged[col] = declared
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return flagged
}

// lintMoneyNamePattern reads the stem regex from lint-migrations.sh so the
// DDL grep and this runtime check cannot disagree on what is money.
func lintMoneyNamePattern(t *testing.T, repo string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(repo, "scripts", "ci", "lint-migrations.sh"))
	if err != nil {
		t.Fatalf("read lint-migrations.sh: %v", err)
	}
	m := regexp.MustCompile(`(?m)^name='([^']+)'$`).FindSubmatch(src)
	if m == nil {
		t.Fatal("lint-migrations.sh: no `name='...'` money-stem pattern line")
	}
	return string(m[1])
}

// lintMoneyMarkedColumns returns the leading column name of every
// migrations/*.up.sql line carrying a `-- lint-money:ok` marker.
func lintMoneyMarkedColumns(t *testing.T, repo string) map[string]bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(repo, "migrations", "*.up.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	leading := regexp.MustCompile(`^\s*"?([A-Za-z0-9_]+)"?\s`)
	marked := map[string]bool{}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(line, "lint-money:ok") {
				continue
			}
			if m := leading.FindStringSubmatch(line); m != nil {
				marked[strings.ToLower(m[1])] = true
			}
		}
	}
	return marked
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestStoreSchemaMigrationVersion runs the indexer/aggregator readiness
// reader against a real migrated TimescaleDB: at the migrations
// head the schema check passes; with the applied head rolled back one
// version it fails, which is what makes their /readyz — and so the deploy
// gate — refuse a binary swapped ahead of its migrations.
func TestStoreSchemaMigrationVersion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	version, dirty, err := store.SchemaMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaMigrationVersion: %v", err)
	}
	if version != v1.ExpectedSchemaVersion || dirty {
		t.Fatalf("SchemaMigrationVersion = (%d, dirty=%v), want (%d, dirty=false)", version, dirty, v1.ExpectedSchemaVersion)
	}
	checker := v1.NewSchemaVersionChecker(store)
	if err := checker.Ping(ctx); err != nil {
		t.Fatalf("schema check at migrations head: %v", err)
	}

	if _, err := store.DB().ExecContext(ctx, `UPDATE schema_migrations SET version = $1`, v1.ExpectedSchemaVersion-1); err != nil {
		t.Fatalf("roll back schema_migrations: %v", err)
	}
	version, _, err = store.SchemaMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaMigrationVersion after rollback: %v", err)
	}
	if version != v1.ExpectedSchemaVersion-1 {
		t.Fatalf("SchemaMigrationVersion after rollback = %d, want %d", version, v1.ExpectedSchemaVersion-1)
	}
	if err := checker.Ping(ctx); err == nil {
		t.Fatal("schema check passed with the applied head one behind the binary; want a mismatch error")
	}

	if _, err := store.DB().ExecContext(ctx, `DELETE FROM schema_migrations`); err != nil {
		t.Fatalf("empty schema_migrations: %v", err)
	}
	version, dirty, err = store.SchemaMigrationVersion(ctx)
	if err != nil || version != 0 || dirty {
		t.Fatalf("SchemaMigrationVersion on empty table = (%d, %v, %v), want (0, false, nil)", version, dirty, err)
	}
}

// TestOpen_CtxCancelStopsBackend guards that pgx keeps sending a CancelRequest on ctx deadline (pgconn asyncClose), so a pgx upgrade that drops it fails here.
func TestOpen_CtxCancelStopsBackend(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()

	cases := []struct {
		name   string
		marker string
		run    func(context.Context, string) error
	}{
		{"exec", "ctx_cancel_backend_exec", func(qctx context.Context, q string) error {
			_, err := db.ExecContext(qctx, q)
			return err
		}},
		{"query", "ctx_cancel_backend_query", func(qctx context.Context, q string) error {
			var v string
			return db.QueryRowContext(qctx, q).Scan(&v)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := "SELECT pg_sleep(30)::text /* " + tc.marker + " */"

			// Control: with no deadline the probe must see the backend active.
			cctx, ccancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- tc.run(cctx, "SELECT pg_sleep(30)::text /* "+tc.marker+"_ctl */") }()
			seen := false
			for i := 0; i < 40 && !seen; i++ {
				seen = backendsRunning(t, ctx, db, tc.marker+"_ctl") > 0
				if !seen {
					time.Sleep(50 * time.Millisecond)
				}
			}
			ccancel()
			<-done
			if !seen {
				t.Fatalf("control: probe never saw the running backend; the test cannot detect a leak")
			}

			qctx, qcancel := context.WithTimeout(ctx, 300*time.Millisecond)
			err := tc.run(qctx, q)
			qcancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err = %v, want errors.Is(err, context.DeadlineExceeded)", err)
			}

			gaveUp := time.Now()
			for {
				n := backendsRunning(t, ctx, db, tc.marker)
				if n == 0 {
					t.Logf("backend gone %s after the client gave up", time.Since(gaveUp).Round(time.Millisecond))
					return
				}
				if time.Since(gaveUp) > 2*time.Second {
					t.Fatalf("%d backend(s) still running pg_sleep %s after the ctx deadline", n, time.Since(gaveUp).Round(time.Millisecond))
				}
				time.Sleep(50 * time.Millisecond)
			}
		})
	}
}

func backendsRunning(t *testing.T, ctx context.Context, db *sql.DB, marker string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_stat_activity
		  WHERE pid <> pg_backend_pid() AND state = 'active'
		    AND strpos(query, 'pg_sleep') > 0 AND strpos(query, $1) > 0`,
		marker).Scan(&n); err != nil {
		t.Fatalf("pg_stat_activity: %v", err)
	}
	return n
}
