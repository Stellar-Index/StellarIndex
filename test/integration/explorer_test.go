//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"math/big"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stellar/go-stellar-sdk/strkey"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
	"github.com/Stellar-Index/StellarIndex/test/harness"
)

// TestGetSourceStatsFoldsFlippedOrientation: a source printing the same market in both stored orientations (native/USDC
// and USDC/native) counts as one market in MarketsCount24h, so per_pair must fold to canonical orientation.
func TestGetSourceStatsFoldsFlippedOrientation(t *testing.T) {
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

	const usdc = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	now := time.Now().UTC()

	// Same source, same market, opposite stored orientations, both
	// inside the 24h window.
	seedGrainTrade(t, ctx, db, 1, "sdex", now.Add(-30*time.Minute), "native", usdc, "1", "0.5", "1")
	seedGrainTrade(t, ctx, db, 2, "sdex", now.Add(-20*time.Minute), usdc, "native", "0.5", "1", "1")

	stats, err := store.GetSourceStats(ctx)
	if err != nil {
		t.Fatalf("GetSourceStats: %v", err)
	}
	var row *timescale.SourceStats
	for i := range stats {
		if stats[i].Source == "sdex" {
			row = &stats[i]
		}
	}
	if row == nil {
		t.Fatalf("sdex missing from GetSourceStats: %+v", stats)
	}
	if row.MarketsCount24h != 1 {
		t.Errorf("sdex MarketsCount24h = %d, want 1 (native/USDC and USDC/native are the same market)", row.MarketsCount24h)
	}
	if row.TradeCount24h != 2 {
		t.Errorf("sdex TradeCount24h = %d, want 2 (both prints still counted)", row.TradeCount24h)
	}
}

// TestGetNetworkStatsFoldsFlippedOrientation: the same fold for GetNetworkStats's MarketsCount24h; the DISTINCT over
// prices_1m must fold a market's two stored orientations before counting.
func TestGetNetworkStatsFoldsFlippedOrientation(t *testing.T) {
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

	const usdc = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	now := time.Now().UTC()

	seedGrainTrade(t, ctx, db, 101, "sdex", now.Add(-30*time.Minute), "native", usdc, "1", "0.5", "1")
	seedGrainTrade(t, ctx, db, 102, "soroswap", now.Add(-20*time.Minute), usdc, "native", "0.5", "1", "1")

	if _, err := db.ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`,
	); err != nil {
		t.Fatalf("refresh cagg prices_1m: %v", err)
	}

	before, err := store.GetNetworkStats(ctx)
	if err != nil {
		t.Fatalf("GetNetworkStats: %v", err)
	}
	if before.MarketsCount24h != 1 {
		t.Errorf("MarketsCount24h = %d, want 1 (native/USDC on two venues, both orientations, is one market)", before.MarketsCount24h)
	}
}

// TestGetNetworkStatsLatestLedgerReadsOnlyLiveCursors: the home page's latest_ledger is the live tip, not the highest
// ledger any one-shot job's shard cursor reached. Each job writes its own namespace ("census-backfill", "projected-
// rebuild", ...), so a literal "backfill" denylist wrongly counts them as live.
func TestGetNetworkStatsLatestLedgerReadsOnlyLiveCursors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cursors := []struct {
		source, sub string
		ledger      uint32
	}{
		{"ledgerstream", "", 62_000_000},
		{"projector", "blend", 61_999_990},
		{"backfill", "0-70000000:sdex", 70_000_000},
		{"census-backfill", "shard-3", 69_000_000},
		{"projected-rebuild", "soroswap:60000000-68000000", 68_000_000},
		{"tag-signer", "shard-1", 67_000_000},
	}
	for _, c := range cursors {
		if err := store.UpsertCursor(ctx, c.source, c.sub, c.ledger); err != nil {
			t.Fatalf("UpsertCursor(%s,%s): %v", c.source, c.sub, err)
		}
	}

	got, err := store.GetNetworkStats(ctx)
	if err != nil {
		t.Fatalf("GetNetworkStats: %v", err)
	}
	if got.LatestLedger != 62_000_000 {
		t.Errorf("LatestLedger = %d, want 62000000 (the ledgerstream tip; one-shot job cursors are not live)", got.LatestLedger)
	}
}

// TestUsageDailyBatchUpsertSpansChunks: a batch larger than two chunks with a duplicate key inside it lands every row
// exactly once with the per-column maximum (GREATEST); Postgres would reject the statement if the duplicate reached it
// unfolded.
func TestUsageDailyBatchUpsertSpansChunks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	day := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	const subjects = 1201
	rows := make([]usage.RollupRow, 0, subjects+1)
	for i := 0; i < subjects; i++ {
		rows = append(rows, usage.RollupRow{
			Day: day, Subject: fmt.Sprintf("id:acct:batch-%d", i), Endpoint: "/v1/price",
			OK: int64(i + 1), ClientErrors: 2,
		})
	}
	// A second snapshot of subject 7 with a larger OK but smaller 4xx:
	// the row must end on the maximum of each column, 8 / 2.
	rows = append(rows, usage.RollupRow{
		Day: day, Subject: "id:acct:batch-7", Endpoint: "/v1/price", OK: 8, ClientErrors: 1,
	})
	if err := store.UpsertUsageDaily(ctx, rows); err != nil {
		t.Fatalf("UpsertUsageDaily: %v", err)
	}
	// A second identical sweep must be a no-op (idempotent across chunks).
	if err := store.UpsertUsageDaily(ctx, rows); err != nil {
		t.Fatalf("UpsertUsageDaily replay: %v", err)
	}

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM usage_daily WHERE subject LIKE 'id:acct:batch-%'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != subjects {
		t.Fatalf("usage_daily holds %d batch rows, want %d", n, subjects)
	}
	for _, tc := range []struct {
		subject string
		ok, c4  int64
	}{
		{"id:acct:batch-0", 1, 2},
		{"id:acct:batch-7", 8, 2},
		{"id:acct:batch-500", 501, 2},
		{"id:acct:batch-1200", 1201, 2},
	} {
		got, err := store.ReadUsageDaily(ctx, tc.subject, 7)
		if err != nil {
			t.Fatalf("ReadUsageDaily(%s): %v", tc.subject, err)
		}
		if len(got) != 1 || got[0].OK != tc.ok || got[0].ClientErrors != tc.c4 {
			t.Errorf("%s = %+v, want one row ok=%d client=%d", tc.subject, got, tc.ok, tc.c4)
		}
	}
}

// TestUsageDailyBillableByDay: the SQL the month-to-date meter reconciles evicted Redis day keys against. Billable units
// per day are ok + 4xx summed over endpoints, never 5xx or 429, for one subject, in an inclusive [from, to] window.
func TestUsageDailyBillableByDay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const subj = "id:acct:acme"
	rows := []usage.RollupRow{
		{Day: "2026-04-30", Subject: subj, Endpoint: "/v1/price", OK: 1000},
		{Day: "2026-05-01", Subject: subj, Endpoint: "/v1/price", OK: 300, ClientErrors: 20, ServerErrors: 7, Throttled: 9},
		{Day: "2026-05-01", Subject: subj, Endpoint: "/v1/assets", OK: 80},
		{Day: "2026-05-03", Subject: subj, Endpoint: "/v1/price", OK: 5, ClientErrors: 1},
		{Day: "2026-05-04", Subject: subj, Endpoint: "/v1/price", OK: 500},
		{Day: "2026-05-01", Subject: "id:acct:other", Endpoint: "/v1/price", OK: 777},
	}
	if err := store.UpsertUsageDaily(ctx, rows); err != nil {
		t.Fatalf("UpsertUsageDaily: %v", err)
	}

	got, err := store.BillableByDay(ctx, subj, "2026-05-01", "2026-05-03")
	if err != nil {
		t.Fatalf("BillableByDay: %v", err)
	}
	want := map[string]int64{"2026-05-01": 400, "2026-05-03": 6}
	if !maps.Equal(got, want) {
		t.Errorf("BillableByDay = %v, want %v", got, want)
	}
}

// TestUsageDailyRetention pins that `usage_daily`, which holds per-account
// request history, is not kept forever. 0167 must attach exactly one
// armed 12-month retention job, and its down must remove it.
func TestUsageDailyRetention(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 165)
	if n, _, _ := usageDailyRetention(t, ctx, db); n != 0 {
		t.Fatalf("pre-0167 usage_daily retention jobs = %d, want 0", n)
	}

	applyMigrationsUpTo(t, dsn, 167)
	n, dropAfter, scheduled := usageDailyRetention(t, ctx, db)
	if n != 1 {
		t.Fatalf("usage_daily retention jobs after 0167 = %d, want 1", n)
	}
	if dropAfter != "1 year" {
		t.Errorf("usage_daily drop_after = %q, want \"1 year\" (12 months)", dropAfter)
	}
	if !scheduled {
		t.Error("usage_daily retention job is not scheduled — 0167 ships it armed")
	}

	applyMigrationsUpTo(t, dsn, 165)
	if n, _, _ := usageDailyRetention(t, ctx, db); n != 0 {
		t.Fatalf("usage_daily retention jobs after 0167 down = %d, want 0", n)
	}
}

func usageDailyRetention(t *testing.T, ctx context.Context, db *sql.DB) (jobs int, dropAfter string, scheduled bool) {
	t.Helper()
	const q = `
		SELECT COUNT(*),
		       COALESCE(MAX(config->>'drop_after'), ''),
		       COALESCE(BOOL_AND(scheduled), false)
		  FROM timescaledb_information.jobs
		 WHERE proc_name = 'policy_retention'
		   AND hypertable_name = 'usage_daily'`
	if err := db.QueryRowContext(ctx, q).Scan(&jobs, &dropAfter, &scheduled); err != nil {
		t.Fatalf("read usage_daily retention job: %v", err)
	}
	return jobs, dropAfter, scheduled
}

// TestUsageDailyRollupReSweepIdempotent: the usage-rollup worker hands the sink the full cumulative per-(day, subject,
// endpoint) counters on every sweep and never resets Redis, which is only safe because UpsertUsageDaily merges with
// GREATEST(existing, incoming), not additively.
func TestUsageDailyRollupReSweepIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// day is now-relative because ReadUsageDaily reads a trailing now-anchored window; a hardcoded date would age out of the
	// window and fail the test untouched.
	day := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	const subj = "key:kid_bill_1"

	// One day of real traffic to /v1/price for one subject: 100 ok, 5 4xx,
	// 2 5xx, 3 throttled. These are the CUMULATIVE per-day counters the
	// Redis detail hash holds at sweep time.
	batch := []usage.RollupRow{{
		Day: day, Subject: subj, Endpoint: "/v1/price",
		OK: 100, ClientErrors: 5, ServerErrors: 2, Throttled: 3,
	}}

	// The worker sweeps the SAME cumulative batch every 5 minutes with no
	// new traffic in between. Six sweeps == 30 minutes of re-folding.
	for i := 0; i < 6; i++ {
		if err := store.UpsertUsageDaily(ctx, batch); err != nil {
			t.Fatalf("sweep %d UpsertUsageDaily: %v", i, err)
		}
	}

	rows, err := store.ReadUsageDaily(ctx, subj, 7)
	if err != nil {
		t.Fatalf("ReadUsageDaily: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ReadUsageDaily returned %d rows, want 1 (one endpoint, one day): %+v", len(rows), rows)
	}
	got := rows[0]
	// EXACTLY the single-sweep values — NOT 6x. An additive merge would
	// yield ok=600, client=30, server=12, throttled=18 here.
	if got.OK != 100 {
		t.Errorf("ok_count = %d after 6 sweeps, want 100 (additive over-count would give 600)", got.OK)
	}
	if got.ClientErrors != 5 {
		t.Errorf("client_error_count = %d, want 5 (additive would give 30)", got.ClientErrors)
	}
	if got.ServerErrors != 2 {
		t.Errorf("server_error_count = %d, want 2 (additive would give 12)", got.ServerErrors)
	}
	if got.Throttled != 3 {
		t.Errorf("throttled_count = %d, want 3 (additive would give 18)", got.Throttled)
	}
	// The wire-shape total /v1/account/usage serves: requests = ok + 4xx + 5xx.
	if reqs := got.OK + got.ClientErrors + got.ServerErrors; reqs != 107 {
		t.Errorf("wire requests = %d, want 107 (billing over-count if higher)", reqs)
	}
}

// TestUsageDailyRollupWithinDayGrowth: within a day the Redis counters only grow, so GREATEST must keep the latest
// (largest) cumulative value, never the sum of partial snapshots. A sweep at t=5m sees 50 ok and at t=10m 100 ok; the
// row must read 100, not 150.
func TestUsageDailyRollupWithinDayGrowth(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	day := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	const subj = "key:kid_bill_2"

	// Sweep 1: half a day's traffic captured.
	if err := store.UpsertUsageDaily(ctx, []usage.RollupRow{{
		Day: day, Subject: subj, Endpoint: "/v1/assets/{asset_id}", OK: 50,
	}}); err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	// Sweep 2: the day's full cumulative total (the counter grew to 100).
	if err := store.UpsertUsageDaily(ctx, []usage.RollupRow{{
		Day: day, Subject: subj, Endpoint: "/v1/assets/{asset_id}", OK: 100,
	}}); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	// Sweep 3: a late-arriving Redis snapshot that is SMALLER (e.g. the
	// detail hash partially expired, or a mid-day flush). GREATEST must
	// hold the row at 100 and never regress to 40.
	if err := store.UpsertUsageDaily(ctx, []usage.RollupRow{{
		Day: day, Subject: subj, Endpoint: "/v1/assets/{asset_id}", OK: 40,
	}}); err != nil {
		t.Fatalf("sweep 3: %v", err)
	}

	rows, err := store.ReadUsageDaily(ctx, subj, 7)
	if err != nil {
		t.Fatalf("ReadUsageDaily: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ReadUsageDaily returned %d rows, want 1: %+v", len(rows), rows)
	}
	if got := rows[0].OK; got != 100 {
		t.Errorf("ok_count = %d, want 100 (GREATEST of 50/100/40; additive would give 190, regress would give 40)", got)
	}
}

// gAccountFromSeed returns a strkey-valid 56-char G-address whose
// ed25519 key's first byte is `seed`. Deterministic for a given
// seed so fixture assertions stay reproducible.
func gAccountFromSeed(t *testing.T, seed byte) string {
	t.Helper()
	var raw [32]byte
	raw[0] = seed
	s, err := strkey.Encode(strkey.VersionByteAccountID, raw[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	return s
}

// TestStoreRoundTrip: write-then-read through the trade, oracle and cursor paths on a real TimescaleDB with our
// migrations applied.
func TestStoreRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// ─── Trades ─────────────────────────────────────────────────
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, _ := c.NewPair(c.NativeAsset(), usdc)

	tr := c.Trade{
		Source:      "sdex",
		Ledger:      52_430_001,
		TxHash:      "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe",
		OpIndex:     0,
		Timestamp:   time.Now().UTC().Truncate(time.Second),
		Pair:        pair,
		BaseAmount:  c.NewAmount(big.NewInt(1_000_000_000)), // 100 XLM in stroops
		QuoteAmount: c.NewAmount(big.NewInt(12_420_000)),    // 12.42 USDC
		Maker:       "maker-acc",
		Taker:       "taker-acc",
	}

	if err := store.InsertTrade(ctx, tr); err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	// Idempotent re-insert should not error (ON CONFLICT DO NOTHING).
	if err := store.InsertTrade(ctx, tr); err != nil {
		t.Fatalf("InsertTrade (duplicate): %v", err)
	}

	n, err := store.CountTrades(ctx)
	if err != nil || n != 1 {
		t.Fatalf("CountTrades = %d, err=%v — want 1 row after duplicate-insert", n, err)
	}

	latest, err := store.LatestTradesForPair(ctx, pair, 5)
	if err != nil {
		t.Fatalf("LatestTradesForPair: %v", err)
	}
	if len(latest) != 1 {
		t.Fatalf("expected 1 trade, got %d", len(latest))
	}
	got := latest[0]
	if !got.Equal(tr) {
		t.Fatalf("trade identity not preserved: %+v", got)
	}
	if got.BaseAmount.Cmp(tr.BaseAmount) != 0 {
		t.Errorf("base_amount lost: got %s want %s", got.BaseAmount, tr.BaseAmount)
	}
	if got.QuoteAmount.Cmp(tr.QuoteAmount) != 0 {
		t.Errorf("quote_amount lost: got %s want %s", got.QuoteAmount, tr.QuoteAmount)
	}

	// ─── Oracle updates ─────────────────────────────────────────
	price, _ := new(big.Int).SetString("1242000000000000", 10)
	up := c.OracleUpdate{
		Source:     "reflector-dex",
		ContractID: "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
		Ledger:     52_430_001,
		TxHash:     "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe",
		OpIndex:    0,
		Timestamp:  time.Now().UTC().Truncate(time.Second),
		Asset:      c.NativeAsset(),
		Quote:      usdc,
		Price:      c.NewAmount(price),
		Decimals:   14,
		Confidence: 0.95,
		// OracleUpdate.Validate requires a checksum-valid Observer strkey ("GRELAYER_FAKE" is 13 chars, not 56), so generate a
		// valid G-address from a deterministic seed.
		Observer: gAccountFromSeed(t, 0xAA),
	}
	if err := store.InsertOracleUpdate(ctx, up); err != nil {
		t.Fatalf("InsertOracleUpdate: %v", err)
	}

	gotUp, err := store.LatestOracleUpdateForAsset(ctx, "reflector-dex", c.NativeAsset())
	if err != nil {
		t.Fatalf("LatestOracleUpdateForAsset: %v", err)
	}
	if !gotUp.Equal(up) {
		t.Fatalf("oracle identity lost: %+v", gotUp)
	}
	if gotUp.Price.Cmp(up.Price) != 0 {
		t.Errorf("price lost: got %s want %s", gotUp.Price, up.Price)
	}
	if gotUp.Decimals != 14 {
		t.Errorf("decimals lost: got %d want 14", gotUp.Decimals)
	}

	// Not-found path.
	_, err = store.LatestOracleUpdateForAsset(ctx, "reflector-dex", usdc)
	if err == nil {
		t.Fatal("expected ErrNotFound for USDC (never inserted for this source)")
	}

	// ─── Cursors ────────────────────────────────────────────────
	if err := store.UpsertCursor(ctx, "soroswap", "", 52_430_001); err != nil {
		t.Fatalf("UpsertCursor: %v", err)
	}
	cur, err := store.GetCursor(ctx, "soroswap", "")
	if err != nil {
		t.Fatalf("GetCursor: %v", err)
	}
	if cur.LastLedger != 52_430_001 {
		t.Errorf("cursor lost: got %d", cur.LastLedger)
	}

	// Update path.
	if err := store.UpsertCursor(ctx, "soroswap", "", 52_430_100); err != nil {
		t.Fatal(err)
	}
	cur, _ = store.GetCursor(ctx, "soroswap", "")
	if cur.LastLedger != 52_430_100 {
		t.Errorf("cursor update lost: got %d", cur.LastLedger)
	}

	// Second subsource for the same source shouldn't interfere.
	if err := store.UpsertCursor(ctx, "soroswap", "pair:CAB...", 99); err != nil {
		t.Fatal(err)
	}
	cur, _ = store.GetCursor(ctx, "soroswap", "pair:CAB...")
	if cur.LastLedger != 99 {
		t.Errorf("sub cursor wrong: got %d", cur.LastLedger)
	}
	cur, _ = store.GetCursor(ctx, "soroswap", "")
	if cur.LastLedger != 52_430_100 {
		t.Errorf("root cursor wrong after sub insert: got %d", cur.LastLedger)
	}

	// ListCursors: the upserts above leave 2 cursors, soroswap/"" and soroswap/"pair:CAB...", returned sorted by (source,
	// sub_source).
	all, err := store.ListCursors(ctx)
	if err != nil {
		t.Fatalf("ListCursors: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListCursors returned %d, want 2", len(all))
	}
	if all[0].Source != "soroswap" || all[0].Sub != "" {
		t.Errorf("ListCursors[0] = %+v, want soroswap/\"\"", all[0])
	}
	if all[1].Source != "soroswap" || all[1].Sub != "pair:CAB..." {
		t.Errorf("ListCursors[1] = %+v, want soroswap/pair:CAB...", all[1])
	}
	// UpdatedAt must be populated by the server-side now() call.
	for _, c := range all {
		if c.UpdatedAt.IsZero() {
			t.Errorf("cursor %s/%s has zero UpdatedAt", c.Source, c.Sub)
		}
	}

	// Cursor monotonic-advance guard: the DB refuses to regress last_ledger (ON CONFLICT DO UPDATE ... WHERE
	// EXCLUDED.last_ledger > ingestion_cursors.last_ledger), so the Go-level advance-only rule is not the only defence
	// against a two-indexer race.
	if err := store.UpsertCursor(ctx, "soroswap", "", 10_000); err != nil {
		t.Fatalf("UpsertCursor (regression attempt): %v", err)
	}
	cur, _ = store.GetCursor(ctx, "soroswap", "")
	if cur.LastLedger != 52_430_100 {
		t.Errorf("regression-attempt should have been ignored; got %d, want 52430100",
			cur.LastLedger)
	}
	// Equal-value attempt also no-ops (WHERE > , not >=).
	if err := store.UpsertCursor(ctx, "soroswap", "", 52_430_100); err != nil {
		t.Fatalf("UpsertCursor (same value): %v", err)
	}
	cur, _ = store.GetCursor(ctx, "soroswap", "")
	if cur.LastLedger != 52_430_100 {
		t.Errorf("same-value upsert shouldn't change stored cursor")
	}
	// Advancement still works.
	if err := store.UpsertCursor(ctx, "soroswap", "", 52_430_200); err != nil {
		t.Fatal(err)
	}
	cur, _ = store.GetCursor(ctx, "soroswap", "")
	if cur.LastLedger != 52_430_200 {
		t.Errorf("advance after regression-attempt lost: got %d", cur.LastLedger)
	}

	// first_ledger semantics (migration 0046): the cursor created above started at last_ledger=52_430_001, so UpsertCursor's
	// INSERT branch must capture that as first_ledger and keep it across every later advance.
	if cur.FirstLedger != 52_430_001 {
		t.Errorf("FirstLedger not captured on insert / drifted across updates: got %d, want 52430001",
			cur.FirstLedger)
	}

	// A brand-new (source, sub) pair: first_ledger == last_ledger
	// on the very first write.
	if err := store.UpsertCursor(ctx, "phoenix", "", 60_000_000); err != nil {
		t.Fatalf("UpsertCursor phoenix: %v", err)
	}
	phoenixCur, err := store.GetCursor(ctx, "phoenix", "")
	if err != nil {
		t.Fatalf("GetCursor phoenix: %v", err)
	}
	if phoenixCur.FirstLedger != 60_000_000 {
		t.Errorf("fresh cursor FirstLedger = %d, want 60000000", phoenixCur.FirstLedger)
	}
	if phoenixCur.LastLedger != 60_000_000 {
		t.Errorf("fresh cursor LastLedger = %d, want 60000000", phoenixCur.LastLedger)
	}

	// Advance the phoenix cursor and confirm FirstLedger sticks
	// at the original value — the SET clause must NOT touch it.
	if err := store.UpsertCursor(ctx, "phoenix", "", 60_500_000); err != nil {
		t.Fatalf("UpsertCursor phoenix advance: %v", err)
	}
	phoenixCur, _ = store.GetCursor(ctx, "phoenix", "")
	if phoenixCur.FirstLedger != 60_000_000 {
		t.Errorf("FirstLedger drifted on advance: got %d, want 60000000 (anchor must be preserved)",
			phoenixCur.FirstLedger)
	}
	if phoenixCur.LastLedger != 60_500_000 {
		t.Errorf("LastLedger after advance = %d, want 60500000", phoenixCur.LastLedger)
	}
}

// TestCursorFirstLedgerBackfillMigration: for every existing backfill cursor, first_ledger equals the `from` integer
// parsed from sub_source. Rows are seeded at schema version 45 and the real 0046 up file applied, so the test cannot
// drift from the migration SQL.
func TestCursorFirstLedgerBackfillMigration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 45)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	_, err = db.ExecContext(ctx,
		// 51000000 written without the PG16 underscore digit separator —
		// the pinned image is timescale/timescaledb:…-pg15, where
		// `51_000_000` is a syntax error.
		`INSERT INTO ingestion_cursors (source, sub_source, last_ledger)
		   VALUES ('backfill', '50500000-53174999:soroswap', 51000000)`,
	)
	if err != nil {
		t.Fatalf("insert pre-migration row: %v", err)
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO ingestion_cursors (source, sub_source, last_ledger)
		   VALUES ('backfill', 'malformed-no-decoder', 1)`,
	)
	if err != nil {
		t.Fatalf("insert malformed row: %v", err)
	}

	applyMigrationsUpTo(t, dsn, 46)

	var affected int64
	err = db.QueryRowContext(ctx,
		`SELECT count(*) FROM ingestion_cursors
		  WHERE source = 'backfill' AND first_ledger IS NOT NULL`,
	).Scan(&affected)
	if err != nil {
		t.Fatalf("count backfilled rows: %v", err)
	}
	if affected != 1 {
		t.Errorf("migration backfilled %d rows, want 1 (only the soroswap range matches the regex)", affected)
	}

	// Verify the soroswap range got 50500000.
	var firstLedger sql.NullInt64
	err = db.QueryRowContext(ctx,
		`SELECT first_ledger FROM ingestion_cursors
		  WHERE source = 'backfill' AND sub_source = '50500000-53174999:soroswap'`,
	).Scan(&firstLedger)
	if err != nil {
		t.Fatalf("select soroswap first_ledger: %v", err)
	}
	if !firstLedger.Valid || firstLedger.Int64 != 50_500_000 {
		t.Errorf("first_ledger = %v, want 50500000", firstLedger)
	}

	// Malformed sub_source: regex filter skipped it, first_ledger
	// stays NULL (better than silently writing 0).
	err = db.QueryRowContext(ctx,
		`SELECT first_ledger FROM ingestion_cursors
		  WHERE source = 'backfill' AND sub_source = 'malformed-no-decoder'`,
	).Scan(&firstLedger)
	if err != nil {
		t.Fatalf("select malformed first_ledger: %v", err)
	}
	if firstLedger.Valid {
		t.Errorf("malformed first_ledger = %v, want NULL", firstLedger.Int64)
	}
}

// TestCursorFirstLedgerMigrationReversible: rolling back 0046 drops the column without disturbing existing (source,
// sub_source, last_ledger) rows.
func TestCursorFirstLedgerMigrationReversible(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Seed a row via the production path so first_ledger is set.
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = store.Close() }()
	if err := store.UpsertCursor(ctx, "comet", "", 51_500_000); err != nil {
		t.Fatalf("UpsertCursor: %v", err)
	}

	// Roll the column back via the down migration's DROP COLUMN.
	if _, err := db.ExecContext(ctx, `ALTER TABLE ingestion_cursors DROP COLUMN first_ledger`); err != nil {
		t.Fatalf("down migration ALTER DROP COLUMN: %v", err)
	}

	// last_ledger should still be there.
	var lastLedger int64
	err = db.QueryRowContext(ctx,
		`SELECT last_ledger FROM ingestion_cursors WHERE source = 'comet' AND sub_source = ''`,
	).Scan(&lastLedger)
	if err != nil {
		t.Fatalf("select last_ledger after down: %v", err)
	}
	if lastLedger != 51_500_000 {
		t.Errorf("post-rollback last_ledger = %d, want 51500000", lastLedger)
	}

	// Column should be gone.
	var exists bool
	err = db.QueryRowContext(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM information_schema.columns
		   WHERE table_name = 'ingestion_cursors' AND column_name = 'first_ledger'
		)
	`).Scan(&exists)
	if err != nil {
		t.Fatalf("information_schema check: %v", err)
	}
	if exists {
		t.Error("first_ledger column still present after down migration")
	}
}

// startTimescale gives this package's ~40 call sites the shared bootstrap. Every container start goes through
// harness.StartTimescale so the image pin, container flags and readiness gate cannot drift. Each call starts its own
// container and returns the DSN.
func startTimescale(t *testing.T, ctx context.Context) string {
	t.Helper()
	return harness.StartTimescale(t, ctx)
}

func applyMigrations(t *testing.T, dsn string) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
	m, err := migrate.New("file://"+migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	// Quiesce the CAGG refresh policies here for every test container: tests refresh views by hand with `CALL
	// refresh_continuous_aggregate(...)` and otherwise race the policy job TimescaleDB fires after
	// add_continuous_aggregate_policy (55P03 "concurrent refresh" flake).
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open for policy quiesce: %v", err)
	}
	defer db.Close()
	quiesceCAGGRefreshPolicies(t, context.Background(), db)
}

// quiesceCAGGRefreshPolicies unschedules every continuous-aggregate refresh-policy job. Tests that refresh explicitly
// otherwise race the policy job, and overlapping refreshes of one CAGG fail with 55P03 "concurrent refresh". It changes
// nothing the tests assert (views are materialized by hand); it only makes the manual refresh deterministic.
func quiesceCAGGRefreshPolicies(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		SELECT alter_job(job_id, scheduled => false)
		  FROM timescaledb_information.jobs
		 WHERE proc_name = 'policy_refresh_continuous_aggregate'`); err != nil {
		t.Fatalf("quiesce CAGG refresh policies: %v", err)
	}
}

// TestTWAPPointsInRange_TimeWeighted: the twap_1h CAGG read is time-weighted at 1-minute resolution, not trade-count
// weighted. Minute A has 3 trades at 1.0 and minute B 1 trade at 3.0: a trade-count mean is 1.5, the time-weighted mean
// is (1.0 + 3.0)/2 = 2.0, which we assert.
func TestTWAPPointsInRange_TimeWeighted(t *testing.T) {
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
	pair, _ := c.NewPair(c.NativeAsset(), usdc)

	// 2 days ago, snapped to the hour → both the hour bucket
	// (bucket <= now()-1h) AND the day bucket (bucket <= now()-1d) are
	// fully closed, and both minutes share one hour + one day bucket.
	base := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Hour)
	const stroops = 100_000_000 // 10 XLM
	mk := func(ts time.Time, opIdx uint32, quote int64) c.Trade {
		return c.Trade{
			Source:      "sdex",
			Ledger:      52_430_001,
			TxHash:      "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe",
			OpIndex:     opIdx,
			Timestamp:   ts,
			Pair:        pair,
			BaseAmount:  c.NewAmount(big.NewInt(stroops)),
			QuoteAmount: c.NewAmount(big.NewInt(quote)),
			Maker:       "maker-acc",
			Taker:       "taker-acc",
		}
	}
	// Minute A (base): 3 prints at price 1.0 (quote == base).
	// Minute B (base+1m): 1 print at price 3.0 (quote == 3·base).
	trades := []c.Trade{
		mk(base.Add(0*time.Second), 0, stroops),
		mk(base.Add(10*time.Second), 1, stroops),
		mk(base.Add(20*time.Second), 2, stroops),
		mk(base.Add(1*time.Minute), 3, 3*stroops),
	}
	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	quiesceCAGGRefreshPolicies(t, ctx, db)
	// prices_1m first (the twap CAGG's source), then the hierarchical
	// twap_1h — order matters for a CAGG-on-CAGG refresh.
	for _, cagg := range []string{"prices_1m", "twap_1h"} {
		if _, err := db.ExecContext(ctx,
			"CALL refresh_continuous_aggregate('"+cagg+"', NULL, NULL)"); err != nil {
			t.Fatalf("refresh %s: %v", cagg, err)
		}
	}

	pts, err := store.TWAPPointsInRange(ctx, pair, timescale.Granularity1h, time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("TWAPPointsInRange: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("got %d twap buckets, want 1 (hour %s)", len(pts), base.Format(time.RFC3339))
	}
	twap, ok := new(big.Rat).SetString(pts[0].VWAP)
	if !ok {
		t.Fatalf("twap %q not numeric", pts[0].VWAP)
	}
	// Expect 2.0 (time-weighted), reject 1.5 (count-weighted). Allow a
	// tiny tolerance for NUMERIC text rounding.
	want := big.NewRat(2, 1)
	diff := new(big.Rat).Sub(twap, want)
	diff.Abs(diff)
	if diff.Cmp(big.NewRat(1, 1000)) > 0 {
		t.Errorf("twap = %s, want ~2.0 (time-weighted; 1.5 would be trade-count-weighted)", pts[0].VWAP)
	}

	// 1d grain must also be gated + readable (same bucket, one day).
	if _, err := db.ExecContext(ctx,
		"CALL refresh_continuous_aggregate('twap_1d', NULL, NULL)"); err != nil {
		t.Fatalf("refresh twap_1d: %v", err)
	}
	dayPts, err := store.TWAPPointsInRange(ctx, pair, timescale.Granularity1d, time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("TWAPPointsInRange(1d): %v", err)
	}
	if len(dayPts) != 1 {
		t.Fatalf("got %d daily twap buckets, want 1", len(dayPts))
	}

	// Unsupported grain must error (the storage-side gate).
	if _, err := store.TWAPPointsInRange(ctx, pair, timescale.Granularity15m, time.Time{}, time.Time{}, 0); err == nil {
		t.Error("TWAPPointsInRange(15m) = nil error, want unsupported-granularity error")
	}
}

// TestTWAPSampleCount_CoverageWeighted: migration 0126's sample_count materializes in twap_1h and TWAPPointsInRange
// folds the two stored directions by minute coverage, not trade count.
func TestTWAPSampleCount_CoverageWeighted(t *testing.T) {
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
	fwd, _ := c.NewPair(c.NativeAsset(), usdc) // XLM/USDC (requested)
	rev, _ := c.NewPair(usdc, c.NativeAsset()) // USDC/XLM (flipped storage)

	base := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Hour)
	const stroops = 100_000_000 // 10 units
	mk := func(pair c.Pair, ts time.Time, opIdx uint32, baseAmt, quoteAmt int64) c.Trade {
		return c.Trade{
			Source:      "sdex",
			Ledger:      52_430_001,
			TxHash:      "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe",
			OpIndex:     opIdx,
			Timestamp:   ts,
			Pair:        pair,
			BaseAmount:  c.NewAmount(big.NewInt(baseAmt)),
			QuoteAmount: c.NewAmount(big.NewInt(quoteAmt)),
			Maker:       "maker-acc",
			Taker:       "taker-acc",
		}
	}
	var trades []c.Trade
	var op uint32
	// Direction A: 5 distinct minutes, 1 trade each, price 0.5.
	for m := 0; m < 5; m++ {
		trades = append(trades, mk(fwd, base.Add(time.Duration(m)*time.Minute), op, stroops, stroops/2))
		op++
	}
	// Direction B: 1 minute, 20 trades, price 5.0 (USDC/XLM).
	for k := 0; k < 20; k++ {
		trades = append(trades, mk(rev, base.Add(10*time.Minute).Add(time.Duration(k)*time.Second), op, stroops, 5*stroops))
		op++
	}
	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	quiesceCAGGRefreshPolicies(t, ctx, db)
	for _, cagg := range []string{"prices_1m", "twap_1h"} {
		if _, err := db.ExecContext(ctx,
			"CALL refresh_continuous_aggregate('"+cagg+"', NULL, NULL)"); err != nil {
			t.Fatalf("refresh %s: %v", cagg, err)
		}
	}

	// 1. sample_count materialized, and equals each direction's minute count.
	for _, want := range []struct {
		b, q string
		n    int64
	}{
		{fwd.Base.String(), fwd.Quote.String(), 5},
		{rev.Base.String(), rev.Quote.String(), 1},
	} {
		var sc int64
		if err := db.QueryRowContext(ctx,
			"SELECT sample_count FROM twap_1h WHERE base_asset = $1 AND quote_asset = $2 AND bucket = $3",
			want.b, want.q, base,
		).Scan(&sc); err != nil {
			t.Fatalf("read twap_1h.sample_count for %s/%s: %v", want.b, want.q, err)
		}
		if sc != want.n {
			t.Errorf("twap_1h.sample_count[%s/%s] = %d, want %d (minute coverage)", want.b, want.q, sc, want.n)
		}
	}

	// 2. TWAPPointsInRange serves the coverage-weighted union, not the
	//    trade-count-weighted (0.26) or equal-weighted (0.35) answer.
	pts, err := store.TWAPPointsInRange(ctx, fwd, timescale.Granularity1h, time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("TWAPPointsInRange: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("got %d twap buckets, want 1 (both directions fold into one)", len(pts))
	}
	served, ok := new(big.Rat).SetString(pts[0].VWAP)
	if !ok {
		t.Fatalf("served twap %q not numeric", pts[0].VWAP)
	}
	near := func(want *big.Rat) bool {
		d := new(big.Rat).Sub(served, want)
		d.Abs(d)
		return d.Cmp(big.NewRat(1, 1000)) <= 0
	}
	if !near(big.NewRat(45, 100)) {
		t.Errorf("served TWAP = %s, want coverage-weighted 0.45", pts[0].VWAP)
	}
	if near(big.NewRat(26, 100)) {
		t.Errorf("served TWAP = %s is the trade-count-weighted 0.26 — the M-B defect", pts[0].VWAP)
	}
	if near(big.NewRat(35, 100)) {
		t.Errorf("served TWAP = %s is the equal-weighted 0.35 — coverage ignored", pts[0].VWAP)
	}
}
