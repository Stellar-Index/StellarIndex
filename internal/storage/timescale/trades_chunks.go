// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ─── `trades` chunk primitives for the chunk-wise usd_volume restamp ───
//
// An UPDATE into a COMPRESSED Timescale chunk is serviced by
// decompressing it inside the transaction, and none of the restamp's
// join clauses can become a scan key on a `segmentby` / `orderby`
// column, so what gets decompressed is the WHOLE chunk. Measured on
// production 2026-09-03 running `usd-volume-restamp -tier xlm-base
// -write` over [2026-01-01, 2026-07-21]: all 90 `trades` chunks in the
// window were compressed (policy: compress_after 7 days), one 2,000-row
// batch took over 14 minutes, and the run sustained ~1,574 rows/min
// against a 28.6M-row write set — a 12-day job. The dry run is
// read-only and never showed it.
//
// Escaping that needs BOTH halves. This file is one of them; the other
// is the statement's own `ts` bound, without which the UPDATE names the
// hypertable and every one of its 258 compressed chunks is a result
// relation whatever this file decompressed — see
// [Store.applyXLMBaseRestampBatch] and the "Chunk mode" section of
// docs/operations/usd-volume-rederive-2026-08.md.
//
// The remedy is to invert the order: decompress the chunk ONCE, run the
// same restamp inside it (a plain heap UPDATE), and compress it again.
// These are the store-side primitives that mode is built from. They are
// deliberately thin — one statement each — so the scripted-driver tests
// can pin the exact SQL, and so the one non-trivial piece,
// [Store.RestampTradesChunk], is nothing but the ORDER of those statements
// plus the rule that a chunk is never left decompressed on a failure.

// TradeChunk is one `trades` hypertable chunk as the chunk-wise restamp
// sees it: its identity, its time range and its two sizes.
type TradeChunk struct {
	Schema string
	Name   string
	// [RangeStart, RangeEnd) is the chunk's time slice, UTC.
	RangeStart time.Time
	RangeEnd   time.Time
	// Compressed reports whether the chunk is compressed right now.
	Compressed bool
	// UncompressedBytes is what the chunk occupies decompressed: Timescale's
	// recorded pre-compression size for a compressed chunk, the current
	// on-disk size for one that is not. It is the number the free-space
	// pre-flight sizes against.
	UncompressedBytes int64
	// CompressedBytes is the post-compression size; 0 for an uncompressed
	// chunk.
	CompressedBytes int64
}

// String is the schema-qualified chunk name, as show_chunks prints it.
func (c TradeChunk) String() string { return c.Schema + "." + c.Name }

// tradesChunksInRangeSelect lists the `trades` chunks whose range
// intersects [$1, $2), oldest first. The two sizes come from
// chunk_compression_stats (the recorded before/after totals of a
// compressed chunk) with chunks_detailed_size as the fallback for a chunk
// that is not compressed.
const tradesChunksInRangeSelect = `
	SELECT c.chunk_schema, c.chunk_name, c.range_start, c.range_end, c.is_compressed,
	       COALESCE(s.before_compression_total_bytes, d.total_bytes, 0)::bigint AS uncompressed_bytes,
	       COALESCE(s.after_compression_total_bytes, 0)::bigint                 AS compressed_bytes
	  FROM timescaledb_information.chunks c
	  LEFT JOIN chunk_compression_stats('trades') s
	         ON s.chunk_schema = c.chunk_schema AND s.chunk_name = c.chunk_name
	  LEFT JOIN chunks_detailed_size('trades') d
	         ON d.chunk_schema = c.chunk_schema AND d.chunk_name = c.chunk_name
	 WHERE c.hypertable_name = 'trades'
	   AND c.range_start < $2
	   AND c.range_end > $1
	 ORDER BY c.range_start
`

// TradesChunksInRange returns every `trades` chunk intersecting [from,
// to), in range order. Read-only: it touches the catalog, never a chunk.
func (s *Store) TradesChunksInRange(ctx context.Context, from, to time.Time) ([]TradeChunk, error) {
	rows, err := s.db.QueryContext(ctx, tradesChunksInRangeSelect, from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("timescale: list trades chunks [%s, %s): %w",
			from.Format(time.RFC3339), to.Format(time.RFC3339), err)
	}
	defer func() { _ = rows.Close() }()
	var out []TradeChunk
	for rows.Next() {
		var c TradeChunk
		if err := rows.Scan(&c.Schema, &c.Name, &c.RangeStart, &c.RangeEnd, &c.Compressed,
			&c.UncompressedBytes, &c.CompressedBytes); err != nil {
			return nil, fmt.Errorf("timescale: scan trades chunk: %w", err)
		}
		c.RangeStart, c.RangeEnd = c.RangeStart.UTC(), c.RangeEnd.UTC()
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: list trades chunks: %w", err)
	}
	return out, nil
}

// The chunk regclass is assembled SERVER-SIDE from the two identifiers
// (format('%I.%I') quotes them), so neither ever reaches the SQL text.
const (
	tradesChunkDecompress = `SELECT decompress_chunk(format('%I.%I', $1::text, $2::text)::regclass, if_compressed => true)`
	tradesChunkCompress   = `SELECT compress_chunk(format('%I.%I', $1::text, $2::text)::regclass, if_not_compressed => true)`
	tradesChunkBytes      = `SELECT total_bytes::bigint FROM chunks_detailed_size('trades') WHERE chunk_schema = $1 AND chunk_name = $2`
)

// DecompressTradesChunk decompresses one chunk; a no-op if it already is.
// Its locks are taken under a bounded wait and retried — see the convoy
// note below.
func (s *Store) DecompressTradesChunk(ctx context.Context, c TradeChunk) error {
	if err := s.execUnderBoundedLockWait(ctx, ctx, tradesChunkDecompressLock, tradesChunkDecompress, c); err != nil {
		return fmt.Errorf("timescale: decompress chunk %s: %w", c, err)
	}
	return nil
}

// CompressTradesChunk compresses one chunk; a no-op if it already is. Its
// locks are taken under the same per-request bound as the decompress but
// with a far larger budget, because giving up here LEAVES THE CHUNK
// DECOMPRESSED — see the convoy note below.
func (s *Store) CompressTradesChunk(ctx context.Context, c TradeChunk) error {
	return s.compressTradesChunk(ctx, ctx, c)
}

// compressTradesChunk is [Store.CompressTradesChunk] with the caller's own
// context (live) separate from the one the statement runs on, which the
// re-compress detaches from cancellation.
func (s *Store) compressTradesChunk(ctx, live context.Context, c TradeChunk) error {
	if err := s.execUnderBoundedLockWait(ctx, live, tradesChunkCompressLock, tradesChunkCompress, c); err != nil {
		return fmt.Errorf("timescale: compress chunk %s: %w", c, err)
	}
	return nil
}

// ─── the lock convoy: why every WAIT is bounded and the WORK is not ──────
//
// PRODUCTION INCIDENT 2026-09-10, 00:12–00:31 UTC (r1). A deploy restarted
// stellarindex-aggregator; its cold-start VWAP alias-map aggregation
// spilled to disk (wait_event = IO/BufFileRead) and held AccessShareLock
// on `trades` for 18+ minutes. A `usd-volume-restamp -chunks` run was
// mid-window and its decompress_chunk asked for AccessExclusiveLock on a
// chunk of that hypertable. It could not have it, so it QUEUED — and a
// pending exclusive request is not a private wait: PostgreSQL puts every
// LATER request for that object behind it, however trivial and however
// compatible with the lock actually held. The measured pile-up:
//
//	decompress_chunk (restamp)      blocked 1,984 s
//	UPDATE trades  x2 (restamp)     blocked 1,164 s
//	postgres_exporter scrapes x3    blocked   917 s
//	chunks_detailed_size (watcher)  blocked   904 s
//
// The exporter being in that list is why it mattered: alerting went
// cascade-blind (stellarindex_postgres_exporter_down, direct scrape HTTP
// 000 after 30 s), stellarindex_aggregator_silent paged, the restamp
// stalled 33 minutes and /v1/status went `degraded`. Both systemd units
// read `active` throughout. It cleared in under 20 s once the aggregator's
// SELECT was cancelled by hand. Seven aggregator restarts in the preceding
// 14 hours did NOT jam, so a restart is not the trigger: the COLLISION of
// a heavy cold-start read with a restamp's decompress/compress phase is,
// and it recurs for as long as r1 is kept saturated with restamps while
// deploys continue.
//
// WHAT THE 5 s BOUND COVERS. `SET LOCAL lock_timeout` covers EVERY lock
// request in the attempt's transaction, not only the first. On TimescaleDB
// 2.26.4 both functions take AccessShareLock on `trades`, then
// ExclusiveLock on the chunk (readers still pass), do their work, and
// then ask for AccessExclusiveLock on the chunk: that late request is the
// one a long reader blocks, and it is bounded at 5 s like the rest. No
// request of ours may sit pending longer, because a pending exclusive
// request queues every later reader of the chunk and the API's request
// timeout is 15 s. `lock_timeout` does nothing to a statement that HOLDS
// its locks and is working; the 1.5-hour decompress of the 159.7 GB
// outlier chunk is untouched, and no `statement_timeout` is used.
//
// THE COST OF THAT BOUND, AND HOW IT IS AVOIDED. A refusal of the late
// request throws the attempt's work away: up to 1.5 hours of decompress.
// Two things keep that rare and visible:
//
//   - before each attempt, a lock holder on the chunk (or its compressed
//     chunk) whose transaction is older than [longLockHolderAge] is waited
//     out, with no request of ours pending, instead of starting work it
//     would block at the end.
//   - each attempt first takes the functions' own opening locks with
//     `LOCK TABLE`, in their order, so a refusal there is known to be
//     cheap and one inside the function is known to have cost work.
//
// FAILING IS SAFE, AND FAILING IS NOT THE FIRST ANSWER. Each attempt is
// one transaction, so a refusal rolls back atomically: verified on 2.26.4
// / PG 15, a refused decompress left the chunk compressed and a refused
// compress left it decompressed with every row readable. Between attempts
// this process has NO request pending for [lockWaitPolicy.drain], so the
// queue behind the last one drains. The budget is charged only for time
// spent waiting (refused requests, drains, long holders), never for work.
// A retry after a refusal that cost work starts only while the caller is
// still running and the last attempt's length fits in what is left of
// the budget on the wall clock, so a re-compress after a SIGTERM cannot
// start a fresh 47-minute attempt that the stop window would kill. A late
// attempt cut by SIGKILL anyway rolls back and leaves the chunk decompressed (safe).
//
// WHAT THIS DOES NOT COVER. A convoy whose head is somebody ELSE's
// exclusive request (a by-hand ALTER, a migration, the compression
// policy's own proc) is untouched by this; that is what the
// stellarindex_pg_lock_convoy alert exists for. A decompress that HOLDS
// its locks for 1.5 hours still blocks writers of that chunk for 1.5
// hours. And a holder in another role is invisible to the long-holder
// check unless this role can read its pg_stat_activity row.

// lockWaitPolicy is how hard one statement may ask for its locks: `wait`
// per request, `drain` of clear air between attempts, `budget` of total
// waiting.
type lockWaitPolicy struct {
	wait   time.Duration
	drain  time.Duration
	budget time.Duration
}

var (
	// tradesChunkDecompressLock bounds decompress_chunk.
	//
	// wait: the longest a request of ours may sit pending. Under the API's
	// 15 s request timeout and the exporter's 30 s scrape timeout, and four
	// orders of magnitude above what taking an uncontended lock costs.
	//
	// drain: the 2026-09-10 convoy drained in under 20 s once its head was
	// removed; 15 s of clear air per 5 s of asking keeps the drain ahead
	// of the ask.
	//
	// budget: generous enough to outlast a legitimate long reader — the
	// aggregator pool's own statement_timeout backstop is 30 min
	// (config.BackgroundStatementTimeout) — and finite because failing
	// here is the SAFE direction.
	tradesChunkDecompressLock = lockWaitPolicy{wait: 5 * time.Second, drain: 15 * time.Second, budget: 35 * time.Minute}
	// tradesChunkCompressLock bounds compress_chunk. Same per-request
	// bound and pause; the budget is much larger because by the time this
	// runs the chunk is ALREADY decompressed and giving up is the
	// expensive outcome. It stays finite so a run under run-heavy-job.sh
	// cannot sit past its SIGTERM-to-SIGKILL window
	// (HEAVY_JOB_STOP_TIMEOUT=2h on the restamp's launch line) without
	// saying why.
	tradesChunkCompressLock = lockWaitPolicy{wait: 5 * time.Second, drain: 15 * time.Second, budget: 90 * time.Minute}
)

// longLockHolderAge is the transaction age past which a holder of a lock
// on the chunk is waited out rather than raced: four times the API's
// request timeout, and far below an 18-minute read.
const longLockHolderAge = 60 * time.Second

// pgLockNotAvailable is SQLSTATE 55P03, what PostgreSQL raises when
// `lock_timeout` expires ("canceling statement due to lock timeout").
// Matched on the code rather than the message so a server with a
// localized lc_messages still retries.
const pgLockNotAvailable = "55P03"

// isLockNotAvailable reports whether err is a `lock_timeout` expiry.
// pgx's *pgconn.PgError exposes the SQLSTATE through SQLState(); the
// interface assertion keeps this file off pgconn for one string.
func isLockNotAvailable(err error) bool {
	var coded interface{ SQLState() string }
	if errors.As(err, &coded) {
		return coded.SQLState() == pgLockNotAvailable
	}
	return false
}

// errLongLockHolder is wrapped when the budget ran out waiting on a
// long-running holder, before any attempt was refused.
var errLongLockHolder = errors.New("a long-running transaction holds a lock on the chunk")

// longLockHolderSelect finds the oldest other transaction older than $3
// seconds holding any lock on the chunk or its compressed chunk. It reads
// only pg_locks, pg_stat_activity and the catalog, so it takes no lock a
// convoy could queue it behind.
const longLockHolderSelect = `
	WITH target AS (
	  SELECT to_regclass(format('%I.%I', $1::text, $2::text))::oid AS relid
	  UNION ALL
	  SELECT to_regclass(format('%I.%I', cc.schema_name, cc.table_name))::oid
	    FROM _timescaledb_catalog.chunk ch
	    JOIN _timescaledb_catalog.chunk cc ON cc.id = ch.compressed_chunk_id
	   WHERE ch.schema_name = $1::text AND ch.table_name = $2::text
	)
	SELECT a.pid, l.mode, l.relation::regclass::text,
	       EXTRACT(EPOCH FROM clock_timestamp() - a.xact_start)::bigint
	  FROM pg_locks l
	  JOIN target t ON t.relid = l.relation
	  JOIN pg_stat_activity a ON a.pid = l.pid
	 WHERE l.locktype = 'relation'
	   AND l.granted
	   AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
	   AND l.pid <> pg_backend_pid()
	   -- Postgres cancels a non-wraparound autovacuum when compress asks for its lock; waiting on it burns the budget.
	   AND NOT (a.backend_type = 'autovacuum worker' AND a.query NOT LIKE '%(to prevent wraparound)%')
	   AND a.xact_start < clock_timestamp() - make_interval(secs => $3::double precision)
	 ORDER BY a.xact_start
	 LIMIT 1
`

// longLockHolder describes the oldest long-running holder of a lock on
// the chunk, or returns "" when there is none.
func (s *Store) longLockHolder(ctx context.Context, c TradeChunk) (string, error) {
	var (
		pid       int
		mode, rel string
		ageSec    int64
	)
	err := s.db.QueryRowContext(ctx, longLockHolderSelect, c.Schema, c.Name, longLockHolderAge.Seconds()).Scan(&pid, &mode, &rel, &ageSec)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("check for long-running lock holders: %w", err)
	}
	return fmt.Sprintf("pid %d holding %s on %s in a transaction open %s", pid, mode, rel, time.Duration(ageSec)*time.Second), nil
}

// lockTally is what one bounded call has spent, for the retry decision
// and the operator's error.
type lockTally struct {
	clk          lockClock
	start        time.Time
	charged      time.Duration // waiting only: refused requests, drains, long holders
	workLost     time.Duration // work thrown away by refusals inside the statement
	cheap, late  int           // refusals before and inside the statement
	holderWaits  int
	lastHolder   string
	attemptsMade int
}

func (t *lockTally) String() string {
	s := fmt.Sprintf("after %s and %d attempt(s): %d refused before any work, %d refused inside the statement (%s of work lost); "+
		"%s of waiting charged",
		t.clk.since(t.start).Round(time.Millisecond), t.attemptsMade, t.cheap, t.late, t.workLost.Round(time.Millisecond),
		t.charged.Round(time.Millisecond))
	if t.holderWaits > 0 {
		s += fmt.Sprintf("; %d wait(s) on a long-running holder, last: %s", t.holderWaits, t.lastHolder)
	}
	return s
}

// execUnderBoundedLockWait runs one chunk statement, each attempt in its
// own transaction with every lock request bounded at p.wait, retrying
// for as long as the ONLY thing that failed was a lock request and the
// policy allows it. Any other error is returned on the spot — a retry
// loop that swallowed, say, an out-of-disk compress would be a worse bug
// than the one this fixes. `live` is the caller's own context: once it is
// done, no retry that would repeat work starts.
func (s *Store) execUnderBoundedLockWait(ctx, live context.Context, p lockWaitPolicy, query string, c TradeChunk) error {
	clk := s.lockClock
	r := &lockRetry{t: lockTally{clk: clk, start: clk.now()}, p: p, live: live}
	for {
		holder, err := s.longLockHolder(ctx, c)
		if err != nil {
			return err
		}
		if holder != "" {
			if err := r.waitOutHolder(ctx, holder); err != nil {
				return err
			}
			continue
		}
		if done, err := s.retryAttempt(ctx, r, query, c); done {
			return err
		}
		if err := r.drainAfterRefusal(ctx); err != nil {
			return err
		}
	}
}

// lockRetry is the state of one execUnderBoundedLockWait call.
type lockRetry struct {
	t    lockTally
	p    lockWaitPolicy
	live context.Context
}

func (r *lockRetry) giveUp(why string, err error) error {
	return fmt.Errorf("gave up on the chunk's locks (%s) %s; %s per request, %s between attempts: something else holds a "+
		"conflicting lock — identify it with pg_blocking_pids() and see docs/operations/runbooks/postgres.md: %w",
		why, &r.t, r.p.wait, r.p.drain, err)
}

// mayWait: another cheap wait fits the budget, and once the caller has
// gone, the wall clock as well.
func (r *lockRetry) mayWait() bool {
	if r.live.Err() != nil && r.t.clk.since(r.t.start)+r.p.drain >= r.p.budget {
		return false
	}
	return r.t.charged+r.p.drain < r.p.budget
}

func (r *lockRetry) budgetSpent(err error) error {
	return r.giveUp(fmt.Sprintf("the %s budget is spent", r.p.budget), err)
}

func (r *lockRetry) drainAfterRefusal(ctx context.Context) error {
	if err := r.t.drain(ctx, r.p.drain); err != nil {
		return fmt.Errorf("stopped waiting for the chunk's locks %s: %w", &r.t, err)
	}
	return nil
}

// waitOutHolder charges one wait on a long-running lock holder.
func (r *lockRetry) waitOutHolder(ctx context.Context, holder string) error {
	r.t.holderWaits++
	r.t.lastHolder = holder
	if !r.mayWait() {
		return r.budgetSpent(errLongLockHolder)
	}
	return r.drainAfterRefusal(ctx)
}

// retryAttempt runs one attempt. done is true when the call must return
// err (nil on success); false means a lock refusal was charged and the
// caller should drain and retry.
func (s *Store) retryAttempt(ctx context.Context, r *lockRetry, query string, c TradeChunk) (done bool, err error) {
	r.t.attemptsMade++
	began := r.t.clk.now()
	stmtElapsed, inStatement, err := s.chunkLockAttempt(ctx, r.t.clk, r.p.wait, query, c)
	if err == nil {
		return true, nil
	}
	if !isLockNotAvailable(err) {
		return true, err
	}
	// A refusal inside the statement ended a wait of at most p.wait;
	// the rest of the statement's time was work.
	var work time.Duration
	if inStatement {
		work = stmtElapsed - min(stmtElapsed, r.p.wait)
	}
	r.t.charged += r.t.clk.since(began) - work
	r.t.workLost += work
	if !inStatement {
		r.t.cheap++
		if !r.mayWait() {
			return true, r.budgetSpent(err)
		}
		return false, nil
	}
	r.t.late++
	switch remaining := r.p.budget - r.t.clk.since(r.t.start); {
	case r.live.Err() != nil:
		return true, r.giveUp("the caller has stopped, so the work is not repeated", err)
	case stmtElapsed+r.p.drain > remaining:
		return true, r.giveUp(fmt.Sprintf("another %s attempt does not fit the %s left of the %s budget",
			stmtElapsed.Round(time.Second), remaining.Round(time.Second), r.p.budget), err)
	}
	return false, nil
}

// lockClock is the retry loop's time source; the zero value is the wall
// clock. Tests inject one so a loaded runner cannot change what is charged.
type lockClock struct {
	nowFn   func() time.Time
	sleepFn func(context.Context, time.Duration) error
}

func (c lockClock) now() time.Time {
	if c.nowFn != nil {
		return c.nowFn()
	}
	return time.Now()
}

func (c lockClock) since(t time.Time) time.Duration { return c.now().Sub(t) }

func (c lockClock) sleep(ctx context.Context, d time.Duration) error {
	if c.sleepFn != nil {
		return c.sleepFn(ctx, d)
	}
	return sleepCtx(ctx, d)
}

// drain waits d with no request pending and charges what it took.
func (t *lockTally) drain(ctx context.Context, d time.Duration) error {
	began := t.clk.now()
	err := t.clk.sleep(ctx, d)
	t.charged += t.clk.since(began)
	return err
}

// chunkLockAttempt is one attempt: BEGIN; bound every lock request; take
// the statement's opening locks with LOCK TABLE (AccessShare on `trades`,
// then Exclusive on the chunk — the order both functions use on 2.26.4);
// run; COMMIT. inStatement reports whether a failure came from the
// statement itself, after its opening locks were held, and stmtElapsed how
// long the statement ran.
//
// `SET LOCAL`, and POSTGRES scopes it — not the driver. A session-level
// `SET` on a pooled connection outlives the call (pgx v5's stdlib adapter
// resets nothing on reuse); COMMIT/ROLLBACK unwinds LOCAL even on the
// error path.
func (s *Store) chunkLockAttempt(ctx context.Context, clk lockClock, wait time.Duration, query string, c TradeChunk) (stmtElapsed time.Duration, inStatement bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", wait.Milliseconds()),
		"LOCK TABLE ONLY trades IN ACCESS SHARE MODE",
		fmt.Sprintf("LOCK TABLE ONLY %s IN EXCLUSIVE MODE", pgx.Identifier{c.Schema, c.Name}.Sanitize()),
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return 0, false, err
		}
	}
	began := clk.now()
	if _, err := tx.ExecContext(ctx, query, c.Schema, c.Name); err != nil {
		return clk.since(began), true, err
	}
	return clk.since(began), false, tx.Commit()
}

// sleepCtx waits d, or returns early with the context's error. The
// re-compress runs on a context detached from the caller's cancellation
// ([Store.recompressTradesChunk]) and so waits the full pause by design;
// the decompress runs on the caller's own context and abandons the retry
// on a SIGTERM rather than holding the process open past its grace.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// TradesChunkBytes is the chunk's current on-disk total (heap + indexes +
// toast, including its compressed half when it has one).
func (s *Store) TradesChunkBytes(ctx context.Context, c TradeChunk) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, tradesChunkBytes, c.Schema, c.Name).Scan(&n); err != nil {
		return 0, fmt.Errorf("timescale: size of chunk %s: %w", c, err)
	}
	return n, nil
}

// tradesDataVolumePathSelect resolves the directory the `trades`
// hypertable's storage lives under: its tablespace's location when it has
// one, the server's data_directory otherwise. The chunk-wise restamp
// measures free space on the filesystem holding that path — which only
// means something when the tool runs ON the database host, the case the
// pre-flight checks for.
const tradesDataVolumePathSelect = `
	SELECT COALESCE(NULLIF(pg_tablespace_location(t.oid), ''), current_setting('data_directory'))
	  FROM pg_class c
	  LEFT JOIN pg_tablespace t ON t.oid = c.reltablespace
	 WHERE c.oid = 'trades'::regclass
`

// TradesDataVolumePath returns the directory whose filesystem holds the
// `trades` hypertable. Reading data_directory needs pg_read_all_settings
// (or superuser); the error is returned as-is so the caller can fall back
// to an operator-supplied figure.
func (s *Store) TradesDataVolumePath(ctx context.Context) (string, error) {
	var path string
	if err := s.db.QueryRowContext(ctx, tradesDataVolumePathSelect).Scan(&path); err != nil {
		return "", fmt.Errorf("timescale: resolve trades data volume: %w", err)
	}
	return path, nil
}

// ─── the `trades` compression policy ─────────────────────────────────────
//
// migrations/0205 runs the `trades` compression policy as the custom job
// `trades_compression_policy` (the built-in policy under a lock_timeout), a
// background job on a 12-hour schedule that selects every chunk older than
// the lag whose status is not fully-compressed and calls compress_chunk on
// it. A chunk this tool has decompressed by hand IS such
// a chunk: over a multi-day run the policy's next fire would re-compress
// the open chunk between two of the tool's batches, and the next batch
// would then run the per-row decompression path the chunk mode exists to
// escape — silently, because a DML into a compressed chunk does not
// error, it crawls. The policy is therefore PAUSED for the duration of a
// write run and re-enabled on every exit path. These are the primitives.

// TradesCompressionPolicy is the `trades` compression policy job as the
// chunk-wise restamp needs it.
type TradesCompressionPolicy struct {
	// JobID is the argument to alter_job.
	JobID int
	// Scheduled reports whether the job is currently enabled.
	Scheduled bool
	// CompressAfter is the policy's lag: a chunk is compressed once its
	// whole range is older than now() - CompressAfter.
	CompressAfter time.Duration
}

// ErrNoTradesCompressionPolicy is returned when `trades` has no
// compression policy job at all.
var ErrNoTradesCompressionPolicy = errors.New("timescale: no compression policy job on trades")

// tradesCompressionPolicySelect resolves the job by what it is (the
// trades_compression_policy procedure in the schema unqualified `trades`
// resolves to), never by a job id that happened to be 1000 on one host. A
// custom job carries no hypertable in timescaledb_information.jobs, hence
// the proc name. The lag is read out of the job's config in seconds so the
// caller never parses an interval's text form.
const tradesCompressionPolicySelect = `
	SELECT job_id, scheduled,
	       EXTRACT(EPOCH FROM (config->>'compress_after')::interval)::bigint AS compress_after_seconds
	  FROM timescaledb_information.jobs
	 WHERE proc_schema = current_schema()
	   AND proc_name = 'trades_compression_policy'
`

// TradesCompressionPolicy returns the `trades` compression policy job.
// [ErrNoTradesCompressionPolicy] when there is none — a caller that would
// pause it must refuse rather than assume there is nothing to pause.
func (s *Store) TradesCompressionPolicy(ctx context.Context) (TradesCompressionPolicy, error) {
	var (
		p       TradesCompressionPolicy
		seconds sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, tradesCompressionPolicySelect).Scan(&p.JobID, &p.Scheduled, &seconds)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return p, ErrNoTradesCompressionPolicy
	case err != nil:
		return p, fmt.Errorf("timescale: resolve trades compression policy: %w", err)
	case !seconds.Valid || seconds.Int64 <= 0:
		return p, fmt.Errorf("timescale: trades compression policy job %d has no compress_after in its config", p.JobID)
	}
	p.CompressAfter = time.Duration(seconds.Int64) * time.Second
	return p, nil
}

// jobSetScheduled is alter_job's scheduled toggle; the casts keep the
// overload resolution unambiguous.
const jobSetScheduled = `SELECT alter_job($1::integer, scheduled => $2::boolean)`

// SetJobScheduled enables or disables one background job.
func (s *Store) SetJobScheduled(ctx context.Context, jobID int, scheduled bool) error {
	if _, err := s.db.ExecContext(ctx, jobSetScheduled, jobID, scheduled); err != nil {
		return fmt.Errorf("timescale: alter_job(%d, scheduled => %v): %w", jobID, scheduled, err)
	}
	return nil
}

// TradeChunkRestampResult is what one bracketed chunk cost: its size
// before, decompressed, and after re-compression, and the wall time. For
// a chunk that was not compressed at listing the three sizes are before,
// before again, and after the work.
type TradeChunkRestampResult struct {
	Chunk             TradeChunk
	BytesBefore       int64
	BytesDecompressed int64
	BytesAfter        int64
	Elapsed           time.Duration
}

// ChunkRestampStep names the two statements in the bracket that can
// outlive the process issuing them: a 160 GB decompress or re-compress
// is what run-heavy-job.sh's TimeoutStopSec is sized for (the restamp's
// launch line exports HEAVY_JOB_STOP_TIMEOUT=2h; the wrapper's own
// default is 5min, and a host that has not had the heavy-job-wrapper
// tag applied is still on systemd's 90 s), and a dropped connection
// aborts the statement with the chunk in whatever state it was in. The
// bracket announces each through the caller's `before` hook so a trace
// exists BEFORE the statement is issued.
type ChunkRestampStep int

const (
	// ChunkRestampDecompress precedes decompress_chunk.
	ChunkRestampDecompress ChunkRestampStep = iota + 1
	// ChunkRestampCompress precedes compress_chunk.
	ChunkRestampCompress
)

// RestampTradesChunk runs `work` inside the chunk, restoring the state the
// chunk was LISTED in ([TradeChunk.Compressed]) afterwards:
//
//	compressed at listing:    size → decompress_chunk → size → work → compress_chunk → size
//	uncompressed at listing:  size → work → size
//
// The contract that matters is the second half of the first line: once a
// compressed chunk has been decompressed, EVERY exit path compresses it
// again before returning — including a failed `work` and a cancelled
// context. The compress runs on a context detached from the caller's
// cancellation, because the likeliest mid-chunk failure under
// run-heavy-job.sh is a SIGTERM, and a cancelled context would fail the
// very statement that puts a 160 GB chunk back. Only when the re-compress
// itself fails is the chunk left decompressed, and then the error says so
// by name.
//
// A chunk that was NOT compressed at listing is left that way. The chunks
// newer than the compression policy's lag are deliberately uncompressed
// (the ledgerstream cursor-regression replay upserts into them), and a
// chunk an earlier killed run left open is the policy's to compress once
// it is re-enabled; compressing either here would be this tool deciding
// something that is not its decision.
//
// `before`, when not nil, is called with the step about to be issued —
// the caller's chance to print the by-hand repair before a statement
// that may outlive the process. A failed decompress runs nothing: the
// chunk is still compressed and untouched.
func (s *Store) RestampTradesChunk(ctx context.Context, c TradeChunk, work func(context.Context) error, before func(ChunkRestampStep)) (res TradeChunkRestampResult, err error) {
	res = TradeChunkRestampResult{Chunk: c}
	start := time.Now()
	defer func() { res.Elapsed = time.Since(start) }()
	if before == nil {
		before = func(ChunkRestampStep) {}
	}

	if res.BytesBefore, err = s.TradesChunkBytes(ctx, c); err != nil {
		return res, err
	}
	if !c.Compressed {
		res.BytesDecompressed = res.BytesBefore
		if werr := work(ctx); werr != nil {
			return res, fmt.Errorf("timescale: chunk %s restamp failed (the chunk was not compressed at listing and is left that way): %w", c, werr)
		}
		if res.BytesAfter, err = s.TradesChunkBytes(context.WithoutCancel(ctx), c); err != nil {
			return res, err
		}
		return res, nil
	}

	before(ChunkRestampDecompress)
	if derr := s.DecompressTradesChunk(ctx, c); derr != nil {
		return res, derr
	}
	// From here on the chunk is on disk decompressed, and the re-compress
	// is DEFERRED rather than called at the tail: a failed work, a
	// cancelled context and a Go panic inside the work all reach it. A
	// panic still propagates afterwards — the chunk is put back first.
	defer func() { err = s.recompressTradesChunk(ctx, c, &res, before, err) }()
	if res.BytesDecompressed, err = s.TradesChunkBytes(ctx, c); err != nil {
		return res, err
	}
	return res, work(ctx)
}

// recompressTradesChunk is the deferred half of [Store.RestampTradesChunk]
// for a chunk that was compressed at listing: announce, compress the chunk
// on a cancellation-proof context, then fold the work's outcome (werr) and
// the compress's outcome into one error the operator can act on. The
// compress is unconditional — it is the whole point of this function
// existing separately from the bracket.
func (s *Store) recompressTradesChunk(ctx context.Context, c TradeChunk, res *TradeChunkRestampResult, before func(ChunkRestampStep), werr error) error {
	rctx := context.WithoutCancel(ctx)
	before(ChunkRestampCompress)
	cerr := s.compressTradesChunk(rctx, ctx, c)
	switch {
	case werr != nil && cerr != nil:
		return fmt.Errorf("timescale: chunk %s restamp failed AND the re-compress failed (%v) — the chunk is LEFT DECOMPRESSED; "+
			"compress it by hand: SELECT compress_chunk('%s'); restamp error: %w", c, cerr, c, werr)
	case werr != nil:
		return fmt.Errorf("timescale: chunk %s restamp failed (the chunk was re-compressed before this error was raised): %w", c, werr)
	case cerr != nil:
		return fmt.Errorf("timescale: chunk %s re-compress failed — the chunk is LEFT DECOMPRESSED; compress it by hand: SELECT compress_chunk('%s'): %w", c, c, cerr)
	}
	after, err := s.TradesChunkBytes(rctx, c)
	if err != nil {
		return err
	}
	res.BytesAfter = after
	return nil
}

// ─── is the chunk STILL decompressed? ────────────────────────────────────
//
// The compression policy is paused for a chunk run, but a pause is not a
// fence: a by-hand `compress_chunk`, a fire of the policy that started
// before the pause, or another actor's mitigation can compress the open
// chunk between two of the run's batches. An UPDATE into a compressed chunk
// does not fail — it decompresses the chunk wholesale inside the
// transaction, the path the chunk mode exists to escape, and the run's
// own lifted decompression cap removes the error that would otherwise
// surface it — so the run would crawl for hours before anyone noticed. The guard is a catalog read before EVERY batch: one row
// of timescaledb_information.chunks against a 20,000-row UPDATE.

// tradesChunkIsCompressed reads one chunk's compression state right now.
const tradesChunkIsCompressed = `SELECT is_compressed FROM timescaledb_information.chunks WHERE chunk_schema = $1 AND chunk_name = $2`

// TradesChunkCompressed reports whether the chunk is compressed at this
// moment — the catalog's answer, not the listing's.
func (s *Store) TradesChunkCompressed(ctx context.Context, c TradeChunk) (bool, error) {
	var compressed bool
	if err := s.db.QueryRowContext(ctx, tradesChunkIsCompressed, c.Schema, c.Name).Scan(&compressed); err != nil {
		return false, fmt.Errorf("timescale: compression state of chunk %s: %w", c, err)
	}
	return compressed, nil
}

// ErrTradesChunkRecompressed is returned by
// [Store.ApplyXLMBaseUSDVolumeRestampInChunk] when the chunk it is writing
// into reads is_compressed = true ahead of a batch: something compressed
// the chunk underneath the run. The batch is not written.
var ErrTradesChunkRecompressed = errors.New("timescale: chunk was re-compressed underneath the restamp")

// stillDecompressed is the guard itself, as a hook to run ahead of a
// write: the catalog's answer for chunk c, turned into a refusal. Shared
// by both tiers' in-chunk applies so neither can drift into writing
// through the per-row path the chunk walk exists to escape.
func (s *Store) stillDecompressed(c TradeChunk) func(context.Context) error {
	return func(ctx context.Context) error {
		compressed, err := s.TradesChunkCompressed(ctx, c)
		if err != nil {
			return err
		}
		if compressed {
			return fmt.Errorf("%w: %s reads is_compressed = true ahead of the next batch — the batch is NOT written, "+
				"because an UPDATE into a compressed chunk decompresses it wholesale", ErrTradesChunkRecompressed, c)
		}
		return nil
	}
}

// ApplyXLMBaseUSDVolumeRestampInChunk is [Store.ApplyXLMBaseUSDVolumeRestamp]
// for a plan whose rows all lie in chunk c, with the guard above ahead of
// every batch. On [ErrTradesChunkRecompressed] the rows written so far are
// committed and reported; the caller stops the walk rather than letting the
// next UPDATE run the per-row path.
func (s *Store) ApplyXLMBaseUSDVolumeRestampInChunk(ctx context.Context, c TradeChunk, plan *XLMBaseRestampPlan, generation int64, batch int) (int64, error) {
	return s.ApplyUSDVolumeRestampPlanInChunk(ctx, c, plan, generation, batch)
}

// RestampExactTierUSDVolumeInChunk is [Store.RestampExactTierUSDVolume]
// for a window that lies inside chunk c, with the same guard ahead of the
// UPDATE. The exact tier has no row batching — its transaction IS the
// caller's `-slice` window — so the guard runs once per slice, which is
// once per statement, exactly as the xlm-base guard runs once per batch.
func (s *Store) RestampExactTierUSDVolumeInChunk(ctx context.Context, c TradeChunk, p USDVolumeRestampParams) (int64, error) {
	if err := s.stillDecompressed(c)(ctx); err != nil {
		return 0, err
	}
	return s.RestampExactTierUSDVolume(ctx, p)
}

// ─── one run per hypertable: the run lock ────────────────────────────────
//
// run-heavy-job.sh's flock is per JOB NAME, and nothing stops an operator
// launching under a second name — so as far as the wrapper is concerned two
// `-chunks -write` runs can be alive at once. Two of them on one
// hypertable is the one thing the policy dance cannot survive: the second
// finds the policy already unscheduled, and re-enables it at ITS exit
// while the first is still walking, which hands the first run's open chunk
// to the policy's next fire — and the span ends up split across two
// generations. The run therefore holds a session-level advisory lock on a
// DEDICATED connection for its whole life. Session-level rather than
// transaction-level because the run is thousands of transactions; a
// dedicated connection because database/sql would otherwise hand the
// pooled connection holding the lock to any other statement, and the
// release could run on a different one. Postgres drops the lock with the
// connection, which is what a SIGKILL amounts to.

// USDVolumeRestampLockName is the text the lock key is hashed from —
// `hashtext(name)`, the spelling the platform stores' claims use — so an
// operator can reproduce the key by name.
const USDVolumeRestampLockName = "usd-volume-restamp:trades"

const (
	usdVolumeRestampTryLock = `SELECT pg_try_advisory_lock(hashtext($1::text))`
	usdVolumeRestampUnlock  = `SELECT pg_advisory_unlock(hashtext($1::text))`
)

// ErrUSDVolumeRestampLockHeld is returned by [Store.TryUSDVolumeRestampLock]
// when another session holds the lock: another run is alive.
var ErrUSDVolumeRestampLockHeld = errors.New("timescale: advisory lock hashtext('" + USDVolumeRestampLockName + "') is held by another session")

// TryUSDVolumeRestampLock takes the run lock on a dedicated connection and
// returns the release, which also closes that connection. It never waits:
// a held lock is [ErrUSDVolumeRestampLockHeld], and the caller refuses to
// start.
func (s *Store) TryUSDVolumeRestampLock(ctx context.Context) (release func(context.Context) error, err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("timescale: dedicated connection for the restamp lock: %w", err)
	}
	var got bool
	if err := conn.QueryRowContext(ctx, usdVolumeRestampTryLock, USDVolumeRestampLockName).Scan(&got); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("timescale: pg_try_advisory_lock(hashtext('%s')): %w", USDVolumeRestampLockName, err)
	}
	if !got {
		_ = conn.Close()
		return nil, ErrUSDVolumeRestampLockHeld
	}
	return func(ctx context.Context) error {
		defer func() { _ = conn.Close() }()
		var released bool
		if err := conn.QueryRowContext(ctx, usdVolumeRestampUnlock, USDVolumeRestampLockName).Scan(&released); err != nil {
			return fmt.Errorf("timescale: pg_advisory_unlock(hashtext('%s')): %w", USDVolumeRestampLockName, err)
		}
		if !released {
			return fmt.Errorf("timescale: pg_advisory_unlock(hashtext('%s')) returned false: the lock was not held by this session", USDVolumeRestampLockName)
		}
		return nil
	}, nil
}

// ─── is the policy's proc executing right now? ───────────────────────────

// jobRunningSelect reads both status columns of job_stats. On TimescaleDB
// 2.26 `job_status` is the live signal: the view reports 'Running' while
// pg_stat_activity holds an active backend under the job's application
// name, and that check precedes the 'Paused' branch, so a run that is
// mid-flight when alter_job unschedules it still reads 'Running'.
// `last_run_status` is NULL for the duration of a run and only records
// the outcome afterwards; it is read as well so a version that reports
// the run there instead is also honoured. A job counts as running when
// EITHER column says so.
const jobRunningSelect = `SELECT job_status, last_run_status FROM timescaledb_information.job_stats WHERE job_id = $1`

// JobRunning reports whether the background job's proc is executing right
// now. A job with no stats row has never run and is not running.
func (s *Store) JobRunning(ctx context.Context, jobID int) (bool, error) {
	var jobStatus, lastRun sql.NullString
	err := s.db.QueryRowContext(ctx, jobRunningSelect, jobID).Scan(&jobStatus, &lastRun)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("timescale: job %d status: %w", jobID, err)
	}
	return jobStatus.String == "Running" || lastRun.String == "Running", nil
}
