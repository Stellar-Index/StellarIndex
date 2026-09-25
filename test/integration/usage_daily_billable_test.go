//go:build integration

package integration_test

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

// TestUsageDailyBillableByDay drives the SQL the month-to-date meter
// reconciles evicted Redis day keys against (GH-1274): per-day billable
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
