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
// A per-row UPDATE into a COMPRESSED Timescale chunk decompresses the
// row's whole compressed batch inside the transaction, every time.
// Measured on production 2026-09-03 running `usd-volume-restamp -tier
// xlm-base -write` over [2026-01-01, 2026-07-21]: all 90 `trades` chunks
// in the window were compressed (policy: compress_after 7 days), one
// 2,000-row batch took over 14 minutes, and the run sustained ~1,574
// rows/min against a 28.6M-row write set — a 12-day job. The dry run is
// read-only and never showed it.
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
func (s *Store) DecompressTradesChunk(ctx context.Context, c TradeChunk) error {
	if _, err := s.db.ExecContext(ctx, tradesChunkDecompress, c.Schema, c.Name); err != nil {
		return fmt.Errorf("timescale: decompress chunk %s: %w", c, err)
	}
	return nil
}

// CompressTradesChunk compresses one chunk; a no-op if it already is.
func (s *Store) CompressTradesChunk(ctx context.Context, c TradeChunk) error {
	if _, err := s.db.ExecContext(ctx, tradesChunkCompress, c.Schema, c.Name); err != nil {
		return fmt.Errorf("timescale: compress chunk %s: %w", c, err)
	}
	return nil
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
// does not fail — it decompresses per row, the ~1,574 rows/min path the
// chunk mode exists to escape — so the run would crawl for hours before
// anyone noticed. The guard is a catalog read before EVERY batch: one row
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

// ApplyXLMBaseUSDVolumeRestampInChunk is [Store.ApplyXLMBaseUSDVolumeRestamp]
// for a plan whose rows all lie in chunk c, with the guard above ahead of
// every batch. On [ErrTradesChunkRecompressed] the rows written so far are
// committed and reported; the caller stops the walk rather than letting the
// next UPDATE run the per-row path.
func (s *Store) ApplyXLMBaseUSDVolumeRestampInChunk(ctx context.Context, c TradeChunk, plan *XLMBaseRestampPlan, generation int64, batch int) (int64, error) {
	return s.applyXLMBaseRestampBatches(ctx, plan, generation, batch, func(ctx context.Context) error {
		compressed, err := s.TradesChunkCompressed(ctx, c)
		if err != nil {
			return err
		}
		if compressed {
			return fmt.Errorf("%w: %s reads is_compressed = true ahead of the next batch — the batch is NOT written, "+
				"because an UPDATE into a compressed chunk decompresses per row", ErrTradesChunkRecompressed, c)
		}
		return nil
	})
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
