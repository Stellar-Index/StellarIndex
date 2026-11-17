//go:build integration

package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestSourceExclusions_InsertListLiftAndChecks pins the 0231 control table: rows round-trip, a lift stamps and keeps the
// row (a second lift is refused), the CHECKs reject malformed rows, and nothing touches trades.
func TestSourceExclusions_InsertListLiftAndChecks(t *testing.T) {
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

	from := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	wide, err := store.InsertSourceExclusion(ctx, timescale.SourceExclusion{
		Source: "binance", From: from, Reason: "venue halted", Actor: "ops",
	})
	if err != nil {
		t.Fatalf("insert source-wide: %v", err)
	}
	pair, err := store.InsertSourceExclusion(ctx, timescale.SourceExclusion{
		Source: "soroswap", Base: "crypto:BTC", Quote: "native", From: from, Until: from.Add(time.Hour),
		Reason: "pool drained", Actor: "ops",
	})
	if err != nil {
		t.Fatalf("insert pair: %v", err)
	}

	active, err := store.ActiveSourceExclusions(ctx)
	if err != nil || len(active) != 2 {
		t.Fatalf("active = %+v, %v; want 2 rows", active, err)
	}
	if a := active[0]; a.ID != wide || a.Base != "" || a.Quote != "" || !a.Until.IsZero() || !a.From.Equal(from) {
		t.Fatalf("source-wide row = %+v", a)
	}
	if p := active[1]; p.ID != pair || p.Base != "crypto:BTC" || p.Quote != "native" || !p.Until.Equal(from.Add(time.Hour)) {
		t.Fatalf("pair row = %+v", p)
	}

	if err := store.LiftSourceExclusion(ctx, wide, "ops", "venue back"); err != nil {
		t.Fatalf("lift: %v", err)
	}
	if err := store.LiftSourceExclusion(ctx, wide, "ops", "again"); !errors.Is(err, timescale.ErrSourceExclusionLifted) {
		t.Fatalf("second lift err = %v, want ErrSourceExclusionLifted", err)
	}
	if err := store.LiftSourceExclusion(ctx, 999, "ops", "nope"); !errors.Is(err, timescale.ErrNotFound) {
		t.Fatalf("unknown-id lift err = %v, want ErrNotFound", err)
	}
	if active, err = store.ActiveSourceExclusions(ctx); err != nil || len(active) != 1 || active[0].ID != pair {
		t.Fatalf("active after lift = %+v, %v; want only %d", active, err, pair)
	}
	all, err := store.ListSourceExclusions(ctx, false)
	if err != nil || len(all) != 2 {
		t.Fatalf("all = %+v, %v; want both rows kept", all, err)
	}
	if l := all[0]; l.Active() || l.LiftedBy != "ops" || l.LiftReason != "venue back" {
		t.Fatalf("lifted row = %+v", l)
	}

	bad := map[string]string{
		"pair half-set": `INSERT INTO source_exclusions (source, base_asset, from_ts, reason, actor)
			VALUES ('binance', 'native', now(), 'r', 'a')`,
		"until not after from": `INSERT INTO source_exclusions (source, from_ts, until_ts, reason, actor)
			VALUES ('binance', '2026-11-17T09:00:00Z', '2026-11-17T09:00:00Z', 'r', 'a')`,
		"blank reason": `INSERT INTO source_exclusions (source, from_ts, reason, actor)
			VALUES ('binance', now(), '   ', 'a')`,
		"lift fields partial": `INSERT INTO source_exclusions (source, from_ts, reason, actor, lifted_at)
			VALUES ('binance', now(), 'r', 'a', now())`,
	}
	for name, q := range bad {
		if _, err := db.ExecContext(ctx, q); err == nil {
			t.Errorf("%s: insert accepted, want a CHECK violation", name)
		}
	}

	var trades int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM trades`).Scan(&trades); err != nil || trades != 0 {
		t.Fatalf("trades rows = %d, %v; the control plane must not write trades", trades, err)
	}
}
