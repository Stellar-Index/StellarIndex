//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
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
