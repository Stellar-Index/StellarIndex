//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

// TestUsageDailyBatchUpsertSpansChunks drives the chunked multi-row
// upsert against real Postgres: a batch larger than two chunks, with a
// duplicate key inside it, lands every row exactly once with the
// per-column maximum — the GREATEST contract the single-row statement
// gave, now over a statement Postgres would reject if the duplicate
// reached it unfolded.
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

// TestUsageDailyBillableByDay drives the SQL the month-to-date meter
// reconciles evicted Redis day keys against: per-day billable
// units are ok + 4xx summed over endpoints, never 5xx or 429, for one
// subject, inside an inclusive [from, to] window.
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

// TestUsageDailyRollupReSweepIdempotent pins the billing-adjacent
// invariant behind GET /v1/account/usage: the 5-minute usage-rollup
// worker (internal/usage.Rollup.Sweep) hands the sink the FULL
// CUMULATIVE per-(day, subject, endpoint) counters on EVERY sweep — it
// deliberately never resets or checkpoints the Redis source. That is
// only safe because timescale.Store.UpsertUsageDaily merges with
// GREATEST(existing, incoming), NOT additively. If that merge were ever
// flipped to `col = usage_daily.col + EXCLUDED.col`, every 5-minute
// sweep would RE-ADD the cumulative value and usage_daily would report
// k*N requests for N real requests — a permanent, non-self-correcting
// over-count on the surface a metered plan bills against.
//
// The unit suite (internal/usage/rollup_test.go) only proves the sweep
// hands the same cumulative batch on replay, then delegates: "idempotence
// is then the sink's GREATEST()-merge contract." Nothing exercised that
// contract against real Postgres until this test — so a regression that
// broke it would ship green. This drives the REAL SQL and asserts N, not
// kN. Flip either GREATEST to `+` in usage_daily.go and this goes red.
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

	// day is now-relative: ReadUsageDaily reads a trailing now-anchored
	// window, so a hardcoded date is a calendar time-bomb — the original
	// window, so a hardcoded date ages out of the window and the test starts
	// failing untouched.
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

// TestUsageDailyRollupWithinDayGrowth pins the other half of the
// GREATEST contract: WITHIN a day the Redis counters only grow, so each
// sweep's cumulative value is >= the last. GREATEST must keep the LATEST
// (largest) value, never the sum of the partial snapshots. A sweep at
// t=5m sees 50 ok; a sweep at t=10m sees the full 100 ok. The persisted
// row must read 100 — additive would read 150.
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
