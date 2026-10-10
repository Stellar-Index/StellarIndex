//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
	"github.com/Stellar-Index/StellarIndex/internal/sources/reflector"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
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

// TestCascadeMISCONF_EndToEnd is the integration twin of
// internal/api/v1/cache_unavailable_test.go's stub-based tests. It
// uses a real Redis container in real MISCONF state to drive the
// 503-on-cache-unavailable mapping end-to-end.
//
// Skipped automatically when Docker isn't available — mirrors the
// existing pattern in test/integration/migrations_test.go.
//
// Nominal runtime: ~10s on a warm Docker cache, ~30s on a cold one.
func TestCascadeMISCONF_EndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	rdb, redisCtr := startRedis(t, ctx)
	t.Cleanup(func() {
		_ = redisCtr.Terminate(context.Background())
	})

	// Verify Redis is alive end-to-end before we start chaos.
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatalf("redis ping: %v", err)
	}

	// Build a v1.Server wired with our test reader that talks to
	// the real Redis container. The reader's LatestOracleUpdatesForAsset
	// does a Redis SET on every call — under MISCONF that SET fails
	// with the MISCONF prefix, which is exactly the failure shape
	// the production cascade-affected handlers see in the May-10
	// SEV-2 (commit a91f901b's rationale).
	oracle := &redisOracleReader{rdb: rdb}
	srv := v1.New(v1.Options{Oracle: oracle})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// ─── 1. baseline: healthy Redis → 200 ─────────────────────────
	t.Run("baseline_200", func(t *testing.T) {
		resp := httpGet(t, ts.URL+"/v1/oracle/latest?asset=native")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("baseline status = %d, want 200 (Redis healthy)", resp.StatusCode)
		}
	})

	// ─── 2. force MISCONF, assert 503 + Retry-After ───────────────
	t.Run("misconf_503_with_retry_after", func(t *testing.T) {
		probeSet := func() error {
			return rdb.Set(ctx, "misconf-probe-handler", "v", 0).Err()
		}
		if err := forceMISCONF(ctx, redisCtr, probeSet); err != nil {
			t.Fatalf("forceMISCONF: %v", err)
		}
		// Heal on test failure so subsequent sub-tests aren't blocked.
		defer func() {
			if err := healMISCONF(ctx, redisCtr); err != nil {
				t.Logf("heal on cleanup: %v", err)
			}
		}()

		// Wait briefly for the new state to propagate; BGSAVE is
		// asynchronous, the stop-writes flag flips once the fork
		// fails. Retry the request a few times rather than sleeping
		// blindly — typical settle is <1s.
		var resp *http.Response
		var err error
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			resp, err = http.Get(ts.URL + "/v1/oracle/latest?asset=native")
			if err == nil && resp.StatusCode == http.StatusServiceUnavailable {
				break
			}
			if resp != nil {
				resp.Body.Close()
			}
			time.Sleep(500 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("GET under MISCONF: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusServiceUnavailable {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 503 (MISCONF cascade); body=%s",
				resp.StatusCode, body)
		}
		if got := resp.Header.Get("Retry-After"); got != "30" {
			t.Errorf("Retry-After = %q, want 30 (writeCacheUnavailableProblem invariant)", got)
		}
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), "errors/cache-unavailable") {
			t.Errorf("body missing errors/cache-unavailable type URL: %s", body)
		}
		// problem+json must be a valid envelope.
		var problem map[string]any
		if err := json.Unmarshal(body, &problem); err != nil {
			t.Errorf("response body not valid JSON: %v", err)
		}
		if got, _ := problem["status"].(float64); int(got) != http.StatusServiceUnavailable {
			t.Errorf("problem.status = %v, want 503", problem["status"])
		}
	})

	// ─── 3. heal Redis, assert routes return to nominal ──────────
	t.Run("recovery_200", func(t *testing.T) {
		if err := healMISCONF(ctx, redisCtr); err != nil {
			t.Fatalf("heal: %v", err)
		}
		// Poll until the route returns to 200 — heal is async; typical
		// settle is <2s once BGSAVE reports ok.
		deadline := time.Now().Add(30 * time.Second)
		var lastStatus int
		for time.Now().Before(deadline) {
			resp, err := http.Get(ts.URL + "/v1/oracle/latest?asset=native")
			if err != nil {
				time.Sleep(500 * time.Millisecond)
				continue
			}
			lastStatus = resp.StatusCode
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatalf("did not recover to 200 within 30s; last status = %d", lastStatus)
	})

	// ─── 4. predicate sanity — go-redis surfaces MISCONF the way
	//        IsCacheUnavailable expects ───────────────────────────
	t.Run("go_redis_misconf_classification", func(t *testing.T) {
		// Repro MISCONF directly via the client — bypass the handler
		// to verify go-redis's error shape hasn't drifted in a way
		// the predicate would miss. This is a defence-in-depth check;
		// if it ever fails, IsCacheUnavailable needs a new branch.
		probeSet := func() error {
			return rdb.Set(ctx, "misconf-probe-direct", "v", 0).Err()
		}
		if err := forceMISCONF(ctx, redisCtr, probeSet); err != nil {
			t.Fatalf("forceMISCONF: %v", err)
		}
		defer func() { _ = healMISCONF(ctx, redisCtr) }()

		// Retry briefly for state to settle.
		var setErr error
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			setErr = rdb.Set(ctx, "misconf-probe", "v", 0).Err()
			if setErr != nil {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
		if setErr == nil {
			t.Fatalf("expected MISCONF error from SET, got nil")
		}
		if !v1.IsCacheUnavailable(setErr) {
			t.Errorf("IsCacheUnavailable did not classify go-redis MISCONF error: %v", setErr)
		}
		// Also classify the wrapped form (the orchestrator wraps via
		// fmt.Errorf("redis set %s: %w", key, err)).
		wrapped := fmt.Errorf("redis set vwap:foo: %w", setErr)
		if !v1.IsCacheUnavailable(wrapped) {
			t.Errorf("IsCacheUnavailable did not classify wrapped MISCONF: %v", wrapped)
		}
	})
}

// ─── helpers ──────────────────────────────────────────────────────

// startRedis spins up a single-node Redis container with the same
// settings the dev compose uses (`stop-writes-on-bgsave-error yes`
// is on by default in Redis 7) PLUS `--save 1 1` so BGSAVE auto-
// fires on the first write — that's how forceMISCONF provokes the
// stop-writes flag without needing CONFIG SET (which Redis 7.4
// rejects at runtime for the `dir` key as a protected config).
//
// Returns a go-redis client + the container handle so the caller
// can exec docker commands.
func startRedis(t *testing.T, ctx context.Context) (*redis.Client, testcontainers.Container) {
	t.Helper()
	ctr, err := testcontainers.Run(ctx,
		"redis:7.4-alpine",
		testcontainers.WithExposedPorts("6379/tcp"),
		// --save 1 1: trigger a BGSAVE after any single write within
		// 1 second. Combined with chmod 0 /data in forceMISCONF, this
		// drives the persistence layer into err state on the next
		// write after the chmod.
		testcontainers.WithCmd("redis-server", "--save", "1", "1"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("Ready to accept connections").
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		// Mirror the migrations_test.go pattern: skip when Docker is
		// unavailable so the test is safe to run on a laptop without
		// the daemon. Real CI nodes have Docker; this branch is a
		// developer-convenience.
		if isDockerUnavailable(err) {
			t.Skipf("docker unavailable, skipping integration test: %v", err)
		}
		t.Fatalf("start redis: %v", err)
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := ctr.MappedPort(ctx, "6379")
	if err != nil {
		t.Fatalf("container mapped port: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%s", host, port.Port()),
	})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, ctr
}

// forceMISCONF puts Redis into the stop-writes state by:
//  1. chmod 0 /data — strips write permission from the snapshot
//     dir. CONFIG SET dir is rejected at runtime in Redis 7.4 as
//     a protected config, so we change the FS underneath instead.
//  2. SET a probe key — triggers the `--save 1 1` rule (started
//     with that flag in startRedis), which fires BGSAVE within 1s.
//  3. Poll INFO persistence until rdb_last_bgsave_status:err.
//
// Once BGSAVE has failed once, every subsequent write returns
// `MISCONF Redis is configured to save RDB snapshots, but it's
// currently unable to persist to disk` — exactly the May-10 SEV-2
// surface (and what a91f901b's helper maps to HTTP 503).
//
// probeSet is the caller's go-redis-driven write — using the same
// client that subsequent assertions use ensures we hit the same
// connection pool and same surface.
func forceMISCONF(ctx context.Context, ctr testcontainers.Container, probeSet func() error) error {
	// Re-arm the safety net: healMISCONF turns it off on cleanup,
	// and Redis won't block writes on BGSAVE failure without it. On
	// the first call this is a no-op (default is "yes" on image
	// start); on the second call this is what makes the test
	// re-armable. Authoritative writes-blocked behaviour requires
	// this flag, NOT just rdb_last_bgsave_status:err.
	if err := execIgnoringBGSAVE(ctx, ctr,
		[]string{
			"redis-cli", "CONFIG", "SET",
			"stop-writes-on-bgsave-error", "yes",
		}); err != nil {
		return fmt.Errorf("re-arm stop-writes: %w", err)
	}
	if err := execIgnoringBGSAVE(ctx, ctr,
		[]string{"chmod", "0", "/data"}); err != nil {
		return fmt.Errorf("chmod /data: %w", err)
	}
	// Trigger the save rule. Multiple writes may be needed because
	// the `--save N M` threshold could already have been satisfied
	// by an earlier write that BGSAVEd cleanly.
	if err := probeSet(); err != nil && !strings.Contains(err.Error(), "MISCONF") {
		return fmt.Errorf("probe SET: %w", err)
	}
	// Authoritative readiness check: keep probing SET until it
	// returns MISCONF. rdb_last_bgsave_status:err is a NECESSARY
	// but not SUFFICIENT condition — Redis only sets the
	// stop-writes-on-bgsave-error trip-wire on the next write
	// attempt after the failed BGSAVE. Looping on the SET itself
	// gives us the end-state guarantee callers actually want.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		err := probeSet()
		if err != nil && strings.Contains(err.Error(), "MISCONF") {
			return nil
		}
		// Cross-check: if BGSAVE has fired in err state but
		// stop-writes hasn't tripped yet, sleeping briefly gives
		// the trip-wire time to engage on the next probe.
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("redis did not enter MISCONF (writes still succeeding) within 15s")
}

// healMISCONF restores Redis to a writeable state: chmod 0755 /data,
// trigger a BGSAVE that succeeds, clear the stop-writes flag.
func healMISCONF(ctx context.Context, ctr testcontainers.Container) error {
	if err := execIgnoringBGSAVE(ctx, ctr,
		[]string{"chmod", "0755", "/data"}); err != nil {
		return fmt.Errorf("chmod /data: %w", err)
	}
	// BGSAVE here is safe — `dir` is back to writeable. Use
	// redis-cli to avoid needing a live go-redis client (the caller
	// may still be in the middle of a MISCONF-blocked op).
	_ = execIgnoringBGSAVE(ctx, ctr, []string{"redis-cli", "BGSAVE"})
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		out, err := execCapture(ctx, ctr,
			[]string{"redis-cli", "INFO", "persistence"})
		if err == nil && strings.Contains(out, "rdb_last_bgsave_status:ok") {
			// BGSAVE recovered; clear the safety net (matches the
			// chaos scenario sequence in test/chaos/scenarios/
			// 04-redis-misconf.sh).
			return execIgnoringBGSAVE(ctx, ctr,
				[]string{
					"redis-cli", "CONFIG", "SET",
					"stop-writes-on-bgsave-error", "no",
				})
		}
		_ = execIgnoringBGSAVE(ctx, ctr, []string{"redis-cli", "BGSAVE"})
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("BGSAVE did not report ok within 15s")
}

// execIgnoringBGSAVE is a thin wrapper around ctr.Exec that ignores
// the "Background saving started" stderr noise from BGSAVE — Redis
// returns 0 but writes a status line to stdout that confuses our
// captured-output asserts elsewhere. We only care that the command
// dispatched.
func execIgnoringBGSAVE(ctx context.Context, ctr testcontainers.Container, cmd []string) error {
	code, _, err := ctr.Exec(ctx, cmd)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("exit %d", code)
	}
	return nil
}

// execCapture runs cmd and returns combined stdout as a string.
func execCapture(ctx context.Context, ctr testcontainers.Container, cmd []string) (string, error) {
	code, r, err := ctr.Exec(ctx, cmd)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("exit %d", code)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// httpGet wraps http.Get with a per-request timeout, mirroring the
// r1-smoke.sh per-request budget.
func httpGet(t *testing.T, url string) *http.Response {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

// isDockerUnavailable mirrors migrations_test.go's behaviour: skip
// when Docker isn't reachable rather than fail the test. We don't
// have a single sentinel for this — testcontainers wraps with its
// own error type — so check both the message and the wrap chain.
func isDockerUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, hint := range []string{
		"Cannot connect to the Docker daemon",
		"docker daemon",
		"connect: no such file or directory",
		"context deadline exceeded",
		"rootless docker",
	} {
		if strings.Contains(msg, hint) {
			return true
		}
	}
	return false
}

// ─── redisOracleReader — minimal v1.OracleReader hitting real Redis
//
// Every call performs a Redis SET to mirror the cascade-affected
// handlers' cache-write pattern. Under MISCONF that SET fails with
// the MISCONF prefix; the handler wraps it (via the existing
// observability seam) and the v1 layer's IsCacheUnavailable
// predicate flips the response to 503 + Retry-After.

type redisOracleReader struct {
	rdb *redis.Client
}

// LatestOracleUpdatesForAsset returns an empty slice on healthy
// Redis (the asset has no observations seeded — we're only testing
// the cache-write failure surface, not the data path). Under
// MISCONF the SET fails and the error propagates to the handler.
func (r *redisOracleReader) LatestOracleUpdatesForAsset(
	ctx context.Context, asset c.Asset, sourceFilter string,
) ([]c.OracleUpdate, error) {
	// Touch Redis with a write — this is the operation that fails
	// with MISCONF in production (per a91f901b's diagnosis).
	if err := r.rdb.Set(ctx, "oracle:probe:"+asset.String(), "v", time.Minute).Err(); err != nil {
		return nil, fmt.Errorf("redis set oracle:probe:%s: %w", asset.String(), err)
	}
	return nil, nil
}

func (r *redisOracleReader) LatestOracleUpdatesForAssets(
	ctx context.Context, assets []c.Asset, sourceFilter string,
) ([]c.OracleUpdate, error) {
	if err := r.rdb.Set(ctx, "oracle:probe-multi", "v", time.Minute).Err(); err != nil {
		return nil, fmt.Errorf("redis set oracle:probe-multi: %w", err)
	}
	return nil, nil
}

func (r *redisOracleReader) LatestOracleStreams(
	ctx context.Context,
) ([]c.OracleUpdate, error) {
	if err := r.rdb.Set(ctx, "oracle:streams-probe", "v", time.Minute).Err(); err != nil {
		return nil, fmt.Errorf("redis set oracle:streams-probe: %w", err)
	}
	return nil, nil
}

// TestRunCensusDay_PrunesByCloseTimeAndSwapsPartition runs the census rollup
// against a real server on the certified Tier-1 schema and proves the two
// things its unit tests cannot:
//
//  1. the day window WHERE close_time >= d AND close_time < d+1 is served by
//     a minmax skip index (idx_ce_close_time) that DROPS granules outside
//     the day — contract_events is partitioned and sorted by ledger_seq, so
//     before the index existed the 30-minute rollup read every granule of
//     the table per run;
//  2. RunCensusDay's private-staging CREATE / INSERT / REPLACE PARTITION /
//     DROP sequence executes and lands exact per-contract counts for the
//     day and nothing from the neighbouring day.
func TestRunCensusDay_PrunesByCloseTimeAndSwapsPartition(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	const (
		baseLedger = uint32(90_000_001)
		contractA  = "CTEST_CENSUS_A_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		contractB  = "CTEST_CENSUS_B_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	)
	day := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	inDay := day.Add(6 * time.Hour)
	nextDay := day.Add(24 * time.Hour).Add(time.Hour)

	event := func(seq uint32, at time.Time, contract string, eventIndex uint32) chstore.ContractEventRow {
		return chstore.ContractEventRow{
			LedgerSeq: seq, CloseTime: at, TxHash: fmt.Sprintf("%064x", seq),
			OpIndex: 0, EventIndex: eventIndex, ContractID: contract, EventType: "contract",
			TopicCount: 0, TopicsXDR: []string{}, DataXDR: "", OpArgsXDR: []string{}, InSuccessfulCall: 1,
		}
	}
	// Two separate flushes → two parts, so the neighbouring day is a granule
	// of its own that the skip index can drop. Merges are paused so the
	// server cannot fold both days into one granule underneath the EXPLAIN.
	if err := conn.Exec(ctx, "SYSTEM STOP MERGES stellar.contract_events"); err != nil {
		t.Fatalf("stop merges: %v", err)
	}
	t.Cleanup(func() { _ = conn.Exec(context.Background(), "SYSTEM START MERGES stellar.contract_events") })
	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	for _, ext := range []chstore.LedgerExtract{
		{
			Ledger: chstore.LedgerRow{LedgerSeq: baseLedger, CloseTime: inDay, ProtocolVersion: 22, SorobanEventCount: 3},
			Events: []chstore.ContractEventRow{
				event(baseLedger, inDay, contractA, 0),
				event(baseLedger, inDay, contractA, 1),
				event(baseLedger, inDay, contractB, 2),
			},
		},
		{
			Ledger: chstore.LedgerRow{LedgerSeq: baseLedger + 1, CloseTime: nextDay, ProtocolVersion: 22, SorobanEventCount: 1},
			Events: []chstore.ContractEventRow{event(baseLedger+1, nextDay, contractA, 0)},
		},
	} {
		if err := sink.Add(ctx, ext); err != nil {
			t.Fatalf("sink add: %v", err)
		}
		if err := sink.Flush(ctx); err != nil {
			t.Fatalf("sink flush: %v", err)
		}
	}

	// (1) the index is declared on the live table …
	var idxType, idxExpr string
	if err := conn.QueryRow(ctx, `SELECT type, expr FROM system.data_skipping_indices
		WHERE database = 'stellar' AND table = 'contract_events' AND name = 'idx_ce_close_time'`).
		Scan(&idxType, &idxExpr); err != nil {
		t.Fatalf("stellar.contract_events has no skip index idx_ce_close_time — the census day window full-scans: %v", err)
	}
	if idxType != "minmax" || idxExpr != "close_time" {
		t.Fatalf("idx_ce_close_time = %s(%s), want minmax(close_time)", idxType, idxExpr)
	}
	// … and the planner actually drops granules with it for the rollup's own
	// predicate shape.
	plan := explain(t, ctx, conn, fmt.Sprintf(`EXPLAIN indexes = 1 SELECT count() FROM stellar.contract_events
		WHERE close_time >= toDateTime('%s', 'UTC') AND close_time < toDateTime('%s', 'UTC')`,
		day.Format("2006-01-02 15:04:05"), day.Add(24*time.Hour).Format("2006-01-02 15:04:05")))
	selected, initial := skipIndexGranules(t, plan, "idx_ce_close_time")
	if selected >= initial {
		t.Fatalf("idx_ce_close_time dropped no granules (%d/%d) — the neighbouring day's part should have been pruned:\n%s", selected, initial, plan)
	}

	// (2) the rollup lands exact counts for the day and only the day.
	if err := chstore.RunCensusDay(ctx, addr, day, false, t.Logf); err != nil {
		t.Fatalf("RunCensusDay: %v", err)
	}
	rows, err := conn.Query(ctx, `SELECT contract_id, events, last_ledger FROM stellar.contracts_census_daily
		WHERE day = ? AND contract_id IN (?, ?) ORDER BY contract_id`, day, contractA, contractB)
	if err != nil {
		t.Fatalf("read census: %v", err)
	}
	defer func() { _ = rows.Close() }()
	got := map[string][2]uint64{}
	for rows.Next() {
		var id string
		var events uint64
		var last uint32
		if err := rows.Scan(&id, &events, &last); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[id] = [2]uint64{events, uint64(last)}
	}
	want := map[string][2]uint64{contractA: {2, uint64(baseLedger)}, contractB: {1, uint64(baseLedger)}}
	if len(got) != len(want) {
		t.Fatalf("census rows = %v, want %v", got, want)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("census[%s] = (events %d, last_ledger %d), want (events %d, last_ledger %d)", id, got[id][0], got[id][1], w[0], w[1])
		}
	}

	// (3) staging is per-run and private: the run's own table is dropped on
	// exit, and the Tier-1 schema declares no shared twin for anything to
	// leave behind.
	var staging []string
	srows, err := conn.Query(ctx, `SELECT name FROM system.tables
		WHERE database = 'stellar' AND name LIKE 'contracts_census_daily_staging%' ORDER BY name`)
	if err != nil {
		t.Fatalf("list staging tables: %v", err)
	}
	defer func() { _ = srows.Close() }()
	for srows.Next() {
		var n string
		if err := srows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		staging = append(staging, n)
	}
	if len(staging) != 0 {
		t.Fatalf("census staging tables left after the run: %v, want none (the shared stellar.contracts_census_daily_staging is dead DDL; the private one is dropped)", staging)
	}
}

// explain returns EXPLAIN output as one newline-joined string.
func explain(t *testing.T, ctx context.Context, conn driver.Conn, q string) string {
	t.Helper()
	rows, err := conn.Query(ctx, q)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var lines []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			t.Fatalf("explain scan: %v", err)
		}
		lines = append(lines, l)
	}
	return strings.Join(lines, "\n")
}

// skipIndexGranules finds the `Skip` block for the named index in an
// `EXPLAIN indexes = 1` plan and returns its "Granules: selected/initial".
func skipIndexGranules(t *testing.T, plan, name string) (selected, initial int) {
	t.Helper()
	lines := strings.Split(plan, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != "Name: "+name {
			continue
		}
		for _, m := range lines[i+1:] {
			m = strings.TrimSpace(m)
			if !strings.HasPrefix(m, "Granules: ") {
				continue
			}
			parts := strings.SplitN(strings.TrimPrefix(m, "Granules: "), "/", 2)
			if len(parts) != 2 {
				break
			}
			a, errA := strconv.Atoi(parts[0])
			b, errB := strconv.Atoi(parts[1])
			if errA != nil || errB != nil {
				break
			}
			return a, b
		}
		break
	}
	t.Fatalf("plan does not consult skip index %s:\n%s", name, plan)
	return 0, 0
}

// TestContiguousThroughDay_StopsAtHole runs the census walk bound's SQL on a
// real server: a hole on day D pins the bound to D (so D+1 is not computed
// and D stays the resume point), and healing the hole releases it.
func TestContiguousThroughDay_StopsAtHole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// Isolated range above every other suite ledger (the watermark test uses
	// 215M) with the latest close times, so this test owns both the global
	// ledger max and the "last ledger before the day" lookup.
	const base = uint32(300_000_000)
	d := time.Date(2040, 3, 10, 0, 0, 0, 0, time.UTC)
	next := d.Add(24 * time.Hour)

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	// Owning the global max is only safe while this test runs: left behind, these
	// rows sit above sdex_orderbook_lake_hole_test's range, which must be the tip.
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer ccancel()
		conn := dialClickHouse(t, cctx, "stellar")
		if err := conn.Exec(cctx, fmt.Sprintf(`ALTER TABLE stellar.ledgers DELETE
			WHERE ledger_seq BETWEEN %d AND %d SETTINGS mutations_sync = 2`, base, base+4)); err != nil {
			t.Errorf("purge census fixture ledgers: %v", err)
		}
	})
	seed := func(seq uint32, at time.Time) {
		t.Helper()
		if err := sink.Add(ctx, chstore.LedgerExtract{Ledger: chstore.LedgerRow{
			LedgerSeq: seq, CloseTime: at, LedgerHash: "aa00", PrevHash: "bb00", ProtocolVersion: 22,
			BucketListHash: "cc00", TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		}}); err != nil {
			t.Fatalf("sink add ledger %d: %v", seq, err)
		}
		if err := sink.Flush(ctx); err != nil {
			t.Fatalf("flush ledger %d: %v", seq, err)
		}
	}
	seed(base, d.Add(-time.Hour))
	seed(base+1, d.Add(time.Hour))
	// base+2 (day D, 02:00) is the LiveSink hole.
	seed(base+3, d.Add(3*time.Hour))
	seed(base+4, next.Add(time.Hour))

	got, ok, err := chstore.ContiguousThroughDay(ctx, addr, d)
	if err != nil || !ok || !got.Equal(d) {
		t.Fatalf("hole on %s: ContiguousThroughDay = (%s, %v, %v), want (%s, true, nil)", d, got, ok, err, d)
	}

	// Nothing at or after D+2, and ledger base+5 is not there yet: nothing
	// is contiguous from the start of that day.
	if _, ok, err := chstore.ContiguousThroughDay(ctx, addr, next.Add(24*time.Hour)); err != nil || ok {
		t.Fatalf("day past the lake tip: ok=%v err=%v, want ok=false", ok, err)
	}

	seed(base+2, d.Add(2*time.Hour)) // ch-live-catchup heals the hole
	got, ok, err = chstore.ContiguousThroughDay(ctx, addr, d)
	if err != nil || !ok || !got.Equal(next) {
		t.Fatalf("healed: ContiguousThroughDay = (%s, %v, %v), want (%s, true, nil)", got, ok, err, next)
	}
}

// TestRunCensusDay_RefusesShrink runs the shrink check's SQL on a real
// server: a live partition of 2 contracts is not replaced by a recompute of
// a day the lake holds no events for, unless shrinkOK.
func TestRunCensusDay_RefusesShrink(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	day := time.Date(2040, 5, 1, 0, 0, 0, 0, time.UTC)
	if err := conn.Exec(ctx, `INSERT INTO stellar.contracts_census_daily (day, contract_id, events, last_ledger, last_seen)
		VALUES (?, 'CTEST_SHRINK_A', 7, 1, ?), (?, 'CTEST_SHRINK_B', 3, 1, ?)`, day, day, day, day); err != nil {
		t.Fatalf("seed live partition: %v", err)
	}
	liveRows := func() uint64 {
		t.Helper()
		var n uint64
		if err := conn.QueryRow(ctx, `SELECT count() FROM stellar.contracts_census_daily WHERE day = ?`, day).Scan(&n); err != nil {
			t.Fatalf("count live partition: %v", err)
		}
		return n
	}

	err := chstore.RunCensusDay(ctx, addr, day, false, t.Logf)
	if !errors.Is(err, chstore.ErrCensusShrink) {
		t.Fatalf("empty recompute over a 2-row live day returned %v, want ErrCensusShrink", err)
	}
	if n := liveRows(); n != 2 {
		t.Fatalf("live partition has %d row(s) after a refused shrink, want the original 2", n)
	}

	if err := chstore.RunCensusDay(ctx, addr, day, true, t.Logf); err != nil {
		t.Fatalf("shrinkOK recompute: %v", err)
	}
	if n := liveRows(); n != 0 {
		t.Fatalf("live partition has %d row(s) after a -shrink-ok recompute of an empty day, want 0", n)
	}
}

// TestClickHouseContractEventsRMTDedup is the live-ClickHouse proof that
// ContractEventsRecent (Go-side adjacent-row dedup — a SQL LIMIT 1 BY disables
// reverse read-in-order and costs 100× on busy contracts) and EventsByTx (FINAL) must serve each contract
// event EXACTLY ONCE even while
// stellar.contract_events — a ReplacingMergeTree — holds an un-merged duplicate
// part (the legitimate post-heal / ch-rebuild / partial-flush-retry state).
//
// Determinism: the two same-key inserts create two parts, and a background merge
// would collapse them on its own (making a non-deduping reader accidentally pass).
// SYSTEM STOP MERGES pins the table in its un-merged state for the duration, so
// the dedup MUST come from the query — reverting the fix makes both readers
// return 4 rows (2 events x 2 parts) instead of 2.
func TestClickHouseContractEventsRMTDedup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		ledger     = uint32(70_100_001)
		contractID = "CTEST_W4_RMT_DEDUP_AAAAAAAAAAAAAAAAAAAAAAAAAAA"
		txHash     = "2222222222222222222222222222222222222222222222222222222222222222"
	)
	closeTime := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)

	// Pin the table un-merged so the two parts genuinely coexist at read time.
	raw := dialClickHouse(t, ctx, "stellar")
	if err := raw.Exec(ctx, "SYSTEM STOP MERGES stellar.contract_events"); err != nil {
		t.Fatalf("SYSTEM STOP MERGES: %v", err)
	}
	t.Cleanup(func() {
		_ = raw.Exec(context.Background(), "SYSTEM START MERGES stellar.contract_events")
	})

	// Two DISTINCT events in one tx — proves dedup collapses duplicate PARTS
	// without also collapsing distinct events (event_index 0 vs 1).
	ext := chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{
			LedgerSeq: ledger, CloseTime: closeTime, LedgerHash: "dd00dd00", PrevHash: "ee00ee00",
			ProtocolVersion: 22, TxCount: 1, OpCount: 1, SorobanEventCount: 2,
		},
		Events: []chstore.ContractEventRow{
			{
				LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHash, OpIndex: 0, EventIndex: 0,
				ContractID: contractID, EventType: "contract", TopicCount: 1, Topic0Sym: "mint",
				TopicsXDR: []string{scval.MustEncodeSymbol("mint")}, DataXDR: scval.MustEncodeString("a"),
				OpArgsXDR: []string{}, InSuccessfulCall: 1,
			},
			{
				LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHash, OpIndex: 0, EventIndex: 1,
				ContractID: contractID, EventType: "contract", TopicCount: 1, Topic0Sym: "transfer",
				TopicsXDR: []string{scval.MustEncodeSymbol("transfer")}, DataXDR: scval.MustEncodeString("b"),
				OpArgsXDR: []string{}, InSuccessfulCall: 1,
			},
		},
	}

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })

	// Two separate flushes of the IDENTICAL extract → two un-merged parts, each
	// carrying the same two primary-key rows (the RMT idempotent-re-ingest state).
	for i := 0; i < 2; i++ {
		if err := sink.Add(ctx, ext); err != nil {
			t.Fatalf("sink add (pass %d): %v", i, err)
		}
		if err := sink.Flush(ctx); err != nil {
			t.Fatalf("sink flush (pass %d): %v", i, err)
		}
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	// ── ContractEventsRecent: LIMIT 1 BY primary key ────────────────────────
	recent, err := er.ContractEventsRecent(ctx, contractID, 100, chstore.ContractEventsCursor{})
	if err != nil {
		t.Fatalf("ContractEventsRecent: %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("ContractEventsRecent returned %d rows, want 2 — the LIMIT 1 BY dedup must collapse "+
			"the duplicate un-merged part (pre-fix: 4)", len(recent))
	}
	// Distinct events preserved: exactly one row per event_index (0 and 1).
	seen := map[uint32]int{}
	for _, e := range recent {
		seen[e.EventIndex]++
	}
	if seen[0] != 1 || seen[1] != 1 {
		t.Fatalf("ContractEventsRecent event_index multiplicity = %v, want each exactly once", seen)
	}

	// ── EventsByTx: FINAL ───────────────────────────────────────────────────
	byTx, err := er.EventsByTx(ctx, ledger, txHash)
	if err != nil {
		t.Fatalf("EventsByTx: %v", err)
	}
	if len(byTx) != 2 {
		t.Fatalf("EventsByTx returned %d rows, want 2 — FINAL must collapse the duplicate un-merged "+
			"part (pre-fix: 4)", len(byTx))
	}
	seenTx := map[uint32]int{}
	for _, e := range byTx {
		seenTx[e.EventIndex]++
	}
	if seenTx[0] != 1 || seenTx[1] != 1 {
		t.Fatalf("EventsByTx event_index multiplicity = %v, want each exactly once", seenTx)
	}
}

// TestClickHouseStringTopicPrefilterAdmitsPhoenixPool is the live-ClickHouse
// proof for the lake half of the string-topic prefilter.
//
// StreamContractEventsFiltered's topic[0] prefilter must not be
// `topic_0_sym IN (…)` alone: extract.go fills that column from
// `Topics[0].GetSym()` — Symbol ONLY — so it is EMPTY for every event whose
// topic[0] is an ScvString. Phoenix's factory publishes
// ("create","liquidity_pool") as two Strings, so every consumer that asks the
// lake for creationSym "create" (seed-protocol-contracts, and the -ch
// re-derive's gatedPrefilter walk) matched ZERO rows over a lake that holds
// those events from ledger 51,572,026 — the walk looked clean and admitted
// nothing.
//
// The row inserted below is the REAL r1 capture, byte-for-byte: contract id,
// both topic blobs, the body and the empty topic_0_sym are copied from
// test/fixtures/phoenix/factory-create/
// factory_2026-07-02_ledgers_63293663-63293708.jsonl (ledger 63,293,708, the
// factory's create of CBENABXP…). Only the ledger/tx coordinates are moved
// into this test's private range.
//
// The assertion runs the whole loop the defect broke — lake row → SQL
// prefilter → streamed event → decoder → identity gate — and ends on the one
// observation that cannot be faked: the registry's live-upsert hook receiving
// the announced pool. Reverting topic0Predicate to `topic_0_sym IN (…)` makes
// the stream return 1 row instead of 2 and seeds nothing.
func TestClickHouseStringTopicPrefilterAdmitsPhoenixPool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		lo = uint32(70_300_001)
		hi = uint32(70_300_003)
		// The real phoenix factory, and the pool the captured event
		// announces (body decoded: a single ScvAddress).
		factoryContract = phoenix.MainnetFactory
		announcedPool   = "CBENABXP6C4C7WG6KB7JQOTDS5GIIXF3IX3PIYNZFCDZDWUHITO2HZ4S"
		// Verbatim from the capture: ScvString("create"),
		// ScvString("liquidity_pool") and the ScvAddress body.
		realCreateTopic0 = "AAAADgAAAAZjcmVhdGUAAA=="
		realCreateTopic1 = "AAAADgAAAA5saXF1aWRpdHlfcG9vbAAA"
		realCreateBody   = "AAAAEgAAAAFI0Abv8Lgv2N5Qfpg6Y5dMhFy7Rfb0Ybkoh5Hah0Tdow=="
		// The factory's other captured event, ("Factory","Updated Config"):
		// also String topics, also empty topic_0_sym. It must NOT match.
		decoyTopic0 = "AAAADgAAAAdGYWN0b3J5AA=="
		decoyTopic1 = "AAAADgAAAA5VcGRhdGVkIENvbmZpZwAA"
		decoyBody   = "AAAAAQ=="

		txCreate = "1111111111111111111111111111111111111111111111111111111111111111"
		txSymbol = "2222222222222222222222222222222222222222222222222222222222222222"
		txDecoy  = "3333333333333333333333333333333333333333333333333333333333333333"
	)
	closeTime := time.Date(2026, 7, 2, 10, 24, 5, 0, time.UTC)

	row := func(ledger uint32, tx string, topics []string, topic0Sym, body string) chstore.ContractEventRow {
		return chstore.ContractEventRow{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: tx, OpIndex: 0, EventIndex: 0,
			ContractID: factoryContract, EventType: "contract",
			TopicCount: uint8(len(topics)), //nolint:gosec // two topics, fixed above.
			Topic0Sym:  topic0Sym, TopicsXDR: topics, DataXDR: body,
			OpArgsXDR: []string{}, InSuccessfulCall: 1,
		}
	}

	ext := chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{
			LedgerSeq: lo, CloseTime: closeTime, LedgerHash: "aa11aa11", PrevHash: "bb22bb22",
			ProtocolVersion: 23, TxCount: 3, OpCount: 3, SorobanEventCount: 3,
		},
		Events: []chstore.ContractEventRow{
			// (1) The real String-topic create: topic_0_sym EMPTY, exactly as
			// extract.go writes it.
			row(lo, txCreate, []string{realCreateTopic0, realCreateTopic1}, "", realCreateBody),
			// (2) A Symbol-topic "create" from the same emitter — the arm that
			// already worked. Pinned so widening the predicate cannot silently
			// drop the encoding every other gated source relies on.
			row(lo+1, txSymbol, []string{scval.MustEncodeSymbol("create"), realCreateTopic1},
				"create", realCreateBody),
			// (3) A different String topic[0] from the same emitter: must be
			// excluded, or the predicate has stopped filtering.
			row(lo+2, txDecoy, []string{decoyTopic0, decoyTopic1}, "", decoyBody),
		},
	}

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	if err := sink.Add(ctx, withEventTxs(ext)); err != nil {
		t.Fatalf("sink add: %v", err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush: %v", err)
	}

	// The prefilter as seed-protocol-contracts and gatedPrefilter issue it:
	// scope to the factory, ask for creationSym "create".
	var streamed []events.Event
	if err := chstore.StreamContractEventsFiltered(ctx, addr, lo, hi,
		[]string{factoryContract}, []string{phoenix.EventActionCreate}, nil,
		true, false, false,
		func(ev events.Event) error {
			streamed = append(streamed, ev)
			return nil
		}); err != nil {
		t.Fatalf("StreamContractEventsFiltered: %v", err)
	}

	if len(streamed) != 2 {
		t.Fatalf("prefilter on creationSym %q returned %d events, want 2 (the ScvString create AND "+
			"the ScvSymbol create; the ScvString one has an EMPTY topic_0_sym, so a "+
			"topic_0_sym-only predicate returns just 1 and the phoenix factory walk admits nothing)",
			phoenix.EventActionCreate, len(streamed))
	}
	byTx := map[string]events.Event{}
	for _, ev := range streamed {
		byTx[ev.TxHash] = ev
	}
	if _, ok := byTx[txCreate]; !ok {
		t.Errorf("the real ScvString ((\"create\",\"liquidity_pool\")) event did not survive the "+
			"prefilter; streamed tx hashes = %v", streamedTxHashes(byTx))
	}
	if _, ok := byTx[txSymbol]; !ok {
		t.Errorf("the ScvSymbol(\"create\") event did not survive the prefilter — widening the "+
			"predicate must not drop the topic_0_sym arm; streamed tx hashes = %v", streamedTxHashes(byTx))
	}
	if _, ok := byTx[txDecoy]; ok {
		t.Error("the (\"Factory\",\"Updated Config\") event survived a prefilter for \"create\" — " +
			"the predicate has stopped filtering")
	}

	// Close the loop: the streamed lake row must actually admit the pool.
	// Seeding is the only observable a decoder that merely RECOGNISES the
	// event cannot produce.
	var seeded []string
	dec := phoenix.NewDecoder(contractid.WithHook(func(child, factory string, ledger uint32) {
		if factory != factoryContract {
			t.Errorf("seeded %s with provenance factory %s, want %s", child, factory, factoryContract)
		}
		seeded = append(seeded, child)
	}))
	createEvent, ok := byTx[txCreate]
	if !ok {
		t.Fatal("cannot run the admission leg: the create event was filtered out above")
	}
	if !dec.Matches(createEvent) {
		t.Fatalf("phoenix decoder rejects the factory's create event as streamed from the lake")
	}
	if _, err := dec.Decode(createEvent); err != nil {
		t.Fatalf("decode streamed create event: %v", err)
	}
	if len(seeded) != 1 || seeded[0] != announcedPool {
		t.Fatalf("lake-streamed create event seeded %v, want exactly [%s] — the announced pool must "+
			"reach the identity gate for a factory-created pool's swaps to be attributed (F048)",
			seeded, announcedPool)
	}
}

// streamedTxHashes returns the streamed tx hashes, for a readable
// failure message.
func streamedTxHashes(m map[string]events.Event) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestDistinctTopicShapes_NonSymbolTopic0 is the proof on a real
// ClickHouse: events whose topic[0] is not a Symbol all carry an empty topic_0_sym,
// so keying on (contract, topic_0_sym) alone collapsed them into one shape and
// one exemplar. They must split on topics_xdr[1], topics_xdr[2] and arity,
// while Symbol shapes keep their (contract, topic_0_sym) identity.
func TestDistinctTopicShapes_NonSymbolTopic0(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	// A ledger range and contract nothing else in the suite writes.
	const lo, contract = uint32(145_000_000), "CGH807SHAPEFIXTURE"
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer ccancel()
		if err := dialClickHouse(t, cctx, "stellar").Exec(cctx, fmt.Sprintf(`ALTER TABLE stellar.contract_events
			DELETE WHERE contract_id = '%s' SETTINGS mutations_sync = 2`, contract)); err != nil {
			t.Errorf("purge shape fixture events: %v", err)
		}
	})

	type ev struct {
		sym    string
		topics []string
	}
	fixture := []ev{
		{"", []string{"T0", "T1a"}},
		{"", []string{"T0", "T1b"}},       // differs from the first only in topics_xdr[2]
		{"", []string{"T0", "T1a", "T2"}}, // differs only in arity
		{"swap", []string{"SWAP", "P1"}},  // Symbol shapes ignore topic[1] ...
		{"swap", []string{"SWAP", "P2"}},  // ... so these two are one shape
	}
	for i, e := range fixture {
		q := fmt.Sprintf(`INSERT INTO stellar.contract_events
			(ledger_seq, close_time, tx_hash, op_index, event_index, contract_id, event_type,
			 topic_count, topic_0_sym, topics_xdr, data_xdr, op_args_xdr, in_successful_call)
			VALUES (%d, '2026-08-01 00:00:00', 'gh807tx', 0, %d, '%s', 'contract', %d, '%s', ['%s'], 'D%d', [], 1)`,
			lo+uint32(i), i, contract, len(e.topics), e.sym, strings.Join(e.topics, "','"), i)
		if err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("insert fixture event %d: %v", i, err)
		}
	}

	shapes, err := chstore.DistinctTopicShapes(ctx, addr, lo, lo+uint32(len(fixture)), nil)
	if err != nil {
		t.Fatalf("DistinctTopicShapes: %v", err)
	}
	var got []string
	for _, s := range shapes {
		if s.ContractID != contract {
			continue
		}
		got = append(got, fmt.Sprintf("%s|%s|%d|%s", s.Topic0Sym, strings.Join(s.Topics, ","), s.Count, s.DataXDR))
	}
	// Sorted by count desc, then (topic_0_sym, t0, t1, arity); each exemplar is
	// its own shape's event, and the Symbol shape's is its latest (argMax by ledger).
	want := []string{
		"swap|SWAP,P2|2|D4",
		"|T0,T1a|1|D0",
		"|T0,T1a,T2|1|D2",
		"|T0,T1b|1|D1",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("shapes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
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

// TestDailySupplyFlowsPerKindSplit executes the daily supply-flows query
// against real ClickHouse: per-kind sums beside the net, i128 amounts above
// 2^63 intact, and a re-inserted flow collapsed by FINAL.
func TestDailySupplyFlowsPerKindSplit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	const contract = "CDAILYKINDSPLITXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"
	const insert = `INSERT INTO stellar.supply_flows
		(contract_id, ledger_seq, close_time, tx_hash, op_index, event_index, kind, amount)
		VALUES (?, ?, ?, ?, 0, ?, ?, toInt128(?))`
	day1 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 9, 3, 23, 59, 0, 0, time.UTC)
	rows := []struct {
		ledger uint32
		at     time.Time
		tx     string
		ev     uint32
		kind   string
		amount string
	}{
		{80_000_001, day1, "a1", 0, "mint", "18446744073709551621"}, // 2^64 + 5
		{80_000_001, day1, "a1", 1, "burn", "21"},
		{80_000_002, day1, "a2", 0, "clawback", "4"},
		{80_050_000, day2, "b1", 0, "mint", "100"},
		{80_050_000, day2, "b1", 0, "mint", "100"}, // same flow identity: FINAL collapses it
	}
	for _, r := range rows {
		if err := conn.Exec(ctx, insert, contract, r.ledger, r.at, r.tx, r.ev, r.kind, r.amount); err != nil {
			t.Fatalf("insert %s: %v", r.kind, err)
		}
	}

	sr, err := chstore.NewSupplyReader(ctx, addr)
	if err != nil {
		t.Fatalf("new supply reader: %v", err)
	}
	t.Cleanup(func() { _ = sr.Close() })
	got, err := sr.DailySupplyFlowsForContracts(ctx, []string{contract})
	if err != nil {
		t.Fatalf("DailySupplyFlowsForContracts: %v", err)
	}
	type day struct{ date, mint, burn, clawback, net string }
	want := []day{
		{"2026-09-01", "18446744073709551621", "21", "4", "18446744073709551596"},
		{"2026-09-03", "100", "0", "0", "100"},
	}
	wantFlows := []uint64{3, 1}
	if len(got) != len(want) {
		t.Fatalf("got %d days, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		gd := day{g.Day.Format(time.DateOnly), g.Mint.String(), g.Burn.String(), g.Clawback.String(), g.Net.String()}
		if gd != w || g.Flows != wantFlows[i] {
			t.Errorf("day %d = %+v flows=%d, want %+v flows=%d", i, gd, g.Flows, w, wantFlows[i])
		}
	}
}

// TestSupplyObservedAt_StampsLedgerCloseTimeNotWallClock is the M4-callers
// end-to-end proof against a REAL ClickHouse lake. The bug: the supply-snapshot
// ledger resolvers (internal/ops/supply/supply.go::resolveSnapshotLedger and
// cmd/stellarindex-aggregator/main.go::supplyAggregatorLedgers.LatestKnownLedger)
// stamped a snapshot's ObservedAt with time.Now().UTC() instead of the chosen
// ledger's real close time — so a re-derived HISTORICAL supply snapshot carried
// the wall-clock write-time, corrupting point-in-time supply/observation
// queries (the operator re-derives supply constantly).
//
// The fix resolves the ledger's real close_time from stellar.ledgers via
// *clickhouse.ExplorerReader.CloseTimeForLedger and stamps THAT. This test
// seeds one stellar.ledgers row whose close_time is ~2.5y stale, resolves it
// through the production reader, feeds it through the production XLM computer
// exactly as the resolver→computer path does, and asserts the snapshot's
// ObservedAt equals the seeded close time — NOT ≈now. Callers that skipped the lake
// discarded this resolved value entirely (they never read the lake), so this
// stale, non-wall-clock ObservedAt is precisely what they could not produce.
func TestSupplyObservedAt_StampsLedgerCloseTimeNotWallClock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// Isolated high ledger_seq + a deliberately stale close time (~2.5y before
	// this test runs) so a wall-clock stamp is unmistakable and no other test's
	// rows can collide.
	const ledger = uint32(210_000_007)
	closeTime := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)

	ext := chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{
			LedgerSeq: ledger, CloseTime: closeTime, LedgerHash: "a1b2", PrevHash: "c3d4",
			ProtocolVersion: 22, BucketListHash: "e5f6",
			TxCount: 1, OpCount: 1, SorobanEventCount: 0,
			TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		},
	}
	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	// Left behind, this row owns the global max ledger_seq with a 2024 close time,
	// which empties clickhouse_storage_test's NetworkThroughput tip window.
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer ccancel()
		conn := dialClickHouse(t, cctx, "stellar")
		if err := conn.Exec(cctx, fmt.Sprintf(`ALTER TABLE stellar.ledgers DELETE
			WHERE ledger_seq = %d SETTINGS mutations_sync = 2`, ledger)); err != nil {
			t.Errorf("purge supply fixture ledger: %v", err)
		}
	})
	if err := sink.Add(ctx, ext); err != nil {
		t.Fatalf("sink add: %v", err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush: %v", err)
	}

	reader, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	// 1. The production close-time read returns the ledger's REAL close time.
	got, found, err := reader.CloseTimeForLedger(ctx, ledger)
	if err != nil {
		t.Fatalf("CloseTimeForLedger: %v", err)
	}
	if !found {
		t.Fatalf("CloseTimeForLedger(%d) found=false — seeded ledger row not read back", ledger)
	}
	if !got.Equal(closeTime) {
		t.Fatalf("CloseTimeForLedger = %v, want the seeded close time %v", got, closeTime)
	}
	if time.Since(got) < 365*24*time.Hour {
		t.Fatalf("resolved close time %v is suspiciously close to now — reader returned wall-clock, not the lake close time", got)
	}

	// 2. The resolver→computer path stamps that close time onto the snapshot's
	//    ObservedAt (the XLM total is a constant, so a nil reserve reader is
	//    fine — this mirrors internal/supply/xlm_test.go's fixture).
	computer, err := supply.NewXLMComputer(nil, nil)
	if err != nil {
		t.Fatalf("NewXLMComputer: %v", err)
	}
	snap, err := computer.Compute(ctx, ledger, got)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if !snap.ObservedAt.Equal(closeTime) {
		t.Errorf("snapshot ObservedAt = %v, want the ledger close time %v (wall-clock stamp regression)", snap.ObservedAt, closeTime)
	}
	if time.Since(snap.ObservedAt) < 365*24*time.Hour {
		t.Errorf("snapshot ObservedAt %v is suspiciously close to now — the wall-clock M4-callers bug is back", snap.ObservedAt)
	}
}
