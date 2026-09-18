//go:build integration

package integration_test

import (
	"context"
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

// TestFreezeWindowLadders_EscalationSurvivesSiblingWrite is the DB-backed
// regression for the pair-keyed durable ladder, driven through the
// production entry point — freeze.Writer wired to the real
// timescale.FreezeEventSink — and deliberately through NOTHING that did not
// exist before the fix, so it fails against the pre-0163 code for the right
// reason rather than by not compiling.
//
// Scenario. One pair, two of its windows frozen independently:
//
//   - the 1h window has spent the whole ADR-0019 ladder and ESCALATED. The
//     ADR holds it "until manual unfreeze"; a P1 has paged a human.
//   - the 5m window then fires a fresh freeze of its own and, like every
//     frozen window, mirrors its ladder durably on its tick.
//
// Redis is then lost and the aggregator restarts, so the durable record is
// the only authority left.
//
// Pre-fix that record was one ladder per (asset, quote), i.e. the LAST
// writer's. The 1h window rehydrated the 5m window's ten-minute,
// zero-extension ladder — an escalated freeze silently resumed
// auto-unfreezing — and the 24h window, never frozen, rehydrated it too.
func TestFreezeWindowLadders_EscalationSurvivesSiblingWrite(t *testing.T) {
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

	newWriter := func() *freeze.Writer {
		t.Helper()
		w, werr := freeze.NewWriter(rdb, 0,
			freeze.WithEventSink(sink),
			freeze.WithLadderStore(sink, 0))
		if werr != nil {
			t.Fatalf("NewWriter: %v", werr)
		}
		return w
	}

	asset, _ := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	quote, _ := c.NewFiatAsset("USD")
	decision := anomaly.Decision{
		Action:       anomaly.ActionFreeze,
		Class:        anomaly.ClassStablecoin,
		DeviationPct: 14.2,
		Reason:       "phase2:3_signal_AND confidence=0.121 z=8.44 sources=1",
	}

	const (
		short = 5 * time.Minute
		long  = time.Hour
		day   = 24 * time.Hour
	)
	now := time.Now().UTC().Truncate(time.Microsecond)
	escalated := freeze.State{
		FiredAt:        now.Add(-2*time.Hour - 5*time.Minute),
		HoldUntil:      now.Add(25 * time.Minute),
		ExtensionsUsed: freeze.DefaultMaxExtensions,
		Escalated:      true,
		Corroborated:   true,
	}
	fresh := freeze.State{
		FiredAt:   now.Add(-time.Minute),
		HoldUntil: now.Add(9 * time.Minute),
	}

	w := newWriter()
	if err := w.MarkHoldForWindow(ctx, asset, quote, long, "0.874500000000",
		decision, escalated, 30*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(1h): %v", err)
	}
	if err := w.MarkHoldForWindow(ctx, asset, quote, short, "0.874500000000",
		decision, fresh, 14*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(5m): %v", err)
	}
	// One market event is still ONE freeze_events row: the window dimension
	// belongs to the ladder, not to the /v1/anomalies timeline.
	if n := countOpenFreezeRows(t, ctx, dsn, asset.String(), quote.String()); n != 1 {
		t.Fatalf("two frozen windows opened %d freeze_events rows, want 1", n)
	}

	mr.FlushAll()            // Redis is lost…
	restarted := newWriter() // …and the aggregator restarts.

	gotLong, ok, err := restarted.LoadStateForWindow(ctx, asset, quote, long)
	if err != nil || !ok {
		t.Fatalf("LoadStateForWindow(1h) = (ok=%v, err=%v), want present", ok, err)
	}
	if !gotLong.Escalated || gotLong.ExtensionsUsed != freeze.DefaultMaxExtensions ||
		!gotLong.HoldUntil.Equal(escalated.HoldUntil) || !gotLong.FiredAt.Equal(escalated.FiredAt) {
		t.Errorf("1h window rehydrated %+v, want its own escalated ladder %+v — a sibling "+
			"window's later durable write replaced it, so an ESCALATED freeze resumes "+
			"auto-unfreezing after a Redis loss", gotLong, escalated)
	}

	gotShort, _, err := restarted.LoadStateForWindow(ctx, asset, quote, short)
	if err != nil {
		t.Fatalf("LoadStateForWindow(5m): %v", err)
	}
	if gotShort.Escalated || gotShort.ExtensionsUsed != 0 ||
		!gotShort.HoldUntil.Equal(fresh.HoldUntil) || !gotShort.FiredAt.Equal(fresh.FiredAt) {
		t.Errorf("5m window rehydrated %+v, want its own fresh ladder %+v", gotShort, fresh)
	}

	gotDay, present, err := restarted.LoadStateForWindow(ctx, asset, quote, day)
	if err != nil {
		t.Fatalf("LoadStateForWindow(24h): %v", err)
	}
	if gotDay.Active() {
		t.Errorf("24h window rehydrated %+v — it was never frozen", gotDay)
	}
	if !present {
		t.Error("the pair is frozen, so a window with no ladder of its own must still read " +
			"PRESENT: absent under a live freeze is the operator override")
	}

	// The pair-level view is the fail-closed summary, because the recovery
	// worker and `freeze-unfreeze -list` read it with no window to name:
	// the furthest hold, the highest rung, escalated if ANY window is.
	pair, ok, err := sink.LoadLadder(ctx, asset, quote)
	if err != nil || !ok {
		t.Fatalf("LoadLadder = (ok=%v, err=%v), want the pair-level summary", ok, err)
	}
	if !pair.Escalated || pair.ExtensionsUsed != freeze.DefaultMaxExtensions ||
		!pair.HoldUntil.Equal(escalated.HoldUntil) {
		t.Errorf("pair-level summary = %+v, want escalated / %d extensions / hold %v — "+
			"it must never under-state the pair's worst window",
			pair, freeze.DefaultMaxExtensions, escalated.HoldUntil)
	}

	// The 5m window auto-releases while the 1h stays frozen. Its durable
	// entry must go with it; the 1h window's must not move.
	if err := restarted.RetireWindowLadder(ctx, asset, quote, short); err != nil {
		t.Fatalf("RetireWindowLadder(5m): %v", err)
	}
	mr.FlushAll()
	if got, _, lerr := restarted.LoadStateForWindow(ctx, asset, quote, short); lerr != nil || got.Active() {
		t.Errorf("5m window after its release = (%+v, err=%v), want no ladder — the durable "+
			"record kept a freeze that had already ended", got, lerr)
	}
	if got, ok, lerr := restarted.LoadStateForWindow(ctx, asset, quote, long); lerr != nil || !ok || !got.Escalated {
		t.Errorf("1h window after its sibling's release = (%+v, ok=%v, err=%v), want its "+
			"escalated ladder untouched", got, ok, lerr)
	}

	// The operator override still ends EVERY window: Clear retires the
	// ladder, MarkRecovered closes the row (freeze-unfreeze does both).
	if err := restarted.Clear(ctx, asset, quote); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if err := sink.MarkRecovered(ctx, asset, quote); err != nil {
		t.Fatalf("MarkRecovered: %v", err)
	}
	for _, window := range []time.Duration{short, long, day} {
		got, ok, lerr := restarted.LoadStateForWindow(ctx, asset, quote, window)
		if lerr != nil || ok || got.Active() {
			t.Errorf("window %s after the operator override = (%+v, ok=%v, err=%v), want absent — "+
				"a human could not end an escalated freeze", window, got, ok, lerr)
		}
	}
}
