//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	"github.com/Stellar-Index/StellarIndex/internal/sources/reflector"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The completeness verdict write and the replay-rewind dirty-window delete
// must not be two independent statements: the write has to report that the
// never-regress guard rejected it, or a run with a -to below the stored tip
// deletes the window on the strength of a verdict that was never stored.
//
// These tests drive [timescale.Store.PublishCompletenessVerdict] on real
// TimescaleDB. RED on the unfixed behaviour: make the publish "upsert,
// ignore rows-affected, then clear" (what compute-completeness did with
// UpsertCompletenessSnapshot + ClearProjectionDirtyWindow) and both tests
// fail on "the dirty window was deleted".

const verdictPublishSource = "cctp"

// verdictSnap is a CLEAN verdict at the given tip — the only kind that may
// earn a window clear, and the only kind the guard can reject.
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

// TestUpsertCompletenessSnapshot_ProblemArmNeverLowersTip pins that
// the guard's second arm ("OR EXCLUDED.first_problem_ledger > 0")
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
	// second arm applies the write so the problem is recorded, but
	// tip_ledger must not regress below the stored, more-advanced
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

func storedVerdict(t *testing.T, ctx context.Context, store *timescale.Store) timescale.CompletenessSnapshot { //nolint:revive // t-first matches the package's other helpers.
	t.Helper()
	snaps, err := store.ListCompletenessSnapshots(ctx)
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	for _, s := range snaps {
		if s.Source == verdictPublishSource {
			return s
		}
	}
	t.Fatalf("no stored verdict for %s", verdictPublishSource)
	return timescale.CompletenessSnapshot{}
}

// The ClickHouse projection reconcile is an aggregate, so a
// failed reconcile (nonzero delta, blind spots, floor loss) leaves
// first_problem_ledger at 0. A lower-tip run whose reconcile FOUND a
// mismatch therefore missed the guard's problem arm and was dropped,
// leaving the stored complete=true standing over a projection the run had
// just proven wrong. RED on the unfixed guard (problem arm keyed on
// first_problem_ledger alone): Applied=false and complete stays true.
func TestPublishCompletenessVerdict_LowerTipReconcileFailureIsRecorded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store := openVerdictStore(t, ctx, dsn)

	if err := store.UpsertCompletenessSnapshot(ctx, verdictSnap(63_500_000)); err != nil {
		t.Fatalf("seed stored verdict: %v", err)
	}

	// Clean substrate + recognition (lake_complete, first_problem 0); the
	// reconcile over [50.0M, 63.0M] found delta != 0.
	failed := verdictSnap(63_000_000)
	failed.Complete, failed.ProjectionOK, failed.FoundProblem = false, false, true
	failed.Detail = "projection: delta=-3 over [50000000,63000000]"
	pub, err := store.PublishCompletenessVerdict(ctx, failed, nil)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !pub.Applied {
		t.Fatal("Applied = false: a lower-tip run whose reconcile found a mismatch was discarded by the never-regress guard")
	}
	got := storedVerdict(t, ctx, store)
	if got.Complete || got.ProjectionOK {
		t.Errorf("stored complete=%v projection_ok=%v, want both false (the found mismatch must replace the stale clean verdict)", got.Complete, got.ProjectionOK)
	}
	if !got.LakeComplete {
		t.Error("stored lake_complete = false, want true (a projection failure never gates the lake axis)")
	}
	if got.Tip != 63_500_000 {
		t.Errorf("tip_ledger = %d, want 63500000 (the problem arm must not regress the monotonic tip)", got.Tip)
	}
}

// The problem arm must admit only a FOUND failure. A lower-tip run whose
// projection was not evaluated (e.g. `-pass -to` below the stored
// watermark: complete=false, projection_ok=false, projection_verified_from
// 0, no problem ledger) proved nothing and must not replace the stored
// clean verdict.
func TestPublishCompletenessVerdict_LowerTipNotEvaluatedIsRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store := openVerdictStore(t, ctx, dsn)

	if err := store.UpsertCompletenessSnapshot(ctx, verdictSnap(63_500_000)); err != nil {
		t.Fatalf("seed stored verdict: %v", err)
	}

	notEvaluated := verdictSnap(63_000_000)
	notEvaluated.Complete, notEvaluated.ProjectionOK = false, false
	notEvaluated.ProjectionVerifiedFrom = 0
	notEvaluated.Detail = "projection: not evaluated (earlier claim failed at genesis)"
	pub, err := store.PublishCompletenessVerdict(ctx, notEvaluated, nil)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if pub.Applied {
		t.Error("Applied = true: a lower-tip run that evaluated nothing overwrote the stored verdict")
	}
	got := storedVerdict(t, ctx, store)
	if !got.Complete || !got.ProjectionOK || got.ProjectionVerifiedFrom != 50_000_000 || got.Tip != 63_500_000 {
		t.Errorf("stored verdict changed: complete=%v projection_ok=%v projection_verified_from=%d tip=%d, want true/true/50000000/63500000",
			got.Complete, got.ProjectionOK, got.ProjectionVerifiedFrom, got.Tip)
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

// TestComputeCompleteness_OneSourceErrorDoesNotWithholdTheRest drives the real
// compute-completeness subcommand on real TimescaleDB.
//
// soroswap is the FIRST catalogue source. A trigger makes its verdict write
// fail, standing in for any per-source error (an RPC seed gap, a lake
// deadline, a served-floor read). The loop must not return at that first
// error, or no later source gets a verdict and /v1/coverage keeps serving every
// source's prior verdict while the run looks like one failed source. Every
// other source is evaluated and published, soroswap publishes nothing,
// and the run still fails. -skip-recognition only because an empty lake is
// (rightly) refused as a vacuous recognition scan before the loop is reached.
func TestComputeCompleteness_OneSourceErrorDoesNotWithholdTheRest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	chAddr := clickhouseAddr(t)
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfgPath := filepath.Join(t.TempDir(), "stellarindex.toml")
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf("[storage]\npostgres_dsn = %q\n", dsn)), 0o600); err != nil {
		t.Fatal(err)
	}

	const failSoroswap = `
CREATE FUNCTION fail_soroswap_verdict() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.source = 'soroswap' THEN
        RAISE EXCEPTION 'injected soroswap verdict failure';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER fail_soroswap_verdict BEFORE INSERT OR UPDATE ON completeness_snapshots
    FOR EACH ROW EXECUTE FUNCTION fail_soroswap_verdict();`
	if _, err := store.DB().ExecContext(ctx, failSoroswap); err != nil {
		t.Fatalf("install fault trigger: %v", err)
	}

	runErr := chops.Run([]string{"compute-completeness", "-config", cfgPath, "-ch", "-ch-addr", chAddr, "-to", "70000000", "-skip-recognition", "-write"})
	if runErr == nil || !strings.Contains(runErr.Error(), "soroswap") {
		t.Fatalf("run err = %v, want a non-nil error naming soroswap (a failed source must fail the run)", runErr)
	}

	published := map[string]bool{}
	rows, err := store.DB().QueryContext(ctx, `SELECT source FROM completeness_snapshots`)
	if err != nil {
		t.Fatalf("read snapshots: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		published[s] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	if published["soroswap"] {
		t.Error("soroswap has a verdict row despite its write failing")
	}
	for _, src := range []string{"aquarius", "phoenix", "blend"} {
		if !published[src] {
			t.Errorf("%s has no verdict: soroswap's error withheld every later source's verdict (published: %v)", src, published)
		}
	}
}

func mevSupersedeState(t *testing.T, ctx context.Context, store *timescale.Store, key string) (legs int, accounts string, detectedAt time.Time) {
	t.Helper()
	err := store.DB().QueryRowContext(ctx,
		`SELECT jsonb_array_length(detail -> 'legs'), array_to_string(accounts, ','), detected_at
		   FROM mev_events WHERE dedup_key = $1`, key).Scan(&legs, &accounts, &detectedAt)
	if err != nil {
		t.Fatalf("select %s: %v", key, err)
	}
	return legs, accounts, detectedAt
}

// TestStorage_MEVEventEvidenceSupersedes: a re-scan whose legs
// contain the stored legs replaces the stored evidence (a later scan found
// another victim), a re-scan that saw fewer legs never overwrites, and
// neither counts as a new event or moves the first detection's time.
func TestStorage_MEVEventEvidenceSupersedes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	store := openMEVStore(t, ctx)

	const key = "sandwich:supersede:GATK"
	first := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	legA := `{"tx_hash":"a","role":"bracket"}`
	legV1 := `{"tx_hash":"v1","role":"victim"}`
	legV2 := `{"tx_hash":"v2","role":"victim"}`
	write := func(at time.Time, accounts []string, legs string) bool {
		t.Helper()
		ok, err := store.InsertMEVEvent(ctx, domain.MEVStoredEvent{
			Kind: "sandwich", Ledger: 61_000_000, DetectedAtLedger: 61_000_000, Timestamp: at,
			TxHashes: []string{"a"}, Accounts: accounts, DedupKey: key,
			DetailJSON: []byte(`{"legs":[` + legs + `],"note":"n"}`),
		})
		if err != nil {
			t.Fatalf("InsertMEVEvent: %v", err)
		}
		return ok
	}

	if !write(first, []string{"GATK", "GV1"}, legA+","+legV1) {
		t.Fatal("first detection reported inserted=false")
	}
	if write(first.Add(5*time.Minute), []string{"GATK", "GV1", "GV2"}, legA+","+legV1+","+legV2) {
		t.Error("a superseding re-scan reported inserted=true")
	}
	legs, accounts, at := mevSupersedeState(t, ctx, store, key)
	if legs != 3 || accounts != "GATK,GV1,GV2" {
		t.Errorf("after a containing re-scan: legs=%d accounts=%q, want 3 and GATK,GV1,GV2", legs, accounts)
	}
	if !at.Equal(first) {
		t.Errorf("detected_at moved to %v; the first detection's time is the event's identity", at)
	}

	if write(first.Add(10*time.Minute), []string{"GATK", "GV2"}, legA+","+legV2) {
		t.Error("a narrower re-scan reported inserted=true")
	}
	if legs, accounts, _ := mevSupersedeState(t, ctx, store, key); legs != 3 || accounts != "GATK,GV1,GV2" {
		t.Errorf("a narrower re-scan overwrote the stored evidence: legs=%d accounts=%q", legs, accounts)
	}
}

// TestOracleUpdates_ReingestIsIdempotent pins why ts in oracle_updates'
// primary key is safe on re-ingest: ts is a pure function of the event
// payload and the ledger header close time, never the wall clock, so a
// live pass and a replay of the same mainnet event produce the same key
// and the second pass lands no new row and no new entry tally.
func TestOracleUpdates_ReingestIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c.InstallAliasRegistry(nil)

	raw, err := os.ReadFile(filepath.Join("..", "fixtures", "reflector", "v6-2026-04-23", "62251160_9322ba2f5c95.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fx struct {
		ContractID     string   `json:"contract_id"`
		Ledger         uint32   `json:"ledger"`
		TxHash         string   `json:"tx_hash"`
		LedgerClosedAt string   `json:"ledger_closed_at"`
		Topics         []string `json:"topics"`
		Value          string   `json:"value"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	closedAt, err := time.Parse(time.RFC3339, fx.LedgerClosedAt)
	if err != nil {
		t.Fatal(err)
	}

	dec := reflector.NewDecoder(reflector.VariantDEX, fx.ContractID)
	decode := func(ledgerClosedAt string) []c.OracleUpdate {
		t.Helper()
		out, err := dec.Decode(events.Event{
			Type: "contract", ContractID: fx.ContractID, Ledger: fx.Ledger, TxHash: fx.TxHash,
			LedgerClosedAt: ledgerClosedAt, Topic: fx.Topics, Value: fx.Value,
		})
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		us := make([]c.OracleUpdate, 0, len(out))
		for _, ev := range out {
			us = append(us, ev.(reflector.UpdateEvent).Update)
		}
		return us
	}

	// Live ingest stamps the close time from the ledger header; a replay
	// reads it back from the ClickHouse lake in a non-UTC zone at worst.
	live := decode(fx.LedgerClosedAt)
	replay := decode(closedAt.In(time.FixedZone("UTC+3", 3*3600)).Format(time.RFC3339))
	if len(live) == 0 || len(live) != len(replay) {
		t.Fatalf("decoded %d live rows and %d replay rows, want the same non-zero count", len(live), len(replay))
	}
	for i := range live {
		if !live[i].Timestamp.Equal(replay[i].Timestamp) {
			t.Fatalf("row %d ts: live %s, replay %s; ts must not depend on when the event is ingested",
				i, live[i].Timestamp, replay[i].Timestamp)
		}
		// The topic's publication time, distinct from the close time, proves
		// the payload-derived arm ran rather than the close-time fallback.
		if live[i].Timestamp.Equal(closedAt) {
			t.Fatalf("row %d ts = ledger close %s, want the oracle's publication time from topic[2]", i, closedAt)
		}
	}

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	counts := func() (rows, tally int64) {
		t.Helper()
		if err := store.DB().QueryRowContext(ctx,
			`SELECT count(*) FROM oracle_updates WHERE ledger = $1 AND tx_hash = $2`,
			fx.Ledger, fx.TxHash).Scan(&rows); err != nil {
			t.Fatalf("count rows: %v", err)
		}
		if err := store.DB().QueryRowContext(ctx,
			`SELECT COALESCE(sum(entry_count), 0) FROM source_entry_counts WHERE source = $1`,
			live[0].Source).Scan(&tally); err != nil {
			t.Fatalf("read tally: %v", err)
		}
		return rows, tally
	}

	for _, u := range live {
		if err := store.InsertOracleUpdate(ctx, u); err != nil {
			t.Fatalf("live insert: %v", err)
		}
	}
	rows, tally := counts()
	if rows != int64(len(live)) || tally != int64(len(live)) {
		t.Fatalf("after live pass: rows=%d tally=%d, want %d each", rows, tally, len(live))
	}

	for _, u := range replay {
		if err := store.InsertOracleUpdate(ctx, u); err != nil {
			t.Fatalf("replay insert: %v", err)
		}
	}
	if r2, t2 := counts(); r2 != rows || t2 != tally {
		t.Fatalf("after replay: rows=%d tally=%d, want unchanged %d/%d", r2, t2, rows, tally)
	}
}
