//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestFreezeReleaseWindow_RedisLossKeepsEscalatedSibling is the DB-backed
// regression for a release that read a LOST marker as "no sibling is
// frozen": freeze.Writer wired as cmd/stellarindex-aggregator wires it,
// over the real timescale.FreezeEventSink.
//
// One pair, two windows frozen: 1h escalated (ADR-0019 holds it "until
// manual unfreeze"), 5m about to recover. Redis loses the marker — the one
// situation the 0163 per-window durable ladders exist for — and the 5m
// window releases. Pre-fix the release fell through to Writer.Clear, whose
// SaveLadder(State{}) NULLs hold_until AND window_ladders: the row went
// from `"3600": {escalated, extensions_used: 4}` to no ladder at all, and
// the 1h window rehydrated nothing.
func TestFreezeReleaseWindow_RedisLossKeepsEscalatedSibling(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sink := timescale.NewFreezeEventSink(store)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	w, err := freeze.NewWriter(rdb, 0, freeze.WithEventSink(sink), freeze.WithLadderStore(sink, 0))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	asset, _ := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	quote, _ := c.NewFiatAsset("USD")
	decision := anomaly.Decision{
		Action: anomaly.ActionFreeze, Class: anomaly.ClassStablecoin, Reason: "phase2:3_signal_AND",
	}
	const (
		short = 5 * time.Minute
		long  = time.Hour
	)
	now := time.Now().UTC().Truncate(time.Microsecond)
	escalated := freeze.State{
		FiredAt: now.Add(-2*time.Hour - 5*time.Minute), HoldUntil: now.Add(25 * time.Minute),
		ExtensionsUsed: freeze.DefaultMaxExtensions, Escalated: true, Corroborated: true,
	}
	recovering := freeze.State{FiredAt: now.Add(-11 * time.Minute), HoldUntil: now.Add(9 * time.Minute), UnfreezeStreak: 1}
	if err := w.MarkHoldForWindow(ctx, asset, quote, long, "0.874500000000", decision, escalated, 30*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(1h): %v", err)
	}
	if err := w.MarkHoldForWindow(ctx, asset, quote, short, "0.874500000000", decision, recovering, 14*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(5m): %v", err)
	}

	mr.FlushAll() // Redis loses the marker while both windows are frozen.
	kept, err := w.ReleaseWindow(ctx, asset, quote, short)
	if err != nil {
		t.Fatalf("ReleaseWindow(5m): %v", err)
	}
	if !kept {
		t.Error("ReleaseWindow reported the freeze cleared while the durable record held the 1h window's escalated ladder")
	}

	// The row itself: still one open row, still carrying the pair-level
	// escalation, and window_ladders holding the 1h entry and ONLY it.
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var (
		hold sql.NullTime
		esc  sql.NullBool
		ext  sql.NullInt64
		raw  sql.NullString
	)
	if err := db.QueryRowContext(ctx, `
		SELECT hold_until, escalated, extensions_used, window_ladders::text
		  FROM freeze_events
		 WHERE asset_id = $1 AND quote_id = $2 AND recovered_at IS NULL`,
		asset.String(), quote.String()).Scan(&hold, &esc, &ext, &raw); err != nil {
		t.Fatalf("read open freeze row: %v", err)
	}
	if !hold.Valid || !hold.Time.Equal(escalated.HoldUntil) || !esc.Bool || ext.Int64 != int64(freeze.DefaultMaxExtensions) {
		t.Errorf("pair-level ladder after the 5m release = (hold_until=%v valid=%v, escalated=%v, extensions_used=%d), "+
			"want the 1h window's (%v, true, %d): the release retired the whole durable record",
			hold.Time.UTC(), hold.Valid, esc.Bool, ext.Int64, escalated.HoldUntil, freeze.DefaultMaxExtensions)
	}
	ladders := map[string]freeze.State{}
	if raw.Valid {
		if err := json.Unmarshal([]byte(raw.String), &ladders); err != nil {
			t.Fatalf("decode window_ladders %q: %v", raw.String, err)
		}
	}
	if got, held := ladders["3600"]; !held || !got.Escalated || len(ladders) != 1 {
		t.Errorf("window_ladders after the 5m release = %s, want exactly the 1h window's escalated entry", raw.String)
	}

	// And what the 1h window's next cold read gets from it.
	got, ok, err := w.LoadStateForWindow(ctx, asset, quote, long)
	if err != nil || !ok || !got.Escalated || got.ExtensionsUsed != freeze.DefaultMaxExtensions ||
		!got.HoldUntil.Equal(escalated.HoldUntil) {
		t.Errorf("LoadStateForWindow(1h) = (%+v, ok=%v, err=%v), want its escalated ladder %+v",
			got, ok, err, escalated)
	}
	if got, _, _ := w.LoadStateForWindow(ctx, asset, quote, short); got.Active() {
		t.Errorf("the released 5m window's durable ladder survived: %+v", got)
	}

	// The last window's release is still a clear: nothing left to protect.
	// (The operator override reaches the same end through Writer.Clear.)
	kept, err = w.ReleaseWindow(ctx, asset, quote, long)
	if err != nil || kept {
		t.Errorf("ReleaseWindow(1h) with no sibling left = (kept=%v, err=%v), want (false, nil)", kept, err)
	}
	if _, ok, _ := w.LoadStateForWindow(ctx, asset, quote, long); ok {
		t.Error("the durable ladder outlived the pair's last release")
	}
}
