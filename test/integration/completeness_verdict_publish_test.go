//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Findings F072 / K013 (audit 2026-09-02): the completeness verdict write
// and the replay-rewind dirty-window delete were two independent
// statements, and the first could not report that the CS-083 never-regress
// guard had rejected it. A run with a -to below the stored tip therefore
// deleted the window on the strength of a verdict that was never stored.
//
// These tests drive [timescale.Store.PublishCompletenessVerdict] on real
// TimescaleDB. RED on the unfixed behaviour: make the publish "upsert,
// ignore rows-affected, then clear" (what compute-completeness did with
// UpsertCompletenessSnapshot + ClearProjectionDirtyWindow) and both tests
// fail on "the dirty window was deleted".

const verdictPublishSource = "cctp"

// verdictSnap is a CLEAN verdict at the given tip — the only kind that may
// earn a window clear, and the only kind the CS-083 guard can reject.
func verdictSnap(tip uint32) timescale.CompletenessSnapshot {
	return timescale.CompletenessSnapshot{
		Source: verdictPublishSource, Genesis: 50_000_000, Tip: tip, Watermark: tip,
		CoveragePct: 1, Complete: true, LakeComplete: true,
		ProjectionVerifiedFrom: 50_000_000,
		SubstrateOK:            true, RecognitionOK: true, ProjectionOK: true,
		Detail: "complete",
	}
}

func openVerdictStore(t *testing.T, ctx context.Context, dsn string) *timescale.Store { //nolint:revive // t-first matches the package's other helpers (startTimescale).
	t.Helper()
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// recordVerdictWindow records the replay-rewind window and returns it as
// compute-completeness would read it (updated_at included — the clear's
// optimistic predicate needs the stored value, not a guess).
func recordVerdictWindow(t *testing.T, ctx context.Context, store *timescale.Store, from, to uint32) timescale.ProjectionDirtyWindow { //nolint:revive // t-first matches the package's other helpers.
	t.Helper()
	if err := store.RecordProjectionDirtyWindow(ctx, timescale.ProjectionDirtyWindow{
		Source: verdictPublishSource, From: from, To: to,
		Reason: timescale.ProjectorReplayReason(to, from),
	}); err != nil {
		t.Fatalf("record dirty window: %v", err)
	}
	wins, err := store.ProjectionDirtyWindows(ctx)
	if err != nil {
		t.Fatalf("read dirty windows: %v", err)
	}
	w, ok := wins[verdictPublishSource]
	if !ok {
		t.Fatalf("dirty window was not recorded")
	}
	return w
}

func verdictWindowPending(t *testing.T, ctx context.Context, store *timescale.Store) bool { //nolint:revive // t-first matches the package's other helpers.
	t.Helper()
	wins, err := store.ProjectionDirtyWindows(ctx)
	if err != nil {
		t.Fatalf("read dirty windows: %v", err)
	}
	_, ok := wins[verdictPublishSource]
	return ok
}

func storedVerdictTip(t *testing.T, ctx context.Context, store *timescale.Store) uint32 { //nolint:revive // t-first matches the package's other helpers.
	t.Helper()
	snaps, err := store.ListCompletenessSnapshots(ctx)
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	for _, s := range snaps {
		if s.Source == verdictPublishSource {
			return s.Tip
		}
	}
	t.Fatalf("no stored verdict for %s", verdictPublishSource)
	return 0
}

// TestPublishCompletenessVerdict_RejectedVerdictKeepsDirtyWindow is the
// finding's own scenario, with its own numbers: stored tip 63.5M, a window
// [62.0M, 62.5M], and an operator re-verify run at -to 63.0M that reconciles
// the window clean. The guard rejects the verdict; the window must survive.
// The destructive branch is then proven WITH its trigger: a run at/above the
// stored tip is applied and clears the window.
func TestPublishCompletenessVerdict_RejectedVerdictKeepsDirtyWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store := openVerdictStore(t, ctx, dsn)

	if err := store.UpsertCompletenessSnapshot(ctx, verdictSnap(63_500_000)); err != nil {
		t.Fatalf("seed stored verdict: %v", err)
	}
	win := recordVerdictWindow(t, ctx, store, 62_000_000, 62_500_000)
	clearWin := &timescale.DirtyWindowClear{From: win.From, To: win.To, UpdatedAt: win.UpdatedAt}

	// The regressive run: tip below the stored tip, no problem found.
	pub, err := store.PublishCompletenessVerdict(ctx, verdictSnap(63_000_000), clearWin)
	if err != nil {
		t.Fatalf("publish (regressive): %v", err)
	}
	if pub.Applied {
		t.Fatalf("regressive verdict reported Applied — the CS-083 guard must reject tip 63.0M under a stored 63.5M")
	}
	if pub.WindowCleared {
		t.Errorf("WindowCleared=true for a verdict that was never stored")
	}
	if got := storedVerdictTip(t, ctx, store); got != 63_500_000 {
		t.Errorf("stored tip = %d, want 63500000 (the guard must keep the more-advanced verdict)", got)
	}
	if !verdictWindowPending(t, ctx, store) {
		t.Fatalf("the dirty window was deleted although the verdict that justified the delete was rejected (F072)")
	}

	// A verdict with no clear never touches the window, applied or not.
	if _, err := store.PublishCompletenessVerdict(ctx, verdictSnap(63_550_000), nil); err != nil {
		t.Fatalf("publish (no clear): %v", err)
	}
	if !verdictWindowPending(t, ctx, store) {
		t.Fatalf("a publish with a nil clear deleted the dirty window")
	}

	// The trigger: a run at/above the stored tip IS applied and DOES clear.
	pub, err = store.PublishCompletenessVerdict(ctx, verdictSnap(63_600_000), clearWin)
	if err != nil {
		t.Fatalf("publish (advancing): %v", err)
	}
	if !pub.Applied || !pub.WindowCleared {
		t.Fatalf("advancing verdict: Applied=%v WindowCleared=%v, want both true", pub.Applied, pub.WindowCleared)
	}
	if got := storedVerdictTip(t, ctx, store); got != 63_600_000 {
		t.Errorf("stored tip = %d, want 63600000", got)
	}
	if verdictWindowPending(t, ctx, store) {
		t.Fatalf("an applied clean verdict did not clear the window it discharged")
	}

	// A re-recorded window (updated_at moved) survives even an applied
	// verdict — the optimistic predicate travels into the transaction.
	recordVerdictWindow(t, ctx, store, 62_000_000, 62_500_000)
	pub, err = store.PublishCompletenessVerdict(ctx, verdictSnap(63_700_000), clearWin) // stale updated_at
	if err != nil {
		t.Fatalf("publish (stale window identity): %v", err)
	}
	if !pub.Applied || pub.WindowCleared {
		t.Fatalf("stale-identity clear: Applied=%v WindowCleared=%v, want true/false", pub.Applied, pub.WindowCleared)
	}
	if !verdictWindowPending(t, ctx, store) {
		t.Fatalf("a window re-recorded after this run read it was deleted")
	}
}

// TestPublishCompletenessVerdict_RacingAdvanceRejectsAndKeepsWindow
// interleaves two writers on two connections. When the publish STARTS, its
// tip (63.0M) is above the committed stored tip (62.9M), so a check made
// before the write would say "this will apply". A concurrent run holds the
// verdict row with an uncommitted advance to 63.5M; the publish parks on
// that row lock, the advance commits, and READ COMMITTED re-evaluates the
// guard against 63.5M — rejected. Only rows-affected inside the same
// transaction can see that, which is the fix's whole claim.
func TestPublishCompletenessVerdict_RacingAdvanceRejectsAndKeepsWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store := openVerdictStore(t, ctx, dsn)
	racer := openVerdictStore(t, ctx, dsn) // second pool → second connection

	if err := store.UpsertCompletenessSnapshot(ctx, verdictSnap(62_900_000)); err != nil {
		t.Fatalf("seed stored verdict: %v", err)
	}
	win := recordVerdictWindow(t, ctx, store, 62_000_000, 62_500_000)

	// Writer 1: the concurrent run's advance, held open.
	tx, err := racer.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("racer begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`UPDATE completeness_snapshots SET tip_ledger = 63500000, watermark_ledger = 63500000 WHERE source = $1`,
		verdictPublishSource); err != nil {
		t.Fatalf("racer advance: %v", err)
	}

	// Writer 2: the regressive-after-the-race publish.
	type result struct {
		pub timescale.VerdictPublication
		err error
	}
	done := make(chan result, 1)
	go func() {
		pub, perr := store.PublishCompletenessVerdict(ctx, verdictSnap(63_000_000),
			&timescale.DirtyWindowClear{From: win.From, To: win.To, UpdatedAt: win.UpdatedAt})
		done <- result{pub, perr}
	}()

	// Non-vacuity: the publish must be parked IN THE VERDICT UPSERT, behind
	// the racer's row lock. If it parked anywhere else (or not at all) the
	// interleave never armed and a pass would prove nothing.
	parked := waitForVerdictLockWait(t, ctx, racer.DB(), done2finished(done))
	if !strings.Contains(parked, "INSERT INTO completeness_snapshots") {
		t.Fatalf("publish parked in the wrong statement — interleave not armed.\nparked in: %s", parked)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("racer commit: %v", err)
	}

	var res result
	select {
	case res = <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("publish did not return within 30s of the racer's commit")
	}
	if res.err != nil {
		t.Fatalf("publish: %v", res.err)
	}
	if res.pub.Applied || res.pub.WindowCleared {
		t.Fatalf("Applied=%v WindowCleared=%v, want false/false: the guard re-evaluated against the racer's committed 63.5M must reject tip 63.0M",
			res.pub.Applied, res.pub.WindowCleared)
	}
	if got := storedVerdictTip(t, ctx, store); got != 63_500_000 {
		t.Errorf("stored tip = %d, want the racer's 63500000", got)
	}
	if !verdictWindowPending(t, ctx, store) {
		t.Fatalf("the dirty window was deleted although the racing advance made the guard reject this run's verdict (F072 / K013)")
	}
}

// TestUpsertCompletenessSnapshot_ProblemArmNeverLowersTip is finding T379:
// the CS-083 guard's second arm ("OR EXCLUDED.first_problem_ledger > 0")
// exists so a newly-discovered problem is always recorded even when it
// can't advance the tip — but the UPDATE SET applied tip_ledger =
// EXCLUDED.tip_ledger unconditionally, so a regressive-tip run that found a
// problem also silently lowered the stored (monotonic, network-head) tip.
// RED on the unfixed query: after a problem-arm write with a lower tip, the
// stored tip_ledger drops to the regressive run's tip instead of holding.
func TestUpsertCompletenessSnapshot_ProblemArmNeverLowersTip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store := openVerdictStore(t, ctx, dsn)

	if err := store.UpsertCompletenessSnapshot(ctx, verdictSnap(63_500_000)); err != nil {
		t.Fatalf("seed stored verdict: %v", err)
	}

	// A regressive-window run (smaller tip) that ALSO found a problem: the
	// second CS-083 arm applies the write so the problem is recorded, but
	// tip_ledger must not regress below the previously stored, more-advanced
	// tip.
	problem := verdictSnap(63_000_000)
	problem.Complete = false
	problem.FirstProblem = 63_100_000
	problem.Detail = "gap detected"
	if err := store.UpsertCompletenessSnapshot(ctx, problem); err != nil {
		t.Fatalf("upsert (problem arm): %v", err)
	}

	snaps, err := store.ListCompletenessSnapshots(ctx)
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	var got *timescale.CompletenessSnapshot
	for i := range snaps {
		if snaps[i].Source == verdictPublishSource {
			got = &snaps[i]
		}
	}
	if got == nil {
		t.Fatalf("no stored verdict for %s", verdictPublishSource)
	}
	if got.Tip != 63_500_000 {
		t.Errorf("tip_ledger = %d, want 63500000 (the problem arm must not regress the monotonic tip)", got.Tip)
	}
	if got.FirstProblem != 63_100_000 {
		t.Errorf("first_problem_ledger = %d, want 63100000 (the problem itself must still be recorded)", got.FirstProblem)
	}
	if got.Complete {
		t.Errorf("complete = true, want false (the problem arm's other fields still land)")
	}
}

// done2finished adapts the publish's result channel to the waiter's
// "did it already finish?" probe WITHOUT consuming the result.
func done2finished[T any](done chan T) func() bool {
	return func() bool { return len(done) > 0 }
}

// waitForVerdictLockWait blocks until some backend in the test database is
// waiting on a heavyweight lock and returns the statement it is parked in.
// Fails — never passes vacuously — if the publish returns first.
func waitForVerdictLockWait(t *testing.T, ctx context.Context, db *sql.DB, finished func() bool) string { //nolint:revive // t-first matches the package's other helpers (startTimescale).
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var parked string
		err := db.QueryRowContext(ctx, `
			SELECT query FROM pg_stat_activity
			 WHERE datname = current_database()
			   AND wait_event_type = 'Lock'
			   AND pid <> pg_backend_pid()
			 LIMIT 1`).Scan(&parked)
		switch {
		case err == nil:
			return parked
		case !errors.Is(err, sql.ErrNoRows):
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if finished() {
			t.Fatalf("the racing writer finished without ever waiting on a row lock — the interleave never armed, so this test would prove nothing")
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the racing writer never blocked on a row lock within 30s")
	return ""
}
