// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ─── `usd-volume-restamp -tier xlm-base -chunks` — chunk-by-chunk ───────
//
// # Why a second walk exists
//
// The day walk (usd_volume_restamp_xlmbase.go) UPDATEs rows in place, in
// -batch transactions, wherever they are. On production 2026-09-03 that
// meant every one of the 90 `trades` chunks in [2026-01-01, 2026-07-21]
// was COMPRESSED (policy: compress_after 7 days; TimescaleDB 2.26.4;
// max_tuples_decompressed_per_dml_transaction = 100000), and a DML into a
// compressed chunk decompresses the touched batch per row: one 2,000-row
// UPDATE took over 14 minutes, the run sustained ~1,574 rows/min against
// a 28.6M-row write set, and it was stopped with 0 rows committed. The
// dry run never showed it — it only reads.
//
// This walk inverts the order. For each chunk intersecting the window,
// oldest first:
//
//  1. SELECT decompress_chunk(<chunk>, if_compressed => true) — for a
//     chunk that was compressed at listing;
//  2. the SAME restamp (plan + apply, the same functions, the same
//     generation guard) restricted to that chunk's slice of the window,
//     in -chunk-batch UPDATE transactions (a plain heap UPDATE now);
//  3. SELECT compress_chunk(<chunk>, if_not_compressed => true) — again
//     only for a chunk that was compressed at listing: the bracket
//     RESTORES the listed state, it does not decide one;
//  4. a progress line (rows changed, seconds, bytes before → decompressed
//     → after) and a heartbeat tick, so it runs unattended under
//     run-heavy-job.sh.
//
// Sizing that motivates every guard below: 379 GB uncompressed / 25 GB
// compressed across the 90 chunks, largest chunk 160 GB, pool 4.69 TB
// free.
//
// # The guards
//
//   - ONE RUN AT A TIME. run-heavy-job.sh's lock is per job NAME and the
//     runbook mandates a unique name per attempt, so the wrapper does not
//     stop a second `-chunks -write` from starting beside a live one. A
//     -write run therefore holds the session advisory lock
//     hashtext('usd-volume-restamp:trades') on a dedicated connection for
//     its whole life ([timescale.Store.TryUSDVolumeRestampLock]); a held
//     lock is a refusal, before the policy is touched. The server drops
//     the lock with the connection, so a SIGKILL cannot leave it behind.
//   - THE COMPRESSION POLICY IS PAUSED for a -write run. migrations/0001
//     attaches add_compression_policy('trades', '7 days') on a 12-hour
//     schedule, and its proc compresses every chunk older than the lag
//     that is not fully compressed — which a chunk this walk has just
//     decompressed is. Over a multi-day run the policy would re-compress
//     the open chunk between two batches, and the next batch would crawl
//     through the per-row path this mode exists to escape, without an
//     error. The job is resolved by what it is (policy_compression on
//     trades), re-read under the lock, paused before the first decompress,
//     and re-enabled on EVERY exit path on a context that survives the
//     run's cancellation — BEFORE the lock is released, so a run waiting
//     on the lock never inherits a paused policy; the re-enable SQL is
//     printed before the pause is issued so a SIGKILL leaves a trace. No
//     policy to resolve is a refusal, never a silent "nothing to pause".
//   - A POLICY THAT IS ALREADY UNSCHEDULED at start is a refusal unless
//     -resume-paused-policy is given. The state means a previous attempt
//     was killed before its re-enable (the runbook's by-hand repair is
//     alter_job(scheduled => true)) or something else paused it on
//     purpose; either way this run does not silently take ownership of
//     it. With the flag it proceeds and re-enables the policy at exit.
//   - A POLICY RUN IN FLIGHT IS WAITED OUT. The pause stops the NEXT fire;
//     a fire that started before it finishes on its own, and a decompress
//     issued while it runs races it. After the pause the run polls
//     timescaledb_information.job_stats and waits — bounded, with a
//     progress line per poll — while the job reads Running, and the
//     chunks are LISTED AGAIN once it is idle, so the compressed state the
//     walk restores is the one nothing else is changing.
//   - THE CHUNK IS CHECKED AHEAD OF EVERY BATCH. A pause is not a fence: a
//     by-hand compress_chunk can take the open chunk back between two
//     batches, and an UPDATE into a compressed chunk does not fail, it
//     crawls. Every batch inside a chunk goes through
//     [timescale.Store.ApplyXLMBaseUSDVolumeRestampInChunk], which reads
//     the chunk's is_compressed first; true STOPS the walk, loudly, with
//     the chunk's name and the RESUME line. Never the slow path.
//   - DRY RUN is still the default. It prints the chunk plan (count, the
//     two byte totals, the largest chunk), the pre-flight verdict and
//     what it would do to the policy, then walks the window READ-ONLY on
//     the chunks as they are, exactly as the day walk's dry run does.
//     Nothing is decompressed, nothing is paused.
//   - PRE-FLIGHT: a -write run refuses to start unless free space on the
//     data volume EXCEEDS 2 x the largest chunk's uncompressed size, and
//     the same figure is checked again before EVERY decompress against
//     that chunk's own size. Free space is MEASURED by statfs on the
//     directory the database reports for `trades` — which is only
//     meaningful on the database host — or, when it cannot be measured,
//     taken from an explicit -min-free-bytes with a loud warning.
//   - LIVE-ADJACENT REFUSAL: a window whose right edge reaches into the
//     policy's lag (to + 1 day > now - compress_after) is refused unless
//     -allow-live-adjacent is given. Chunks there are deliberately
//     uncompressed — the ledgerstream cursor-regression replay upserts
//     into them — and the in-place walk is the right tool for them.
//   - the live-tail refusal and the -from/-to window rules run BEFORE the
//     dispatch into this walk (usdVolumeRestamp), unchanged; the
//     derive_generation guard is inside the apply, unchanged; a
//     -generation in the future is refused there too.
//   - A FAILED CHUNK IS RE-COMPRESSED before the tool exits non-zero —
//     [timescale.Store.RestampTradesChunk]'s contract, including on a
//     cancelled context. The walk stops at the failing chunk. Before each
//     decompress and each re-compress the by-hand repair (the exact
//     compress_chunk and the policy re-enable) is printed to stderr,
//     because either statement can outlive the 90 s between
//     run-heavy-job.sh's SIGTERM and its SIGKILL.
//   - RESUMABLE: every chunk is first PROBED read-only, slice by slice,
//     stopping at the first slice that would change a row. A chunk with
//     nothing to change — its rows already at the run's generation, or
//     already holding the anchor's value — probes clean to its end and is
//     skipped without being decompressed, so a rerun of the same command
//     walks past the finished prefix at dry-run cost and resumes at the
//     first unfinished chunk. -generation lets the rerun carry the first
//     run's generation so the whole span ends up at ONE generation
//     (INV-3).

// xlmBaseChunkStore is the chunk walk's seam: the day walk's plan/apply
// pair plus the chunk and policy primitives. *timescale.Store satisfies
// it.
type xlmBaseChunkStore interface {
	xlmBaseRestampStore
	TradesChunksInRange(ctx context.Context, from, to time.Time) ([]timescale.TradeChunk, error)
	TradesDataVolumePath(ctx context.Context) (string, error)
	TradesCompressionPolicy(ctx context.Context) (timescale.TradesCompressionPolicy, error)
	SetJobScheduled(ctx context.Context, jobID int, scheduled bool) error
	JobRunning(ctx context.Context, jobID int) (bool, error)
	TryUSDVolumeRestampLock(ctx context.Context) (release func(context.Context) error, err error)
	RestampTradesChunk(ctx context.Context, c timescale.TradeChunk, work func(context.Context) error, before func(timescale.ChunkRestampStep)) (timescale.TradeChunkRestampResult, error)
	ApplyXLMBaseUSDVolumeRestampInChunk(ctx context.Context, c timescale.TradeChunk, plan *timescale.XLMBaseRestampPlan, generation int64, batch int) (int64, error)
}

// xlmBaseChunkOptions is the chunk walk's own flag set, resolved.
type xlmBaseChunkOptions struct {
	// Batch is -chunk-batch: rows per UPDATE transaction inside a
	// decompressed chunk.
	Batch int
	// MinFreeBytes is -min-free-bytes: the operator's assertion of free
	// space on the data volume, used INSTEAD of a measurement. 0 = measure.
	MinFreeBytes int64
	// AllowLiveAdjacent is -allow-live-adjacent: walk a window whose right
	// edge reaches into the compression policy's lag anyway.
	AllowLiveAdjacent bool
	// ResumePausedPolicy is -resume-paused-policy: proceed when the
	// compression policy is already unscheduled at start, and re-enable it
	// at exit. Without it that state is a refusal.
	ResumePausedPolicy bool
	// PolicyIdleTimeout bounds the wait for a policy run in flight after
	// the pause. Zero = defaultPolicyIdleTimeout.
	PolicyIdleTimeout time.Duration
	// PolicyPoll is the interval between polls of that wait. Zero =
	// defaultPolicyPoll. A seam for the tests.
	PolicyPoll time.Duration
	// FreeBytes measures free space on the filesystem holding path. nil =
	// statfs. A seam for the tests.
	FreeBytes func(path string) (uint64, error)
	// Now is the instant the live-adjacent check is made against. Zero =
	// time.Now(). A seam for the tests.
	Now time.Time
	// Out receives the plan, the verdict, the per-chunk lines and the
	// report. nil = os.Stdout.
	Out io.Writer
	// Err receives the run header, the policy notices and the by-hand
	// repair traces. nil = os.Stderr.
	Err io.Writer
}

// chunkFreeSpaceHeadroom is the pre-flight multiplier: free space must
// exceed the chunk's uncompressed size times this. It is a GUARD against
// filling the data volume, not a bound on what a chunk can take. The
// decompress peaks at about 1 x plus the compressed copy; the UPDATEs then
// add a new tuple version per changed row (non-HOT after a decompress, so
// heap AND index growth, up to about 1 x more); and compress_chunk writes
// the compressed copy beside the bloated heap before truncating it. The
// worst case is therefore around 2 x plus the WAL all three generate; a
// run that gets near it should free space or narrow the window rather
// than lean on this number.
const chunkFreeSpaceHeadroom = 2.0

// runXLMBaseChunkRestamp is the `-chunks` entry point, called by
// usdVolumeRestamp once the shared window/flag/live-tail validation has
// passed. The named return is what lets the deferred policy re-enable
// fold its own failure into the run's error.
func runXLMBaseChunkRestamp(ctx context.Context, store xlmBaseChunkStore, cfgPath string, from, to time.Time, opts xlmBaseRestampOptions, copts xlmBaseChunkOptions) (err error) { //nolint:gocognit,funlen // linear: header, policy, live-adjacent, plan, pre-flight, pause, walk, report — each guard beside the print that explains it.
	run := newXLMBaseRestampRun(store, opts)
	run.batch = copts.Batch
	out, errw := copts.Out, copts.Err
	if out == nil {
		out = os.Stdout
	}
	if errw == nil {
		errw = os.Stderr
	}
	now := copts.Now
	if now.IsZero() {
		now = time.Now()
	}
	windowEnd := to.AddDate(0, 0, 1)

	_, _ = fmt.Fprintf(errw, "usd-volume-restamp: tier=xlm-base mode=chunks window [%s, %s] slice=%s chunk_batch=%d generation=%d max-generation=%d fill_null=%v min_rel_delta=%s\n",
		from.Format(time.DateOnly), to.Format(time.DateOnly), opts.Slice, copts.Batch,
		opts.Generation, opts.MaxGeneration, opts.FillNull, ratPercent(opts.MinRelDelta))

	policy, err := store.TradesCompressionPolicy(ctx)
	if err != nil {
		if errors.Is(err, timescale.ErrNoTradesCompressionPolicy) {
			return fmt.Errorf("usd-volume-restamp: -chunks refuses to start: %w. The mode pauses that policy for the run so it cannot "+
				"re-compress the open chunk between two batches; with no job to resolve it cannot promise that, and it will not guess. "+
				"The in-place walk (no -chunks) does not need the policy", err)
		}
		return fmt.Errorf("usd-volume-restamp: %w", err)
	}
	adjacent, err := checkRestampLiveAdjacent(to, now, policy.CompressAfter, copts.AllowLiveAdjacent)
	if err != nil {
		return err
	}
	if adjacent {
		_, _ = fmt.Fprintf(errw, "WARNING: -allow-live-adjacent: the window's right edge (%s) reaches into the compression policy's lag (now - %s = %s); chunks there are uncompressed and stay so.\n",
			windowEnd.Format(time.RFC3339), policy.CompressAfter, now.Add(-policy.CompressAfter).UTC().Format(time.RFC3339))
	}

	chunks, err := store.TradesChunksInRange(ctx, from, windowEnd)
	if err != nil {
		return fmt.Errorf("usd-volume-restamp: %w", err)
	}
	plan := summarizeTradesChunkPlan(chunks)
	_, _ = fmt.Fprint(out, renderTradesChunkPlan(from, to, chunks, plan))
	if len(chunks) == 0 {
		_, _ = fmt.Fprintf(out, "usd-volume-restamp: no trades chunks intersect [%s, %s] — nothing to do\n",
			from.Format(time.DateOnly), to.Format(time.DateOnly))
		return nil
	}

	pf := chunkRestampPreflight(ctx, store, plan.Largest.UncompressedBytes, copts)
	_, _ = fmt.Fprint(out, pf.render())
	if pf.Err != nil {
		if opts.Write {
			return fmt.Errorf("usd-volume-restamp: pre-flight refused -write: %w", pf.Err)
		}
		_, _ = fmt.Fprintf(out, "PRE-FLIGHT WOULD REFUSE -write: %v\n", pf.Err)
	}
	if !opts.Write {
		_, _ = fmt.Fprintf(out, "DRY RUN: would take session advisory lock hashtext('%s') (one -chunks -write run per database), then pause compression policy job %d on trades (scheduled=%v, compress_after=%s) before the first decompress, wait out a fire in flight, and re-enable it at exit.\n",
			timescale.USDVolumeRestampLockName, policy.JobID, policy.Scheduled, policy.CompressAfter)
		if !policy.Scheduled && !copts.ResumePausedPolicy {
			_, _ = fmt.Fprintln(out, "DRY RUN: -write WOULD REFUSE: the policy is already unscheduled; re-enable it by hand ("+policyReenableSQL(policy)+") or pass -resume-paused-policy.")
		}
		_, _ = fmt.Fprintln(out, "DRY RUN: nothing is decompressed; the row plan below is read from the chunks as they are.")
	} else {
		var finish func(error) error
		chunks, finish, err = beginChunkWriteRun(ctx, store, from, windowEnd, chunks, copts, out, errw)
		if err != nil {
			return err
		}
		defer func() { err = finish(err) }()
	}

	walk := &xlmBaseChunkWalk{run: run, chunks: store, copts: copts, out: out, errw: errw, reenableSQL: policyReenableSQL(policy)}
	for i, c := range chunks {
		lo, hi := clampTradesChunk(c, from, windowEnd)
		if err := walk.chunk(ctx, i+1, len(chunks), c, lo, hi); err != nil {
			_, _ = fmt.Fprint(out, chunkRestampResumeHint(cfgPath, from, to, opts, copts))
			return err
		}
	}
	run.printReport(from, to)
	_, _ = fmt.Fprint(out, run.summary(cfgPath, from, to))
	return nil
}

// checkRestampLiveAdjacent refuses a -chunks window whose right edge
// reaches the span the compression policy has not compressed yet: the
// chunks newer than now - compress_after are deliberately uncompressed
// (the ledgerstream cursor-regression replay upserts into them) and the
// in-place walk is the right tool there. -allow-live-adjacent walks it
// anyway; those chunks are then restamped in place and left uncompressed.
// Returns whether the window IS adjacent, so the caller can warn when the
// override was used.
func checkRestampLiveAdjacent(to, now time.Time, compressAfter time.Duration, allow bool) (adjacent bool, err error) {
	windowEnd := to.AddDate(0, 0, 1)
	edge := now.UTC().Add(-compressAfter)
	if !windowEnd.After(edge) {
		return false, nil
	}
	if allow {
		return true, nil
	}
	return true, fmt.Errorf("usd-volume-restamp: -chunks refuses -to %s: the window's right edge (%s) reaches into the compression policy's lag "+
		"(now - compress_after %s = %s). Chunks there are deliberately uncompressed — the ledgerstream cursor-regression replay upserts into them — "+
		"and -chunks has nothing to offer them; restamp that span with the in-place walk (no -chunks), or pass -allow-live-adjacent to walk it "+
		"anyway (those chunks are restamped in place and stay uncompressed)",
		to.Format(time.DateOnly), windowEnd.Format(time.RFC3339), compressAfter, edge.Format(time.RFC3339))
}

// policyReenableSQL is the by-hand re-enable, spelled once so every trace
// prints the same statement.
func policyReenableSQL(p timescale.TradesCompressionPolicy) string {
	return fmt.Sprintf("SELECT alter_job(%d, scheduled => true);", p.JobID)
}

// pauseTradesCompressionPolicy pauses the policy for a write run and
// returns the re-enable. The re-enable runs on a context detached from
// the run's cancellation — the likeliest reason it runs is a SIGTERM,
// and a cancelled context would fail the very statement that puts the
// policy back — and folds its own failure into the run's error, naming
// the by-hand statement. The by-hand statement is printed BEFORE the
// pause so a SIGKILL, which runs no deferred function, leaves the trace
// in the job log.
func pauseTradesCompressionPolicy(ctx context.Context, store xlmBaseChunkStore, p timescale.TradesCompressionPolicy, errw io.Writer) (func(error) error, error) {
	reenable := policyReenableSQL(p)
	_, _ = fmt.Fprintf(errw, "usd-volume-restamp: compression policy job %d on trades (compress_after %s): PAUSING it for this run so it cannot re-compress the open chunk between batches; it is re-enabled on every exit path.\n",
		p.JobID, p.CompressAfter)
	if !p.Scheduled {
		_, _ = fmt.Fprintln(errw, "NOTE: -resume-paused-policy: the policy was already unscheduled when this run started (a previous attempt killed before its re-enable); this run takes it over and re-enables it when it exits.")
	}
	_, _ = fmt.Fprintf(errw, "  if this process is KILLED (run-heavy-job.sh escalates SIGTERM to SIGKILL after 90 s), the policy stays paused; re-enable it by hand:\n    %s\n", reenable)
	if err := store.SetJobScheduled(ctx, p.JobID, false); err != nil {
		return nil, fmt.Errorf("usd-volume-restamp: pause compression policy job %d: %w", p.JobID, err)
	}
	return func(runErr error) error {
		if err := store.SetJobScheduled(context.WithoutCancel(ctx), p.JobID, true); err != nil {
			_, _ = fmt.Fprintf(errw, "usd-volume-restamp: FAILED to re-enable compression policy job %d (%v) — run by hand: %s\n", p.JobID, err, reenable)
			return errors.Join(runErr, fmt.Errorf("usd-volume-restamp: compression policy job %d is LEFT PAUSED — re-enable it by hand: %s: %w", p.JobID, reenable, err))
		}
		_, _ = fmt.Fprintf(errw, "usd-volume-restamp: compression policy job %d re-enabled\n", p.JobID)
		return runErr
	}, nil
}

// beginChunkWriteRun is everything a -write run does between the
// pre-flight and the first decompress, in the order that makes each
// guard hold: take the run lock; read the policy again UNDER the lock;
// refuse an already-unscheduled policy unless -resume-paused-policy;
// pause it; wait out a policy run in flight; list the chunks again now
// that nothing else compresses them. It returns the post-pause chunk list
// and the teardown — policy re-enable THEN lock release, each on a context
// that survives the run's cancellation, each folding its own failure into
// the run's error. A failure inside undoes what was taken before it
// returns, so no refusal leaves the lock held or the policy paused by this
// run.
func beginChunkWriteRun(ctx context.Context, store xlmBaseChunkStore, from, windowEnd time.Time, planned []timescale.TradeChunk, copts xlmBaseChunkOptions, out, errw io.Writer) (chunks []timescale.TradeChunk, finish func(error) error, err error) {
	release, err := takeRestampLock(ctx, store, errw)
	if err != nil {
		return nil, nil, err
	}
	finish = func(runErr error) error { return releaseRestampLock(ctx, release, errw, runErr) }
	// fail routes every later refusal through whatever finish has become,
	// so the lock (and, after the pause, the policy) is put back first.
	fail := func(err error) error { return finish(err) }

	policy, err := store.TradesCompressionPolicy(ctx)
	if err != nil {
		return nil, nil, fail(fmt.Errorf("usd-volume-restamp: %w", err))
	}
	if !policy.Scheduled && !copts.ResumePausedPolicy {
		return nil, nil, fail(fmt.Errorf("usd-volume-restamp: -write refuses to start: compression policy job %d on trades is ALREADY unscheduled. "+
			"A previous attempt was killed before its re-enable, or something else paused it on purpose; this run does not take it over silently. "+
			"Either re-enable it by hand — %s — and rerun, or rerun with -resume-paused-policy to have this run own it and re-enable it at exit",
			policy.JobID, policyReenableSQL(policy)))
	}
	reenable, err := pauseTradesCompressionPolicy(ctx, store, policy, errw)
	if err != nil {
		return nil, nil, fail(err)
	}
	unlock := finish
	finish = func(runErr error) error { return unlock(reenable(runErr)) }
	if err := waitTradesPolicyIdle(ctx, store, policy, copts, errw); err != nil {
		return nil, nil, fail(err)
	}
	// The listing that shaped the plan was taken before the pause; the one
	// the walk restores is taken after it, once nothing else compresses
	// trades chunks.
	chunks, err = store.TradesChunksInRange(ctx, from, windowEnd)
	if err != nil {
		return nil, nil, fail(fmt.Errorf("usd-volume-restamp: %w", err))
	}
	if drift := describeChunkListDrift(planned, chunks); drift != "" {
		_, _ = fmt.Fprintf(out, "chunk plan re-read after the pause: %s — the walk uses this listing\n", drift)
	}
	return chunks, finish, nil
}

// restampLockHolderSQL is how an operator finds the session holding the
// run lock: every advisory lock on the server, with its holder. The run
// lock is the only session-level one this codebase takes; the platform
// stores' claims are transaction-level and gone in milliseconds.
const restampLockHolderSQL = `SELECT a.pid, a.application_name, a.backend_start, a.state, l.classid, l.objid
      FROM pg_locks l JOIN pg_stat_activity a USING (pid)
     WHERE l.locktype = 'advisory';`

// takeRestampLock takes the run lock or refuses, naming the lock and how
// to find its holder.
func takeRestampLock(ctx context.Context, store xlmBaseChunkStore, errw io.Writer) (func(context.Context) error, error) {
	release, err := store.TryUSDVolumeRestampLock(ctx)
	if err != nil {
		if errors.Is(err, timescale.ErrUSDVolumeRestampLockHeld) {
			return nil, fmt.Errorf("usd-volume-restamp: -write refuses to start: %w. Another usd-volume-restamp -chunks -write run is alive. "+
				"run-heavy-job.sh's lock is per job NAME and every attempt takes a new name, so the wrapper does not stop a second attempt; this lock does: "+
				"two runs on one hypertable would each pause and re-enable the compression policy on their own schedule, the second's exit would hand the "+
				"first's open chunk to the policy's next fire, and the span would end at two generations. Find the holder:\n    %s\nand let it finish "+
				"(or stop it) before starting another; the lock is released with its connection", err, restampLockHolderSQL)
		}
		return nil, fmt.Errorf("usd-volume-restamp: %w", err)
	}
	_, _ = fmt.Fprintf(errw, "usd-volume-restamp: holding session advisory lock hashtext('%s') for this run; released at exit, or by the server when the connection drops\n",
		timescale.USDVolumeRestampLockName)
	return release, nil
}

// releaseRestampLock releases the run lock on a context detached from the
// run's cancellation and folds a failure into the run's error. The lock
// goes with its connection regardless, which the message says.
func releaseRestampLock(ctx context.Context, release func(context.Context) error, errw io.Writer, runErr error) error {
	if err := release(context.WithoutCancel(ctx)); err != nil {
		_, _ = fmt.Fprintf(errw, "usd-volume-restamp: FAILED to release the run lock (%v); the connection that held it is closed, which releases it\n", err)
		return errors.Join(runErr, fmt.Errorf("usd-volume-restamp: release run lock: %w", err))
	}
	_, _ = fmt.Fprintln(errw, "usd-volume-restamp: run lock released")
	return runErr
}

// The bound on waiting for a policy run in flight, and the poll interval.
// Ten minutes is several times the longest compression run the 7-day
// chunks have taken; a job still running past it is not the one this run
// expected, and the run refuses rather than decompresses beside it.
const (
	defaultPolicyIdleTimeout = 10 * time.Minute
	defaultPolicyPoll        = 15 * time.Second
)

// waitTradesPolicyIdle waits, after the pause, while the policy's proc is
// still executing: the pause stops the NEXT fire, a fire in flight
// finishes on its own, and a decompress issued beside it races it. One
// progress line per poll; a bounded wait, refused at the bound.
func waitTradesPolicyIdle(ctx context.Context, store xlmBaseChunkStore, p timescale.TradesCompressionPolicy, copts xlmBaseChunkOptions, errw io.Writer) error {
	timeout, poll := copts.PolicyIdleTimeout, copts.PolicyPoll
	if timeout <= 0 {
		timeout = defaultPolicyIdleTimeout
	}
	if poll <= 0 {
		poll = defaultPolicyPoll
	}
	start := time.Now()
	for {
		running, err := store.JobRunning(ctx, p.JobID)
		if err != nil {
			return fmt.Errorf("usd-volume-restamp: %w", err)
		}
		if !running {
			return nil
		}
		waited := time.Since(start)
		if waited >= timeout {
			return fmt.Errorf("usd-volume-restamp: compression policy job %d is still RUNNING %s after it was paused (the pause stops the next fire; a run in flight finishes on its own) — "+
				"decompressing beside it would race it; let it finish, then rerun", p.JobID, waited.Round(time.Second))
		}
		_, _ = fmt.Fprintf(errw, "usd-volume-restamp: compression policy job %d is running (a fire that started before the pause); waiting for it to finish before the first decompress — %s of at most %s\n",
			p.JobID, waited.Round(time.Second), timeout)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// describeChunkListDrift says how the post-pause listing differs from the
// one the plan was printed from; "" when it does not.
func describeChunkListDrift(planned, now []timescale.TradeChunk) string {
	state := make(map[string]bool, len(planned))
	for _, c := range planned {
		state[c.String()] = c.Compressed
	}
	var changed []string
	for _, c := range now {
		was, listed := state[c.String()]
		switch {
		case !listed:
			changed = append(changed, c.String()+" (new)")
		case was != c.Compressed:
			changed = append(changed, fmt.Sprintf("%s (%s, was %s)", c, chunkState(c), chunkState(timescale.TradeChunk{Compressed: was})))
		}
	}
	if len(changed) == 0 && len(now) == len(planned) {
		return ""
	}
	return fmt.Sprintf("%d chunk(s) (plan had %d); changed: %s", len(now), len(planned), strings.Join(changed, ", "))
}

// xlmBaseChunkWalk drives one run over its chunks.
type xlmBaseChunkWalk struct {
	run         *xlmBaseRestampRun
	chunks      xlmBaseChunkStore
	copts       xlmBaseChunkOptions
	out         io.Writer
	errw        io.Writer
	reenableSQL string
}

// chunk handles one chunk's slice [lo, hi) of the window.
func (w *xlmBaseChunkWalk) chunk(ctx context.Context, idx, n int, c timescale.TradeChunk, lo, hi time.Time) error {
	r := w.run
	label := fmt.Sprintf("chunk %d/%d %s [%s, %s)", idx, n, c, lo.Format(time.RFC3339), hi.Format(time.RFC3339))
	if !r.write {
		res, err := r.walk(ctx, lo, hi, xlmBaseWalkFull)
		if err != nil {
			return err
		}
		r.totals.Merge(res.stats)
		_, _ = fmt.Fprintf(w.out, "%s: would change %d row(s) (scanned %d, null-fill %d, already correct %d) — chunk %s, untouched\n",
			label, res.stats.Changed, res.stats.Scanned, res.stats.NullFilled, res.stats.Unchanged, chunkState(c))
		return nil
	}

	// Read-only probe on the chunk as it is: is there anything to do?
	probe, err := r.walk(ctx, lo, hi, xlmBaseWalkProbe)
	if err != nil {
		return err
	}
	if probe.stats.Changed == 0 {
		_, _ = fmt.Fprintf(w.out, "%s: nothing to change — skipped, chunk left %s\n", label, chunkState(c))
		r.hb.Progress(r.progress, uint64(lo.Unix())) //nolint:gosec // post-1970 timestamp
		return nil
	}

	if c.Compressed {
		// Free space is re-measured against THIS chunk right before it is
		// decompressed: the run is long, and the pool is shared.
		if pf := chunkRestampPreflight(ctx, w.chunks, c.UncompressedBytes, w.copts); pf.Err != nil {
			return fmt.Errorf("usd-volume-restamp: %s: pre-flight refused before the decompress (the chunk is untouched): %w", label, pf.Err)
		}
	}
	// Every batch inside the chunk goes through the guarded apply, which
	// reads the chunk's is_compressed ahead of the UPDATE.
	inChunk := func(ctx context.Context, plan *timescale.XLMBaseRestampPlan, generation int64, batch int) (int64, error) {
		return w.chunks.ApplyXLMBaseUSDVolumeRestampInChunk(ctx, c, plan, generation, batch)
	}
	var inside xlmBaseWalk
	res, err := w.chunks.RestampTradesChunk(ctx, c, func(ctx context.Context) error {
		var werr error
		inside, werr = r.walkWith(ctx, lo, hi, xlmBaseWalkFull, inChunk)
		return werr
	}, func(step timescale.ChunkRestampStep) { w.trace(label, c, step) })
	if err != nil {
		if errors.Is(err, timescale.ErrTradesChunkRecompressed) {
			return fmt.Errorf("usd-volume-restamp: %s: STOPPED — the chunk was re-compressed underneath the run after %d row(s) were written into it. "+
				"Nothing was written into it compressed, and the walk does not continue on the per-row path. Check that the compression policy is "+
				"the one this run paused and that nothing else compresses trades chunks (a by-hand compress_chunk, another run), then rerun: %w",
				label, inside.written, err)
		}
		return fmt.Errorf("usd-volume-restamp: %s: %w", label, err)
	}
	r.totals.Merge(inside.stats)
	r.hb.Progress(r.progress, uint64(lo.Unix())) //nolint:gosec // post-1970 timestamp
	if c.Compressed {
		_, _ = fmt.Fprintf(w.out, "%s: changed %d row(s) (planned %d) in %.0fs; bytes %s -> %s -> %s\n",
			label, inside.written, inside.stats.Changed, res.Elapsed.Seconds(),
			fmtBytes(res.BytesBefore), fmtBytes(res.BytesDecompressed), fmtBytes(res.BytesAfter))
		return nil
	}
	_, _ = fmt.Fprintf(w.out, "%s: changed %d row(s) (planned %d) in %.0fs; bytes %s -> %s; chunk left uncompressed (not compressed at listing)\n",
		label, inside.written, inside.stats.Changed, res.Elapsed.Seconds(),
		fmtBytes(res.BytesBefore), fmtBytes(res.BytesAfter))
	return nil
}

// trace prints the by-hand repair BEFORE the statement that may outlive
// the process: a SIGKILL 90 s after SIGTERM drops the connection, the
// server aborts whichever of decompress_chunk / compress_chunk was
// running, and the chunk stays as it was — decompressed, if the
// re-compress was the one interrupted. Neither LEFT DECOMPRESSED nor the
// RESUME line is printed in that case; this is.
func (w *xlmBaseChunkWalk) trace(label string, c timescale.TradeChunk, step timescale.ChunkRestampStep) {
	switch step {
	case timescale.ChunkRestampDecompress:
		_, _ = fmt.Fprintf(w.errw, "%s: decompressing (%s uncompressed). If this process is killed before the chunk's progress line, put the chunk and the policy back by hand:\n    SELECT compress_chunk('%s');\n    %s\n",
			label, fmtBytes(c.UncompressedBytes), c, w.reenableSQL)
	case timescale.ChunkRestampCompress:
		_, _ = fmt.Fprintf(w.errw, "%s: re-compressing — a statement that can outlive SIGTERM's 90 s grace; if this process is killed now the chunk STAYS DECOMPRESSED:\n    SELECT compress_chunk('%s');\n    %s\n",
			label, c, w.reenableSQL)
	}
}

// chunkState names a chunk's compression state for the progress lines.
func chunkState(c timescale.TradeChunk) string {
	if c.Compressed {
		return "compressed"
	}
	return "uncompressed"
}

// clampTradesChunk intersects a chunk's range with the run window
// [from, to). The chunk that holds -from usually starts before it and the
// chunk that holds -to usually ends after it; only the overlap is walked,
// so the run never touches rows outside the window it was given.
func clampTradesChunk(c timescale.TradeChunk, from, to time.Time) (lo, hi time.Time) {
	lo, hi = c.RangeStart, c.RangeEnd
	if from.After(lo) {
		lo = from
	}
	if to.Before(hi) {
		hi = to
	}
	return lo, hi
}

// tradesChunkPlan is the operator-facing summary of the chunks a run
// would walk — printed by the dry run AND the write run before anything
// is decompressed.
type tradesChunkPlan struct {
	Count             int
	CompressedCount   int
	TotalUncompressed int64
	TotalCompressed   int64
	// Largest is the chunk with the biggest uncompressed size; the
	// pre-flight sizes against it.
	Largest timescale.TradeChunk
}

func summarizeTradesChunkPlan(chunks []timescale.TradeChunk) tradesChunkPlan {
	var p tradesChunkPlan
	for _, c := range chunks {
		p.Count++
		if c.Compressed {
			p.CompressedCount++
		}
		p.TotalUncompressed += c.UncompressedBytes
		p.TotalCompressed += c.CompressedBytes
		if c.UncompressedBytes > p.Largest.UncompressedBytes {
			p.Largest = c
		}
	}
	return p
}

// renderTradesChunkPlan renders the chunk plan. A string rather than a
// print so the dry run's output is testable.
func renderTradesChunkPlan(from, to time.Time, chunks []timescale.TradeChunk, p tradesChunkPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== chunk plan: %d trades chunk(s) intersect [%s, %s] — %d compressed, %d not\n",
		p.Count, from.Format(time.DateOnly), to.Format(time.DateOnly), p.CompressedCount, p.Count-p.CompressedCount)
	if p.Count == 0 {
		return b.String()
	}
	fmt.Fprintf(&b, "    uncompressed %s total; largest %s = %s [%s, %s)\n",
		fmtBytes(p.TotalUncompressed), fmtBytes(p.Largest.UncompressedBytes), p.Largest,
		p.Largest.RangeStart.Format(time.DateOnly), p.Largest.RangeEnd.Format(time.DateOnly))
	fmt.Fprintf(&b, "    compressed %s total\n", fmtBytes(p.TotalCompressed))
	fmt.Fprintf(&b, "    %4s  %-46s  %-23s  %-12s  %12s  %12s\n", "#", "chunk", "range", "state", "uncompressed", "compressed")
	for i, c := range chunks {
		fmt.Fprintf(&b, "    %4d  %-46s  %s..%s  %-12s  %12s  %12s\n",
			i+1, c.String(), c.RangeStart.Format(time.DateOnly), c.RangeEnd.Format(time.DateOnly),
			chunkState(c), fmtBytes(c.UncompressedBytes), fmtBytes(c.CompressedBytes))
	}
	if n := p.Count - p.CompressedCount; n > 0 {
		fmt.Fprintf(&b, "    %d chunk(s) not compressed at listing are restamped in place and LEFT uncompressed; the compression policy compresses them on its own schedule once it is re-enabled.\n", n)
	}
	return b.String()
}

// chunkPreflight is the free-space verdict and everything that went into
// it, so the printed line can show its working.
type chunkPreflight struct {
	Largest  int64
	Required uint64
	Path     string
	PathErr  error
	// Measured is statfs's answer for Path; MeasureErr why there is none.
	Measured   uint64
	MeasuredOK bool
	MeasureErr error
	// Override is -min-free-bytes when given.
	Override int64
	// Err is the refusal, nil when the run may proceed.
	Err error
}

// chunkRestampPreflight decides whether a -write run may start — and,
// called again per chunk with that chunk's size, whether the next
// decompress may go ahead.
//
// The figure compared is, in order of preference: -min-free-bytes when
// the operator gave one (trusted as stated, warned about loudly);
// otherwise statfs on the directory the database reports for `trades`.
// Neither available is a refusal, not a guess.
func chunkRestampPreflight(ctx context.Context, store interface {
	TradesDataVolumePath(context.Context) (string, error)
}, largest int64, copts xlmBaseChunkOptions,
) chunkPreflight {
	p := chunkPreflight{Largest: largest, Override: copts.MinFreeBytes}
	if largest > 0 {
		p.Required = uint64(float64(largest) * chunkFreeSpaceHeadroom)
	}
	p.Path, p.PathErr = store.TradesDataVolumePath(ctx)
	if p.PathErr == nil {
		free := copts.FreeBytes
		if free == nil {
			free = freeBytesOnPath
		}
		p.Measured, p.MeasureErr = free(p.Path)
		p.MeasuredOK = p.MeasureErr == nil
	}

	var have uint64
	switch {
	case p.Override > 0:
		have = uint64(p.Override)
	case p.MeasuredOK:
		have = p.Measured
	case p.PathErr != nil:
		p.Err = fmt.Errorf("cannot resolve the data volume (%v); run on the database host as a role that can read data_directory, "+
			"or check free space there yourself and pass -min-free-bytes N", p.PathErr)
		return p
	default:
		p.Err = fmt.Errorf("cannot measure free space on %s (%v) — this host is not the database host; "+
			"check free space there yourself and pass -min-free-bytes N", p.Path, p.MeasureErr)
		return p
	}
	if have <= p.Required {
		p.Err = fmt.Errorf("free space %s is not more than %s (%.1f x the chunk's uncompressed size, %s) — "+
			"decompressing that chunk could fill the data volume; free space or narrow the window",
			p.describeHave(have), fmtBytes(int64(p.Required)), chunkFreeSpaceHeadroom, fmtBytes(p.Largest)) //nolint:gosec // Required derives from an int64
	}
	return p
}

// describeHave names the figure the verdict used and where it came from.
func (p chunkPreflight) describeHave(have uint64) string {
	if p.Override > 0 {
		return fmt.Sprintf("%s (-min-free-bytes, NOT measured)", fmtBytes(p.Override))
	}
	return fmt.Sprintf("%s on %s", fmtBytes(int64(have)), p.Path) //nolint:gosec // statfs product, bounded by the volume size
}

// render is the pre-flight block as printed.
func (p chunkPreflight) render() string {
	var b strings.Builder
	switch {
	case p.Override > 0:
		fmt.Fprintf(&b, "pre-flight: free %s (-min-free-bytes, NOT measured)\n", fmtBytes(p.Override))
		b.WriteString("WARNING: trusting -min-free-bytes as the free space on the data volume; nothing was measured, and nothing is re-measured per chunk.\n")
		if p.MeasuredOK {
			fmt.Fprintf(&b, "         (statfs on %s reports %s free on THIS host)\n", p.Path, fmtBytes(int64(p.Measured))) //nolint:gosec // statfs product
		}
	case p.MeasuredOK:
		fmt.Fprintf(&b, "pre-flight: free %s on %s (measured; re-measured before every decompress)\n", fmtBytes(int64(p.Measured)), p.Path) //nolint:gosec // statfs product
	default:
		b.WriteString("pre-flight: free space UNKNOWN\n")
	}
	verdict := "OK"
	if p.Err != nil {
		verdict = "REFUSED"
	}
	fmt.Fprintf(&b, "            need > %s (%.1f x the largest chunk's uncompressed size; a guard, not a bound) — %s\n",
		fmtBytes(int64(p.Required)), chunkFreeSpaceHeadroom, verdict) //nolint:gosec // Required derives from an int64
	return b.String()
}

// freeBytesOnPath is statfs: the bytes an unprivileged writer could still
// put on the filesystem holding path.
func freeBytesOnPath(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return st.Bavail * uint64(st.Bsize), nil //nolint:gosec,unconvert // block size is positive; the field's width differs per OS
}

// chunkRestampResumeHint is the chunk walk's RESUME line: the same
// command, carrying the run's generation and every flag that shaped its
// population. Finished chunks are probed read-only and skipped; the
// failed chunk was re-compressed unless the error above says LEFT
// DECOMPRESSED.
func chunkRestampResumeHint(cfgPath string, from, to time.Time, opts xlmBaseRestampOptions, copts xlmBaseChunkOptions) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nRESUME: stellarindex-ops usd-volume-restamp -config %s -tier xlm-base -chunks -from %s -to %s -generation %d",
		cfgPath, from.Format(time.DateOnly), to.Format(time.DateOnly), opts.Generation)
	b.WriteString(xlmBaseResumeFlags(opts))
	if copts.Batch != defaultChunkBatch {
		fmt.Fprintf(&b, " -chunk-batch %d", copts.Batch)
	}
	if copts.MinFreeBytes > 0 {
		fmt.Fprintf(&b, " -min-free-bytes %d", copts.MinFreeBytes)
	}
	if copts.AllowLiveAdjacent {
		b.WriteString(" -allow-live-adjacent")
	}
	if opts.Write {
		b.WriteString(" -write")
	}
	b.WriteString("\n  (finished chunks are probed read-only and skipped; the failed chunk was re-compressed unless the error says LEFT DECOMPRESSED)\n")
	return b.String()
}

// xlmBaseResumeFlags renders the flags BOTH walks' RESUME lines carry:
// everything that shapes the population (-fill-null, -sources,
// -max-generation, -min-rel-delta) and the slice. A resume that dropped
// -sources would restamp every DEX source where one was asked for; one
// that dropped -max-generation would admit rows the first run excluded.
func xlmBaseResumeFlags(opts xlmBaseRestampOptions) string {
	var b strings.Builder
	if opts.FillNull {
		b.WriteString(" -fill-null")
	}
	if opts.Slice != time.Hour {
		fmt.Fprintf(&b, " -slice %s", opts.Slice)
	}
	if len(opts.Allow) > 0 {
		sources := make([]string, 0, len(opts.Allow))
		for s := range opts.Allow {
			sources = append(sources, s)
		}
		sort.Strings(sources)
		fmt.Fprintf(&b, " -sources %s", strings.Join(sources, ","))
	}
	if opts.MaxGeneration != opts.Generation {
		fmt.Fprintf(&b, " -max-generation %d", opts.MaxGeneration)
	}
	if opts.MinRelDelta != nil {
		fmt.Fprintf(&b, " -min-rel-delta %s", opts.MinRelDelta.FloatString(6))
	}
	return b.String()
}

// defaultChunkBatch is -chunk-batch's default. Ten times the day walk's
// -batch: inside a decompressed chunk an UPDATE is a plain heap write,
// so the per-transaction footprint the smaller batch bounded is gone.
const defaultChunkBatch = 20_000

// validateRestampGeneration checks an explicit -generation. Negative is
// nonsense; a value in the FUTURE is worse: every default run's
// -max-generation is its own start time, so rows stamped above that sit
// beyond the reach of every later run and can never be re-derived again
// — a typo here is a permanent lock-out on a money column.
func validateRestampGeneration(gen int64, now time.Time) error {
	if gen < 0 {
		return fmt.Errorf("usd-volume-restamp: -generation must be >= 0, got %d", gen)
	}
	if nowUnix := now.Unix(); gen > nowUnix {
		return fmt.Errorf("usd-volume-restamp: -generation %d is in the future (now is %d): rows stamped with it would sit above every default run's "+
			"-max-generation and could never be re-derived again — pass the generation a previous run printed, or nothing", gen, nowUnix)
	}
	return nil
}

// fmtBytes renders a byte count the way pg_size_pretty does (1024-based,
// one decimal), so the plan reads like the psql the operator checks it
// against.
func fmtBytes(n int64) string {
	const unit = 1024
	if n < unit && n > -unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit || m <= -unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}
