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
	bucket := fs.String("bucket", "", "galexie bucket override. Default: the range vs ingestion.live_seam_ledger picks archive-or-live; with no seam configured it stays cfg.Storage.S3BucketLive, which does NOT hold historic ranges — pass the archive bucket for those (see backfillBucket)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	flushEvery := fs.Int("flush-every", 500, "flush to ClickHouse every N ledgers (per worker)")
	parallel := fs.Int("parallel", 1, "number of concurrent range-walkers")
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
	fmt.Fprintf(os.Stderr, "ch-backfill: streaming ledgers %d..%d from %q -> ClickHouse %s (%d worker(s))\n",
		*from, *to, streamBucket, *chAddr, len(chunks))

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
			return chBackfillChunk(gctx, i, chunk, lsCfg, passphrase, *chAddr, *flushEvery, logProgress)
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
	fmt.Fprintf(os.Stderr, "ch-backfill: done — [%d,%d] complete from %q\n", *from, *to, streamBucket)
	return nil
}

// backfillBucket resolves which galexie bucket a bounded ch-backfill walk
// reads, given the operator's -bucket override and the requested range.
//
// ch-backfill used to default to cfg.Storage.S3BucketLive unconditionally,
// which is silently wrong for exactly the ranges this command exists to
// serve. The live bucket holds only what galexie has exported since this
// node started (on r1 it is also the TRIMMED one — see
// docs/operations/runbooks/consolidated-deploy-plan-2026-07-18.md §4), so a
// historic range resolves to zero objects there. Because every ops walker
// opts into TolerateTrailingMissing (opsutil.NewBoundedLedgerStreamConfig),
// that walk ends WITHOUT an error, and scripts/ops/ch-full-backfill.sh
// records the window as DONE on exit 0 — a permanent hole in the lake,
// recorded as success. backfillCoverage closes the second half of that
// trap; this closes the first.
//
// Resolution order:
//  1. An explicit -bucket always wins: the operator knows their layout, and
//     every runbook that matters already passes it.
//  2. With a live seam configured (ingestion.live_seam_ledger), the RANGE
//     decides — entirely below the seam reads the archive, entirely at or
//     above it reads live. A range that STRADDLES the seam is an error, not
//     a guess: one walk reads one bucket, so either choice silently drops
//     the ledgers on the other side, which is the same vacuous-success
//     failure in a subtler shape.
//  3. With NO seam configured (r1 today), the live bucket — unchanged.
//     Without a seam the live bucket's floor is not knowable from config,
//     and guessing it is what produced this whole class of bug in the
//     first place. Silently switching the default to the archive would
//     also break scripts/ops/ch-live-catchup.sh, which heals live-era
//     holes and extends the tip with no -bucket at all: the archive is an
//     hourly MIRROR of live, so it lags the tip by up to an hour and that
//     timer would fail on every run until the mirror caught up. So the
//     default stays, and backfillCoverage is what makes a wrong bucket
//     unmissable instead of silent — a historic range now fails naming
//     galexie-live and telling the operator to pass the archive bucket.
//     Setting ingestion.live_seam_ledger promotes this host to case 2 and
//     makes the choice automatic, but it also changes the INDEXER's read
//     path (StreamArchiveThenLive), so it is an operator decision, not a
//     default this function should assume.
func backfillBucket(cfg config.Config, override string, from, to uint32) (string, error) {
	if override != "" {
		return override, nil
	}
	archive, live := cfg.Storage.S3BucketArchive, cfg.Storage.S3BucketLive
	seam := cfg.Ingestion.LiveSeamLedger
	switch {
	case seam > 0 && to < seam:
		if archive == "" {
			return "", fmt.Errorf("ledgers %d..%d are below the live seam %d but storage.s3_bucket_archive is unset — pass -bucket", from, to, seam)
		}
		return archive, nil
	case seam > 0 && from >= seam:
		if live == "" {
			return "", fmt.Errorf("ledgers %d..%d are at/above the live seam %d but storage.s3_bucket_live is unset — pass -bucket", from, to, seam)
		}
		return live, nil
	case seam > 0:
		return "", fmt.Errorf("ledgers %d..%d straddle the live seam %d — %q holds [%d,tip] and %q holds the history below it, and one walk reads exactly one bucket; split the range at %d or pass -bucket explicitly",
			from, to, seam, live, seam, archive, seam)
	case live != "":
		return live, nil
	case archive != "":
		return archive, nil
	default:
		return "", fmt.Errorf("no galexie bucket configured — set storage.s3_bucket_archive / s3_bucket_live or pass -bucket")
	}
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
func chBackfillChunk(
	ctx context.Context,
	idx int,
	chunk opsutil.RangeChunk,
	lsCfg ledgerstream.Config,
	passphrase, chAddr string,
	flushEvery int,
	logProgress func(workerIdx int, seq uint32),
) error {
	sink, err := clickhouse.Open(ctx, chAddr, flushEvery)
	if err != nil {
		return err
	}
	defer func() { _ = sink.Close(ctx) }()

	walkErr := ledgerstream.Stream(ctx, lsCfg, chunk.From, chunk.To,
		func(lcm sdkxdr.LedgerCloseMeta) error {
			ext, eerr := clickhouse.ExtractLedger(lcm, passphrase)
			if eerr != nil {
				fmt.Fprintf(os.Stderr, "ch-backfill: worker %d extract ledger %d: %v\n", idx, lcm.LedgerSequence(), eerr)
				return nil
			}
			if aerr := sink.Add(ctx, ext); aerr != nil {
				return aerr // a ClickHouse write failure is fatal; retry the range
			}
			logProgress(idx, lcm.LedgerSequence())
			return nil
		},
	)
	if walkErr != nil {
		return walkErr
	}
	// Clean completion: flush the chunk's tail before Close so the final
	// partial batch lands.
	return sink.Flush(ctx)
}
