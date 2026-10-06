// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"
)

// ─── the bounded exclusive-lock wait (2026-09-10 convoy) ─────────────────
//
// r1, 00:12–00:31 UTC: a restamp's decompress_chunk could not have its
// AccessExclusiveLock because a cold-start aggregator read held
// AccessShareLock on `trades`, so it QUEUED — and PostgreSQL queues every
// later request for that object behind a PENDING exclusive one. Three
// postgres_exporter scrapes (917 s), two of the restamp's own UPDATEs
// (1,164 s) and the size watcher (904 s) piled up behind it; the exporter
// being in that list is what made the alerting layer cascade-blind.
//
// These pin the properties the fix rests on: every request is bounded, a
// refusal is RETRIED rather than surrendered to (a re-compress that gave
// up leaves 160 GB decompressed), an error that is NOT a lock refusal is
// never retried, and the budget is charged for waiting, never for work.

// lockTimeoutErr is a driver error shaped like pgx's *pgconn.PgError:
// [isLockNotAvailable] matches on SQLSTATE via errors.As, never on the
// message, so a server with a localized lc_messages still retries.
type lockTimeoutErr struct{ code string }

func (e lockTimeoutErr) Error() string {
	return "ERROR: canceling statement due to lock timeout (SQLSTATE " + e.code + ")"
}
func (e lockTimeoutErr) SQLState() string { return e.code }

// testChunk is the chunk every test here addresses.
var testChunk = TradeChunk{Schema: "_timescaledb_internal", Name: "_hyper_1_10_chunk", Compressed: true}

// noLongHolder is the long-holder check finding nobody.
func noLongHolder() scriptedResult {
	return scriptedResult{cols: []string{"pid", "mode", "relation", "age"}}
}

// longHolder is the long-holder check finding an 18-minute reader.
func longHolder() scriptedResult {
	return scriptedResult{
		cols: []string{"pid", "mode", "relation", "age"},
		rows: [][]driver.Value{{int64(4242), "AccessShareLock", "_timescaledb_internal._hyper_1_10_chunk", int64(1100)}},
	}
}

// lockRefused scripts one attempt that loses the race for its opening
// lock: the check, the bound and the `trades` lock succeed, the chunk's
// LOCK TABLE comes back 55P03 and the statement never runs.
func lockRefused() []scriptedResult {
	return []scriptedResult{noLongHolder(), {}, {}, {err: lockTimeoutErr{code: pgLockNotAvailable}}}
}

// workThenRefused scripts one attempt that got its opening locks, worked
// for d, then lost the race for the lock the statement takes at its end.
func workThenRefused(d time.Duration) []scriptedResult {
	return []scriptedResult{noLongHolder(), {}, {}, {}, {delay: d, err: lockTimeoutErr{code: pgLockNotAvailable}}}
}

// lockGranted scripts one attempt that gets every lock.
func lockGranted() []scriptedResult {
	return []scriptedResult{noLongHolder(), {}, {}, {}, {}}
}

func countStatements(stmts []string, substr string) int {
	n := 0
	for _, s := range stmts {
		if strings.Contains(s, substr) {
			n++
		}
	}
	return n
}

// TestDecompressTradesChunk_BoundsThePendingLockRequest is the incident's
// direct lesson: the statement that convoyed the database must never be
// issued with an unbounded lock wait. The bound is the PRODUCTION one and
// it is transaction-LOCAL — a session-level `SET` would ride the pooled
// connection into every later statement that landed on it.
func TestDecompressTradesChunk_BoundsThePendingLockRequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		call   func(*Store, TradeChunk) error
		stmt   string
		policy lockWaitPolicy
	}{
		{
			name:   "decompress",
			call:   func(s *Store, c TradeChunk) error { return s.DecompressTradesChunk(context.Background(), c) },
			stmt:   "decompress_chunk(",
			policy: tradesChunkDecompressLock,
		},
		{
			name:   "compress",
			call:   func(s *Store, c TradeChunk) error { return s.CompressTradesChunk(context.Background(), c) },
			stmt:   "compress_chunk(",
			policy: tradesChunkCompressLock,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, conn := newScriptedStore(t, lockGranted()...)
			c := TradeChunk{Schema: "_timescaledb_internal", Name: "_hyper_1_10_chunk", Compressed: true}
			if err := tc.call(store, c); err != nil {
				t.Fatalf("err = %v", err)
			}
			got := conn.statements()
			if len(got) != 5 {
				t.Fatalf("issued %d statements, want 5 (long-holder check, lock bound, two opening locks, the statement):\n%s", len(got), strings.Join(got, "\n"))
			}
			if !strings.Contains(got[0], "pg_locks") {
				t.Errorf("statement 0 = %q, want the long-holder check", got[0])
			}
			// The bound is the policy's own value, not a literal that can
			// drift away from it, and it precedes every lock request.
			if tc.policy.wait != 5*time.Second {
				t.Fatalf("policy wait = %s; this assertion was written for 5s", tc.policy.wait)
			}
			for i, want := range []string{
				"SET LOCAL lock_timeout = '5000ms'",
				"LOCK TABLE ONLY trades IN ACCESS SHARE MODE",
				`LOCK TABLE ONLY "_timescaledb_internal"."_hyper_1_10_chunk" IN EXCLUSIVE MODE`,
			} {
				if got[i+1] != want {
					t.Errorf("statement %d = %q, want %q", i+1, got[i+1], want)
				}
			}
			if !strings.Contains(got[4], tc.stmt) {
				t.Errorf("statement 4 = %q, want %s", got[4], tc.stmt)
			}
			// Transaction-scoped: the driver saw a COMMIT, so POSTGRES
			// unwinds the GUC rather than the pool inheriting it.
			if !conn.committed() {
				t.Error("the bounded statement did not run in its own transaction; a session-level SET would leak onto the pooled connection")
			}
		})
	}
}

// TestExecUnderBoundedLockWait_RetriesARefusedLock: a refusal is not a
// failure. The whole point of withdrawing after 5 s is to come back — a
// re-compress that gave up on the first 55P03 would leave the 160 GB
// chunk decompressed, which is strictly worse than the convoy.
func TestExecUnderBoundedLockWait_RetriesARefusedLock(t *testing.T) {
	t.Parallel()
	var script []scriptedResult
	for range 3 {
		script = append(script, lockRefused()...)
	}
	script = append(script, lockGranted()...)
	store, conn := newScriptedStore(t, script...)

	fast := lockWaitPolicy{wait: 5 * time.Second, drain: time.Millisecond, budget: time.Minute}
	err := store.execUnderBoundedLockWait(context.Background(), context.Background(), fast, tradesChunkCompress, testChunk)
	if err != nil {
		t.Fatalf("err = %v, want the fourth attempt to succeed", err)
	}
	if n := countStatements(conn.statements(), "IN EXCLUSIVE MODE"); n != 4 {
		t.Errorf("made %d attempts, want 4 (three refused, one granted)", n)
	}
	if n := countStatements(conn.statements(), "compress_chunk("); n != 1 {
		t.Errorf("issued compress_chunk %d times, want 1: a refused opening lock never starts the work", n)
	}
	// EVERY attempt is bounded, not just the first: an unbounded retry
	// would park exactly the pending request the first one withdrew.
	if n := countStatements(conn.statements(), "SET LOCAL lock_timeout"); n != 4 {
		t.Errorf("%d of the 4 attempts bounded their lock wait, want all 4", n)
	}
}

// TestExecUnderBoundedLockWait_GivesUpNamingTheConvoy: the budget is
// finite, and exhausting it produces an error an operator can act on —
// naming the lock, the effort spent and the runbook — rather than a bare
// 55P03 that reads like a Postgres hiccup.
func TestExecUnderBoundedLockWait_GivesUpNamingTheConvoy(t *testing.T) {
	t.Parallel()
	var script []scriptedResult
	for range 40 { // far more than the budget below can consume
		script = append(script, lockRefused()...)
	}
	store, conn := newScriptedStore(t, script...)

	stubborn := lockWaitPolicy{wait: 5 * time.Second, drain: 5 * time.Millisecond, budget: 40 * time.Millisecond}
	err := store.execUnderBoundedLockWait(context.Background(), context.Background(), stubborn, tradesChunkDecompress, testChunk)
	if err == nil {
		t.Fatal("err = nil, want the exhausted budget to surface")
	}
	if !isLockNotAvailable(err) {
		t.Errorf("err = %v, want it to still wrap the 55P03 so callers can classify it", err)
	}
	for _, want := range []string{"gave up on the chunk's locks", "refused before any work", "pg_blocking_pids()", "runbooks/postgres.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want containing %q", err, want)
		}
	}
	if n := countStatements(conn.statements(), "IN EXCLUSIVE MODE"); n < 2 {
		t.Errorf("made %d attempt(s) before giving up, want it to retry at least once", n)
	}
}

// TestExecUnderBoundedLockWait_DoesNotRetryOtherErrors: the retry is for
// lock CONTENTION and nothing else. Retrying an out-of-disk compress for
// 90 minutes would bury the real failure and burn the budget that exists
// to get the chunk closed.
func TestExecUnderBoundedLockWait_DoesNotRetryOtherErrors(t *testing.T) {
	t.Parallel()
	boom := errors.New("compress_chunk: could not extend file: No space left on device")
	store, conn := newScriptedStore(t, noLongHolder(), scriptedResult{}, scriptedResult{}, scriptedResult{}, scriptedResult{err: boom})

	fast := lockWaitPolicy{wait: 5 * time.Second, drain: time.Millisecond, budget: time.Minute}
	err := store.execUnderBoundedLockWait(context.Background(), context.Background(), fast, tradesChunkCompress, testChunk)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the driver's error unchanged", err)
	}
	if n := countStatements(conn.statements(), "compress_chunk("); n != 1 {
		t.Errorf("made %d attempts, want exactly 1 — a non-lock error is not retried", n)
	}
}

// TestExecUnderBoundedLockWait_StopsOnCancellation: the decompress runs on
// the caller's context, so a SIGTERM under run-heavy-job.sh must end the
// retry loop rather than hold the process open through its stop grace.
// (The re-compress runs on a context detached from that cancellation —
// [Store.recompressTradesChunk] — which is what keeps the chunk closable
// on the way out.)
func TestExecUnderBoundedLockWait_StopsOnCancellation(t *testing.T) {
	t.Parallel()
	var script []scriptedResult
	for range 4 {
		script = append(script, lockRefused()...)
	}
	store, conn := newScriptedStore(t, script...)

	// The SIGTERM lands while the run is between attempts, waiting out
	// the drain — the window the loop spends most of its time in.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slow := lockWaitPolicy{wait: 5 * time.Second, drain: time.Hour, budget: 24 * time.Hour}
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	err := store.execUnderBoundedLockWait(ctx, ctx, slow, tradesChunkDecompress, testChunk)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("took %s to notice the cancellation; it waited out the drain", elapsed)
	}
	if n := countStatements(conn.statements(), "IN EXCLUSIVE MODE"); n != 1 {
		t.Errorf("made %d attempts on a cancelled context, want 1", n)
	}
}

// TestTradesChunkLockPolicies_CompressIsTheOneThatMayNotGiveUp pins the
// asymmetry the whole design rests on, because it is the property a future
// "let's make these consistent" edit would quietly destroy:
//
//   - a decompress that never runs changed NOTHING — the chunk is still
//     compressed and a rerun resumes at it — so its budget is the smaller
//     one;
//   - a compress that never runs leaves a 160 GB chunk open on a pool with
//     4.69 TB free, with the compression policy paused, which is the exact
//     state this tool exists to avoid. Its budget is much larger.
//
// The per-attempt wait is the same for both and stays well under the
// 30 s postgres_exporter scrape timeout that the convoy blew through —
// that is what keeps the alerting layer alive while either statement asks.
func TestTradesChunkLockPolicies_CompressIsTheOneThatMayNotGiveUp(t *testing.T) {
	t.Parallel()
	const exporterScrapeTimeout = 30 * time.Second
	for name, p := range map[string]lockWaitPolicy{
		"decompress": tradesChunkDecompressLock,
		"compress":   tradesChunkCompressLock,
	} {
		if p.wait <= 0 || p.wait >= exporterScrapeTimeout {
			t.Errorf("%s wait = %s, want a positive bound well under the %s exporter scrape timeout", name, p.wait, exporterScrapeTimeout)
		}
		if p.drain <= p.wait {
			t.Errorf("%s drain = %s, want more clear air than the %s it spends asking, so the queue behind it drains", name, p.drain, p.wait)
		}
		if p.budget <= p.drain {
			t.Errorf("%s budget = %s, want room for more than one attempt", name, p.budget)
		}
	}
	if tradesChunkCompressLock.budget <= tradesChunkDecompressLock.budget {
		t.Errorf("compress budget %s <= decompress budget %s — giving up on the re-compress leaves the chunk DECOMPRESSED, so it must be the one that tries hardest",
			tradesChunkCompressLock.budget, tradesChunkDecompressLock.budget)
	}
}

// TestExecUnderBoundedLockWait_ChargesWaitingNotWorking: the budget is for
// WAITING. An attempt that worked for most of the budget and was refused
// at its end-stage lock must not leave the cheap acquire retries after it
// with no budget, which is what a wall-clock deadline set before attempt 1
// does to a 90-minute decompress.
func TestExecUnderBoundedLockWait_ChargesWaitingNotWorking(t *testing.T) {
	t.Parallel()
	script := workThenRefused(180 * time.Millisecond)
	for range 12 {
		script = append(script, lockRefused()...)
	}
	script = append(script, lockGranted()...)
	store, _ := newScriptedStore(t, script...)

	// 180 ms of work + 12 refusals x 20 ms of drain = 420 ms of wall time,
	// past the 400 ms budget; the waiting alone is about 265 ms.
	p := lockWaitPolicy{wait: 5 * time.Millisecond, drain: 20 * time.Millisecond, budget: 400 * time.Millisecond}
	if err := store.execUnderBoundedLockWait(context.Background(), context.Background(), p, tradesChunkDecompress, testChunk); err != nil {
		t.Fatalf("err = %v, want the granted attempt reached: the work time was charged to the wait budget", err)
	}
}

// TestExecUnderBoundedLockWait_LateRefusalThatDoesNotFitGivesUpAtOnce: a
// refusal inside the statement threw its work away, and repeating that
// work is only started when it fits what is left of the budget. The error
// reports the real elapsed time and the work lost, not the budget.
func TestExecUnderBoundedLockWait_LateRefusalThatDoesNotFitGivesUpAtOnce(t *testing.T) {
	t.Parallel()
	script := workThenRefused(80 * time.Millisecond)
	script = append(script, lockGranted()...)
	store, conn := newScriptedStore(t, script...)

	p := lockWaitPolicy{wait: 5 * time.Millisecond, drain: 20 * time.Millisecond, budget: 100 * time.Millisecond}
	err := store.execUnderBoundedLockWait(context.Background(), context.Background(), p, tradesChunkCompress, testChunk)
	if err == nil || !isLockNotAvailable(err) {
		t.Fatalf("err = %v, want the late 55P03 surfaced at once", err)
	}
	if n := countStatements(conn.statements(), "compress_chunk("); n != 1 {
		t.Errorf("made %d attempts, want 1: an 80 ms attempt does not fit the 20 ms left", n)
	}
	for _, want := range []string{"1 attempt(s)", "1 refused inside the statement", "of work lost", "does not fit", "pg_blocking_pids()"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want containing %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "over 100ms") {
		t.Errorf("err = %v reports the budget as the time tried", err)
	}
}

// TestExecUnderBoundedLockWait_NoRepeatedWorkOnceTheCallerHasGone: the
// re-compress runs on a context detached from the caller's, so after a
// SIGTERM it still retries a cheap refusal (that is how the chunk gets
// closed) but never starts the work over (that is what the SIGKILL would
// cut off).
func TestExecUnderBoundedLockWait_NoRepeatedWorkOnceTheCallerHasGone(t *testing.T) {
	t.Parallel()
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	p := lockWaitPolicy{wait: 5 * time.Millisecond, drain: time.Millisecond, budget: time.Minute}

	t.Run("cheap refusal is retried", func(t *testing.T) {
		t.Parallel()
		store, _ := newScriptedStore(t, append(lockRefused(), lockGranted()...)...)
		if err := store.execUnderBoundedLockWait(context.Background(), gone, p, tradesChunkCompress, testChunk); err != nil {
			t.Fatalf("err = %v, want the cheap refusal retried after the caller stopped", err)
		}
	})
	t.Run("late refusal is not", func(t *testing.T) {
		t.Parallel()
		store, conn := newScriptedStore(t, append(workThenRefused(time.Millisecond), lockGranted()...)...)
		err := store.execUnderBoundedLockWait(context.Background(), gone, p, tradesChunkCompress, testChunk)
		if err == nil || !strings.Contains(err.Error(), "caller has stopped") {
			t.Fatalf("err = %v, want a give-up naming the stopped caller", err)
		}
		if n := countStatements(conn.statements(), "compress_chunk("); n != 1 {
			t.Errorf("made %d attempts, want 1", n)
		}
	})
}

// TestExecUnderBoundedLockWait_WaitsOutALongHolderBeforeWorking: a
// transaction that has held a lock on the chunk for longer than
// [longLockHolderAge] would block the end-stage request and throw the
// work away, so no attempt starts until it is gone — and that wait is
// charged to the budget, with the holder named when the budget runs out.
func TestExecUnderBoundedLockWait_WaitsOutALongHolderBeforeWorking(t *testing.T) {
	t.Parallel()
	p := lockWaitPolicy{wait: 5 * time.Millisecond, drain: 5 * time.Millisecond, budget: time.Minute}

	t.Run("then works", func(t *testing.T) {
		t.Parallel()
		store, conn := newScriptedStore(t, append([]scriptedResult{longHolder(), longHolder()}, lockGranted()...)...)
		if err := store.execUnderBoundedLockWait(context.Background(), context.Background(), p, tradesChunkDecompress, testChunk); err != nil {
			t.Fatalf("err = %v", err)
		}
		got := conn.statements()
		if n := countStatements(got, "pg_locks"); n != 3 {
			t.Errorf("ran the long-holder check %d times, want 3", n)
		}
		if n := countStatements(got, "LOCK TABLE"); n != 2 {
			t.Errorf("issued %d LOCK TABLE, want 2: none while the holder was there", n)
		}
	})
	t.Run("budget spent", func(t *testing.T) {
		t.Parallel()
		var script []scriptedResult
		for range 40 {
			script = append(script, longHolder())
		}
		store, conn := newScriptedStore(t, script...)
		short := lockWaitPolicy{wait: 5 * time.Millisecond, drain: 5 * time.Millisecond, budget: 40 * time.Millisecond}
		err := store.execUnderBoundedLockWait(context.Background(), context.Background(), short, tradesChunkDecompress, testChunk)
		if !errors.Is(err, errLongLockHolder) {
			t.Fatalf("err = %v, want errLongLockHolder", err)
		}
		if !strings.Contains(err.Error(), "pid 4242") {
			t.Errorf("err = %v, want the holder named", err)
		}
		if n := countStatements(conn.statements(), "LOCK TABLE"); n != 0 {
			t.Errorf("issued %d LOCK TABLE while a long holder was present, want 0", n)
		}
	})
}

// TestExecUnderBoundedLockWait_ChargesARefusedOpeningLock: the wait that
// ended in a refused LOCK TABLE is charged, not only the drain after it.
func TestExecUnderBoundedLockWait_ChargesARefusedOpeningLock(t *testing.T) {
	t.Parallel()
	var script []scriptedResult
	for range 40 {
		r := lockRefused()
		r[len(r)-1].delay = 30 * time.Millisecond
		script = append(script, r...)
	}
	store, conn := newScriptedStore(t, script...)

	p := lockWaitPolicy{wait: 30 * time.Millisecond, drain: time.Millisecond, budget: 100 * time.Millisecond}
	if err := store.execUnderBoundedLockWait(context.Background(), context.Background(), p, tradesChunkDecompress, testChunk); err == nil {
		t.Fatal("err = nil, want the budget spent")
	}
	if n := countStatements(conn.statements(), "IN EXCLUSIVE MODE"); n > 4 {
		t.Errorf("made %d attempts of 30 ms waiting against a 100 ms budget, want at most 4", n)
	}
}

func TestLongLockHolderSelectExcludesCancellableAutovacuum(t *testing.T) {
	for _, want := range []string{"a.backend_type = 'autovacuum worker'", "(to prevent wraparound)"} {
		if !strings.Contains(longLockHolderSelect, want) {
			t.Errorf("longLockHolderSelect missing %q", want)
		}
	}
}
