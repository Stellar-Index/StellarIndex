//go:build integration

package integration_test

import (
	"context"
	"database/sql"
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

// ─── window_ladders beside a binary that does not know it exists ───
//
// migrations/README.md rule 9 keeps the schema at 0163 across a binary
// rollback, so the PREVIOUS binary runs against the new column. It neither
// reads nor writes it: it keeps advancing the four 0119 pair-level columns
// and leaves window_ladders exactly as the new binary last wrote it. After
// the roll-forward the map is therefore STALE, and a reader that prefers it
// resumes every window from where the new binary left off — discarding an
// escalation the old binary made. That is the under-hold direction.

// rollbackFixture is a production-wired writer over real TimescaleDB plus a
// raw handle for playing the previous binary's SQL.
type rollbackFixture struct {
	w            *freeze.Writer
	sink         *timescale.FreezeEventSink
	db           *sql.DB
	loseRedis    func()
	asset, quote c.Asset
	decision     anomaly.Decision
}

func newRollbackFixture(t *testing.T, ctx context.Context, dsn string) *rollbackFixture {
	t.Helper()
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
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	asset, _ := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	quote, _ := c.NewFiatAsset("USD")
	return &rollbackFixture{
		w: w, sink: sink, db: db, loseRedis: mr.FlushAll, asset: asset, quote: quote,
		decision: anomaly.Decision{Action: anomaly.ActionFreeze, Class: anomaly.ClassStablecoin, Reason: "phase2:3_signal_AND"},
	}
}

// previousBinarySaveLadder is the released binary's SaveLadder, verbatim:
// the four 0119 columns, and nothing about window_ladders.
func (f *rollbackFixture) previousBinarySaveLadder(t *testing.T, ctx context.Context, st freeze.State) {
	t.Helper()
	res, err := f.db.ExecContext(ctx, `
		UPDATE freeze_events
		   SET hold_until = $3::timestamptz, extensions_used = $4, escalated = $5, corroborated = $6
		 WHERE asset_id = $1 AND quote_id = $2 AND recovered_at IS NULL`,
		f.asset.String(), f.quote.String(), st.HoldUntil.UTC(), st.ExtensionsUsed, st.Escalated, st.Corroborated)
	if err != nil {
		t.Fatalf("previous-binary SaveLadder: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("previous-binary SaveLadder matched %d rows, want 1", n)
	}
}

// TestFreezeWindowLadders_PreviousBinaryEscalationSurvivesRollForward: the
// new binary leaves the 1h window on the last rung; the rolled-back binary
// escalates the freeze; the new binary comes back and Redis is lost.
func TestFreezeWindowLadders_PreviousBinaryEscalationSurvivesRollForward(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	f := newRollbackFixture(t, ctx, dsn)
	now := time.Now().UTC().Truncate(time.Microsecond)

	climbing := freeze.State{
		FiredAt: now.Add(-100 * time.Minute), HoldUntil: now.Add(4 * time.Minute),
		ExtensionsUsed: freeze.DefaultMaxExtensions, Corroborated: true,
	}
	if err := f.w.MarkHoldForWindow(ctx, f.asset, f.quote, time.Hour, "0.874500000000", f.decision, climbing, 9*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(1h): %v", err)
	}
	escalatedHold := now.Add(30 * time.Minute)
	f.previousBinarySaveLadder(t, ctx, freeze.State{
		HoldUntil: escalatedHold, ExtensionsUsed: freeze.DefaultMaxExtensions, Escalated: true, Corroborated: true,
	})

	f.loseRedis()
	got, ok, err := f.w.LoadStateForWindow(ctx, f.asset, f.quote, time.Hour)
	if err != nil || !ok {
		t.Fatalf("LoadStateForWindow(1h) = (ok=%v, err=%v), want present", ok, err)
	}
	if !got.Escalated || !got.HoldUntil.Equal(escalatedHold) || got.ExtensionsUsed != freeze.DefaultMaxExtensions {
		t.Errorf("1h window rehydrated %+v, want escalated with hold_until %v: the pair-level columns "+
			"say ESCALATED, the stale window_ladders entry won, and the freeze resumes auto-unfreezing",
			got, escalatedHold)
	}
	// A window the map never named still answers to the ownerless ladder,
	// as it does for any row whose ladder has no recorded owner.
	if day, _, _ := f.w.LoadStateForWindow(ctx, f.asset, f.quote, 24*time.Hour); !day.Escalated {
		t.Errorf("24h window rehydrated %+v, want the ownerless escalated ladder", day)
	}

	// The write path reads the row through the same rule, so the window's
	// next tick PERSISTS what it rehydrated. The record must converge on
	// the escalation, not write the stale map back over the columns.
	if err := f.w.MarkHoldForWindow(ctx, f.asset, f.quote, time.Hour, "0.874500000000", f.decision, got, 35*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(1h) after the roll-forward: %v", err)
	}
	pair, ok, err := f.sink.LoadLadder(ctx, f.asset, f.quote)
	if err != nil || !ok || !pair.Escalated || !pair.HoldUntil.Equal(escalatedHold) {
		t.Errorf("pair-level ladder after the window's next tick = (%+v, ok=%v, err=%v), want it still "+
			"escalated to %v", pair, ok, err, escalatedHold)
	}
	f.loseRedis()
	if again, _, _ := f.w.LoadStateForWindow(ctx, f.asset, f.quote, time.Hour); !again.Escalated {
		t.Errorf("1h window after its next tick and a second Redis loss = %+v, want it escalated", again)
	}
}

// TestFreezeWindowLadders_StaleMapKeepsItsOwnEscalation is the OTHER stale
// direction, and the reason a stale map is folded rather than discarded.
// The new binary recorded the 1h escalation in the map; the rolled-back
// binary's 5m window then wrote ITS fresh ladder over the pair-level
// columns (last writer wins — the defect 0163 exists for) with a later
// hold. The columns are now "ahead" on hold and BEHIND on escalation.
func TestFreezeWindowLadders_StaleMapKeepsItsOwnEscalation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	f := newRollbackFixture(t, ctx, dsn)
	now := time.Now().UTC().Truncate(time.Microsecond)

	escalated := freeze.State{
		FiredAt: now.Add(-2*time.Hour - 5*time.Minute), HoldUntil: now.Add(10 * time.Minute),
		ExtensionsUsed: freeze.DefaultMaxExtensions, Escalated: true,
	}
	if err := f.w.MarkHoldForWindow(ctx, f.asset, f.quote, time.Hour, "0.874500000000", f.decision, escalated, 15*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(1h): %v", err)
	}
	laterHold := now.Add(40 * time.Minute)
	f.previousBinarySaveLadder(t, ctx, freeze.State{HoldUntil: laterHold}) // fresh: rung 0, not escalated

	f.loseRedis()
	got, ok, err := f.w.LoadStateForWindow(ctx, f.asset, f.quote, time.Hour)
	if err != nil || !ok {
		t.Fatalf("LoadStateForWindow(1h) = (ok=%v, err=%v), want present", ok, err)
	}
	if !got.Escalated || got.ExtensionsUsed != freeze.DefaultMaxExtensions || got.HoldUntil.Before(laterHold) {
		t.Errorf("1h window rehydrated %+v, want it still escalated at rung %d and held to at least %v",
			got, freeze.DefaultMaxExtensions, laterHold)
	}
}

// TestFreezeWindowLadders_SteadyStateNeverReadsAsStale is the guard on the
// transform itself: on a row only this binary has written, through the
// production writer and the real driver, the staleness rule must not fire.
// If it did it would mint an ownerless ladder for EVERY freeze and
// rehydrate it onto windows that were never frozen — the cross-window
// contamination this whole unit removes.
//
// The columns and the map are NOT stored at the same precision: hold_until
// is timestamptz (whole microseconds) while the jsonb entry keeps the
// nanoseconds time.Now gave it, so the holds below carry nanoseconds on
// purpose. Against this driver the column comes back TRUNCATED, i.e.
// earlier than its entry, never later; the rule's independence from that
// is pinned without a database in
// internal/storage/timescale.TestPairLadderAhead_IgnoresStoragePrecision.
func TestFreezeWindowLadders_SteadyStateNeverReadsAsStale(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	f := newRollbackFixture(t, ctx, dsn)
	// 1600ns past the second: stored as 1µs in the column, 1.6µs in the map.
	base := time.Now().UTC().Truncate(time.Second)
	escalated := freeze.State{
		FiredAt: base.Add(-2 * time.Hour), HoldUntil: base.Add(25*time.Minute + 1600*time.Nanosecond),
		ExtensionsUsed: freeze.DefaultMaxExtensions, Escalated: true,
	}
	fresh := freeze.State{FiredAt: base.Add(-time.Minute), HoldUntil: base.Add(9*time.Minute + 700*time.Nanosecond)}
	if err := f.w.MarkHoldForWindow(ctx, f.asset, f.quote, time.Hour, "0.874500000000", f.decision, escalated, 30*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(1h): %v", err)
	}
	if err := f.w.MarkHoldForWindow(ctx, f.asset, f.quote, 5*time.Minute, "0.874500000000", f.decision, fresh, 14*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(5m): %v", err)
	}

	ladders, unowned, ok, err := f.sink.LoadWindowLadders(ctx, f.asset, f.quote)
	if err != nil || !ok {
		t.Fatalf("LoadWindowLadders = (ok=%v, err=%v), want present", ok, err)
	}
	if unowned.Active() {
		t.Errorf("a row only this binary wrote produced an ownerless ladder %+v — sub-microsecond "+
			"rounding read as staleness", unowned)
	}
	// Byte-identical round trip of what each window wrote.
	if got := ladders[time.Hour]; !got.HoldUntil.Equal(escalated.HoldUntil) || !got.Escalated || got.ExtensionsUsed != escalated.ExtensionsUsed {
		t.Errorf("1h entry read back %+v, want %+v", got, escalated)
	}
	if got := ladders[5*time.Minute]; !got.HoldUntil.Equal(fresh.HoldUntil) || got.Escalated || got.ExtensionsUsed != 0 {
		t.Errorf("5m entry read back %+v, want %+v — a sibling's escalation leaked into it", got, fresh)
	}
	f.loseRedis()
	if day, _, _ := f.w.LoadStateForWindow(ctx, f.asset, f.quote, 24*time.Hour); day.Active() {
		t.Errorf("24h window rehydrated %+v — it was never frozen", day)
	}
}
