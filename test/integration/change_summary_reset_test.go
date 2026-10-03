//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestDeleteChangeSummary_ClearsRatchetedExtremes pins the repair path: the
// upsert's GREATEST/LEAST never lowers a stored ath, and deleting the row
// lets the next upsert start from the corrected values.
func TestDeleteChangeSummary_ClearsRatchetedExtremes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()

	now := time.Now().UTC().Truncate(time.Second)
	row := func(ath string) timescale.ChangeSummaryRow {
		cur := "0.11"
		low := "0.10"
		return timescale.ChangeSummaryRow{
			EntityType: "coin", EntityID: "crypto:XLM", RefreshedAt: now, CurrentValue: cur,
			ATHValue: &ath, ATHAt: &now, ATLValue: &low, ATLAt: &now,
		}
	}
	if err := store.UpsertChangeSummary(ctx, row("9000")); err != nil {
		t.Fatalf("upsert bad: %v", err)
	}
	if err := store.UpsertChangeSummary(ctx, row("0.12")); err != nil {
		t.Fatalf("upsert good: %v", err)
	}
	got, err := store.GetChangeSummary(ctx, "coin", "crypto:XLM")
	if err != nil {
		t.Fatal(err)
	}
	if got.ATHValue == nil || *got.ATHValue != "9000" {
		t.Fatalf("ratchet should have held the bad ath, got %v", got.ATHValue)
	}

	existed, err := store.DeleteChangeSummary(ctx, "coin", "crypto:XLM")
	if err != nil || !existed {
		t.Fatalf("delete = %v, %v; want true, nil", existed, err)
	}
	if existed, err = store.DeleteChangeSummary(ctx, "coin", "crypto:XLM"); err != nil || existed {
		t.Fatalf("second delete = %v, %v; want false, nil", existed, err)
	}
	if err := store.UpsertChangeSummary(ctx, row("0.12")); err != nil {
		t.Fatalf("upsert after reset: %v", err)
	}
	got, err = store.GetChangeSummary(ctx, "coin", "crypto:XLM")
	if err != nil {
		t.Fatal(err)
	}
	if got.ATHValue == nil || *got.ATHValue != "0.12" {
		t.Fatalf("ath after reset = %v, want 0.12", got.ATHValue)
	}
}
