package projector

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ---------------------------------------------------------------------------
// Replay-window flag (issue #325). A `stellarindex-ops projector-replay`
// rewind is an INTENDED lag: the 2026-08-29 reflector-fx replay rewound the
// cursor 2,574,496 ledgers on purpose and
// stellarindex_projector_lag_high ticketed for the whole ~4h catch-up,
// carrying no information the operator did not already have — and masking a
// genuine lag on the same source for the duration.
//
// The discriminator is obs.ProjectorReplayWindowActive: 1 while the cursor is
// still inside the window the replay tool RECORDED (migration 0125), 0
// otherwise. The assertion surface here is the gauge's VALUE under each of
// the four states the alert rules join against — not a mock's call log.
// ---------------------------------------------------------------------------

func replayWindowGauge(t *testing.T, source string) float64 {
	t.Helper()
	return testutil.ToFloat64(obs.ProjectorReplayWindowActive.WithLabelValues(source))
}

// newReplayHarness wires a projector over the fake store with `source`
// registered (refreshReplayWindows publishes one series per REGISTERED
// source) and its projector cursor parked at `cursor`. The tip sits far
// above the cursor so every cycle sees real lag — the exact condition the
// lag alert fires on.
func newReplayHarness(t *testing.T, source string, cursor uint32, windows map[string]timescale.ProjectionDirtyWindow) (*Projector, *fakeStore, Source) {
	t.Helper()
	store := &fakeStore{
		projectorCursor: cursor,
		haveCursor:      true,
		tipLedger:       cursor + 5, // lagging, but nothing to project
		rows:            []sorobanevents.Row{},
		dirtyWindows:    windows,
	}
	src := Source{Name: source, Decoder: &ledgerEchoDecoder{}}
	p := &Projector{
		store:    store,
		logger:   discardLog(),
		sink:     func(context.Context, consumer.Event) error { return nil },
		registry: Registry{Sources: []Source{src}},
	}
	return p, store, src
}

// runCycleThenRefresh drives one real projector cycle (which is what records
// the cursor position) and then one watcher refresh, exactly as production
// interleaves them.
func runCycleThenRefresh(p *Projector, src Source) {
	window := uint32(BatchLimit)
	var tracker poisonTracker
	var wedge wedgeTracker
	p.cycleOneSource(context.Background(), src, &window, &tracker, &wedge)
	p.refreshReplayWindows(context.Background())
}

// TestReplayWindow_CursorInsideOperatorRewindFlagsActive is the issue-#325
// reproduction: after `projector-replay -source reflector-fx -from 61602787`
// rewound the cursor below the recorded window's to_ledger, the projector
// must publish replay_window_active=1 so the lag rule's `unless` arm
// suppresses the expected ticket (and the stalled-replay rule arms).
func TestReplayWindow_CursorInsideOperatorRewindFlagsActive(t *testing.T) {
	const source = "replay-window-inside"
	// Shape of the live incident: rewound to 61,602,787, pre-rewind cursor
	// (the window's to_ledger) 64,177,283.
	p, _, src := newReplayHarness(t, source, 61_602_786, map[string]timescale.ProjectionDirtyWindow{
		source: {Source: source, From: 61_602_787, To: 64_177_283, Reason: "projector-replay rewind 64177283 -> 61602787"},
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
// the suppression honest. The dirty-window row survives until
// compute-completeness re-verifies the range (up to a day later), so keying
// the flag on the row's existence alone would blind the lag alert long after
// the replay finished. The flag is bounded by the CURSOR: once it regains
// the window's to_ledger (its pre-rewind position) the remaining catch-up is
// ordinary lag and is alertable again.
func TestReplayWindow_CursorPastWindowEndClearsFlag(t *testing.T) {
	const source = "replay-window-past-end"
	win := map[string]timescale.ProjectionDirtyWindow{
		source: {Source: source, From: 61_602_787, To: 64_177_283, Reason: "projector-replay rewind 64177283 -> 61602787"},
	}
	// Cursor has climbed one ledger PAST the pre-rewind position; the row is
	// still pending (compute-completeness has not run yet).
	p, _, src := newReplayHarness(t, source, 64_177_284, win)

	runCycleThenRefresh(p, src)

	if got := replayWindowGauge(t, source); got != 0 {
		t.Fatalf("replay_window_active = %v, want 0 (cursor has regained its pre-rewind position — the pending dirty row must not keep suppressing lag)", got)
	}
}

// TestReplayWindow_RebuildStyleWindowDoesNotSuppress covers the OTHER writer
// of the same primitive: `stellarindex-ops projected-rebuild` also calls
// RecordProjectionDirtyWindow, but its one-writer guard keeps its range
// BELOW the live projector cursor and it never rewinds that cursor. Such a
// window is not an intended lag, so it must not raise the flag.
func TestReplayWindow_RebuildStyleWindowDoesNotSuppress(t *testing.T) {
	const source = "replay-window-rebuild"
	p, _, src := newReplayHarness(t, source, 64_500_000, map[string]timescale.ProjectionDirtyWindow{
		source: {Source: source, From: 60_000_000, To: 62_000_000, Reason: "projected-rebuild -write [60000000,62000000]"},
	})

	runCycleThenRefresh(p, src)

	if got := replayWindowGauge(t, source); got != 0 {
		t.Fatalf("replay_window_active = %v, want 0 (a projected-rebuild window is below the live cursor and never rewinds it)", got)
	}
}

// TestReplayWindow_RunStartsTheWatcher pins the WIRING: the flag is only
// worth anything if the live projector actually publishes it, so Run must
// start the watcher (and its first pass must land without waiting a whole
// ReplayWindowRefreshInterval). Without the goroutine in Run the gauge
// never leaves the value below and this test times out on a wrong 0 —
// exactly the shape of "the rule suppresses nothing in production".
func TestReplayWindow_RunStartsTheWatcher(t *testing.T) {
	const source = "replay-window-run-wiring"
	p, _, src := newReplayHarness(t, source, 61_602_786, map[string]timescale.ProjectionDirtyWindow{
		source: {Source: source, From: 61_602_787, To: 64_177_283},
	})
	// One cycle so the watcher has a cursor to compare against, exactly as
	// the source goroutine's own immediate first cycle does in production.
	window := uint32(BatchLimit)
	var tracker poisonTracker
	var wedge wedgeTracker
	p.cycleOneSource(context.Background(), src, &window, &tracker, &wedge)
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
		source: {Source: source, From: 61_602_787, To: 64_177_283},
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
