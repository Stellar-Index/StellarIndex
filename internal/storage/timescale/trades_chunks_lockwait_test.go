// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
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
// These pin the three properties the fix rests on, in the order they
// matter: the request is bounded, a refusal is RETRIED rather than
// surrendered to (a re-compress that gave up leaves 160 GB decompressed),
// and an error that is NOT a lock refusal is never retried.

// lockTimeoutErr is a driver error shaped like pgx's *pgconn.PgError:
// [isLockNotAvailable] matches on SQLSTATE via errors.As, never on the
// message, so a server with a localized lc_messages still retries.
type lockTimeoutErr struct{ code string }

func (e lockTimeoutErr) Error() string {
	return "ERROR: canceling statement due to lock timeout (SQLSTATE " + e.code + ")"
}
func (e lockTimeoutErr) SQLState() string { return e.code }

// lockRefused scripts one attempt that loses the lock race: the
// transaction's `SET LOCAL lock_timeout` succeeds, the statement it
// bounds comes back 55P03.
func lockRefused() []scriptedResult {
	return []scriptedResult{{}, {err: lockTimeoutErr{code: pgLockNotAvailable}}}
}

// lockGranted scripts one attempt that gets the lock.
func lockGranted() []scriptedResult { return []scriptedResult{{}, {}} }

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
			if len(got) != 2 {
				t.Fatalf("issued %d statements, want 2 (the lock bound + the statement):\n%s", len(got), strings.Join(got, "\n"))
			}
			// The bound is the policy's own value, not a literal that can
			// drift away from it.
			want := "SET LOCAL lock_timeout = '5000ms'"
			if tc.policy.wait != 5*time.Second {
				t.Fatalf("policy wait = %s; this assertion was written for 5s", tc.policy.wait)
			}
			if got[0] != want {
				t.Errorf("statement 0 = %q, want %q", got[0], want)
			}
			if !strings.Contains(got[1], tc.stmt) {
				t.Errorf("statement 1 = %q, want %s", got[1], tc.stmt)
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
	err := store.execUnderBoundedLockWait(context.Background(), fast, tradesChunkCompress, "_timescaledb_internal", "_hyper_1_10_chunk")
	if err != nil {
		t.Fatalf("err = %v, want the fourth attempt to succeed", err)
	}
	if n := countStatements(conn.statements(), "compress_chunk("); n != 4 {
		t.Errorf("issued %d compress attempts, want 4 (three refused, one granted)", n)
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
	err := store.execUnderBoundedLockWait(context.Background(), stubborn, tradesChunkDecompress, "_timescaledb_internal", "_hyper_1_10_chunk")
	if err == nil {
		t.Fatal("err = nil, want the exhausted budget to surface")
	}
	if !isLockNotAvailable(err) {
		t.Errorf("err = %v, want it to still wrap the 55P03 so callers can classify it", err)
	}
	for _, want := range []string{"gave up waiting", "pg_blocking_pids()", "runbooks/pg-lock-convoy.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want containing %q", err, want)
		}
	}
	if n := countStatements(conn.statements(), "decompress_chunk("); n < 2 {
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
	store, conn := newScriptedStore(t, scriptedResult{}, scriptedResult{err: boom})

	fast := lockWaitPolicy{wait: 5 * time.Second, drain: time.Millisecond, budget: time.Minute}
	err := store.execUnderBoundedLockWait(context.Background(), fast, tradesChunkCompress, "_timescaledb_internal", "_hyper_1_10_chunk")
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
	err := store.execUnderBoundedLockWait(ctx, slow, tradesChunkDecompress, "_timescaledb_internal", "_hyper_1_10_chunk")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("took %s to notice the cancellation; it waited out the drain", elapsed)
	}
	if n := countStatements(conn.statements(), "decompress_chunk("); n != 1 {
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
