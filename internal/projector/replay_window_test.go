package projector

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ---------------------------------------------------------------------------
// Replay-window flag. A `stellarindex-ops projector-replay` rewind is an INTENDED lag, so
// stellarindex_projector_lag_high must not ticket it (and must not mask genuine lag).
//
// The discriminator is obs.ProjectorReplayWindowActive: 1 while the cursor is still inside the
// window the replay tool RECORDED, 0 otherwise. The tests assert the gauge's VALUE under each
// of the four states the alert rules join against, not a mock's call log.
// ---------------------------------------------------------------------------

func replayWindowGauge(t *testing.T, source string) float64 {
	t.Helper()
	return testutil.ToFloat64(obs.ProjectorReplayWindowActive.WithLabelValues(source))
}

// newReplayHarness wires a projector over the fake store with `source`
// registered (refreshReplayWindows publishes one series per REGISTERED source) and its
// projector cursor parked at `cursor`. The tip sits far above the cursor so every cycle sees
// real lag, the condition the lag alert fires on.
func newReplayHarness(t *testing.T, source string, cursor uint32, windows map[string]timescale.ProjectionDirtyWindow) (*Projector, *fakeStore, Source) {
	t.Helper()
	store := &fakeStore{
		projectorCursor: cursor,
		haveCursor:      true,
		tipLedger:       cursor + 5, // lagging, but nothing to project
		dirtyWindows:    windows,
	}
	src := Source{Name: source, Decoder: &ledgerEchoDecoder{}}
	p, _ := newLakeEventsProjector(store, &fakeEvents{}, func(context.Context, consumer.Event) error { return nil })
	p.registry = Registry{Sources: []Source{src}}
	return p, store, src
}

// runCycleThenRefresh drives one real projector cycle (which is what records
// the cursor position) and then one watcher refresh, exactly as production
// interleaves them.
func runCycleThenRefresh(p *Projector, src Source) {
	window := uint32(BatchLimit)
	var tracker poisonTracker
	var wedge wedgeTracker
	p.cycleOneSource(context.Background(), src, &window, &tracker, &wedge, p.newSourceLake())
	p.refreshReplayWindows(context.Background())
}

// TestReplayWindow_CursorInsideOperatorRewindFlagsActive: with the cursor rewound below the
// recorded window's to_ledger, the projector must publish replay_window_active=1 so the lag
// rule's `unless` arm suppresses the expected ticket (and the stalled-replay rule arms).
func TestReplayWindow_CursorInsideOperatorRewindFlagsActive(t *testing.T) {
	const source = "replay-window-inside"
	// Shape of the live incident: rewound to 61,602,787, pre-rewind cursor
	// (the window's to_ledger) 64,177,283.
	p, _, src := newReplayHarness(t, source, 61_602_786, map[string]timescale.ProjectionDirtyWindow{
		source: {Source: source, From: 61_602_787, To: 64_177_283, Reason: timescale.ProjectorReplayReason(64_177_283, 61_602_787)},
	})

	runCycleThenRefresh(p, src)

	if got := replayWindowGauge(t, source); got != 1 {
		t.Fatalf("replay_window_active = %v, want 1 (the cursor is inside the operator-recorded rewind window, so the lag is intended)", got)
	}
}

// TestReplayWindow_NoRecordedWindowLeavesLagAlertable is the other half of
// the same rule: ordinary lag (no operator rewind on record) must NEVER be
// suppressed. A gauge that read 1 here would silence the alert this change
// exists to keep honest.
func TestReplayWindow_NoRecordedWindowLeavesLagAlertable(t *testing.T) {
	const source = "replay-window-none"
	p, _, src := newReplayHarness(t, source, 64_000_000, nil)

	runCycleThenRefresh(p, src)

	if got := replayWindowGauge(t, source); got != 0 {
		t.Fatalf("replay_window_active = %v, want 0 (no recorded rewind — a real lag must stay alertable)", got)
	}
}

// TestReplayWindow_CursorPastWindowEndClearsFlag pins the BOUND that keeps
// the suppression honest. The dirty-window row survives until compute-completeness re-verifies
// the range (up to a day), so keying the flag on the row alone would blind the lag alert long
// after the replay finished. Once the cursor regains the window's to_ledger the remaining
// catch-up is ordinary lag and alertable again. The boundary itself is pinned by
// TestReplayWindow_CursorAtPreRewindPositionClearsFlag.
func TestReplayWindow_CursorPastWindowEndClearsFlag(t *testing.T) {
	const source = "replay-window-past-end"
	win := map[string]timescale.ProjectionDirtyWindow{
		source: {Source: source, From: 61_602_787, To: 64_177_283, Reason: timescale.ProjectorReplayReason(64_177_283, 61_602_787)},
	}
	// Cursor has climbed one ledger PAST the pre-rewind position; the row is
	// still pending (compute-completeness has not run yet).
	p, _, src := newReplayHarness(t, source, 64_177_284, win)

	runCycleThenRefresh(p, src)

	if got := replayWindowGauge(t, source); got != 0 {
		t.Fatalf("replay_window_active = %v, want 0 (cursor has regained its pre-rewind position — the pending dirty row must not keep suppressing lag)", got)
	}
}

// ---------------------------------------------------------------------------
// THE OTHER WRITER. `stellarindex-ops projected-rebuild -write` calls the same
// RecordProjectionDirtyWindow, and its range is NOT bounded below the live cursor:
// checkLiveCursorGuard admits liveLastLedger >= to (EQUALITY, and `-to` DEFAULTS to the live
// cursor), and `-allow-live-overlap` bypasses the guard. So a rebuild window routinely covers
// the live cursor's own position, or under the override sits wholly above it.
//
// None of those shapes is an operator REWIND, so none may excuse lag: a source whose projector
// is HELD at that position (sink-retry hold, poison hold, wedge) would have its lag ticket
// suppressed indefinitely, the exact failure stellarindex_projector_lag_high exists to catch.
// The bound is PROVENANCE (timescale.ProjectionDirtyWindow.IsProjectorReplay), not cursor
// arithmetic alone.
// ---------------------------------------------------------------------------

// TestReplayWindow_RebuildWindowBelowCursorDoesNotSuppress is the benign
// shape: a finished rebuild strictly behind a healthy cursor.
func TestReplayWindow_RebuildWindowBelowCursorDoesNotSuppress(t *testing.T) {
	const source = "replay-window-rebuild-below"
	p, _, src := newReplayHarness(t, source, 64_500_000, map[string]timescale.ProjectionDirtyWindow{
		source: {Source: source, From: 60_000_000, To: 62_000_000, Reason: timescale.ProjectedRebuildReason(60_000_000, 62_000_000)},
	})

	runCycleThenRefresh(p, src)

	if got := replayWindowGauge(t, source); got != 0 {
		t.Fatalf("replay_window_active = %v, want 0 (a projected-rebuild window is not an operator rewind)", got)
	}
}

// TestReplayWindow_RebuildWindowAtLiveCursorDoesNotSuppress is the DEFAULT
// projected-rebuild shape: `-to` omitted, so the recorded to_ledger IS the live cursor. Raising
// the flag would silence a projector HELD at that ledger for as long as the dirty row lives
// (up to a day), with no operator rewind on record.
func TestReplayWindow_RebuildWindowAtLiveCursorDoesNotSuppress(t *testing.T) {
	const source = "replay-window-rebuild-at-cursor"
	const liveCursor = 63_419_138
	p, _, src := newReplayHarness(t, source, liveCursor, map[string]timescale.ProjectionDirtyWindow{
		source: {Source: source, From: 63_000_000, To: liveCursor, Reason: timescale.ProjectedRebuildReason(63_000_000, liveCursor)},
	})

	runCycleThenRefresh(p, src)

	if got := replayWindowGauge(t, source); got != 0 {
		t.Fatalf("replay_window_active = %v, want 0 (a projected-rebuild -to defaults to the live cursor; a held projector there must stay alertable)", got)
	}
}

// TestReplayWindow_RebuildWindowAboveLiveCursorDoesNotSuppress is the
// `-allow-live-overlap` shape: the recorded window sits ABOVE the live cursor. A cursor-only
// bound would read 1 for the whole climb to -to, suppressing lag_high with no rewind on record,
// and genuinely slow-but-advancing lag would raise nothing because the stalled-replay rule only
// covers a cursor that has stopped.
func TestReplayWindow_RebuildWindowAboveLiveCursorDoesNotSuppress(t *testing.T) {
	const source = "replay-window-rebuild-above-cursor"
	p, _, src := newReplayHarness(t, source, 63_419_138, map[string]timescale.ProjectionDirtyWindow{
		source: {Source: source, From: 63_419_138, To: 63_671_020, Reason: timescale.ProjectedRebuildReason(63_419_138, 63_671_020)},
	})

	runCycleThenRefresh(p, src)

	if got := replayWindowGauge(t, source); got != 0 {
		t.Fatalf("replay_window_active = %v, want 0 (a -allow-live-overlap rebuild window is not an operator rewind — lag must stay alertable)", got)
	}
}

// TestReplayWindow_CursorAtPreRewindPositionClearsFlag pins the UPPER bound
// as EXCLUSIVE. A projector wedged exactly AT the window's to_ledger has finished its replay,
// and the dirty row outlives the replay by up to a day, so an inclusive bound would suppress
// that wedge's lag ticket indefinitely.
func TestReplayWindow_CursorAtPreRewindPositionClearsFlag(t *testing.T) {
	const source = "replay-window-at-end"
	p, _, src := newReplayHarness(t, source, 64_177_283, map[string]timescale.ProjectionDirtyWindow{
		source: {Source: source, From: 61_602_787, To: 64_177_283, Reason: timescale.ProjectorReplayReason(64_177_283, 61_602_787)},
	})

	runCycleThenRefresh(p, src)

	if got := replayWindowGauge(t, source); got != 0 {
		t.Fatalf("replay_window_active = %v, want 0 (cursor has regained its pre-rewind position — a wedge there must stay alertable)", got)
	}
}

// TestReplayWindow_CursorAtRewindTargetFlagsActive guards the LOWER bound's
// off-by-one from the other side: projector-replay parks the cursor at from_ledger-1
// (rewindTo = target-1), the FIRST legitimate position inside the rewind, which must still
// suppress. A lower bound of `cursor >= From` would disarm the flag when it is most needed.
func TestReplayWindow_CursorAtRewindTargetFlagsActive(t *testing.T) {
	const source = "replay-window-at-target"
	p, _, src := newReplayHarness(t, source, 61_602_786, map[string]timescale.ProjectionDirtyWindow{
		source: {Source: source, From: 61_602_787, To: 64_177_283, Reason: timescale.ProjectorReplayReason(64_177_283, 61_602_787)},
	})

	runCycleThenRefresh(p, src)

	if got := replayWindowGauge(t, source); got != 1 {
		t.Fatalf("replay_window_active = %v, want 1 (rewindTo = target-1 is where the replay parks the cursor)", got)
	}
}

// TestReplayWindow_CursorFarBelowRewindTargetDoesNotSuppress: a cursor well
// below where the recorded rewind parked it was not put there by that
// rewind, so the window does not explain its lag.
func TestReplayWindow_CursorFarBelowRewindTargetDoesNotSuppress(t *testing.T) {
	const source = "replay-window-below-target"
	p, _, src := newReplayHarness(t, source, 50_000_000, map[string]timescale.ProjectionDirtyWindow{
		source: {Source: source, From: 61_602_787, To: 64_177_283, Reason: timescale.ProjectorReplayReason(64_177_283, 61_602_787)},
	})

	runCycleThenRefresh(p, src)

	if got := replayWindowGauge(t, source); got != 0 {
		t.Fatalf("replay_window_active = %v, want 0 (cursor is below the recorded rewind target — this window does not explain it)", got)
	}
}

// TestReplayWindow_RunStartsTheWatcher pins the WIRING: the flag is only
// worth anything if the live projector actually publishes it, so Run must start the watcher
// and its first pass must land without waiting a whole ReplayWindowRefreshInterval. Without
// the goroutine in Run the gauge stays 0 and this test times out.
func TestReplayWindow_RunStartsTheWatcher(t *testing.T) {
	const source = "replay-window-run-wiring"
	p, _, src := newReplayHarness(t, source, 61_602_786, map[string]timescale.ProjectionDirtyWindow{
		source: {Source: source, From: 61_602_787, To: 64_177_283, Reason: timescale.ProjectorReplayReason(64_177_283, 61_602_787)},
	})
	// One cycle so the watcher has a cursor to compare against, exactly as
	// the source goroutine's own immediate first cycle does in production.
	window := uint32(BatchLimit)
	var tracker poisonTracker
	var wedge wedgeTracker
	p.cycleOneSource(context.Background(), src, &window, &tracker, &wedge, p.newSourceLake())
	obs.ProjectorReplayWindowActive.WithLabelValues(source).Set(0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	deadline := time.After(5 * time.Second)
	for {
		if replayWindowGauge(t, source) == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("replay_window_active = %v after 5s of Run, want 1 (Run must start the replay-window watcher and publish on its first pass)",
				replayWindowGauge(t, source))
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel (the watcher goroutine must exit on ctx.Done)")
	}
}

// TestReplayWindow_ReadErrorFailsOpen pins the deliberate asymmetry: the
// gauge's only power is to SUPPRESS a ticket, so a dirty-window read we
// cannot make must publish 0 (alert armed), never a stale or assumed 1.
func TestReplayWindow_ReadErrorFailsOpen(t *testing.T) {
	const source = "replay-window-read-error"
	win := map[string]timescale.ProjectionDirtyWindow{
		source: {Source: source, From: 61_602_787, To: 64_177_283, Reason: timescale.ProjectorReplayReason(64_177_283, 61_602_787)},
	}
	p, store, src := newReplayHarness(t, source, 61_602_786, win)

	// First refresh sees the window and raises the flag...
	runCycleThenRefresh(p, src)
	if got := replayWindowGauge(t, source); got != 1 {
		t.Fatalf("precondition: replay_window_active = %v, want 1", got)
	}

	// ...then the read starts failing: the flag must fall back to 0 rather
	// than latch at its last (suppressing) value.
	store.mu.Lock()
	store.dirtyErr = errors.New("timescale: ProjectionDirtyWindows: connection refused")
	store.mu.Unlock()
	p.refreshReplayWindows(context.Background())

	if got := replayWindowGauge(t, source); got != 0 {
		t.Fatalf("replay_window_active = %v, want 0 (a dirty-window read error must disarm the suppression, not keep silencing lag)", got)
	}
}

// TestReplayWindow_ReadCarriesItsOwnDeadline: every other p.store call in this package runs
// under cycleCtx, so the watcher's read must too. Otherwise a blocked read parks the watcher
// with the gauge holding its last value, and a stale 1 keeps suppressing the lag ticket for a
// source nobody is replaying.
//
// The connection statement_timeout (30m by default) is not a bound worth relying on when a
// local deadline costs two lines.
func TestReplayWindow_ReadCarriesItsOwnDeadline(t *testing.T) {
	const source = "replay-window-deadline"
	p, store, _ := newReplayHarness(t, source, 61_602_786, nil)

	before := time.Now()
	p.refreshReplayWindows(context.Background()) // parent has NO deadline

	store.mu.Lock()
	seen, hadDL, dl := store.dirtySeen, store.dirtyHadDL, store.dirtyDeadline
	store.mu.Unlock()

	if !seen {
		t.Fatal("ProjectionDirtyWindows was never called")
	}
	if !hadDL {
		t.Fatal("the dirty-window read ran with NO deadline on a parent that " +
			"has none — a query stuck behind a lock wait parks the watcher and " +
			"leaves the replay-window gauge latched at its last value (RD-08)")
	}
	// Bounded by PerSourceTimeout, not something far tighter: a budget that trips on ordinary DB
	// slowness would zero the gauge mid-replay and re-arm lag_high for the whole catch-up.
	// `before` is read just before the deadline is computed, so the upper check carries
	// scheduling slack; the lower check is the one with teeth.
	budget := dl.Sub(before)
	if budget <= PerSourceTimeout/2 {
		t.Errorf("read deadline is only %v from call time — that is far "+
			"tighter than PerSourceTimeout (%v), and a budget that trips on "+
			"ordinary DB slowness zeroes the gauge mid-replay and re-arms "+
			"lag_high for the whole catch-up", budget, PerSourceTimeout)
	}
	if budget > PerSourceTimeout+time.Second {
		t.Errorf("read deadline is %v from call time, want ~%v", budget, PerSourceTimeout)
	}
}

// TestReplayWindow_BlockedReadFailsOpen — when the read is cut short, the
// gauge must fall to 0 (alert ARMED), never latch at a suppressing 1.
// Same asymmetry as TestReplayWindow_ReadErrorFailsOpen, reached via an
// interrupted query rather than an error return.
func TestReplayWindow_BlockedReadFailsOpen(t *testing.T) {
	const source = "replay-window-blocked"
	win := map[string]timescale.ProjectionDirtyWindow{
		source: {
			Source: source, From: 61_602_787, To: 64_177_283,
			Reason: timescale.ProjectorReplayReason(64_177_283, 61_602_787),
		},
	}
	p, store, src := newReplayHarness(t, source, 61_602_786, win)

	runCycleThenRefresh(p, src)
	if got := replayWindowGauge(t, source); got != 1 {
		t.Fatalf("precondition: replay_window_active = %v, want 1", got)
	}

	// The read now blocks until its ctx is done.
	store.mu.Lock()
	store.dirtyBlock = make(chan struct{})
	store.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() { defer close(done); p.refreshReplayWindows(ctx) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("refreshReplayWindows did not return after its read was cut " +
			"short — the watcher goroutine is parked")
	}

	if got := replayWindowGauge(t, source); got != 0 {
		t.Fatalf("replay_window_active = %v, want 0 — an interrupted "+
			"dirty-window read must disarm the suppression, not keep "+
			"silencing lag on a stale sample", got)
	}
}
