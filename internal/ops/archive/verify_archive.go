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
	"slices"
	"strconv"
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

// verifyArchive runs verification tiers against a galexie bucket: A chain links, B checkpoint hashes vs the local archive, D
// peer archives, E stellar-archivist scan, C SDF sample. `-tier all` (A, B, D, E) runs sequentially and any mismatch is a hard
// stop; defaults: bucket S3BucketArchive else S3BucketLive, from 2 (ledger 1 has no predecessor), to 0 = unbounded.
func verifyArchive(args []string) (retErr error) { //nolint:funlen,gocognit,gocyclo // linear diagnostic; splitting reduces readability
	fs := flag.NewFlagSet("verify-archive", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	bucketOverride := fs.String("bucket", "", "Override bucket name (default: storage.s3_bucket_archive, then s3_bucket_live)")
	from := fs.Uint("from", 2, "First ledger to verify (inclusive, default 2 — ledger 1 has no predecessor)")
	to := fs.Uint("to", 0, "Last ledger to verify (inclusive, 0 = unbounded/live)")
	tier := fs.String("tier", "chain", "Verification tier: chain (A) | checkpoint (B) | peers (D) | archivist (E) | sdf-sample (C) | all")
	archiveRoot := fs.String("archive-root", "/srv/history-archive",
		"Path to local rs-stellar-archivist mirror (used by checkpoint/peers/all tier; Tier D compares its history/ checkpoints to the peers' consensus)")
	peerList := fs.String("peers", "",
		"Comma-separated peer archive URLs for Tier D (empty → built-in tier-1 default set)")
	peerSamples := fs.Int("peer-samples", 20,
		"Number of checkpoints to sample for Tier D cross-peer diff")
	sdfSamples := fs.Int("sdf-samples", 100,
		"Number of random ledgers in [-from, -to] to compare with SDF's dataset for Tier C (sdf-sample; needs explicit -to, not part of -tier all)")
	archivistBin := fs.String("archivist-bin", "stellar-archivist",
		"Path to the stellar-archivist binary for Tier E (used in archivist/all tier); run as `<bin> scan --verify <url>`")
	archivistURL := fs.String("archivist-url", "",
		"Archive URL for Tier E (empty → file://<archive-root>)")
	archivistTimeout := fs.Duration("archivist-timeout", 30*time.Minute,
		"Maximum runtime for the stellar-archivist scan command")
	failOnMissed := fs.Bool("fail-on-missed", true,
		"Treat checkpointsMissed > 0 as a hard failure (ADR-0017 X1.7). "+
			"Counts only checkpoints absent from INSIDE the mirror's own "+
			"coverage span — a checkpoint the walk reached before the mirror's "+
			"fill job did is reported as unmirrored, never as missed, so this "+
			"flag cannot fire on the trailing edge. Default on; "+
			"-fail-on-missed=false tolerates scattered missed checkpoints "+
			"(a run that matched none still fails).")
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
	doSDFSample := *tier == "sdf-sample"
	if !doChain && !doCheckpoint && !doPeers && !doArchivist && !doSDFSample {
		return fmt.Errorf("unknown -tier %q (expected chain | checkpoint | peers | archivist | sdf-sample | all)", *tier)
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
	var mirrorCoverage archiveMirrorCoverage
	if doCheckpoint {
		// Measure the mirror's span BEFORE the walk so an absent checkpoint is attributed: inside the span it is an archive hole, above it the fill job has not got there yet.
		mirrorCoverage = readArchiveMirrorCoverage(*archiveRoot, mirrorLedger)
		fmt.Fprintf(os.Stderr, "verify-archive: checkpoint anchor against %s (mirror coverage %s)\n",
			*archiveRoot, mirrorCoverage)
		if !mirrorCoverage.Known {
			fmt.Fprintf(os.Stderr, "verify-archive: warn: cannot read the cross-anchor mirror's "+
				"checkpoint span under %s/ledger — every absent checkpoint counts as missed\n", *archiveRoot)
		}
	}

	// systemd Type=notify: READY=1 at start, then WATCHDOG=1 every 30s (unit WatchdogSec=1h); SdNotify is a no-op without $NOTIFY_SOCKET.
	// The ping is GATED on observed walk progress: an unconditional ticker is blind to a hung walker (see verifyArchiveProgress).
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

	// Write metrics on EVERY exit path: a mismatch aborts with an error and is exactly the run the P1 page needs.
	// An unrelated failure re-emits carried-forward totals, so it cannot false-page (see verify_archive_textfile.go).
	if *textfileOutput != "" {
		defer func() {
			// retErr is the verdict: only a clean run advances the last-success gauge, so staleness measures the last success, not the last timer fire.
			if werr := writeVerifyArchiveTextfile(
				*textfileOutput, *tier, collectVerifyArchiveMismatches(),
				retErr == nil, time.Now(),
			); werr != nil {
				fmt.Fprintf(os.Stderr, "verify-archive: write textfile %s: %v\n", *textfileOutput, werr)
				// Never mask the verification error with the bookkeeping one — but a run that cannot publish its divergence
				// signal has failed at its job, so surface it when nothing else did.
				if retErr == nil {
					retErr = fmt.Errorf("write textfile %s: %w", *textfileOutput, werr)
				}
				return
			}
			fmt.Fprintf(os.Stderr, "verify-archive: mismatch metrics written to %s\n", *textfileOutput)
		}()
	}

	// With -state-file, read prior state: -from-last-verified overrides -from with (LastVerifiedLedger - safety_overlap),
	// and the saved hash feeds -resume-from-hash for cross-run chain continuity.
	priorState, stateErr := readVerifyArchiveState(*stateFile)
	if stateErr != nil {
		return stateErr
	}
	effectiveFrom := uint32(*from)
	effectiveResumeHash := *resumeFromHash
	if *fromLastVerified && *stateFile != "" {
		effectiveFrom = resolveIncrementalFrom(priorState, *tier, uint32(*from), uint32(*safetyOverlap))
		// safety-overlap > 0 re-walks the seam, which already proves continuity; the saved hash is at last-verified, not at the overlap start, so checking it would always report a bogus mismatch.
		// Only safety-overlap == 0 (strict cross-run check) uses the saved hash.
		if effectiveResumeHash == "" && *safetyOverlap == 0 {
			effectiveResumeHash = resolveIncrementalResumeHash(priorState, *tier)
		}
		// High-water reported from the BOUNDING tier, not the raw -tier
		// key: `-tier all` records under "chain"+"checkpoint" and has no
		// "all" entry to read.
		priorHighWater := uint32(0)
		if bound, ok := incrementalBoundTier(priorState, incrementalStateTiers(*tier)); ok {
			priorHighWater = bound.LastVerifiedLedger
		}
		fmt.Fprintf(os.Stderr, "verify-archive: incremental run, prior state high-water=%d → effective -from=%d (safety overlap %d)\n",
			priorHighWater, effectiveFrom, *safetyOverlap)
	}

	// Tier A + B (LCM walk via ledgerstream). Skipped when tier=peers.
	if doChain || doCheckpoint {
		// verifyArchiveLCMWalk owns the state file's InProgress section during the walk, writing a per-chunk update so a SIGTERMed run resumes at a chunk boundary; priorState is passed for plan matching.
		walkTier := "chain"
		if doCheckpoint && !doChain {
			walkTier = "checkpoint"
		}
		highestLedger, highestHash, err := verifyArchiveLCMWalk(cfg, bucket, effectiveFrom, uint32(*to), *maxRuntime, *workers,
			doChain, doCheckpoint, *archiveRoot, mirrorCoverage, *failOnMissed, effectiveResumeHash,
			*stateFile, walkTier, priorState)
		if err != nil {
			return err
		}
		// Persist incremental state on success, under each underlying tier. Always write on no-error: updateTierState clears InProgress,
		// which every clean run needs. Re-read first so the walker's per-chunk Done updates are not clobbered.
		if *stateFile != "" {
			latestState, rerr := readVerifyArchiveState(*stateFile)
			if rerr != nil {
				return fmt.Errorf("re-read state %s: %w", *stateFile, rerr)
			}
			now := time.Now().UTC()
			newState := latestState
			advancedTo := highestLedger
			if doChain {
				newState = updateTierState(newState, "chain", highestLedger, highestHash, now)
			}
			if doCheckpoint {
				var anchored uint32
				newState, anchored = applyCheckpointTierState(newState, highestLedger, mirrorCoverage, now)
				if !doChain {
					advancedTo = anchored
				}
			}
			if err := writeVerifyArchiveState(*stateFile, newState); err != nil {
				return fmt.Errorf("write state %s: %w", *stateFile, err)
			}
			if highestLedger > 0 {
				fmt.Fprintf(os.Stderr, "verify-archive: state advanced to ledger %d (file: %s)\n",
					advancedTo, *stateFile)
			} else {
				fmt.Fprintf(os.Stderr, "verify-archive: in-progress chunks cleared, no high-water advance (file: %s)\n", *stateFile)
			}
		}
	}

	// Tier D (multi-peer checkpoint diff). Independent of LCM walk.
	if doPeers {
		if err := verifyArchivePeers(effectiveFrom, uint32(*to), *peerList, *peerSamples, *archiveRoot, *failOnMissed); err != nil {
			return err
		}
	}

	// Tier E (stellar-archivist scan --verify). Independent of LCM walk and peer diff.
	if doArchivist {
		url := *archivistURL
		if url == "" {
			url = "file://" + *archiveRoot
		}
		if err := verifyArchiveArchivist(*archivistBin, url, *archivistTimeout); err != nil {
			return err
		}
	}
	if doSDFSample {
		return verifyArchiveSDFSample(cfg, *bucketOverride, uint32(*from), uint32(*to), *sdfSamples)
	}
	return nil
}

// verifyArchiveLCMWalk runs the Tier A + B passes over every LCM in the bucket range and returns (highestLedger, highestLedgerHashHex, err) for -resume-from-hash.
// Checkpoints above mirrorCoverage count as unmirrored, not missed; failOnMissed makes any missed checkpoint a hard failure (ADR-0017 X1.7).
func verifyArchiveLCMWalk(cfg config.Config, bucket string, from, to uint32, maxRuntime time.Duration, workers int, doChain, doCheckpoint bool, archiveRoot string, mirrorCoverage archiveMirrorCoverage, failOnMissed bool, resumeFromHash string, stateFile, tier string, priorState VerifyArchiveState) (uint32, string, error) { //nolint:funlen,gocognit,gocyclo
	// This is a chain check, not full-coverage delivery: Galexie may not have uploaded the last 1-2 partitions, so opt into TolerateTrailingMissing.
	// The chain up to the last-delivered ledger is what gets reported.
	lsCfg := opsutil.NewBoundedLedgerStreamConfig(cfg, bucket, workers)

	// maxRuntime == 0 → no cap (uncancellable parent). Operators pass 0 for full-archive runs that exceed any single-day
	// budget. No signal handler is wired on either branch, so SIGTERM kills the walk outright; chunks already done are in
	// -state-file.
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

	// Resolve -to=0 to the tip for parallel chunking: SplitRange(from, 0, n) returns ONE chunk, so -workers N would silently go serial.
	// Fail-soft: least-privilege MinIO IAM (GetObject only) denies ListObjectsV2, so log and demote to a serial walk; s3:ListBucket restores parallelism.
	if to == 0 && workers > 1 {
		// Adopt a prior in-progress run's pinned tip: the live tip moves every ~5s and would fail plan-match, discarding every Done chunk on relaunch.
		// The [old_tip, new_tip] delta is picked up by the next -from-last-verified run.
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

	// Resume: if the prior run's per-chunk progress matches this plan (from/to/workers/chunk-count), skip Done chunks so a SIGTERMed bootstrap is multi-night-safe.
	filteredChunks, chunkIdxs, resumeReason := planResumedWalk(priorState, tier, from, to, workers, chunks, doCheckpoint)
	if stateFile != "" {
		fmt.Fprintf(os.Stderr, "verify-archive: %s\n", resumeReason)
	}
	if len(filteredChunks) == 0 {
		// planResumedWalk never returns an empty plan. A zero-chunk walk verifies nothing, and exiting nil would advance last_success_unix for a run that anchored nothing.
		return 0, "", fmt.Errorf("verification FAILED: empty walk plan for [%d, %d] across %d worker(s)", from, to, workers)
	}

	// Seed the in-flight state with the full unfiltered chunk plan so a SIGTERM mid-run still leaves a coherent in-progress
	// record (including already-Done markers). Mutated under stateMu by the OnChunkDone callback below.
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
	// walk — see verifyArchiveProgress.
	verifyArchiveProgress.WalkActive.Store(true)
	defer verifyArchiveProgress.WalkActive.Store(false)

	results, walkErr := runVerifyChunks(
		ctx, lsCfg, filteredChunks,
		doCheckpoint, archiveRoot,
		startedAt, progressEvery,
		chunkOrchestratorOpts{
			MirrorCoverage: mirrorCoverage,
			ChunkIdxs:      chunkIdxs,
			OnChunkDone: func(originalIdx int, res chunkResult) {
				if stateFile == "" {
					return
				}
				stateMu.Lock()
				defer stateMu.Unlock()
				stateNow = markChunkDoneStitch(stateNow, tier, originalIdx, res, time.Now().UTC(), doCheckpoint)
				if err := writeVerifyArchiveState(stateFile, stateNow); err != nil {
					fmt.Fprintf(os.Stderr,
						"verify-archive: warn: per-chunk state write failed (chunk[%d] Done): %v\n",
						originalIdx, err)
				}
			},
		},
	)

	// Aggregate counters across chunks for the final summary. Match the pre-parallel field naming so log-scrapers don't break.
	// highestLedger / highestHash are reported back to the caller so it can persist incremental-run state via -state-file.
	var (
		verified       int
		mismatches     int
		highestLedger  uint32
		highestHashHex string
	)
	for _, r := range results {
		verified += r.Verified
		mismatches += r.Mismatches
		if r.LastSeq > highestLedger {
			highestLedger = r.LastSeq
			highestHashHex = fmt.Sprintf("%x", r.LastHash[:])
		}
	}

	// Stitch cross-chunk boundary integrity (skipped on walkErr; applies to any walk, so checkpoint-only runs get gap detection too).
	// Stitch over the FULL plan, not just walked chunks: a resumed run supplies skipped chunks' boundary terms from persisted evidence, else it compares non-adjacent chunks.
	var stitchErr error
	planResults := results
	if walkErr == nil {
		planResults, stitchErr = fullPlanStitchInput(priorState, tier, len(chunks), chunkIdxs, results)
		if stitchErr == nil {
			stitchErr = stitchChunks(planResults)
		} else {
			planResults = results
		}
	}

	// Checkpoint tallies must cover the FULL PLAN (fullPlanStitchInput rebuilds skipped chunks' counts); summing over results would drop a skipped chunk's missed count and let -fail-on-missed pass over a real hole.
	var checkpointsOK, checkpointsMissed, checkpointsUnmirrored int
	if doCheckpoint {
		for _, r := range planResults {
			checkpointsOK += r.CheckpointsOK
			checkpointsMissed += r.CheckpointsMissed
			checkpointsUnmirrored += r.CheckpointsUnmirrored
		}
	}

	// Cross-run boundary check: with -resume-from-hash, the first chunk's FirstPrevHash must match the previous run's end at -from - 1.
	// Indexed off the PLAN, so a skipped chunk 0 compares at effectiveFrom - 1.
	var resumeErr error
	if walkErr == nil && stitchErr == nil && doChain && resumeFromHash != "" && len(planResults) > 0 && planResults[0].Verified > 0 {
		resumeErr = checkResumeFromHash(resumeFromHash, planResults[0].FirstPrevHash, planResults[0].FirstSeq)
	}

	elapsed := time.Since(startedAt)
	fmt.Fprintf(os.Stderr, "\nverify-archive: verified %d ledgers in %s (%.0f ledgers/s, %d workers)\n",
		verified, elapsed.Round(time.Second), float64(verified)/elapsed.Seconds(), workers)
	if doCheckpoint {
		// missed = absent from INSIDE the mirror's coverage span (a hole in the cross-anchor archive). unmirrored = beyond that
		// span, i.e. the walk reached a checkpoint the mirror's fill job has not delivered yet — a lag, not a hole.
		note := "unmirrored = beyond the mirror's coverage, not a failure"
		if failOnMissed {
			note = "fail-on-missed: any in-coverage miss = hard failure"
		}
		fmt.Fprintf(os.Stderr, "verify-archive: checkpoints matched=%d missed=%d unmirrored=%d, mirror coverage %s (%s)\n",
			checkpointsOK, checkpointsMissed, checkpointsUnmirrored, mirrorCoverage, note)
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
		if checkpointsOK == 0 && checkpointsMissed+checkpointsUnmirrored > 0 {
			fmt.Fprintf(os.Stderr, "verify-archive: checkpoint anchor INCONCLUSIVE — 0 matched (%d missed, %d unmirrored; archive mirror may be stale)\n",
				checkpointsMissed, checkpointsUnmirrored)
		} else {
			fmt.Fprintf(os.Stderr, "verify-archive: checkpoint anchor OK ✓  (%d matched, %d missed, %d unmirrored)\n",
				checkpointsOK, checkpointsMissed, checkpointsUnmirrored)
		}
		if err := checkpointAnchorReached(checkpointsOK, checkpointsMissed, checkpointsUnmirrored); err != nil {
			return 0, "", err
		}
		if err := checkpointAnchorDecision(checkpointsOK, checkpointsMissed, failOnMissed); err != nil {
			return 0, "", err
		}
	}
	_ = mismatches // reserved for future exit-code semantics
	return highestLedger, highestHashHex, nil
}

// checkpointAnchorReached restates the all-missed rule under the coverage taxonomy: a checkpoint-tier run that encountered checkpoints but MATCHED none anchored nothing and is not certified.
// Separating unmirrored absences from checkpointsMissed would otherwise let a walk entirely above the mirror report "0 missed". It cannot fire on tier B, whose high-water is clamped to the mirror (checkpointWatermark).
func checkpointAnchorReached(checkpointsOK, checkpointsMissed, checkpointsUnmirrored int) error {
	if checkpointsOK > 0 || checkpointsMissed+checkpointsUnmirrored == 0 {
		return nil
	}
	return fmt.Errorf("verification FAILED: checkpoint anchor inconclusive — 0 matched "+
		"(%d missed, %d beyond the mirror's coverage) — nothing was anchored",
		checkpointsMissed, checkpointsUnmirrored)
}

// checkpointAnchorDecision is the pure core of the checkpoint-tier verdict. OK == 0 with Missed > 0 verified NOTHING and is fatal regardless of failOnMissed;
// a partial miss fails unless -fail-on-missed=false. checkpointsMissed counts only absences inside the mirror's span; callers skip state-persist on error.
func checkpointAnchorDecision(checkpointsOK, checkpointsMissed int, failOnMissed bool) error {
	if checkpointsOK == 0 && checkpointsMissed > 0 {
		return fmt.Errorf("verification FAILED: checkpoint anchor inconclusive — %d missed, 0 matched — nothing was anchored", checkpointsMissed)
	}
	if failOnMissed && checkpointsMissed > 0 {
		return fmt.Errorf("verification FAILED: %d checkpoint(s) missing from cross-anchor archive (with -fail-on-missed per ADR-0017 X1.7)", checkpointsMissed)
	}
	return nil
}

// defaultTier1Peers is one history-archive root per tier-1 operator org, from captive-core-galexie.cfg HISTORY entries; override with -peers.
var defaultTier1Peers = []string{
	"https://bootes-history.publicnode.org",
	"https://archive.v1.stellar.lobstr.co",
	"https://stellar-history-de-fra.satoshipay.io",
	"https://stellar-history-usc.franklintempleton.com/azuscshf401",
	"https://alpha-history.validator.stellar.creit.tech",
	"https://history.stellar.org/prd/core-live/core_live_001",
	"https://stellar-full-history1.bdnodes.net",
}

// historyCheckpoint is the subset of history-XXXXXXXX.json compared across peers: currentLedger and the bucket-list hashes. `server` and `version` vary by operator and are ignored.
type historyCheckpoint struct {
	CurrentLedger  uint32          `json:"currentLedger"`
	CurrentBuckets []historyBucket `json:"currentBuckets"`
}

type historyBucket struct {
	Curr string          `json:"curr"`
	Snap string          `json:"snap"`
	Next json.RawMessage `json:"next"` // opaque; compare raw bytes
}

// verifyArchiveProgress is the process-wide liveness signal the watchdog ticker reads: Ledgers is bumped per verified ledger, WalkActive is true only during the LCM walk.
// Package-level because there is one walk per process and the ticker starts before it.
var verifyArchiveProgress verifyArchiveProgressTracker

type verifyArchiveProgressTracker struct {
	WalkActive atomic.Bool
	Ledgers    atomic.Uint64
}

// watchdogGate decides per interval whether to feed the watchdog: during the LCM walk the ping needs real progress;
// outside it is unconditional, since the other tiers have their own timeouts and gating on the ledger counter would SIGTERM healthy work.
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

// peerTipStalenessWindow (~24h of ledgers) bounds how far a peer's tip may trail the freshest before it is treated as abandoned,
// so a long-dead peer serving a frozen .well-known file cannot cap the run to genesis-adjacent history.
const peerTipStalenessWindow = 17280

// peerArchiveTip returns the LOWEST tip among peers within peerTipStalenessWindow of the freshest (a higher checkpoint would diff missing against present as a phantom divergence);
// staler peers are excluded and logged. ok=false when no peer answered, so the caller refuses to guess a range.
func peerArchiveTip(client *http.Client, peers []string) (uint32, bool) {
	tips := make(map[string]uint32, len(peers))
	var maxTip uint32
	for _, p := range peers {
		cp, err := fetchHistoryCheckpoint(client, strings.TrimRight(p, "/")+"/.well-known/stellar-history.json")
		if err != nil {
			fmt.Fprintf(os.Stderr, "verify-archive: peer tip probe failed for %s: %v\n", p, err)
			continue
		}
		tips[p] = cp.CurrentLedger
		if cp.CurrentLedger > maxTip {
			maxTip = cp.CurrentLedger
		}
	}
	if len(tips) == 0 {
		return 0, false
	}

	var tip uint32
	found := false
	for p, t := range tips {
		if maxTip-t > peerTipStalenessWindow {
			fmt.Fprintf(os.Stderr, "verify-archive: peer %s tip %d trails freshest tip %d by >%d ledgers — excluded as stale, not used to bound the verified window\n",
				p, t, maxTip, peerTipStalenessWindow)
			continue
		}
		if !found || t < tip {
			tip = t
			found = true
		}
	}
	return tip, found
}

// peerCheckpointQuorum is the minimum responders (strict majority of the configured peers) before agreement counts as network consensus; fewer is inconclusive, not verified.
func peerCheckpointQuorum(numPeers int) int {
	return numPeers/2 + 1
}

// peerCheckpointBounds returns the first and last checkpoint ledger (seq mod 64 == 63) in [from, to], erroring when none.
// The arithmetic runs through uint64 on purpose: unsigned `(to/64*64)-1` underflows for to < 64 and misses a checkpoint equal to `to`.
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

// peerSampleCheckpoints returns evenly-spaced checkpoints in [firstCP, lastCP],
// always including both ends even when sampleN is below 2.
func peerSampleCheckpoints(firstCP, lastCP uint32, sampleN int) []uint32 {
	samples := []uint32{firstCP}
	if lastCP == firstCP {
		return samples
	}
	if sampleN > 1 {
		stride := uint32(1)
		totalCP := (lastCP-firstCP)/64 + 1
		if uint32(sampleN) < totalCP {
			stride = totalCP / uint32(sampleN)
		}
		for seq := firstCP + stride*64; seq < lastCP; seq += stride * 64 {
			samples = append(samples, seq)
		}
	}
	return append(samples, lastCP)
}

// verifyArchivePeers samples checkpoints in [from, to] (always first and last) and cross-compares each peer's history-XXXXXXXX.json; any disagreement is a loud consensus-level failure.
// Our own archive (archiveRoot) is a required participant, since peers agreeing says nothing about our bytes.
func verifyArchivePeers(from, to uint32, peerList string, sampleN int, archiveRoot string, failOnMissed bool) error { //nolint:funlen,gocognit,gocyclo
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
	for _, p := range peers {
		// A non-HTTP peer would fail every fetch and be skipped as merely unreachable.
		if !strings.HasPrefix(p, "https://") && !strings.HasPrefix(p, "http://") {
			return fmt.Errorf("tier peers: %q is not an http(s) archive URL — our own archive is read from -archive-root, not listed in -peers", p)
		}
	}

	client := &http.Client{Timeout: 30 * time.Second}

	// Resolve an unbounded -to against the peers. Fabricating lastCP = firstCP + 640 would sample ten genesis-era slots and print "peer cross-check OK",
	// certifying nothing about what we hold NOW.
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

	samples := peerSampleCheckpoints(firstCP, lastCP, sampleN)

	fmt.Fprintf(os.Stderr, "verify-archive: peer diff — %d peers × %d checkpoints in [%d,%d]\n",
		len(peers), len(samples), firstCP, lastCP)
	for _, p := range peers {
		fmt.Fprintf(os.Stderr, "  peer: %s\n", p)
	}
	selfCov := readArchiveMirrorCoverage(archiveRoot, mirrorHistory)
	fmt.Fprintf(os.Stderr, "  self: %s (mirror coverage %s)\n", archiveRoot, selfCov)
	var self peerSelfTally

	// A majority of configured peers must respond before agreement is reported as consensus (same floor verifyArchivePeers enforces below).
	quorum := peerCheckpointQuorum(len(peers))

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
		if len(observed) < quorum {
			fmt.Fprintf(os.Stderr, "  ledger %d: only %d of %d peers responded (need ≥%d for quorum); skipping (inconclusive)\n",
				seq, len(observed), len(peers), quorum)
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
			fmt.Fprintf(os.Stderr, "  ledger %d: %d of %d peers agree ✓\n", seq, len(observed), len(peers))
			self.compare(archiveRoot, relPath, seq, ref, selfCov)
		}
	}

	fmt.Fprintf(os.Stderr, "\nverify-archive: peer diff — peers_vs_peers: %d consensus-verified checkpoints, %d disagreements; "+
		"self_vs_peers: %d matched, %d diverged, %d missing, %d unmirrored\n",
		matches, mismatches, self.matched, self.diverged, self.missed, self.unmirrored)
	if mismatches > 0 {
		return fmt.Errorf("peer cross-check FAILED (%d disagreements)", mismatches)
	}
	if matches == 0 {
		return fmt.Errorf("peer cross-check INCONCLUSIVE — no checkpoint verified across ≥2 peers")
	}
	if err := self.decision(archiveRoot, failOnMissed); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "verify-archive: peer cross-check OK ✓\n")
	return nil
}

// verifyArchiveArchivist runs `<bin> scan --verify <url>`; --verify makes stellar-archivist recompute every bucket's sha256, which Tier B (trusting the local manifest) does not.
// Stdout streams to stderr; the verdict is the exit code (missing/invalid object, unresolved URL, timeout, or bin not on PATH). -archivist-bin must accept the same argv.
func verifyArchiveArchivist(bin, url string, timeout time.Duration) error {
	fmt.Fprintf(os.Stderr, "verify-archive: archivist scan --verify bin=%s url=%s timeout=%s\n",
		bin, url, timeout)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// gosec G204: bin + url are operator-supplied diagnostic flags on a CLI that ALREADY shells the operator's environment —
	// any "untrusted input" boundary at this point has already been crossed by the operator running this command at all.
	cmd := exec.CommandContext(ctx, bin, "scan", "--verify", url) //nolint:gosec // operator-supplied flags

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

// peerSelfTally counts how our own archive's checkpoint files compared
// with the peers' consensus across a Tier D run.
type peerSelfTally struct {
	matched, diverged, missed, unmirrored int
}

// compare checks our local history-XXXXXXXX.json for seq against the peers' consensus checkpoint. An absent file above the mirror's high-
// water is fill lag (unmirrored); inside its span it is missed.
func (t *peerSelfTally) compare(archiveRoot, relPath string, seq uint32, consensus historyCheckpoint, cov archiveMirrorCoverage) {
	path := filepath.Join(archiveRoot, filepath.FromSlash(relPath))
	body, err := os.ReadFile(path) //nolint:gosec // archiveRoot is operator-supplied via flag
	switch {
	case errors.Is(err, os.ErrNotExist) && cov.outsideCoverage(seq):
		t.unmirrored++
		return
	case errors.Is(err, os.ErrNotExist):
		t.missed++
		fmt.Fprintf(os.Stderr, "  ledger %d: self: %s absent inside the mirror's coverage\n", seq, path)
		return
	case err != nil:
		t.diverged++
		fmt.Fprintf(os.Stderr, "  ledger %d: self: read %s: %v\n", seq, path, err)
		return
	}
	cp, err := parseHistoryCheckpoint(body, path)
	if err != nil {
		t.diverged++
		fmt.Fprintf(os.Stderr, "  ledger %d: self: %v\n", seq, err)
		return
	}
	if !checkpointsEqual(consensus, cp) {
		t.diverged++
		fmt.Fprintf(os.Stderr, "  ledger %d: SELF DIVERGES FROM PEER CONSENSUS (%s)\n", seq, path)
		return
	}
	t.matched++
}

// decision fails a Tier D run whose peer consensus our own archive
// contradicts, or never took part in.
func (t peerSelfTally) decision(archiveRoot string, failOnMissed bool) error {
	switch {
	case t.diverged > 0:
		return fmt.Errorf("peer cross-check FAILED — our archive %s diverges from peer consensus at %d checkpoint(s)", archiveRoot, t.diverged)
	case t.matched == 0:
		return fmt.Errorf("peer cross-check INCONCLUSIVE — our archive %s matched no consensus-verified checkpoint (%d missing, %d unmirrored); "+
			"the peers were verified only against each other", archiveRoot, t.missed, t.unmirrored)
	case failOnMissed && t.missed > 0:
		return fmt.Errorf("peer cross-check FAILED — %d consensus-verified checkpoint(s) missing from our archive %s inside its coverage (with -fail-on-missed)", t.missed, archiveRoot)
	}
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
	return parseHistoryCheckpoint(body, url)
}

// parseHistoryCheckpoint decodes a history-XXXXXXXX.json body read
// from src (a peer URL or a local path).
func parseHistoryCheckpoint(body []byte, src string) (historyCheckpoint, error) {
	var cp historyCheckpoint
	if err := json.Unmarshal(body, &cp); err != nil {
		return historyCheckpoint{}, fmt.Errorf("parse: %w", err)
	}
	// A 200 that is not a real checkpoint (error envelope, null, CDN 200 on a miss) decodes to the zero value; reject it at this single parse chokepoint so two zero checkpoints never compare equal.
	if cp.CurrentLedger == 0 {
		return historyCheckpoint{}, fmt.Errorf("%s: decoded to CurrentLedger=0 (not a real checkpoint)", src)
	}
	return cp, nil
}

// checkpointsEqual compares the consensus-state fields of two history-XXXXXXXX.json records, ignoring server + version, which vary
// legitimately across operators.
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

// archiveMirrorCoverage is the checkpoint span the local cross-anchor mirror (-archive-root) actually holds. The fill job lags the LCM
// walk, so trailing absences are delivery lag while interior ones are defects
// (mirrors TolerateTrailingMissing); Known == false (unreadable mirror) makes every absence a genuine miss.
type archiveMirrorCoverage struct {
	Floor     uint32 // lowest checkpoint ledger the mirror holds
	HighWater uint32 // highest checkpoint ledger the mirror holds
	Known     bool
}

// outsideCoverage reports whether checkpoint seq lies ahead of the mirror's fill job (tolerated trailing edge, not a hole); unknown coverage answers false.
// Deliberately asymmetric: a seq below Floor is a genuine hole (ADR-0017 contract 3), or a partly-restored mirror's leading gap would pass -fail-on-missed.
func (c archiveMirrorCoverage) outsideCoverage(seq uint32) bool {
	return c.Known && seq > c.HighWater
}

// String renders the span for the run's log line.
func (c archiveMirrorCoverage) String() string {
	if !c.Known {
		return "unknown"
	}
	return fmt.Sprintf("[%d, %d]", c.Floor, c.HighWater)
}

// mirrorCategory is one checkpoint-file tree of the archive layout; the fill job maintains ledger/ only, so each tier must measure
// coverage from the tree it reads.
type mirrorCategory struct{ dir, suffix string }

var (
	mirrorLedger  = mirrorCategory{dir: "ledger", suffix: ".xdr.gz"}
	mirrorHistory = mirrorCategory{dir: "history", suffix: ".json"}
)

// readArchiveMirrorCoverage measures the span under <archiveRoot>/<cat.dir> by descending the hex-nested tree to its least and greatest
// leaf (four readdirs per bound, backtracking past empty branches).
func readArchiveMirrorCoverage(archiveRoot string, cat mirrorCategory) archiveMirrorCoverage {
	root := filepath.Join(archiveRoot, cat.dir)
	low, lowOK := extremeMirrorCheckpoint(root, cat, 0, false)
	high, highOK := extremeMirrorCheckpoint(root, cat, 0, true)
	if !lowOK || !highOK {
		return archiveMirrorCoverage{}
	}
	return archiveMirrorCoverage{Floor: low, HighWater: high, Known: true}
}

// mirrorTreeDepth is the directory levels between <archiveRoot>/ledger and a ledger-XXXXXXXX.xdr.gz file: one per each of the first three
// bytes of the hex sequence.
const mirrorTreeDepth = 3

// extremeMirrorCheckpoint returns the least or greatest checkpoint sequence below dir; names are fixed-width hex, so lexical order is numeric.
// Branches with no conforming leaf are skipped so an empty directory cannot shorten the answer.
func extremeMirrorCheckpoint(dir string, cat mirrorCategory, depth int, highest bool) (uint32, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, false
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	if highest {
		slices.Reverse(names)
	}
	for _, name := range names {
		if depth < mirrorTreeDepth {
			if seq, ok := extremeMirrorCheckpoint(filepath.Join(dir, name), cat, depth+1, highest); ok {
				return seq, true
			}
			continue
		}
		if seq, ok := parseMirrorCheckpointFile(cat, name); ok {
			return seq, true
		}
	}
	return 0, false
}

// parseMirrorCheckpointFile reads the checkpoint sequence from a mirror leaf name (ledger-0000007f.xdr.gz gives 127), rejecting non-checkpoint files such as archivist scratch files that would skew the measured bound.
func parseMirrorCheckpointFile(cat mirrorCategory, name string) (uint32, bool) {
	hexSeq, ok := strings.CutPrefix(name, cat.dir+"-")
	if !ok {
		return 0, false
	}
	hexSeq, ok = strings.CutSuffix(hexSeq, cat.suffix)
	if !ok || len(hexSeq) != 8 {
		return 0, false
	}
	seq, err := strconv.ParseUint(hexSeq, 16, 32)
	if err != nil || seq%64 != 63 {
		return 0, false
	}
	return uint32(seq), true
}

// applyCheckpointTierState records the checkpoint tier's high-water bounded to what the mirror could anchor, explains on stderr when the walk outran it, and returns the state and certified ledger.
func applyCheckpointTierState(st VerifyArchiveState, highestLedger uint32, cov archiveMirrorCoverage, now time.Time) (VerifyArchiveState, uint32) {
	anchored := checkpointWatermark(highestLedger, cov)
	if anchored < highestLedger {
		fmt.Fprintf(os.Stderr, "verify-archive: checkpoint high-water held at %d "+
			"(walk reached %d; the cross-anchor mirror holds nothing above %d, so that span "+
			"is not certified and the next run re-anchors it)\n",
			anchored, highestLedger, cov.HighWater)
	}
	if prior := st.Tiers["checkpoint"].LastVerifiedLedger; cov.Known && cov.HighWater <= prior {
		fmt.Fprintf(os.Stderr, "verify-archive: warn: the cross-anchor mirror's high-water %d has not "+
			"advanced past the previously certified %d — this run anchored nothing new; check the "+
			"mirror's fill job\n", cov.HighWater, prior)
	}
	return updateTierState(st, "checkpoint", anchored, "", now), anchored
}

// checkpointWatermark bounds the checkpoint tier's persisted high-water to what this run could ANCHOR: the walk runs ahead of the mirror,
// and certifying the tip would record unanchored checkpoints as verified. updateTierState only moves forward, so the clamp never rewinds.
func checkpointWatermark(highestLedger uint32, cov archiveMirrorCoverage) uint32 {
	if cov.Known && cov.HighWater < highestLedger {
		return cov.HighWater
	}
	return highestLedger
}

// readArchivedLedgerHash reads the ledger hash for checkpoint ledger seq (seq % 64 == 63) from the gzipped LedgerHeaderHistoryEntry stream in <archiveRoot>/ledger/XX/YY/ZZ/.
// Returns (hash, true, nil); (_, false, nil) if the file is not on disk yet; (_, _, err) on a real read or parse error.
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

// extractLedgerHeader pulls the header out of an LCM of either version: V0 (pre-p20) and V1 (p20+) expose a LedgerHeaderHistoryEntry at
// different paths.
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

// hashFromHex inverts hashToHex for state-file boundary hashes. A truncated or non-hex value is an error, never a zero hash,
// which would compare equal to another zero hash and turn a corrupt state file into a passing chain proof.
func hashFromHex(s string) (sdkxdr.Hash, error) {
	var h sdkxdr.Hash
	b, err := hex.DecodeString(s)
	if err != nil {
		return sdkxdr.Hash{}, fmt.Errorf("parse hex %q: %w", s, err)
	}
	if len(b) != len(h) {
		return sdkxdr.Hash{}, fmt.Errorf("hex length %d, want %d (32-byte SHA-256)", len(b), len(h))
	}
	copy(h[:], b)
	return h, nil
}

// wasm-history: walks a galexie bucket over [from, to] and tracks each watched contract's instance executable hash (Created/Updated CONTRACT_DATA with a ContractInstance key).
// Output is JSON keyed by contract C-strkey; read-only (no DB writes or cursor changes).
// Default bucket is S3BucketArchive, since audits span ranges before galexie-live's seam.

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

// storageChange is one Created/Updated/Restored non-Instance ContractData observation (key + change type + ledger, value uninterpreted),
// used by -track-storage-rotations to catch admin flips (e.g. set_pair_wasm, fee_to_setter) that the instance-only filter ignores.
type storageChange struct {
	Ledger     uint32 `json:"ledger"`
	ChangeType string `json:"change_type"` // created | updated | restored
	KeyXDRB64  string `json:"key_xdr_b64"`
	KeyHint    string `json:"key_hint,omitempty"`   // best-effort human-readable summary
	Durability string `json:"durability,omitempty"` // persistent | temporary
}

// contractStorageHistory is the per-contract output of the storage-rotation tracker: one entry per watched contract with ANY observed non-
// Instance ContractData change.
type contractStorageHistory struct {
	Contract string          `json:"contract"`
	Changes  []storageChange `json:"changes"`
}

// codeUpload is one ContractCode LedgerEntry Created or Restored (a WASM upload). Captured globally, since any contract may later reference the hash,
// to preserve every WASM uploaded in the window; Soroban-RPC only serves live, non-evicted hashes.
type codeUpload struct {
	Ledger     uint32 `json:"ledger"`
	WasmHash   string `json:"wasm_hash"`
	SizeBytes  int    `json:"size_bytes"`
	ChangeType string `json:"change_type"` // created | restored
}
