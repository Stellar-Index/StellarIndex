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
// An UPDATE into a compressed chunk decompresses it inside the transaction, and no restamp join
// clause can become a scan key on a `segmentby` / `orderby` column, so the whole chunk is
// decompressed per batch (the dry run never shows it).
//
// The remedy needs both halves: decompress once, restamp as a plain heap UPDATE, recompress; and
// bound the statement's own `ts` ([Store.applyXLMBaseRestampBatch]), or every compressed chunk is
// a result relation anyway. [Store.RestampTradesChunk] owns the order and the rule that a chunk
// is never left decompressed on a failure.

// TradeChunk is one `trades` hypertable chunk as the chunk-wise restamp sees it.
type TradeChunk struct {
	Schema string
	Name   string
	// [RangeStart, RangeEnd) is the chunk's time slice, UTC.
	RangeStart time.Time
	RangeEnd   time.Time
	// Compressed reports whether the chunk is compressed right now.
	Compressed bool
	// UncompressedBytes is the decompressed size (recorded pre-compression size, or current
	// on-disk size if not compressed); the free-space pre-flight sizes against it.
	UncompressedBytes int64
	// CompressedBytes is the post-compression size; 0 for an uncompressed chunk.
	CompressedBytes int64
}

// String is the schema-qualified chunk name, as show_chunks prints it.
func (c TradeChunk) String() string { return c.Schema + "." + c.Name }

// tradesChunksInRangeSelect lists the `trades` chunks whose range intersects [$1, $2), oldest
// first. Sizes come from chunk_compression_stats, with chunks_detailed_size as the fallback for
// an uncompressed chunk.
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

// TradesChunksInRange returns every trades chunk intersecting [from, to) in range order; catalog-only, it never touches a chunk.
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

// The chunk regclass is built SERVER-SIDE (format('%I.%I') quotes), so no identifier reaches SQL text.
const (
	tradesChunkDecompress = `SELECT decompress_chunk(format('%I.%I', $1::text, $2::text)::regclass, if_compressed => true)`
	tradesChunkCompress   = `SELECT compress_chunk(format('%I.%I', $1::text, $2::text)::regclass, if_not_compressed => true)`
	tradesChunkBytes      = `SELECT total_bytes::bigint FROM chunks_detailed_size('trades') WHERE chunk_schema = $1 AND chunk_name = $2`
)

// DecompressTradesChunk decompresses one chunk (no-op if already), taking locks under a bounded wait with retry; see the convoy note below.
func (s *Store) DecompressTradesChunk(ctx context.Context, c TradeChunk) error {
	if err := s.execUnderBoundedLockWait(ctx, ctx, tradesChunkDecompressLock, tradesChunkDecompress, c); err != nil {
		return fmt.Errorf("timescale: decompress chunk %s: %w", c, err)
	}
	return nil
}

// CompressTradesChunk compresses one chunk (no-op if already) under a far larger budget than decompress, because giving up leaves the chunk decompressed.
func (s *Store) CompressTradesChunk(ctx context.Context, c TradeChunk) error {
	return s.compressTradesChunk(ctx, ctx, c)
}

// compressTradesChunk is [Store.CompressTradesChunk] with the caller's context (live) apart from
// the statement's, which the re-compress detaches from cancellation.
func (s *Store) compressTradesChunk(ctx, live context.Context, c TradeChunk) error {
	if err := s.execUnderBoundedLockWait(ctx, live, tradesChunkCompressLock, tradesChunkCompress, c); err != nil {
		return fmt.Errorf("timescale: compress chunk %s: %w", c, err)
	}
	return nil
}

// ─── the lock convoy: why every WAIT is bounded and the WORK is not ──────
//
// A pending AccessExclusiveLock request queues every later request for the object behind it,
// however compatible, so a queued decompress_chunk can blind alerting and degrade /v1/status.
//
// `SET LOCAL lock_timeout` (5 s, under the API's 15 s request timeout) bounds every lock request
// in the attempt, including the late AccessExclusiveLock taken after the work. No
// `statement_timeout`, so a long decompress that holds its locks still runs.
//
// A refused late request throws that work away, so a holder older than [longLockHolderAge] is
// waited out with no request of ours pending, and each attempt first takes the opening locks with
// `LOCK TABLE`, where refusal is cheap. The budget charges only waiting, and a retry starts only
// if the last attempt fits the remaining wall-clock, so a SIGTERM stop window is not overrun. A
// convoy headed by someone else's exclusive request is not covered (stellarindex_pg_lock_convoy).

// lockWaitPolicy is how hard a statement may ask for locks: `wait` per request, `drain` of clear
// air between attempts, `budget` of total waiting.
type lockWaitPolicy struct {
	wait   time.Duration
	drain  time.Duration
	budget time.Duration
}

var (
	// tradesChunkDecompressLock bounds decompress_chunk.
	//
	// wait: under the API's 15 s request timeout, far above an uncontended lock.
	// drain: a convoy drains in under 20 s once its head is gone; 15 s clear per 5 s asked.
	// budget: outlasts a legitimate long reader (statement_timeout backstop is 30 min,
	// config.BackgroundStatementTimeout) and is finite because failing here is safe.
	tradesChunkDecompressLock = lockWaitPolicy{wait: 5 * time.Second, drain: 15 * time.Second, budget: 35 * time.Minute}
	// tradesChunkCompressLock bounds compress_chunk: same wait and drain, a larger budget because
	// the chunk is ALREADY decompressed and giving up is the expensive outcome. Finite so a
	// run-heavy-job.sh run cannot silently outlast its SIGTERM-to-SIGKILL window (2h).
	tradesChunkCompressLock = lockWaitPolicy{wait: 5 * time.Second, drain: 15 * time.Second, budget: 90 * time.Minute}
)

// longLockHolderAge is the transaction age past which a lock holder is waited out rather than
// raced: four times the API request timeout, far below an 18-minute read.
const longLockHolderAge = 60 * time.Second

// pgLockNotAvailable is SQLSTATE 55P03, raised when `lock_timeout` expires. Matched on the code,
// not the message, so a localized lc_messages still retries.
const pgLockNotAvailable = "55P03"

// isLockNotAvailable reports whether err is a `lock_timeout` expiry. The SQLState() interface
// assertion keeps this file off pgconn.
func isLockNotAvailable(err error) bool {
	var coded interface{ SQLState() string }
	if errors.As(err, &coded) {
		return coded.SQLState() == pgLockNotAvailable
	}
	return false
}

// errLongLockHolder is wrapped when the budget ran out waiting on a long-running holder.
var errLongLockHolder = errors.New("a long-running transaction holds a lock on the chunk")

// longLockHolderSelect finds the oldest other transaction older than $3 seconds holding a lock on
// the chunk or its compressed chunk. It reads only pg_locks, pg_stat_activity and the catalog, so
// no convoy can queue it.
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

// longLockHolder describes the oldest long-running lock holder on the chunk, or "" for none.
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

// lockTally is what one bounded call has spent, for the retry decision and the operator's error.
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

// execUnderBoundedLockWait runs one chunk statement, each attempt in its own transaction with lock
// requests bounded at p.wait, retrying only while a lock refusal was the only failure. Any other
// error returns at once: retrying an out-of-disk compress is worse than the convoy. Once `live`
// is done, no retry that would repeat work starts.
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
		"conflicting lock — identify it with pg_blocking_pids() and see docs/operations/runbooks/postgres.md#stellarindex_pg_lock_convoy: %w",
		why, &r.t, r.p.wait, r.p.drain, err)
}

// mayWait: another cheap wait fits the budget, and once the caller has gone, the wall clock too.
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

// retryAttempt runs one attempt. done means return err (nil on success); otherwise a lock refusal
// was charged and the caller should drain and retry.
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
	// A refusal inside the statement ended a wait of at most p.wait; the rest was work.
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

// lockClock is the retry loop's time source (zero value: wall clock); tests inject one.
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

// chunkLockAttempt is one attempt: BEGIN; bound lock requests; take the opening locks with LOCK
// TABLE (AccessShare on `trades`, then Exclusive on the chunk, the order both functions use);
// run; COMMIT. inStatement reports a failure from the statement itself, after its locks were
// held, and stmtElapsed how long it ran.
//
// `SET LOCAL` is unwound by Postgres even on error; a session-level `SET` would outlive the call
// on a pooled connection (pgx's stdlib adapter resets nothing on reuse).
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

// sleepCtx waits d, or returns early with the context's error. The re-compress uses a detached
// context ([Store.recompressTradesChunk]) and waits the full pause by design; the decompress
// abandons the retry on SIGTERM rather than outlast its grace.
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

// TradesChunkBytes is the chunk's on-disk total (heap, indexes, toast and any compressed half).
func (s *Store) TradesChunkBytes(ctx context.Context, c TradeChunk) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, tradesChunkBytes, c.Schema, c.Name).Scan(&n); err != nil {
		return 0, fmt.Errorf("timescale: size of chunk %s: %w", c, err)
	}
	return n, nil
}

// tradesDataVolumePathSelect resolves the `trades` storage directory: its tablespace, else
// data_directory. Free space there is meaningful only on the database host (pre-flight checks).
const tradesDataVolumePathSelect = `
	SELECT COALESCE(NULLIF(pg_tablespace_location(t.oid), ''), current_setting('data_directory'))
	  FROM pg_class c
	  LEFT JOIN pg_tablespace t ON t.oid = c.reltablespace
	 WHERE c.oid = 'trades'::regclass
`

// TradesDataVolumePath returns the directory holding the trades hypertable; data_directory needs pg_read_all_settings and the error is returned as-is so the caller can fall back to an operator figure.
func (s *Store) TradesDataVolumePath(ctx context.Context) (string, error) {
	var path string
	if err := s.db.QueryRowContext(ctx, tradesDataVolumePathSelect).Scan(&path); err != nil {
		return "", fmt.Errorf("timescale: resolve trades data volume: %w", err)
	}
	return path, nil
}

// ─── the `trades` compression policy ─────────────────────────────────────
//
// The policy runs as the custom job `trades_compression_policy` (the built-in under a
// lock_timeout) every 12 hours, compressing every chunk older than the lag. A chunk this tool
// decompressed by hand qualifies: the next fire would recompress it between two batches, and DML
// into a compressed chunk does not error, it crawls. So the policy is paused for a write run and
// re-enabled on every exit path.

// TradesCompressionPolicy is the `trades` compression policy job as the restamp needs it.
type TradesCompressionPolicy struct {
	// JobID is the argument to alter_job.
	JobID int
	// Scheduled reports whether the job is currently enabled.
	Scheduled bool
	// CompressAfter is the policy's lag: a chunk is compressed once its
	// whole range is older than now() - CompressAfter.
	CompressAfter time.Duration
}

// ErrNoTradesCompressionPolicy is returned when `trades` has no compression policy job.
var ErrNoTradesCompressionPolicy = errors.New("timescale: no compression policy job on trades")

// tradesCompressionPolicySelect resolves the job by procedure name, since job ids differ per host
// and a custom job carries no hypertable in timescaledb_information.jobs. The lag is read in
// seconds so no caller parses interval text.
const tradesCompressionPolicySelect = `
	SELECT job_id, scheduled,
	       EXTRACT(EPOCH FROM (config->>'compress_after')::interval)::bigint AS compress_after_seconds
	  FROM timescaledb_information.jobs
	 WHERE proc_schema = current_schema()
	   AND proc_name = 'trades_compression_policy'
`

// TradesCompressionPolicy returns the `trades` compression policy job, or [ErrNoTradesCompressionPolicy]; a caller that would pause it must refuse on that.
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

// jobSetScheduled is alter_job's scheduled toggle; the casts keep overload resolution unambiguous.
const jobSetScheduled = `SELECT alter_job($1::integer, scheduled => $2::boolean)`

// SetJobScheduled enables or disables one background job.
func (s *Store) SetJobScheduled(ctx context.Context, jobID int, scheduled bool) error {
	if _, err := s.db.ExecContext(ctx, jobSetScheduled, jobID, scheduled); err != nil {
		return fmt.Errorf("timescale: alter_job(%d, scheduled => %v): %w", jobID, scheduled, err)
	}
	return nil
}

// TradeChunkRestampResult is what one bracketed chunk cost: sizes before, decompressed and after recompression, and wall time.
type TradeChunkRestampResult struct {
	Chunk             TradeChunk
	BytesBefore       int64
	BytesDecompressed int64
	BytesAfter        int64
	Elapsed           time.Duration
}

// ChunkRestampStep names the two bracket statements that can outlive the process issuing them; a dropped connection aborts the statement with the chunk in whatever state it was in.
type ChunkRestampStep int

const (
	// ChunkRestampDecompress precedes decompress_chunk.
	ChunkRestampDecompress ChunkRestampStep = iota + 1
	// ChunkRestampCompress precedes compress_chunk.
	ChunkRestampCompress
)

// RestampTradesChunk runs `work` inside the chunk and restores the state it was listed in (TradeChunk.Compressed): decompress, work, recompress if it was compressed.
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
	// The recompress is DEFERRED so a failed work, a cancelled context and a panic all reach it; a
	// panic still propagates afterwards, with the chunk put back first.
	defer func() { err = s.recompressTradesChunk(ctx, c, &res, before, err) }()
	if res.BytesDecompressed, err = s.TradesChunkBytes(ctx, c); err != nil {
		return res, err
	}
	return res, work(ctx)
}

// recompressTradesChunk is the deferred half of [Store.RestampTradesChunk]: compress
// unconditionally on a cancellation-proof context, then fold the work's error (werr) and the
// compress's into one error the operator can act on.
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
// A policy pause is not a fence: a by-hand `compress_chunk` or a policy fire that began before
// the pause can compress the open chunk between batches. An UPDATE into a compressed chunk does
// not fail (the run's lifted decompression cap removes the error), it decompresses wholesale, so
// the run would crawl unnoticed. The guard is one catalog row read before each 20,000-row UPDATE.

// tradesChunkIsCompressed reads one chunk's compression state right now.
const tradesChunkIsCompressed = `SELECT is_compressed FROM timescaledb_information.chunks WHERE chunk_schema = $1 AND chunk_name = $2`

// TradesChunkCompressed reports whether the chunk is compressed now (the catalog, not the listing).
func (s *Store) TradesChunkCompressed(ctx context.Context, c TradeChunk) (bool, error) {
	var compressed bool
	if err := s.db.QueryRowContext(ctx, tradesChunkIsCompressed, c.Schema, c.Name).Scan(&compressed); err != nil {
		return false, fmt.Errorf("timescale: compression state of chunk %s: %w", c, err)
	}
	return compressed, nil
}

// ErrTradesChunkRecompressed is returned by Store.ApplyXLMBaseUSDVolumeRestampInChunk when the chunk reads is_compressed = true ahead of a batch; the batch is not written.
var ErrTradesChunkRecompressed = errors.New("timescale: chunk was re-compressed underneath the restamp")

// stillDecompressed is the guard as a pre-write hook, shared by both tiers' in-chunk applies so
// neither can drift into the per-row path the chunk walk exists to escape.
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

// ApplyXLMBaseUSDVolumeRestampInChunk is [Store.ApplyXLMBaseUSDVolumeRestamp] for a plan inside chunk c, guarded before every batch.
func (s *Store) ApplyXLMBaseUSDVolumeRestampInChunk(ctx context.Context, c TradeChunk, plan *XLMBaseRestampPlan, generation int64, batch int) (int64, error) {
	return s.ApplyUSDVolumeRestampPlanInChunk(ctx, c, plan, generation, batch)
}

// RestampExactTierUSDVolumeInChunk is Store.RestampExactTierUSDVolume for a window inside chunk c, guarded once per slice because the exact tier has no row batching.
func (s *Store) RestampExactTierUSDVolumeInChunk(ctx context.Context, c TradeChunk, p USDVolumeRestampParams) (int64, error) {
	if err := s.stillDecompressed(c)(ctx); err != nil {
		return 0, err
	}
	return s.RestampExactTierUSDVolume(ctx, p)
}

// ─── one run per hypertable: the run lock ────────────────────────────────
//
// run-heavy-job.sh's flock is per job name, so a second name can have two `-chunks -write` runs
// alive. The policy pause cannot survive that: the second re-enables the policy at its exit while
// the first is still walking, exposing the first's open chunk. So the run holds a session-level
// advisory lock (the run is thousands of transactions) on a dedicated connection (a pooled one
// could be handed to other statements and the release run on a different one). Postgres drops
// the lock with the connection, e.g. on SIGKILL.

// USDVolumeRestampLockName is the text hashed into the lock key (`hashtext(name)`), so an operator can reproduce the key by name.
const USDVolumeRestampLockName = "usd-volume-restamp:trades"

const (
	usdVolumeRestampTryLock = `SELECT pg_try_advisory_lock(hashtext($1::text))`
	usdVolumeRestampUnlock  = `SELECT pg_advisory_unlock(hashtext($1::text))`
)

// ErrUSDVolumeRestampLockHeld is returned by [Store.TryUSDVolumeRestampLock] when another run is alive.
var ErrUSDVolumeRestampLockHeld = errors.New("timescale: advisory lock hashtext('" + USDVolumeRestampLockName + "') is held by another session")

// TryUSDVolumeRestampLock takes the run lock on a dedicated connection and returns the release, which also closes it; it never waits and a held lock is ErrUSDVolumeRestampLockHeld.
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

// jobRunningSelect reads both job_stats status columns. `job_status` is the live signal: it says
// 'Running' while an active backend holds the job's application name, even after alter_job
// unschedules it mid-run. `last_run_status` is NULL during a run but is read in case a version
// reports it there. Either saying 'Running' counts.
const jobRunningSelect = `SELECT job_status, last_run_status FROM timescaledb_information.job_stats WHERE job_id = $1`

// JobRunning reports whether the job's proc is executing now; no stats row means never run.
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
