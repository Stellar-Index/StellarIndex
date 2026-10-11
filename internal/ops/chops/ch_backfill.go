package chops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"
	"golang.org/x/sync/errgroup"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// runCHBackfill walks a bounded ledger range from galexie and writes the Tier-1 rows to
// ClickHouse (ADR-0034). It is idempotent (ReplacingMergeTree). -parallel N splits the range
// into N contiguous chunks, each with its own goroutine and Sink.
func chBackfill(args []string) error {
	fs, gate := opsutil.NewMutatingFlagSet("ch-backfill")
	cfgPath := fs.String("config", "", "path to stellarindex.toml (required)")
	from := fs.Uint("from", 0, "first ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "last ledger sequence (inclusive, required)")
	bucket := fs.String("bucket", "", "galexie bucket override. Default: the range vs ingestion.live_seam_ledger picks archive-or-live; with no seam configured it stays cfg.Storage.S3BucketLive, which does NOT hold historic ranges — pass the archive bucket for those (see opsutil.ResolveStreamBucket)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	flushEvery := fs.Int("flush-every", 500, "flush to ClickHouse every N ledgers (per worker)")
	parallel := fs.Int("parallel", 1, "number of concurrent range-walkers")
	changesOnly := fs.Bool("changes-only", false, "write only the ledgers row (the per-ledger commit marker) and ledger_entry_changes; skips txs/ops/results/participants/events/supply flows. For re-deriving entry-change ordinals without rewriting the other Tier-1 tables")
	heartbeat := fs.String("heartbeat", "", "node_exporter textfile path for the liveness/progress gauges (C6-020). Empty = "+opsutil.DefaultTextfileDir+"/ops_job_ch_backfill.prom when that directory exists (r1), otherwise no heartbeat at all")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" || *from == 0 || *to == 0 || *to < *from {
		return fmt.Errorf("-config, -from, -to are required; -to must be >= -from")
	}
	if err := gate.RequireStatedMode(); err != nil {
		return fmt.Errorf("ch-backfill: %w", err)
	}
	// A dry run still fetches + decodes every ledger and opens no Sink: the
	// ADR-0043 §2.2 restore-drill mode, measuring RTO without adding rows.
	dryRun := gate.DryRun()
	if *parallel < 1 {
		*parallel = 1
	}
	gate.Banner()

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}

	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	streamBucket, berr := backfillBucket(cfg, *bucket, uint32(*from), uint32(*to))
	if berr != nil {
		return fmt.Errorf("ch-backfill: %w", berr)
	}
	lsCfg := opsutil.NewBoundedLedgerStreamConfig(cfg, streamBucket, *parallel)
	passphrase := cfg.Stellar.Passphrase()

	chunks := opsutil.SplitRange(uint32(*from), uint32(*to), *parallel)
	sinkDesc := fmt.Sprintf("ClickHouse %s", *chAddr)
	if dryRun {
		sinkDesc = "DRY RUN (decode only, nothing written)"
	} else if *changesOnly {
		sinkDesc += " (ledgers + ledger_entry_changes only)"
	}
	fmt.Fprintf(os.Stderr, "ch-backfill: streaming ledgers %d..%d from %q -> %s (%d worker(s))\n",
		*from, *to, streamBucket, sinkDesc, len(chunks))

	prog := newBackfillProgress(*heartbeat)
	defer prog.stop()

	g, gctx := errgroup.WithContext(ctx)
	for i, chunk := range chunks {
		i, chunk := i, chunk // capture
		g.Go(func() error {
			return chBackfillChunk(gctx, i, chunk, lsCfg, passphrase, *chAddr, *flushEvery, dryRun, *changesOnly, prog.record)
		})
	}
	walkErr := g.Wait()

	total := prog.total()
	if walkErr != nil && !errors.Is(walkErr, context.Canceled) {
		return fmt.Errorf("ch-backfill: stream (%d done): %w", total, walkErr)
	}
	prog.reportRate()
	// A walk that did not cover its range must NOT exit 0 — see
	// backfillCoverage.
	if cerr := backfillCoverage(uint32(*from), uint32(*to), total, streamBucket, ctx.Err() != nil); cerr != nil {
		return fmt.Errorf("ch-backfill: %w", cerr)
	}
	prog.markOK()
	if dryRun {
		fmt.Fprintf(os.Stderr, "ch-backfill: done — [%d,%d] re-derived from %q (DRY RUN, nothing written)\n", *from, *to, streamBucket)
		return nil
	}
	fmt.Fprintf(os.Stderr, "ch-backfill: done — [%d,%d] complete from %q\n", *from, *to, streamBucket)
	return nil
}

// backfillProgress owns a walk's pace reporting: the periodic stderr rate line and the
// node_exporter heartbeat that tells a wedged walk from a working one. Both share one
// mutex-guarded counter.
type backfillProgress struct {
	hb      *opsutil.JobHeartbeat
	mu      sync.Mutex
	done    int64
	start   time.Time
	lastLog time.Time
	ok      bool
}

// newBackfillProgress starts the heartbeat. heartbeatPath is the -heartbeat
// flag; see opsutil.NewJobHeartbeat for the empty-path resolution (and for
// why this is silently inert off-r1).
func newBackfillProgress(heartbeatPath string) *backfillProgress {
	hb := opsutil.NewJobHeartbeat("ch-backfill", heartbeatPath, nil)
	if hb.Enabled() {
		fmt.Fprintf(os.Stderr, "ch-backfill: heartbeat -> %s\n", hb.Path())
	}
	hb.Start()
	now := time.Now()
	return &backfillProgress{hb: hb, start: now, lastLog: now}
}

// record is the per-ledger callback handed to every chunk worker.
func (p *backfillProgress) record(workerIdx int, seq uint32) {
	p.mu.Lock()
	p.done++
	t := p.done
	if time.Since(p.lastLog) >= 15*time.Second {
		rate := float64(t) / time.Since(p.start).Seconds()
		fmt.Fprintf(os.Stderr, "ch-backfill: %d ledgers (worker %d at %d, %.1f ledgers/s)\n",
			t, workerIdx, seq, rate)
		p.lastLog = time.Now()
	}
	p.mu.Unlock()
	p.hb.Progress(uint64(t), uint64(seq)) //nolint:gosec // t is a non-negative running count; seq is a ledger sequence
}

// total is the number of ledgers walked so far.
func (p *backfillProgress) total() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done
}

// reportRate prints the run's overall throughput line.
func (p *backfillProgress) reportRate() {
	total := p.total()
	elapsed := time.Since(p.start)
	rate := 0.0
	if elapsed.Seconds() > 0 {
		rate = float64(total) / elapsed.Seconds()
	}
	fmt.Fprintf(os.Stderr, "ch-backfill: %d ledgers in %s (%.1f ledgers/s)\n",
		total, elapsed.Round(time.Second), rate)
}

// markOK records that the walk reached its success path. Anything that
// returns before this — including a short/interrupted walk rejected by
// backfillCoverage — stops as a FAILED run, so the published
// stellarindex_ops_job_last_exit_ok matches the process exit code.
func (p *backfillProgress) markOK() {
	p.mu.Lock()
	p.ok = true
	p.mu.Unlock()
}

// stop writes the heartbeat's terminal state. Deferred by chBackfill so it
// runs on every exit path.
func (p *backfillProgress) stop() {
	p.mu.Lock()
	ok := p.ok
	p.mu.Unlock()
	p.hb.Stop(ok)
}

// backfillBucket resolves which galexie bucket a bounded walk reads. The seam policy lives in
// opsutil.ResolveStreamBucket, shared with census-backfill so the rule has one copy.
func backfillBucket(cfg config.Config, override string, from, to uint32) (string, error) {
	return opsutil.ResolveStreamBucket(cfg, override, from, to)
}

// backfillCoverage turns a walk that did not cover its requested range into a hard error naming
// the bucket. TolerateTrailingMissing makes an entirely absent range look like a clean walk, and
// scripts/ops/ch-full-backfill.sh records a window as done only on exit 0, so a vacuous success
// would drop that window forever. A partial or interrupted walk fails too. Re-running is
// idempotent, so failing closed costs a re-run and failing open costs a silent hole.
func backfillCoverage(from, to uint32, streamed int64, bucket string, interrupted bool) error {
	want := int64(to) - int64(from) + 1
	switch {
	case interrupted:
		return fmt.Errorf("INTERRUPTED after %d of %d ledgers in [%d,%d] from %q — the range is NOT complete; re-run it (idempotent under ReplacingMergeTree)",
			streamed, want, from, to, bucket)
	case streamed == 0:
		return fmt.Errorf("streamed ZERO of %d ledgers for [%d,%d] from %q — that bucket does not hold this range (wrong bucket for a historic range is the usual cause: pass -bucket <archive bucket>); NOT recording this range as backfilled",
			want, from, to, bucket)
	case streamed < want:
		return fmt.Errorf("streamed only %d of %d ledgers for [%d,%d] from %q — %d ledgers are missing from the bucket or failed to extract (see the per-ledger errors above); the range is NOT complete",
			streamed, want, from, to, bucket, want-streamed)
	}
	return nil
}

// chBackfillChunk walks one chunk's range with its own Sink. A ClickHouse write failure is
// fatal so errgroup cancels siblings. dryRun opens no Sink and writes nothing, but still
// fetches and decodes every ledger; the ADR-0043 restore drill uses it because
// clickhouse.Open pins the stellar database, so a scratch lake is not possible.
func chBackfillChunk(
	ctx context.Context,
	idx int,
	chunk opsutil.RangeChunk,
	lsCfg ledgerstream.Config,
	passphrase, chAddr string,
	flushEvery int,
	dryRun, changesOnly bool,
	logProgress func(workerIdx int, seq uint32),
) error {
	var sink *clickhouse.Sink
	if !dryRun {
		opened, err := clickhouse.Open(ctx, chAddr, flushEvery)
		if err != nil {
			return err
		}
		sink = opened
		defer func() { _ = sink.Close(ctx) }()
	}

	walkErr := ledgerstream.Stream(ctx, lsCfg, chunk.From, chunk.To,
		func(lcm sdkxdr.LedgerCloseMeta) error {
			ext, eerr := clickhouse.ExtractLedger(lcm, passphrase)
			if eerr != nil {
				fmt.Fprintf(os.Stderr, "ch-backfill: worker %d extract ledger %d: %v\n", idx, lcm.LedgerSequence(), eerr)
				return nil
			}
			if changesOnly {
				ext = changesOnlyExtract(ext)
			}
			if sink != nil {
				if aerr := sink.Add(ctx, ext); aerr != nil {
					return aerr // a ClickHouse write failure is fatal; retry the range
				}
			}
			logProgress(idx, lcm.LedgerSequence())
			return nil
		},
	)
	if walkErr != nil {
		return walkErr
	}
	if sink == nil {
		return nil
	}
	// Clean completion: flush the chunk's tail before Close so the final
	// partial batch lands.
	return sink.Flush(ctx)
}

// changesOnlyExtract keeps an extract's ledgers row and ledger entry changes
// and drops everything else. The ledgers row stays because Sink.Flush writes
// it last as the per-ledger commit marker for the changes. Built from an empty
// value so a table added to LedgerExtract later is excluded, not rewritten.
func changesOnlyExtract(e clickhouse.LedgerExtract) clickhouse.LedgerExtract {
	return clickhouse.LedgerExtract{Ledger: e.Ledger, Changes: e.Changes}
}
