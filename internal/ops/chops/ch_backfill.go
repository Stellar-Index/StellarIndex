package chops

import (
	"context"
	"errors"
	"flag"
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

// runCHBackfill walks a bounded ledger range from galexie and writes the
// Tier-1 structural rows to ClickHouse (ADR-0034 Phase 2). Mirrors the
// census-backfill ledgerstream walk; the per-ledger work is
// clickhouse.ExtractLedger -> Sink. Idempotent (ReplacingMergeTree), so a
// re-run over the same range is safe.
//
// -parallel N splits [from,to] into N contiguous chunks, each walked by its
// own goroutine with its own Sink (ClickHouse ingests concurrent writers
// well — this is the throughput unlock Postgres couldn't give us). N=1 is
// the plain single-walker path.
func chBackfill(args []string) error {
	fs := flag.NewFlagSet("ch-backfill", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to stellarindex.toml (required)")
	from := fs.Uint("from", 0, "first ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "last ledger sequence (inclusive, required)")
	bucket := fs.String("bucket", "", "galexie bucket override. Default: the range vs ingestion.live_seam_ledger picks archive-or-live; with no seam configured it stays cfg.Storage.S3BucketLive, which does NOT hold historic ranges — pass the archive bucket for those (see opsutil.ResolveStreamBucket)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	flushEvery := fs.Int("flush-every", 500, "flush to ClickHouse every N ledgers (per worker)")
	parallel := fs.Int("parallel", 1, "number of concurrent range-walkers")
	dryRun := fs.Bool("dry-run", false, "re-derive WITHOUT writing: fetch + decode every ledger in the range and report coverage/throughput, but open no ClickHouse Sink. The ADR-0043 §2.2 restore-drill mode — proves the recovery machinery and measures RTO without adding rows to the live lake")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" || *from == 0 || *to == 0 || *to < *from {
		return fmt.Errorf("-config, -from, -to are required; -to must be >= -from")
	}
	if *parallel < 1 {
		*parallel = 1
	}

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
	if *dryRun {
		sinkDesc = "DRY RUN (decode only, nothing written)"
	}
	fmt.Fprintf(os.Stderr, "ch-backfill: streaming ledgers %d..%d from %q -> %s (%d worker(s))\n",
		*from, *to, streamBucket, sinkDesc, len(chunks))

	var (
		mu      sync.Mutex
		total   int64
		start   = time.Now()
		lastLog = time.Now()
	)
	logProgress := func(workerIdx int, seq uint32) {
		mu.Lock()
		total++
		t := total
		if time.Since(lastLog) >= 15*time.Second {
			rate := float64(t) / time.Since(start).Seconds()
			fmt.Fprintf(os.Stderr, "ch-backfill: %d ledgers (worker %d at %d, %.1f ledgers/s)\n",
				t, workerIdx, seq, rate)
			lastLog = time.Now()
		}
		mu.Unlock()
	}

	g, gctx := errgroup.WithContext(ctx)
	for i, chunk := range chunks {
		i, chunk := i, chunk // capture
		g.Go(func() error {
			return chBackfillChunk(gctx, i, chunk, lsCfg, passphrase, *chAddr, *flushEvery, *dryRun, logProgress)
		})
	}
	walkErr := g.Wait()

	if walkErr != nil && !errors.Is(walkErr, context.Canceled) {
		return fmt.Errorf("ch-backfill: stream (%d done): %w", total, walkErr)
	}
	elapsed := time.Since(start)
	rate := 0.0
	if elapsed.Seconds() > 0 {
		rate = float64(total) / elapsed.Seconds()
	}
	fmt.Fprintf(os.Stderr, "ch-backfill: %d ledgers in %s (%.1f ledgers/s)\n",
		total, elapsed.Round(time.Second), rate)
	// A walk that did not cover its range must NOT exit 0 — see
	// backfillCoverage.
	if cerr := backfillCoverage(uint32(*from), uint32(*to), total, streamBucket, ctx.Err() != nil); cerr != nil {
		return fmt.Errorf("ch-backfill: %w", cerr)
	}
	if *dryRun {
		fmt.Fprintf(os.Stderr, "ch-backfill: done — [%d,%d] re-derived from %q (DRY RUN, nothing written)\n", *from, *to, streamBucket)
		return nil
	}
	fmt.Fprintf(os.Stderr, "ch-backfill: done — [%d,%d] complete from %q\n", *from, *to, streamBucket)
	return nil
}

// backfillBucket resolves which galexie bucket a bounded ch-backfill walk
// reads. The seam policy itself now lives in opsutil.ResolveStreamBucket:
// census-backfill carried an INDEPENDENT copy of the same broken
// live-bucket default (fixed 2026-07-25), and two copies of a subtle seam
// rule is how this defect class survives a remediation. This thin wrapper
// stays so the call site and the tests keep reading in ch-backfill's own
// vocabulary.
func backfillBucket(cfg config.Config, override string, from, to uint32) (string, error) {
	return opsutil.ResolveStreamBucket(cfg, override, from, to)
}

// backfillCoverage turns a walk that did not cover its requested range into
// a hard error, naming the bucket it read.
//
// The defect this closes (found live 2026-07-25): ch-backfill exited 0 after
// streaming ZERO ledgers of a nonzero request. TolerateTrailingMissing —
// which every ops walker sets so a `-to` at the live tip doesn't explode —
// makes an ENTIRELY absent range indistinguishable from a clean walk at the
// ledgerstream layer, so a wrong-bucket run reported success. That success
// is load-bearing: scripts/ops/ch-full-backfill.sh appends the window to its
// resume state only when ch-backfill exits 0, so one vacuous success removes
// that window from the backfill forever and leaves a hole the completeness
// verdict then has to find.
//
// The bar is the command's actual contract — "[from,to] is now in
// ClickHouse" — so a PARTIAL walk fails too. Short counts have exactly two
// causes here and both mean the range is not in the lake: the bucket does
// not hold those objects, or per-ledger extraction failed (chBackfillChunk
// logs and skips those, deliberately, so one bad ledger doesn't abort a
// multi-day run — but the run must not then claim the range). An interrupted
// run fails for the same reason: a SIGINT'd window is not a done window.
// Re-running is free — ClickHouse writes are idempotent under
// ReplacingMergeTree — so failing closed costs a re-run and failing open
// costs a silent hole.
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

// chBackfillChunk walks one chunk's range with its own Sink. A ClickHouse
// write failure is fatal (returns the error so errgroup cancels siblings);
// re-running the range is safe under ReplacingMergeTree.
//
// dryRun opens NO Sink and writes nothing: the walk still fetches every
// galexie object and runs the full clickhouse.ExtractLedger decode, so
// coverage and throughput are measured against the real recovery path, but
// the live lake is untouched. This is what the ADR-0043 §2.2 restore drill
// runs — see the drill's own note on why it cannot use a scratch database
// (clickhouse.Open pins the `stellar` database) and why re-deriving into
// the LIVE lake is the wrong default on a capacity-constrained host.
func chBackfillChunk(
	ctx context.Context,
	idx int,
	chunk opsutil.RangeChunk,
	lsCfg ledgerstream.Config,
	passphrase, chAddr string,
	flushEvery int,
	dryRun bool,
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
