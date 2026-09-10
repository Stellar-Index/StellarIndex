// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
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
// [Store.applyXLMBaseRestampBatch] and the 2026-09-06 measurement in
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
// Its exclusive lock is acquired under a bounded wait and retried — see
// the convoy note below.
func (s *Store) DecompressTradesChunk(ctx context.Context, c TradeChunk) error {
	if err := s.execUnderBoundedLockWait(ctx, tradesChunkDecompressLock, tradesChunkDecompress, c.Schema, c.Name); err != nil {
		return fmt.Errorf("timescale: decompress chunk %s: %w", c, err)
	}
	return nil
}

// CompressTradesChunk compresses one chunk; a no-op if it already is. Its
// lock is acquired under the same per-attempt bound as the decompress but
// with a far larger retry budget, because giving up here LEAVES THE CHUNK
// DECOMPRESSED — see the convoy note below.
func (s *Store) CompressTradesChunk(ctx context.Context, c TradeChunk) error {
	if err := s.execUnderBoundedLockWait(ctx, tradesChunkCompressLock, tradesChunkCompress, c.Schema, c.Name); err != nil {
		return fmt.Errorf("timescale: compress chunk %s: %w", c, err)
	}
	return nil
}

// ─── the lock convoy: why the WAIT is bounded and the WORK is not ────────
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
// THE BOUND IS ON ACQUISITION, NOT ON DURATION. `lock_timeout` aborts a
// statement that has been WAITING for a lock too long and does nothing to
// one that HOLDS its locks and is working. That distinction is the whole
// design. Decompressing the 159.7 GB outlier chunk runs about 1.5 hours
// once it has the lock (see [opsutil.JobHeartbeat.ProgressBytes]) and
// nothing here touches it. A `statement_timeout` would have killed it,
// which is why one is not used.
//
// FAILING IS SAFE, AND FAILING IS NOT THE FIRST ANSWER. Both statements
// run in their own transaction, so a lock timeout rolls back atomically
// and the chunk is left in the state it was already in. Verified against
// the deployed pair (TimescaleDB 2.26.4 / PG 15) with a concurrent session
// holding a conflicting lock: the decompress failed at the bound with
// SQLSTATE 55P03 and the chunk still read is_compressed = true; the
// compress failed the same way and left the chunk decompressed with every
// row readable. Nothing half-done in either direction.
//
// But a re-compress that gave up would leave a 160 GB chunk open, which is
// the one outcome this tool exists to avoid. So neither statement gives up
// on a refusal: each is RETRIED until its budget is spent, pausing
// [lockWaitPolicy.drain] between attempts. During that pause this process
// has NO exclusive request pending, so the queue behind the last one
// drains — the pause is the load-bearing half, not the timeout. The
// re-compress carries a much larger budget than the decompress precisely
// because of the asymmetry: a decompress that never runs changed nothing
// (the walk stops there and a rerun resumes at that chunk), while a
// compress that never runs leaves work for a human. On exhaustion both
// surface an ordinary error, so [Store.RestampTradesChunk]'s contract —
// and the operator-facing "compress it by hand" line the walk prints
// around it — is unchanged.
//
// WHAT THIS DOES NOT COVER. A convoy whose head is somebody ELSE's
// exclusive request (a by-hand ALTER, a migration, the compression
// policy's own proc) is untouched by this; that is what the
// stellarindex_pg_lock_convoy alert exists for. And a decompress that
// HOLDS its lock for 1.5 hours still makes every reader of that chunk wait
// 1.5 hours — inherent to decompressing a chunk, and deliberately not
// bounded here.

// lockWaitPolicy is how hard one statement may ask for a lock: `wait` per
// attempt, `drain` of clear air between attempts, `budget` overall.
type lockWaitPolicy struct {
	wait   time.Duration
	drain  time.Duration
	budget time.Duration
}

var (
	// tradesChunkDecompressLock bounds decompress_chunk.
	//
	// wait: the longest an exclusive request of ours may sit pending.
	// Sized against the thing that broke first — the postgres_exporter
	// scrape gives up at 30 s and took the alerting layer with it — so 5 s
	// leaves a 6x margin, and it is four orders of magnitude above what
	// taking an uncontended lock costs.
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
	// tradesChunkCompressLock bounds compress_chunk. Same per-attempt
	// bound and pause; the budget is deliberately much larger, because by
	// the time this runs the chunk is ALREADY decompressed and exhausting
	// the budget is the expensive outcome rather than the cheap one. It
	// stays finite so a run under run-heavy-job.sh cannot sit past its
	// SIGTERM-to-SIGKILL window (HEAVY_JOB_STOP_TIMEOUT=2h on the
	// restamp's launch line) without ever saying why.
	tradesChunkCompressLock = lockWaitPolicy{wait: 5 * time.Second, drain: 15 * time.Second, budget: 90 * time.Minute}
)

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

// execUnderBoundedLockWait runs one statement in its own transaction under
// `SET LOCAL lock_timeout`, retrying for as long as the ONLY thing that
// failed was the lock acquisition and the policy has budget left. Any
// other error is returned on the spot — a retry loop that swallowed, say,
// an out-of-disk compress would be a worse bug than the one this fixes.
//
// `SET LOCAL`, and POSTGRES scopes it — not the driver. A session-level
// `SET` on a pooled connection outlives the call (pgx v5's stdlib adapter
// resets nothing on reuse), which would leave a 5 s lock_timeout riding
// every later statement that landed on that connection; COMMIT/ROLLBACK
// unwinds LOCAL even on the error path. Same discipline as the
// decompression cap in [Store.RestampExactTierUSDVolume].
func (s *Store) execUnderBoundedLockWait(ctx context.Context, p lockWaitPolicy, query string, args ...any) error {
	deadline := time.Now().Add(p.budget)
	for attempt := 1; ; attempt++ {
		err := s.execUnderLockTimeout(ctx, p.wait, query, args...)
		if err == nil {
			return nil
		}
		if !isLockNotAvailable(err) {
			return err
		}
		// No room for another attempt: report the refusal AS a lock wait,
		// with how hard it was tried, so the operator reads "something
		// else is holding it" rather than a bare 55P03.
		if time.Until(deadline) <= p.drain {
			return fmt.Errorf("gave up waiting for the chunk's exclusive lock after %d attempt(s) over %s "+
				"(%s per attempt, %s between): something else holds a conflicting lock — identify it with "+
				"pg_blocking_pids() and see docs/operations/runbooks/pg-lock-convoy.md: %w",
				attempt, p.budget, p.wait, p.drain, err)
		}
		if cerr := sleepCtx(ctx, p.drain); cerr != nil {
			return cerr
		}
	}
}

// execUnderLockTimeout is one attempt: BEGIN, bound the wait, run, COMMIT.
func (s *Store) execUnderLockTimeout(ctx context.Context, wait time.Duration, query string, args ...any) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", wait.Milliseconds())); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return err
	}
	return tx.Commit()
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
// migrations/0001 attaches `add_compression_policy('trades', INTERVAL '7
// days')`, a background job on a 12-hour schedule that selects every chunk
// older than the lag whose status is not fully-compressed and calls
// compress_chunk on it. A chunk this tool has decompressed by hand IS such
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

// tradesCompressionPolicySelect resolves the job by what it is (a
// policy_compression job on the trades hypertable), never by a job id
// that happened to be 1000 on one host. The lag is read out of the job's
// config in seconds so the caller never parses an interval's text form.
const tradesCompressionPolicySelect = `
	SELECT job_id, scheduled,
	       EXTRACT(EPOCH FROM (config->>'compress_after')::interval)::bigint AS compress_after_seconds
	  FROM timescaledb_information.jobs
	 WHERE proc_name = 'policy_compression'
	   AND hypertable_name = 'trades'
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
	cerr := s.CompressTradesChunk(rctx, c)
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
// run-heavy-job.sh's flock is per JOB NAME, and the runbook mandates a
// unique name per attempt — so as far as the wrapper is concerned two
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
