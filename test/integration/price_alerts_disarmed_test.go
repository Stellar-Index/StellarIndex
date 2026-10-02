//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// TestPriceAlertsDisarmed executes migration 0198 up and down and the SQL
// that makes an alert fire once per crossing (GH #664): the claim disarms
// and refuses a disarmed row, the re-arm is a compare-and-swap on
// last_fired_at, and an edit to the rule or a re-enable re-arms it.
func TestPriceAlertsDisarmed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 197)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	acct := insertRawAccount(t, ctx, db, "disarmed")

	// A row that fired before 0198 starts armed.
	var preID uuid.UUID
	if err := db.QueryRowContext(ctx, `
		INSERT INTO price_alerts
		    (account_id, base_asset, quote_asset, condition, threshold, cooldown_seconds, last_fired_at)
		VALUES ($1, 'native', 'fiat:USD', 'above', 0.15, 300, now())
		RETURNING id`, acct).Scan(&preID); err != nil {
		t.Fatalf("seed pre-0198 alert: %v", err)
	}
	applyMigrations(t, dsn)

	alerts := postgresstore.NewPriceAlertStore(postgresstore.New(db))
	pre, err := alerts.GetPriceAlert(ctx, preID)
	if err != nil {
		t.Fatalf("get pre-0198 alert: %v", err)
	}
	if pre.Disarmed {
		t.Error("pre-0198 alert reads disarmed, want armed (column default false)")
	}

	a, err := alerts.CreatePriceAlert(ctx, platform.PriceAlert{
		AccountID: acct, BaseAsset: "native", QuoteAsset: "fiat:USD",
		Condition: platform.AlertAbove, Threshold: "0.15", Enabled: true,
		CooldownSeconds: 300,
	}, 25)
	if err != nil {
		t.Fatalf("create alert: %v", err)
	}
	claim := func(at time.Time) bool {
		t.Helper()
		ok, err := alerts.ClaimPriceAlertFire(ctx, a.ID, at)
		if err != nil {
			t.Fatalf("claim at %s: %v", at, err)
		}
		return ok
	}
	rearm := func(lastFired time.Time) bool {
		t.Helper()
		ok, err := alerts.RearmPriceAlert(ctx, a.ID, lastFired)
		if err != nil {
			t.Fatalf("rearm: %v", err)
		}
		return ok
	}
	disarmed := func() bool {
		t.Helper()
		got, err := alerts.GetPriceAlert(ctx, a.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return got.Disarmed
	}

	fired := time.Now().UTC().Truncate(time.Microsecond)
	if !claim(fired) {
		t.Fatal("first claim on an armed, never-fired alert refused")
	}
	if !disarmed() {
		t.Fatal("claim did not disarm the alert")
	}
	if claim(fired.Add(time.Hour)) {
		t.Error("claimed past the cooldown while disarmed — the condition never cleared, so this is the same crossing")
	}
	if rearm(fired.Add(-time.Second)) {
		t.Error("re-armed with a last_fired_at older than the row's — a stale snapshot must not re-arm a newer fire")
	}
	if !rearm(fired) {
		t.Fatal("re-arm with the observed last_fired_at refused")
	}
	if rearm(fired) {
		t.Error("second re-arm of an armed alert reported rearmed=true")
	}
	if claim(fired.Add(299 * time.Second)) {
		t.Error("re-armed alert claimed inside its 300 s cooldown")
	}
	second := fired.Add(time.Hour)
	if !claim(second) {
		t.Fatal("re-armed alert past its cooldown refused")
	}

	update := func(mut func(*platform.PriceAlert)) {
		t.Helper()
		cur, err := alerts.GetPriceAlert(ctx, a.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		mut(&cur)
		if err := alerts.UpdatePriceAlert(ctx, cur); err != nil {
			t.Fatalf("update: %v", err)
		}
	}
	update(func(p *platform.PriceAlert) { p.CooldownSeconds = 600; p.Threshold = "0.150" })
	if !disarmed() {
		t.Error("a cooldown edit with a value-equal threshold re-armed the alert")
	}
	update(func(p *platform.PriceAlert) { p.Threshold = "0.2" })
	if disarmed() {
		t.Error("a threshold change left the alert disarmed")
	}
	if !claim(second.Add(time.Hour)) {
		t.Fatal("claim after the threshold change refused")
	}
	update(func(p *platform.PriceAlert) { p.Enabled = false })
	if !disarmed() {
		t.Error("disabling re-armed the alert")
	}
	update(func(p *platform.PriceAlert) { p.Enabled = true })
	if disarmed() {
		t.Error("re-enabling left the alert disarmed")
	}

	applyMigrationsUpTo(t, dsn, 197)
	var cols int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'price_alerts' AND column_name = 'disarmed'`).Scan(&cols); err != nil {
		t.Fatalf("read columns: %v", err)
	}
	if cols != 0 {
		t.Errorf("0198 down left price_alerts.disarmed in place")
	}
	applyMigrations(t, dsn)
}
