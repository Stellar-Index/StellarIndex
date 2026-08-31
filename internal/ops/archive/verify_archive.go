package archive

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"
	"github.com/stellar/go-stellar-sdk/support/datastore"
	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

// verifyArchive runs one or more verification tiers against a
// galexie bucket. Per `docs/operations/galexie-backfill.md` and
// ADR-0017, each tier addresses a distinct trust failure mode:
//
//   - Tier A (chain): chain-link integrity — for each ledger N,
//     ledger[N].Header.PreviousLedgerHash == ledger[N-1].Hash.
//     Catches internal corruption, dropped ledgers, replay
//     divergence regardless of upstream trust.
//   - Tier B (checkpoint): cross-check our LCM's hash at every
//     64-ledger checkpoint against the canonical header-hash
//     in the local history-archive (`ledger-XXXXXXXX.xdr.gz`).
//     Catches single-source corruption that's still chain-link-
//     consistent.
//   - Tier D (peers): sample checkpoints within the range and
//     cross-compare history-XXXXXXXX.json across N tier-1
//     validator archives. Consensus-level cryptographic
//     agreement.
//   - Tier E (archivist): shell out to stellar-archivist for a
//     full bucket-by-bucket sha256 audit.
//
// `-tier all` runs every tier sequentially. Any tier mismatch is
// a hard stop with the diverging ledger numbers and hashes
// printed for diagnosis.
//
// Defaults:
//   - bucket: cfg.Storage.S3BucketArchive, falling back to
//     S3BucketLive when -bucket is unset AND S3BucketArchive is
//     empty. Usually set -bucket explicitly when verifying the
//     historical half.
//   - from: 2 (ledger 1 has no predecessor; the chain-link check
//     starts from ledger 2).
//   - to: 0 = unbounded. For a bounded verify of a specific range
//     set both -from and -to.
func verifyArchive(args []string) (retErr error) { //nolint:funlen,gocognit,gocyclo // linear diagnostic; splitting reduces readability
	fs := flag.NewFlagSet("verify-archive", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	bucketOverride := fs.String("bucket", "", "Override bucket name (default: storage.s3_bucket_archive, then s3_bucket_live)")
	from := fs.Uint("from", 2, "First ledger to verify (inclusive, default 2 — ledger 1 has no predecessor)")
	to := fs.Uint("to", 0, "Last ledger to verify (inclusive, 0 = unbounded/live)")
	tier := fs.String("tier", "chain", "Verification tier: chain (A) | checkpoint (B) | peers (D) | archivist (E) | all")
	archiveRoot := fs.String("archive-root", "/srv/history-archive",
		"Path to local rs-stellar-archivist mirror (used by checkpoint/all tier)")
	peerList := fs.String("peers", "",
		"Comma-separated peer archive URLs for Tier D (empty → built-in tier-1 default set)")
	peerSamples := fs.Int("peer-samples", 20,
		"Number of checkpoints to sample for Tier D cross-peer diff")
	archivistBin := fs.String("archivist-bin", "stellar-archivist",
		"Path to rs-stellar-archivist binary for Tier E (used in archivist/all tier)")
	archivistURL := fs.String("archivist-url", "",
		"Archive URL for Tier E (empty → file://<archive-root>)")
	archivistTimeout := fs.Duration("archivist-timeout", 30*time.Minute,
		"Maximum runtime for the rs-stellar-archivist scan command")
	failOnMissed := fs.Bool("fail-on-missed", false,
		"Treat checkpointsMissed > 0 as a hard failure (ADR-0017 X1.7). "+
			"Default off for backward compat with the operator workflow that "+
			"tolerated scattered missed checkpoints; flip to true after the "+
			"cross-anchor archive bootstrap completes (PRs #200/#202/#203).")
	maxRuntime := fs.Duration("max-runtime", 24*time.Hour,
		"Hard cap on total verification runtime. 0 = no cap (run until "+
			"completion or operator interrupt). Default 24h matches the "+
			"backward-compat behaviour but full-archive runs that exceed "+
			"the cap need 0 to avoid context-deadline-exceeded mid-walk.")
	workers := fs.Int("workers", 1,
		"Parallel chunk-walk workers. Each handles a contiguous "+
			"sub-range; cross-chunk chain integrity is stitched after "+
			"all workers complete. 1 (default) preserves the historical "+
			"single-threaded path; 4-8 speeds full-archive runs ~Nx "+
			"until disk I/O on /var/lib/minio saturates. Range [1, 16].")
	resumeFromHash := fs.String("resume-from-hash", "",
		"Expected hash (hex) of the ledger immediately before -from "+
			"(i.e. ledger -from − 1). When set, the first chunk's "+
			"FirstPrevHash must match this value or verification fails. "+
			"Used after a previous run halted partway: the operator "+
			"records the previous run's last verified ledger hash and "+
			"passes it here to prove the cross-run boundary explicitly. "+
			"Empty (default) skips the check — the implicit-overlap "+
			"proof from re-reading -from itself is usually sufficient.")
	metricsListen := fs.String("metrics-listen", "",
		"Bind address for a Prometheus /metrics endpoint scraped during "+
			"the run (e.g. 127.0.0.1:9479). Per-chunk counters + gauges "+
			"let operators dashboard the bottleneck during multi-hour "+
			"sweeps rather than guessing from log tails. Empty (default) "+
			"disables the endpoint.")
	textfileOutput := fs.String("textfile-output", "",
		"Path to write the mismatch counter as a Prometheus textfile "+
			"(node_exporter textfile_collector format), e.g. "+
			"/var/lib/node_exporter/textfile_collector/verify_archive_tier_a.prom. "+
			"This is the export path the P1 stellarindex_stellar_archive_divergence "+
			"page reads on r1 — -metrics-listen cannot serve it because the "+
			"process exits between scrapes (issue #282). Empty (default) = "+
			"no emit.")
	stateFile := fs.String("state-file", "",
		"Path to a JSON state file persisting LastVerifiedLedger per "+
			"tier across runs (e.g. /var/lib/stellarindex/verify-archive-state.json). "+
			"Empty disables both reading and writing — every run is "+
			"full-from-scratch, matching the pre-incremental behaviour.")
	fromLastVerified := fs.Bool("from-last-verified", false,
		"Compute -from from the prior state's LastVerifiedLedger minus "+
			"the safety overlap window, instead of using the -from flag "+
			"value directly. Requires -state-file. Skipped (falls back "+
			"to -from) when the state file is missing or has no prior "+
			"entry for this tier.")
	safetyOverlap := fs.Uint("safety-overlap", 5000,
		"Number of ledgers to re-verify behind the prior LastVerifiedLedger "+
			"when -from-last-verified is set. Catches any anomalies that "+
			"snuck in just before the last run's high-water mark. Default "+
			"5000 ledgers ≈ 17h of chain history at 12s/ledger.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *workers < 1 || *workers > 16 {
		return fmt.Errorf("-workers must be in [1, 16] (got %d)", *workers)
	}
	doChain := *tier == "chain" || *tier == "all"
	doCheckpoint := *tier == "checkpoint" || *tier == "all"
	doPeers := *tier == "peers" || *tier == "all"
	doArchivist := *tier == "archivist" || *tier == "all"
	if !doChain && !doCheckpoint && !doPeers && !doArchivist {
		return fmt.Errorf("unknown -tier %q (expected chain | checkpoint | peers | archivist | all)", *tier)
	}
	if *cfgPath == "" {
		return fmt.Errorf("-config is required")
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}

	bucket := *bucketOverride
	if bucket == "" {
		bucket = cfg.Storage.S3BucketArchive
	}
	if bucket == "" {
		bucket = cfg.Storage.S3BucketLive
	}
	if bucket == "" {
		return fmt.Errorf("no bucket resolved — set -bucket or storage.s3_bucket_archive / storage.s3_bucket_live")
	}

	fmt.Fprintf(os.Stderr, "verify-archive: bucket=%s range=[%d,%d] tier=%s\n", bucket, *from, *to, *tier)
	if doCheckpoint {
		fmt.Fprintf(os.Stderr, "verify-archive: checkpoint anchor against %s\n", *archiveRoot)
	}

	// systemd Type=notify integration: signal READY=1 once at start
	// (so the unit transitions from "activating" to "active") and
	// then ping WATCHDOG=1 every 30s for the rest of the process's
	// life. The matching unit sets WatchdogSec=1h, so the walk has
	// up to an hour of true silence before systemd intervenes —
	// orders of magnitude more headroom than the wall-clock-bound
	// TimeoutStartSec the unit used before, and tied to liveness
	// rather than guessed duration. SdNotify is a no-op when
	// $NOTIFY_SOCKET isn't set (manual `stellarindex-ops verify-
	// archive` invocations from a shell), so this is safe outside
	// systemd too.
	//
	// OBS-07: the ping is GATED on observed walk progress. An
	// unconditional 30s ticker feeds the watchdog for as long as the
	// process is alive, which detects a crash (no process, no pings)
	// but is blind to exactly the failure the unit says WatchdogSec is
	// there for — "binary hung / dead-locked". A wedged chunk walker
	// (say a stuck object read) kept the pings flowing forever. See
	// verifyArchiveProgress.
	if _, err := daemon.SdNotify(false, daemon.SdNotifyReady); err != nil {
		fmt.Fprintf(os.Stderr, "verify-archive: warn: sd_notify READY failed: %v\n", err)
	}
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		gate := newWatchdogGate(&verifyArchiveProgress)
		for range t.C {
			if !gate.shouldPing() {
				fmt.Fprintf(os.Stderr, "verify-archive: warn: no ledger verified since the last watchdog interval — withholding WATCHDOG=1 (systemd's WatchdogSec will act if this persists)\n")
				continue
			}
			_, _ = daemon.SdNotify(false, daemon.SdNotifyWatchdog)
		}
	}()

	// Optional /metrics endpoint. Only the chunk walk emits metrics
	// today (Tiers D + E are bounded-time spot checks, not the
	// multi-hour grind that motivates live dashboarding).
	if *metricsListen != "" {
		stop, err := startVerifyArchiveMetrics(*metricsListen)
		if err != nil {
			return fmt.Errorf("metrics endpoint: %w", err)
		}
		defer stop()
		fmt.Fprintf(os.Stderr, "verify-archive: metrics on http://%s/metrics\n", *metricsListen)
	}

	// Durable metrics for the deployed (systemd-timer) shape. Written
	// on EVERY exit path, success or failure: a mismatch aborts the
	// walk with an error, and that is precisely the run whose counter
	// the P1 page needs to see. A run that fails for an unrelated
	// reason (context deadline, S3 outage) contributes zero
	// increments and just re-emits the carried-forward totals, so it
	// cannot false-page. See verify_archive_textfile.go.
	if *textfileOutput != "" {
		defer func() {
			// retErr is the run's verdict at exit. Only a clean run
			// advances the last-success gauge; a failed one carries the
			// prior value forward, so the staleness page measures when
			// verification last SUCCEEDED rather than when the timer
			// last fired (wave-D ALERT-10).
			if werr := writeVerifyArchiveTextfile(
				*textfileOutput, *tier, collectVerifyArchiveMismatches(),
				retErr == nil, time.Now(),
			); werr != nil {
				fmt.Fprintf(os.Stderr, "verify-archive: write textfile %s: %v\n", *textfileOutput, werr)
				// Never mask the verification error with the
				// bookkeeping one — but a run that cannot publish its
				// divergence signal has failed at its job, so surface
				// it when nothing else did.
				if retErr == nil {
					retErr = fmt.Errorf("write textfile %s: %w", *textfileOutput, werr)
				}
				return
			}
			fmt.Fprintf(os.Stderr, "verify-archive: mismatch metrics written to %s\n", *textfileOutput)
		}()
	}

	// Incremental run support — when -state-file is set, read prior
	// state. -from-last-verified overrides the operator's -from with
	// (prior.LastVerifiedLedger - safety_overlap). The resume hash
	// from prior state feeds -resume-from-hash for strict cross-run
	// chain continuity (so an operator running incremental nightly
	// runs gets the same continuity proof an unbroken single-process
	// walk would have).
	priorState, stateErr := readVerifyArchiveState(*stateFile)
	if stateErr != nil {
		return stateErr
	}
	effectiveFrom := uint32(*from)
	effectiveResumeHash := *resumeFromHash
	if *fromLastVerified && *stateFile != "" {
		effectiveFrom = resolveIncrementalFrom(priorState, *tier, uint32(*from), uint32(*safetyOverlap))
		// Resume-from-hash semantics with safety-overlap:
		//   - safety-overlap > 0 means we walk safety-overlap ledgers
		//     BEFORE last-verified to re-validate the seam region.
		//     The resume-hash check compares the FIRST chunk's
		//     FirstPrevHash (= hash of effectiveFrom-1) against the
		//     supplied expected hash. The state file's last_verified_hash
		//     is the hash AT last-verified, not at last-verified -
		//     safety-overlap - 1. Those are different ledgers; the
		//     check tripped on 2026-05-29 ("resume-from-hash boundary
		//     mismatch at ledger 62637780").
		//   - When safety-overlap > 0, the overlap re-walk already
		//     validates chain continuity by stitching chunks; the
		//     explicit resume-hash check is redundant + wrong. Skip
		//     it.
		//   - When safety-overlap == 0 (operator opted into a strict
		//     cross-run boundary check), continue to use the saved
		//     hash — that's the original strict-mode contract.
		if effectiveResumeHash == "" && *safetyOverlap == 0 {
			effectiveResumeHash = resolveIncrementalResumeHash(priorState, *tier)
		}
		// High-water reported from the BOUNDING tier, not the raw -tier
		// key: `-tier all` records under "chain"+"checkpoint" and has no
		// "all" entry to read (DAT-09).
		priorHighWater := uint32(0)
		if bound, ok := incrementalBoundTier(priorState, incrementalStateTiers(*tier)); ok {
			priorHighWater = bound.LastVerifiedLedger
		}
		fmt.Fprintf(os.Stderr, "verify-archive: incremental run, prior state high-water=%d → effective -from=%d (safety overlap %d)\n",
			priorHighWater, effectiveFrom, *safetyOverlap)
	}

	// Tier A + B (LCM walk via ledgerstream). Skipped when tier=peers.
	if doChain || doCheckpoint {
		// verifyArchiveLCMWalk owns the state file's InProgress
		// section for the in-flight period: it seeds it before the
		// walk + writes a per-chunk update after each chunk Done so
		// a SIGTERMed run can resume from the chunk boundary
		// instead of restarting from genesis. We pass priorState in
		// for resume-plan-matching, then re-read after to apply our
		// own end-of-run updateTierState cleanly.
		walkTier := "chain"
		if doCheckpoint && !doChain {
			walkTier = "checkpoint"
		}
		highestLedger, highestHash, err := verifyArchiveLCMWalk(cfg, bucket, effectiveFrom, uint32(*to), *maxRuntime, *workers,
			doChain, doCheckpoint, *archiveRoot, *failOnMissed, effectiveResumeHash,
			*stateFile, walkTier, priorState)
		if err != nil {
			return err
		}
		// Persist incremental state on success. Tier name comes from
		// the operator's -tier flag — "chain", "checkpoint", or "all"
		// (we record under each underlying tier so a future
		// `-tier chain -from-last-verified` run reads the right one).
		//
		// Always write on no-error (even when highestLedger == 0):
		// updateTierState clears the InProgress section, and a no-
		// advance success (every chunk was already Done from the
		// prior run) still needs that cleanup. Re-read first so the
		// per-chunk Done updates the walker wrote during the run
		// aren't clobbered.
		if *stateFile != "" {
			latestState, rerr := readVerifyArchiveState(*stateFile)
			if rerr != nil {
				return fmt.Errorf("re-read state %s: %w", *stateFile, rerr)
			}
			now := time.Now().UTC()
			newState := latestState
			if doChain {
				newState = updateTierState(newState, "chain", highestLedger, highestHash, now)
			}
			if doCheckpoint {
				newState = updateTierState(newState, "checkpoint", highestLedger, "", now)
			}
			if err := writeVerifyArchiveState(*stateFile, newState); err != nil {
				return fmt.Errorf("write state %s: %w", *stateFile, err)
			}
			if highestLedger > 0 {
				fmt.Fprintf(os.Stderr, "verify-archive: state advanced to ledger %d (file: %s)\n",
					highestLedger, *stateFile)
			} else {
				fmt.Fprintf(os.Stderr, "verify-archive: in-progress chunks cleared, no high-water advance (file: %s)\n", *stateFile)
			}
		}
	}

	// Tier D (multi-peer checkpoint diff). Independent of LCM walk.
	if doPeers {
		if err := verifyArchivePeers(uint32(*from), uint32(*to), *peerList, *peerSamples); err != nil {
			return err
		}
	}

	// Tier E (rs-stellar-archivist scan). Independent of LCM walk and peer diff.
	if doArchivist {
		url := *archivistURL
		if url == "" {
			url = "file://" + *archiveRoot
		}
		if err := verifyArchiveArchivist(*archivistBin, url, *archivistTimeout); err != nil {
			return err
		}
	}
	return nil
}

// verifyArchiveLCMWalk runs the Tier A + B passes over every LCM in
// the given bucket range. Split from verifyArchive so Tier D can run
// standalone without the ledgerstream setup.
//
// failOnMissed: when true, a non-zero checkpointsMissed at the end
// of the walk is treated as a hard failure per ADR-0017 X1.7.
// When false (default), missed checkpoints are reported but tolerated
// — matches the pre-bootstrap operator workflow.
// verifyArchiveLCMWalk returns (highestLedger, highestLedgerHashHex,
// err). On success, highestLedger is the maximum LastSeq across all
// chunk results — used by the caller to advance the persisted
// verify-archive state. highestLedgerHashHex is hex-encoded;
// callers carry it forward as -resume-from-hash on the next run.
func verifyArchiveLCMWalk(cfg config.Config, bucket string, from, to uint32, maxRuntime time.Duration, workers int, doChain, doCheckpoint bool, archiveRoot string, failOnMissed bool, resumeFromHash string, stateFile, tier string, priorState VerifyArchiveState) (uint32, string, error) { //nolint:funlen,gocognit,gocyclo
	// verify-archive's purpose is chain-check, not full-coverage
	// delivery — at the trailing edge Galexie may not have uploaded
	// the next 1-2 partition files yet, and the systemd timer fires
	// every 6h so the operator can't ensure -to stays well behind
	// the tip. newBoundedLedgerStreamConfig opts into
	// TolerateTrailingMissing so the SDK's "is missing" error within
	// ~65k ledgers of -to is tolerated; the chain up to the
	// last-delivered ledger is what we'd report anyway. The
	// 2026-05-25 incident (project_62_diagnosis_2026_05_25) was
	// exactly this: bootstrap walked 62.64M ledgers clean, then
	// failed on the trailing-edge missing file.
	lsCfg := opsutil.NewBoundedLedgerStreamConfig(cfg, bucket, workers)

	// maxRuntime == 0 → no cap (uncancellable parent). Operators
	// pass 0 for full-archive runs that exceed any single-day
	// budget; the binary still honours external SIGTERM via the
	// SDK's signal hooks.
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	if maxRuntime > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), maxRuntime)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	defer cancel()

	// Resolve `-to=0` to the current tip when parallel chunking was
	// asked for. `opsutil.SplitRange(from, 0, n)` hits the `to <= from` guard
	// and silently returns ONE chunk — `-workers N` is then dead code,
	// and what should be an N-way parallel walk degrades to a serial
	// one. Bit me on a manual `-from 2 -to 0 -workers 6` bootstrap run
	// that crawled for 22h instead of ~4h. The systemd timer's
	// `-from-last-verified` incremental mode hit the same shape on
	// every fresh-state bootstrap.
	//
	// Resolution: build a one-shot DataStore from the same DataStore
	// config the walkers will use, query FindLatestLedgerSequence,
	// adopt that as the upper bound for splitRange. Closed
	// immediately — the parallel walkers each construct their own.
	// Skipped when workers ≤ 1 (single-chunk serial walk is what
	// `to=0` is FOR; resolving tip there would defeat the live-tail
	// path) and when `to` already names an explicit upper bound.
	//
	// Fail-soft: tip resolution AND the per-chunk workers'
	// BoundedRange PrepareRange both require bucket `ListObjectsV2`
	// permission. Setups with least-privilege MinIO IAM (r1's
	// `stellarindex_reader` grants GetObject only) deny it. Rather
	// than crash the whole walk, log a clear message and demote to
	// single-chunk serial (UnboundedRange — works without List, the
	// pre-this-fix behaviour). An operator who genuinely wants the
	// parallel speedup grants `s3:ListBucket` to the reader and the
	// next walk picks it up automatically.
	if to == 0 && workers > 1 {
		// First: if there's a prior in-progress run whose plan we can
		// reuse, adopt its pinned tip and skip live-tip resolution.
		// Without this, a SIGTERMed bootstrap discards every Done
		// chunk on the next fire because the live tip has moved
		// (Stellar produces ledgers every ~5s — even a 1-min relaunch
		// gap shifts `to`, and resumeChunks fails plan-match). With
		// pinning, the relaunch walks only the un-Done chunks against
		// the original tip; the [old_tip, new_tip] delta is picked up
		// by the next nightly fire's -from-last-verified increment.
		if pinned, ok := pinnedTipFromPriorRun(priorState, tier, from, workers); ok {
			fmt.Fprintf(os.Stderr, "verify-archive: adopting prior run's pinned tip %d (skipping live-tip resolution to enable per-chunk resume)\n", pinned)
			to = pinned
		} else {
			const listGrantHint = "verify-archive: " +
				"falling back to single-chunk serial walk. Parallel mode " +
				"requires bucket ListObjectsV2 (BoundedRange PrepareRange " +
				"in the per-chunk workers needs it too, not just this tip " +
				"resolution); grant `s3:ListBucket` to the reader IAM to " +
				"enable -workers N parallelism."

			ds, dsErr := datastore.NewDataStore(ctx, lsCfg.DataStore)
			if dsErr != nil {
				fmt.Fprintf(os.Stderr, "verify-archive: tip resolution failed (open datastore: %v); %s\n", dsErr, listGrantHint)
				workers = 1
			} else {
				tip, tipErr := datastore.FindLatestLedgerSequence(ctx, ds)
				_ = ds.Close()
				if tipErr != nil {
					fmt.Fprintf(os.Stderr, "verify-archive: tip resolution failed (find latest ledger: %v); %s\n", tipErr, listGrantHint)
					workers = 1
				} else {
					fmt.Fprintf(os.Stderr, "verify-archive: resolved -to=0 → tip %d for %d-way parallel split\n", tip, workers)
					to = tip
				}
			}
		}
	}

	chunks := opsutil.SplitRange(from, to, workers)
	progressEvery := 10 * time.Second
	startedAt := time.Now()

	if len(chunks) > 1 {
		fmt.Fprintf(os.Stderr, "verify-archive: %d workers across %d chunks of ~%d ledgers each\n",
			workers, len(chunks), (to-from+1)/uint32(workers))
	}

	// Resume-from-prior-run: if the prior run's per-chunk progress
	// matches this run's plan (from/to/workers/chunk-count), skip
	// the chunks already Done. This is what makes a SIGTERMed
	// bootstrap multi-night-safe — the next fire picks up where the
	// last one left off, not from genesis. The state-file is owned
	// by the runner inside this function for the in-flight period;
	// main re-reads after we return to apply its own updateTierState.
	filteredChunks, chunkIdxs, resumeReason := resumeChunks(priorState, tier, from, to, workers, chunks)
	if stateFile != "" {
		fmt.Fprintf(os.Stderr, "verify-archive: %s\n", resumeReason)
	}
	if filteredChunks == nil {
		// Every chunk in the prior run was Done — this run is a
		// no-op success. Caller's updateTierState clears InProgress.
		// Return zero highest-ledger so updateTierState doesn't
		// regress the LastVerifiedLedger high-water mark.
		return 0, "", nil
	}

	// Seed the in-flight state with the full unfiltered chunk plan
	// so a SIGTERM mid-run still leaves a coherent in-progress
	// record (including already-Done markers). Mutated under
	// stateMu by the OnChunkDone callback below.
	stateNow := startTierProgress(priorState, tier, from, to, workers, chunks, time.Now().UTC())
	if stateFile != "" {
		if err := writeVerifyArchiveState(stateFile, stateNow); err != nil {
			// Non-fatal — losing the per-chunk resume is worse than
			// degrading to single-pass behaviour for this run.
			fmt.Fprintf(os.Stderr, "verify-archive: warn: initial in-progress state write failed: %v\n", err)
		}
	}
	var stateMu sync.Mutex

	// Arm the systemd watchdog's progress gate for the duration of the
	// walk — see verifyArchiveProgress (OBS-07).
	verifyArchiveProgress.WalkActive.Store(true)
	defer verifyArchiveProgress.WalkActive.Store(false)

	results, walkErr := runVerifyChunks(
		ctx, lsCfg, filteredChunks,
		doChain, doCheckpoint, archiveRoot,
		startedAt, progressEvery,
		chunkOrchestratorOpts{
			ChunkIdxs: chunkIdxs,
			OnChunkDone: func(originalIdx int, res chunkResult) {
				if stateFile == "" {
					return
				}
				stateMu.Lock()
				defer stateMu.Unlock()
				stateNow = markChunkDone(stateNow, tier, originalIdx, res.LastHash, time.Now().UTC())
				if err := writeVerifyArchiveState(stateFile, stateNow); err != nil {
					fmt.Fprintf(os.Stderr,
						"verify-archive: warn: per-chunk state write failed (chunk[%d] Done): %v\n",
						originalIdx, err)
				}
			},
		},
	)

	// Aggregate counters across chunks for the final summary. Match
	// the pre-parallel field naming so log-scrapers don't break.
	// highestLedger / highestHash are reported back to the caller so
	// it can persist incremental-run state via -state-file.
	var (
		verified          int
		mismatches        int
		checkpointsOK     int
		checkpointsMissed int
		highestLedger     uint32
		highestHashHex    string
	)
	for _, r := range results {
		verified += r.Verified
		mismatches += r.Mismatches
		checkpointsOK += r.CheckpointsOK
		checkpointsMissed += r.CheckpointsMissed
		if r.LastSeq > highestLedger {
			highestLedger = r.LastSeq
			highestHashHex = fmt.Sprintf("%x", r.LastHash[:])
		}
	}

	// Stitch cross-chunk boundary chain integrity. Skip on walkErr
	// (chunks may have aborted mid-flight; boundary check would be
	// noisy on partial results).
	var stitchErr error
	if walkErr == nil && doChain {
		stitchErr = stitchChunks(results)
	}

	// Cross-run boundary check: when -resume-from-hash is set, the
	// first chunk's FirstPrevHash must match (proves continuity with
	// a previous verification run that ended at -from − 1). Runs
	// only when no other error has fired and at least one chunk
	// processed a ledger.
	var resumeErr error
	if walkErr == nil && stitchErr == nil && doChain && resumeFromHash != "" && len(results) > 0 && results[0].Verified > 0 {
		resumeErr = checkResumeFromHash(resumeFromHash, results[0].FirstPrevHash, results[0].FirstSeq)
	}

	elapsed := time.Since(startedAt)
	fmt.Fprintf(os.Stderr, "\nverify-archive: verified %d ledgers in %s (%.0f ledgers/s, %d workers)\n",
		verified, elapsed.Round(time.Second), float64(verified)/elapsed.Seconds(), workers)
	if doCheckpoint {
		if failOnMissed {
			fmt.Fprintf(os.Stderr, "verify-archive: checkpoints matched=%d missed=%d (fail-on-missed: any miss = hard failure)\n",
				checkpointsOK, checkpointsMissed)
		} else {
			fmt.Fprintf(os.Stderr, "verify-archive: checkpoints matched=%d missed=%d (missed = archive file absent, not a failure)\n",
				checkpointsOK, checkpointsMissed)
		}
	}
	if walkErr != nil {
		return 0, "", fmt.Errorf("verification FAILED: %w", walkErr)
	}
	if stitchErr != nil {
		return 0, "", fmt.Errorf("verification FAILED: %w", stitchErr)
	}
	if resumeErr != nil {
		return 0, "", fmt.Errorf("verification FAILED: %w", resumeErr)
	}
	if verified == 0 {
		return 0, "", fmt.Errorf("verified 0 ledgers — bucket empty or range out of scope")
	}
	if doChain {
		fmt.Fprintf(os.Stderr, "verify-archive: chain-link integrity OK ✓\n")
	}
	if doCheckpoint {
		if checkpointsOK == 0 && checkpointsMissed > 0 {
			fmt.Fprintf(os.Stderr, "verify-archive: checkpoint anchor INCONCLUSIVE — %d missed, 0 matched (archive mirror may be stale)\n", checkpointsMissed)
		} else {
			fmt.Fprintf(os.Stderr, "verify-archive: checkpoint anchor OK ✓  (%d matched, %d missed)\n", checkpointsOK, checkpointsMissed)
		}
		if err := checkpointAnchorDecision(checkpointsOK, checkpointsMissed, failOnMissed); err != nil {
			return 0, "", err
		}
	}
	_ = mismatches // reserved for future exit-code semantics
	return highestLedger, highestHashHex, nil
}

// checkpointAnchorDecision is the DB/archive-free core of the
// checkpoint-tier verdict: it decides whether the accumulated
// OK/Missed tallies are acceptable, independent of failOnMissed
// where that flag doesn't apply. Pure — unit-testable without a live
// archive walk.
//
// DAT-09: checkpointsOK == 0 && checkpointsMissed > 0 (every
// checkpoint anchor missed — the run verified NOTHING against the
// cross-anchor archive) is fatal REGARDLESS of failOnMissed. This is
// distinct from a PARTIAL miss (some matched, some missed), which
// only fails when the operator opted into -fail-on-missed. An
// all-missed range was never actually anchored, so it must not be
// certified complete or advance the checkpoint tier's
// LastVerifiedLedger — the caller skips the state-persist on any
// non-nil error returned here.
func checkpointAnchorDecision(checkpointsOK, checkpointsMissed int, failOnMissed bool) error {
	if checkpointsOK == 0 && checkpointsMissed > 0 {
		return fmt.Errorf("verification FAILED: checkpoint anchor inconclusive — %d missed, 0 matched — nothing was anchored", checkpointsMissed)
	}
	if failOnMissed && checkpointsMissed > 0 {
		return fmt.Errorf("verification FAILED: %d checkpoint(s) missing from cross-anchor archive (with -fail-on-missed per ADR-0017 X1.7)", checkpointsMissed)
	}
	return nil
}

// defaultTier1Peers is a representative set of tier-1 validator
// history-archive roots — one URL per operator-org. Chosen from the
// HISTORY entries in /etc/stellar/captive-core-galexie.cfg and
// cross-referenced against SEP-20 home-domain declarations.
//
// Each org runs 3 archives behind the same SCP quorum set; picking
// one per org is sufficient — if org A's nodes disagree internally,
// that's a different (intra-org) problem than what Tier D surfaces.
// Operators can override with -peers if they want more coverage.
var defaultTier1Peers = []string{
	"https://bootes-history.publicnode.org",
	"https://archive.v1.stellar.lobstr.co",
	"https://stellar-history-de-fra.satoshipay.io",
	"https://stellar-history-usc.franklintempleton.com/azuscshf401",
	"https://alpha-history.validator.stellar.creit.tech",
	"http://history.stellar.org/prd/core-live/core_live_001",
	"https://stellar-full-history1.bdnodes.net",
}

// historyCheckpoint is the subset of a history-XXXXXXXX.json that we
// compare across peers. We ignore `server` (version of stellar-core
// that built the archive — varies by operator) and `version` (schema
// version, rarely changes). What must agree across the network is
// the consensus state: currentLedger + the bucket-list hashes.
type historyCheckpoint struct {
	CurrentLedger  uint32          `json:"currentLedger"`
	CurrentBuckets []historyBucket `json:"currentBuckets"`
}

type historyBucket struct {
	Curr string          `json:"curr"`
	Snap string          `json:"snap"`
	Next json.RawMessage `json:"next"` // opaque; compare raw bytes
}

// verifyArchiveProgress is the process-wide liveness signal the
// systemd watchdog ticker reads. Ledgers is bumped by every chunk
// worker for each ledger it verifies; WalkActive is true only for the
// duration of the LCM walk.
//
// Package-level because there is exactly one walk per process (this is
// a CLI subcommand, not a library) and the ticker goroutine starts
// before the walk does — the same shape the obs.VerifyArchive*
// collectors already use.
var verifyArchiveProgress verifyArchiveProgressTracker

type verifyArchiveProgressTracker struct {
	WalkActive atomic.Bool
	Ledgers    atomic.Uint64
}

// watchdogGate decides, once per watchdog interval, whether to feed
// systemd's watchdog. While the LCM walk is running the ping requires
// real forward progress; outside the walk it is unconditional, because
// the other tiers (peer diff, archivist scan) are each bounded by
// their own timeouts and verify no ledgers — gating on the ledger
// counter there would SIGTERM healthy work.
type watchdogGate struct {
	progress *verifyArchiveProgressTracker
	last     uint64
}

func newWatchdogGate(p *verifyArchiveProgressTracker) *watchdogGate {
	return &watchdogGate{progress: p, last: p.Ledgers.Load()}
}

func (g *watchdogGate) shouldPing() bool {
	n := g.progress.Ledgers.Load()
	if g.progress.WalkActive.Load() && n == g.last {
		return false
	}
	g.last = n
	return true
}

// peerArchiveTip resolves how far the peer archives have actually
// published, by reading each one's .well-known/stellar-history.json —
// the same schema as a checkpoint file, whose currentLedger IS the
// archive's tip. Returns the LOWEST tip across the responding peers:
// a checkpoint above that hasn't been uploaded by everyone yet, so
// sampling it would diff a missing file against a present one and read
// as a phantom divergence. ok=false when no peer answered at all —
// the caller refuses to guess a range rather than inventing one.
func peerArchiveTip(client *http.Client, peers []string) (uint32, bool) {
	var tip uint32
	found := false
	for _, p := range peers {
		cp, err := fetchHistoryCheckpoint(client, strings.TrimRight(p, "/")+"/.well-known/stellar-history.json")
		if err != nil {
			fmt.Fprintf(os.Stderr, "verify-archive: peer tip probe failed for %s: %v\n", p, err)
			continue
		}
		if cp.CurrentLedger == 0 {
			continue
		}
		if !found || cp.CurrentLedger < tip {
			tip = cp.CurrentLedger
			found = true
		}
	}
	return tip, found
}

// peerCheckpointBounds returns the first and last checkpoint ledger
// (seq mod 64 == 63) contained in [from, to], erroring when the range
// holds none.
//
// The arithmetic runs through uint64 on purpose: the naive unsigned
// form underflowed for sub-checkpoint inputs — `(to/64*64)-1` wrapped
// to ~4.29e9 for any to < 64, which then passed the `lastCP < firstCP`
// guard and sent the sampler chasing checkpoints past the end of the
// chain. It also picked the checkpoint BELOW `to` when `to` was itself
// a checkpoint; the ceiling form here includes it.
func peerCheckpointBounds(from, to uint32) (uint32, uint32, error) {
	noneErr := fmt.Errorf("range [%d,%d] contains no checkpoint ledgers (checkpoints are at seq mod 64 == 63)", from, to)
	if to < 63 || from > to {
		return 0, 0, noneErr
	}
	// Smallest checkpoint >= from, largest checkpoint <= to.
	firstCP := (uint64(from)+64)/64*64 - 1
	lastCP := (uint64(to)+1)/64*64 - 1
	if firstCP > lastCP {
		return 0, 0, noneErr
	}
	return uint32(firstCP), uint32(lastCP), nil //nolint:gosec // G115: both are <= to, which is a uint32.
}

// verifyArchivePeers samples checkpoints in [from, to] and cross-
// compares each peer's history-XXXXXXXX.json. Any disagreement is a
// consensus-level finding — either one peer has replayed wrong, or
// a fork was retained somewhere. Either way, loud failure.
//
// sampleN is the target number of checkpoints to verify. Actual
// count may be less if the range contains fewer checkpoints; always
// includes the first and last checkpoint for edge coverage.
func verifyArchivePeers(from, to uint32, peerList string, sampleN int) error { //nolint:funlen,gocognit,gocyclo
	peers := defaultTier1Peers
	if peerList != "" {
		peers = strings.Split(peerList, ",")
		for i := range peers {
			peers[i] = strings.TrimSpace(peers[i])
		}
	}
	if len(peers) < 2 {
		return fmt.Errorf("tier peers needs ≥2 archive URLs; got %d", len(peers))
	}

	client := &http.Client{Timeout: 30 * time.Second}

	// Resolve an unbounded -to against the peers themselves (OBS-07).
	// This used to fabricate `lastCP = firstCP + 640` — with the
	// default -from 2 that sampled checkpoints 63..703, i.e. ten
	// GENESIS-era slots, then printed "peer cross-check OK". The whole
	// point of Tier D is consensus agreement on what we hold NOW, so a
	// run that never looks above ledger 703 certifies nothing while
	// reading as a pass.
	effectiveTo := to
	if effectiveTo == 0 {
		tip, ok := peerArchiveTip(client, peers)
		if !ok {
			return fmt.Errorf("tier peers: -to 0 means \"up to the archives' current ledger\", but no peer answered .well-known/stellar-history.json — pass an explicit -to")
		}
		effectiveTo = tip
		fmt.Fprintf(os.Stderr, "verify-archive: peer diff — -to 0 resolved to the lowest peer archive tip %d\n", effectiveTo)
	}

	firstCP, lastCP, err := peerCheckpointBounds(from, effectiveTo)
	if err != nil {
		return err
	}

	// Sample evenly-spaced checkpoints. Always include first and last.
	samples := []uint32{firstCP}
	if lastCP != firstCP && sampleN > 1 {
		stride := uint32(1)
		totalCP := (lastCP-firstCP)/64 + 1
		if uint32(sampleN) < totalCP {
			stride = totalCP / uint32(sampleN)
		}
		for seq := firstCP + stride*64; seq < lastCP; seq += stride * 64 {
			samples = append(samples, seq)
		}
		if samples[len(samples)-1] != lastCP {
			samples = append(samples, lastCP)
		}
	}

	fmt.Fprintf(os.Stderr, "verify-archive: peer diff — %d peers × %d checkpoints in [%d,%d]\n",
		len(peers), len(samples), firstCP, lastCP)
	for _, p := range peers {
		fmt.Fprintf(os.Stderr, "  peer: %s\n", p)
	}

	matches, mismatches := 0, 0
	for _, seq := range samples {
		hexSeq := fmt.Sprintf("%08x", seq)
		relPath := fmt.Sprintf("history/%s/%s/%s/history-%s.json",
			hexSeq[0:2], hexSeq[2:4], hexSeq[4:6], hexSeq)

		observed := make(map[string]historyCheckpoint)
		for _, peer := range peers {
			url := strings.TrimRight(peer, "/") + "/" + relPath
			cp, err := fetchHistoryCheckpoint(client, url)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  ledger %d: peer %s: %v\n", seq, peer, err)
				continue
			}
			observed[peer] = cp
		}
		if len(observed) < 2 {
			fmt.Fprintf(os.Stderr, "  ledger %d: only %d peers responded; skipping (inconclusive)\n",
				seq, len(observed))
			continue
		}

		// Every peer's checkpoint must agree. Pick one as the
		// canonical reference and compare the rest.
		var ref historyCheckpoint
		var refPeer string
		for p, cp := range observed {
			ref = cp
			refPeer = p
			break
		}
		allAgree := true
		for p, cp := range observed {
			if p == refPeer {
				continue
			}
			if !checkpointsEqual(ref, cp) {
				mismatches++
				allAgree = false
				fmt.Fprintf(os.Stderr, "  ledger %d: PEERS DISAGREE\n    ref=%s\n    odd=%s\n",
					seq, refPeer, p)
			}
		}
		if allAgree {
			matches++
			fmt.Fprintf(os.Stderr, "  ledger %d: %d peers agree ✓\n", seq, len(observed))
		}
	}

	fmt.Fprintf(os.Stderr, "\nverify-archive: peer diff — %d consensus-verified checkpoints, %d disagreements\n",
		matches, mismatches)
	if mismatches > 0 {
		return fmt.Errorf("peer cross-check FAILED (%d disagreements)", mismatches)
	}
	if matches == 0 {
		return fmt.Errorf("peer cross-check INCONCLUSIVE — no checkpoint verified across ≥2 peers")
	}
	fmt.Fprintf(os.Stderr, "verify-archive: peer cross-check OK ✓\n")
	return nil
}

// verifyArchiveArchivist runs `<bin> scan <url>` against an archive
// URL (file:// for the local mirror, https:// for any peer's
// published archive) and surfaces the result.
//
// rs-stellar-archivist's scan walks every checkpoint in the
// archive, fetches every referenced bucket file, recomputes the
// sha256 of each, and confirms it matches the manifest. A
// successful scan is a strong integrity signal — orthogonal to
// Tier B (LCM-vs-checkpoint anchor) because Tier B trusts the
// local mirror's manifest, while Tier E re-validates the manifest
// itself by recomputing every bucket hash.
//
// We don't parse the binary's stdout structurally — formatting
// shifts across rs-stellar-archivist releases. Instead we stream
// the output to our stderr (so the operator sees progress) and
// rely on the exit code.
//
// Failure modes:
//   - bin not on $PATH                    → ErrNotFound, exits 127
//   - archive URL doesn't resolve         → non-zero exit
//   - any checkpoint / bucket fails hash  → non-zero exit
//   - takes longer than the timeout       → ctx cancel, killed
//
// The CLI flag default is "stellar-archivist" (the Go binary
// shipped with stellar-archivist). Operators using the Rust port
// (`rs-stellar-archivist`) override via `-archivist-bin`.
func verifyArchiveArchivist(bin, url string, timeout time.Duration) error {
	fmt.Fprintf(os.Stderr, "verify-archive: archivist scan bin=%s url=%s timeout=%s\n",
		bin, url, timeout)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// gosec G204: bin + url are operator-supplied diagnostic flags
	// on a CLI that ALREADY shells the operator's environment —
	// any "untrusted input" boundary at this point has already
	// been crossed by the operator running this command at all.
	cmd := exec.CommandContext(ctx, bin, "scan", url) //nolint:gosec // operator-supplied flags

	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// CommandContext closes stdin and surfaces a context-deadline
		// exit as a *exec.Error wrapping context.DeadlineExceeded;
		// preserve that signal.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("archivist scan timed out after %s — re-run with longer -archivist-timeout", timeout)
		}
		return fmt.Errorf("archivist scan FAILED: %w", err)
	}
	fmt.Fprintf(os.Stderr, "verify-archive: archivist scan OK ✓\n")
	return nil
}

// fetchHistoryCheckpoint retrieves and parses one history-XXXXXXXX.json
// from a peer archive.
func fetchHistoryCheckpoint(client *http.Client, url string) (historyCheckpoint, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return historyCheckpoint{}, err
	}
	req.Header.Set("User-Agent", "stellar-index/verify-archive")
	resp, err := client.Do(req)
	if err != nil {
		return historyCheckpoint{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return historyCheckpoint{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MiB cap
	if err != nil {
		return historyCheckpoint{}, err
	}
	var cp historyCheckpoint
	if err := json.Unmarshal(body, &cp); err != nil {
		return historyCheckpoint{}, fmt.Errorf("parse: %w", err)
	}
	return cp, nil
}

// checkpointsEqual compares the consensus-state fields of two
// history-XXXXXXXX.json records. Ignores server + version which
// vary legitimately across operators.
func checkpointsEqual(a, b historyCheckpoint) bool {
	if a.CurrentLedger != b.CurrentLedger {
		return false
	}
	if len(a.CurrentBuckets) != len(b.CurrentBuckets) {
		return false
	}
	for i := range a.CurrentBuckets {
		if a.CurrentBuckets[i].Curr != b.CurrentBuckets[i].Curr ||
			a.CurrentBuckets[i].Snap != b.CurrentBuckets[i].Snap ||
			string(a.CurrentBuckets[i].Next) != string(b.CurrentBuckets[i].Next) {
			return false
		}
	}
	return true
}

// readArchivedLedgerHash fetches the canonical ledger-hash for
// ledger seq from the local rs-stellar-archivist mirror. seq must
// be a checkpoint ledger (seq % 64 == 63) — that's the last ledger
// in the file named ledger-<hex(seq)>.xdr.gz at path
// <archiveRoot>/ledger/XX/YY/ZZ/ where XX,YY,ZZ are the first three
// bytes of the hex-encoded sequence.
//
// The file is a gzipped, self-delimiting XDR stream of
// LedgerHeaderHistoryEntry records (64 of them, covering ledgers
// seq-63 through seq). We scan until the entry matching seq, then
// return entry.Hash.
//
// Returns (hash, true, nil) on success, (_, false, nil) if the file
// doesn't exist on disk (archive mirror hasn't synced that far), or
// (_, _, err) on any real read/parse error.
func readArchivedLedgerHash(archiveRoot string, seq uint32) (sdkxdr.Hash, bool, error) {
	hexSeq := fmt.Sprintf("%08x", seq)
	path := filepath.Join(archiveRoot, "ledger",
		hexSeq[0:2], hexSeq[2:4], hexSeq[4:6],
		fmt.Sprintf("ledger-%s.xdr.gz", hexSeq))

	f, err := os.Open(path) //nolint:gosec // archiveRoot is operator-supplied via flag
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return sdkxdr.Hash{}, false, nil
		}
		return sdkxdr.Hash{}, false, err
	}
	stream, err := sdkxdr.NewGzStream(f)
	if err != nil {
		_ = f.Close()
		return sdkxdr.Hash{}, false, fmt.Errorf("open gz stream: %w", err)
	}
	defer func() { _ = stream.Close() }()

	var entry sdkxdr.LedgerHeaderHistoryEntry
	for {
		if err := stream.ReadOne(&entry); err != nil {
			if errors.Is(err, io.EOF) {
				return sdkxdr.Hash{}, false,
					fmt.Errorf("checkpoint file %s did not contain ledger %d", path, seq)
			}
			return sdkxdr.Hash{}, false, fmt.Errorf("read entry: %w", err)
		}
		if uint32(entry.Header.LedgerSeq) == seq {
			return entry.Hash, true, nil
		}
	}
}

// extractLedgerHeader pulls the header out of an LCM regardless of
// version. V0 (pre-p20) and V1 (p20+) differ in structure; both
// expose a LedgerHeaderHistoryEntry at different paths.
func extractLedgerHeader(lcm sdkxdr.LedgerCloseMeta) (sdkxdr.LedgerHeader, bool) {
	switch lcm.V {
	case 0:
		if lcm.V0 == nil {
			return sdkxdr.LedgerHeader{}, false
		}
		return lcm.V0.LedgerHeader.Header, true
	case 1:
		if lcm.V1 == nil {
			return sdkxdr.LedgerHeader{}, false
		}
		return lcm.V1.LedgerHeader.Header, true
	case 2:
		if lcm.V2 == nil {
			return sdkxdr.LedgerHeader{}, false
		}
		return lcm.V2.LedgerHeader.Header, true
	}
	return sdkxdr.LedgerHeader{}, false
}

// hashToHex renders an xdr.Hash as a lowercase 64-char hex string.
func hashToHex(h sdkxdr.Hash) string {
	return hex.EncodeToString(h[:])
}

// ─── wasm-history ───────────────────────────────────────────────
//
// wasmHistory walks a galexie bucket over [from, to] and tracks
// when each watched contract's instance executable hash changes.
// Detection signal: any LedgerEntryChange (Created or Updated)
// whose entry is a CONTRACT_DATA with a LedgerKeyContractInstance
// key — that's the contract's instance row, and its Val is an
// ScContractInstance whose Executable field carries the WASM hash.
// Both deploys and `update_current_contract_wasm` invocations
// surface the same way.
//
// Output: a JSON document keyed by contract C-strkey, with the
// timeline of (wasm_hash, from_ledger, to_ledger) ranges.
// Read-only — no DB writes, no Timescale, no cursor changes.
//
// Default bucket is cfg.Storage.S3BucketArchive (historical) since
// audits typically span ranges before galexie-live's seam.

type wasmRange struct {
	WasmHash   string `json:"wasm_hash"`
	FromLedger uint32 `json:"from_ledger"`
	ToLedger   uint32 `json:"to_ledger,omitempty"` // 0 = open / current
}

type contractHistory struct {
	Contract string      `json:"contract"`
	Ranges   []wasmRange `json:"ranges"`
}

// wasmContractState tracks the open (most recently seen) WASM hash
// for one contract, plus the closed ranges that preceded it.
type wasmContractState struct {
	ranges  []wasmRange
	current string // current open WASM hash hex; empty = no open range
}

// storageChange is one observation of a watched contract's
// non-Instance ContractData entry being Created/Updated/Restored.
// Captures *what changed* (key + change type) at *when* (ledger),
// without trying to interpret the value (raw key XDR is enough for
// downstream replay / classification).
//
// Used by the optional `-track-storage-rotations` mode to catch
// admin storage flips like Soroswap factory's `set_pair_wasm`
// rotation, factory parameter changes (fee_to_setter, etc.) — all
// the things wasm-history's instance-only filter ignores.
type storageChange struct {
	Ledger     uint32 `json:"ledger"`
	ChangeType string `json:"change_type"` // created | updated | restored
	KeyXDRB64  string `json:"key_xdr_b64"`
	KeyHint    string `json:"key_hint,omitempty"`   // best-effort human-readable summary
	Durability string `json:"durability,omitempty"` // persistent | temporary
}

// contractStorageHistory is the per-contract output shape for the
// storage-rotation tracker. One entry per watched contract that
// had ANY observed non-Instance ContractData change.
type contractStorageHistory struct {
	Contract string          `json:"contract"`
	Changes  []storageChange `json:"changes"`
}

// codeUpload is one observation of a `ContractCode` LedgerEntry
// being Created or Restored — i.e. someone's UploadContractWasm
// host-fn invocation deposited a new WASM blob into ledger state.
//
// Captured globally (not per-watched-contract) because the WASM
// upload is a one-shot event that any contract may later reference
// via its ExecutableHash. Tracking it lets us preserve a complete
// archive of "every WASM ever uploaded over the walked window" for
// retroactive cross-reference — companion to the on-chain
// Soroban-RPC fetch path (which only works for live, non-evicted
// hashes).
type codeUpload struct {
	Ledger     uint32 `json:"ledger"`
	WasmHash   string `json:"wasm_hash"`
	SizeBytes  int    `json:"size_bytes"`
	ChangeType string `json:"change_type"` // created | restored
}
