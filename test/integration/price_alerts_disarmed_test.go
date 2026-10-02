//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
	"github.com/Stellar-Index/StellarIndex/internal/pricealerts"
)

// TestPriceAlertsDisarmed executes migration 0198 up and down and the SQL
// that makes an alert fire once per crossing (GH #664): the claim disarms
// and refuses a disarmed row, the re-arm is a compare-and-swap on
// last_fired_at, an edit to the rule or a re-enable re-arms it, and a claim
// made against a snapshot the owner has since edited or disabled is refused.
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
	get := func() platform.PriceAlert {
		t.Helper()
		got, err := alerts.GetPriceAlert(ctx, a.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return got
	}
	claimAs := func(snap platform.PriceAlert, at time.Time) bool {
		t.Helper()
		ok, err := alerts.ClaimPriceAlertFire(ctx, snap, at)
		if err != nil {
			t.Fatalf("claim at %s: %v", at, err)
		}
		return ok
	}
	// claim evaluates against the row as it stands, as a sweep with no
	// concurrent edit does.
	claim := func(at time.Time) bool {
		t.Helper()
		return claimAs(get(), at)
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
		return get().Disarmed
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
		cur := get()
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

	// Each edit lands between the evaluator's snapshot and its claim; the
	// claim must not fire the rule the owner just replaced. Each case starts
	// armed and claims an hour later, so a wrong claim cannot mask the next.
	third := second.Add(3 * time.Hour)
	for name, edit := range map[string]func(*platform.PriceAlert){
		"threshold": func(p *platform.PriceAlert) { p.Threshold = "0.3" },
		"condition": func(p *platform.PriceAlert) { p.Condition = platform.AlertBelow },
		"pair":      func(p *platform.PriceAlert) { p.QuoteAsset = "fiat:EUR" },
		"disabled":  func(p *platform.PriceAlert) { p.Enabled = false },
	} {
		snap := get()
		if snap.Disarmed {
			rearm(snap.LastFiredAt)
			snap = get()
		}
		update(edit)
		if claimAs(snap, third) {
			t.Errorf("claimed on a snapshot whose %s was edited before the claim", name)
		}
		update(func(p *platform.PriceAlert) { p.Enabled = true })
		third = third.Add(time.Hour)
	}
	cur := get()
	cur.Threshold += "00"
	if !claimAs(cur, third) {
		t.Fatalf("claim on the current rule (threshold %q, value-equal to the row) refused", cur.Threshold)
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

// TestPriceAlertRearmAfterFailedFanOut drives the evaluator against the real
// store with a nanosecond clock: the claim and the re-arm CAS both bind the
// worker's untruncated now, and timestamptz keeps microseconds, so the CAS
// matches only if both writes reduce the instant identically.
func TestPriceAlertRearmAfterFailedFanOut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	acct := insertRawAccount(t, ctx, db, "rearm-fanout")

	alerts := postgresstore.NewPriceAlertStore(postgresstore.New(db))
	a, err := alerts.CreatePriceAlert(ctx, platform.PriceAlert{
		AccountID: acct, BaseAsset: "native", QuoteAsset: "fiat:USD",
		Condition: platform.AlertAbove, Threshold: "0.15", Enabled: true,
		CooldownSeconds: 300,
	}, 25)
	if err != nil {
		t.Fatalf("create alert: %v", err)
	}

	// 789 ns past the microsecond: rounding and truncation disagree here.
	now := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	hooks := &failingWebhooks{
		hooks: []platform.CustomerWebhook{{
			ID: uuid.New(), AccountID: acct, URL: "https://hooks.example.com/x",
			Events: []string{string(platform.WebhookEventPriceAlert)}, Enabled: true,
		}},
		fail: true,
	}
	prices := staticVWAP{price: "0.20", bucketClose: func() time.Time { return now.Add(-30 * time.Second) }}
	w := pricealerts.New(alerts, hooks, prices, pricealerts.Options{
		Interval: time.Minute,
		Clock:    func() time.Time { return now },
	})

	w.Sweep(ctx)
	if hooks.attempts != 1 {
		t.Fatalf("fan-out attempts = %d, want 1", hooks.attempts)
	}
	got, err := alerts.GetPriceAlert(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LastFiredAt.IsZero() || got.LastFiredAt.Sub(now).Abs() >= time.Microsecond {
		t.Fatalf("last_fired_at = %s, want the claim's stamp %s at microsecond precision", got.LastFiredAt, now)
	}
	if got.Disarmed {
		t.Fatal("alert still disarmed after a fan-out that enqueued nothing — the re-arm CAS missed the claim's stamp")
	}

	hooks.fail = false
	now = now.Add(301 * time.Second)
	w.Sweep(ctx)
	if hooks.enqueued != 1 {
		t.Fatalf("deliveries enqueued after the cooldown = %d, want 1 (the re-armed crossing retries)", hooks.enqueued)
	}
	got, err = alerts.GetPriceAlert(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Disarmed {
		t.Error("a delivered crossing left the alert armed")
	}
}

type failingWebhooks struct {
	hooks              []platform.CustomerWebhook
	fail               bool
	attempts, enqueued int
}

func (f *failingWebhooks) ListWebhooksForAccount(context.Context, uuid.UUID) ([]platform.CustomerWebhook, error) {
	return f.hooks, nil
}

func (f *failingWebhooks) EnqueueDelivery(context.Context, platform.WebhookDelivery) error {
	f.attempts++
	if f.fail {
		return errors.New("enqueue refused")
	}
	f.enqueued++
	return nil
}

type staticVWAP struct {
	price       string
	bucketClose func() time.Time
}

func (s staticVWAP) LatestVWAP(context.Context, canonical.Asset, canonical.Asset) (string, time.Time, bool, error) {
	return s.price, s.bucketClose(), true, nil
}
