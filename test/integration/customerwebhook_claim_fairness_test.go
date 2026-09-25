//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// GH-663 — the claim was plain FIFO, so one endpoint's backlog filled the
// batch and every other customer's events waited behind it (and, with the
// worker serial per endpoint, behind every one of its timeouts). The claim
// now takes each due endpoint's oldest rows first and at most five per
// endpoint per claim. Executes ListPendingDeliveries against real Postgres.
func TestCustomerWebhookClaimIsFairAcrossEndpoints(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := postgresstore.New(db)
	accounts := postgresstore.NewAccountStore(store)
	webhooks := postgresstore.NewWebhookStore(store)

	_, backlogged := freshKillSwitchWebhook(t, ctx, accounts, webhooks, "https://fair-backlog.example/hook")
	_, other := freshKillSwitchWebhook(t, ctx, accounts, webhooks, "https://fair-other.example/hook")

	// Nine rows for the backlogged endpoint, all older than the other
	// endpoint's single row: FIFO alone would claim them first.
	for i := range 9 {
		d := dueDelivery(backlogged, fmt.Sprintf("backlog-%d", i))
		d.NextAttemptAt = time.Now().UTC().Add(-time.Duration(20-i) * time.Minute)
		if err := webhooks.EnqueueDelivery(ctx, d); err != nil {
			t.Fatalf("enqueue backlog %d: %v", i, err)
		}
	}
	if err := webhooks.EnqueueDelivery(ctx, dueDelivery(other, "other")); err != nil {
		t.Fatalf("enqueue other: %v", err)
	}

	// A batch smaller than the backlog: FIFO would hand it only the
	// backlogged endpoint's rows.
	first, err := webhooks.ListPendingDeliveries(ctx, 4)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	counts := map[uuid.UUID]int{}
	for _, d := range first {
		counts[d.WebhookID]++
	}
	if counts[other] != 1 || counts[backlogged] != 3 {
		t.Errorf("first claim (limit 4) = %d other + %d backlogged, want 1 + 3", counts[other], counts[backlogged])
	}

	second, err := webhooks.ListPendingDeliveries(ctx, 25)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(second) != 5 {
		t.Errorf("second claim returned %d rows, want 5 (the per-endpoint cap, with 6 still due)", len(second))
	}
	third, err := webhooks.ListPendingDeliveries(ctx, 25)
	if err != nil {
		t.Fatalf("third claim: %v", err)
	}
	if len(third) != 1 {
		t.Errorf("third claim returned %d rows, want the 1 left", len(third))
	}
	fourth, err := webhooks.ListPendingDeliveries(ctx, 25)
	if err != nil {
		t.Fatalf("fourth claim: %v", err)
	}
	if len(fourth) != 0 {
		t.Errorf("fourth claim returned %d rows, want 0 (every row is claimed and leased)", len(fourth))
	}
	seen := map[uuid.UUID]bool{}
	for _, batch := range [][]platform.WebhookDelivery{first, second, third} {
		for _, d := range batch {
			if seen[d.ID] {
				t.Errorf("delivery %s claimed twice while leased", d.ID)
			}
			seen[d.ID] = true
		}
	}
	if len(seen) != 10 {
		t.Errorf("claimed %d distinct rows across all claims, want all 10", len(seen))
	}
}
