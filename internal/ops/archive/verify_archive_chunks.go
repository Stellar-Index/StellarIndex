package archive

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"
	"golang.org/x/sync/errgroup"

	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

// startVerifyArchiveMetrics spins up a tiny http.Server on addr
// exposing /metrics from the obs Registry. Returns a stop function
// that gracefully shuts down the server (≤ 5 s for in-flight scrapes
// to drain). Used when -metrics-listen is supplied.
//
// Errors when the bind itself fails (port already in use, perms);
// scrape failures during the run don't propagate to the caller —
// the server just logs and continues, so a flaky scraper can't
// stall verification.
func startVerifyArchiveMetrics(addr string) (func(), error) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", obs.Handler())

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	lc := net.ListenConfig{}
	listenCtx, cancelListen := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelListen()
	ln, err := lc.Listen(listenCtx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	go func() {
		_ = srv.Serve(ln)
	}()

	return func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}, nil
}

// checkResumeFromHash compares the first chunk's FirstPrevHash
// against an operator-supplied hex hash. Used to prove cross-run
// continuity: when a previous verification halted partway, the
// operator records its last verified ledger's hash and passes it on
// the resume run via -resume-from-hash; this check enforces the
// boundary explicitly rather than relying on the implicit-overlap
// proof from re-reading the seam ledger.
//
// Errors:
//   - hex parse failures (operator typo, wrong length) — surfaced
//     with a clear message that names the expected format.
//   - hash mismatch — names both hashes + the seam ledger so the
//     operator can audit which side is wrong (likely indexer or
//     archive corruption between runs).
func checkResumeFromHash(expectedHex string, firstPrevHash sdkxdr.Hash, firstSeq uint32) error {
	expectedBytes, err := hex.DecodeString(expectedHex)
	if err != nil {
		return fmt.Errorf("resume-from-hash: parse hex %q: %w", expectedHex, err)
	}
	if len(expectedBytes) != len(firstPrevHash) {
		return fmt.Errorf("resume-from-hash: hex length %d, want %d (32-byte SHA-256)", len(expectedBytes), len(firstPrevHash))
	}
	var expected sdkxdr.Hash
	copy(expected[:], expectedBytes)
	if expected != firstPrevHash {
		// A cross-RUN boundary break is the same correctness class as
		// an in-chunk one: our archive's bytes do not chain onto what
		// the previous run certified. The two returns above are
		// operator input errors (bad hex), NOT divergence — they must
		// not touch the counter.
		obs.VerifyArchiveMismatchesTotal.WithLabelValues("0", "chain").Inc()
		return fmt.Errorf("resume-from-hash boundary mismatch at ledger %d:\n"+
			"  -resume-from-hash         = %s\n"+
			"  observed FirstPrevHash    = %s",
			firstSeq, expectedHex, hashToHex(firstPrevHash))
	}
	return nil
}

// chunkResult is what verifyChunk returns. Carries the running
// counters AND the boundary hashes the orchestrator needs to stitch
// adjacent chunks into one chain-validation pass.
//
// Empty (zero-value) firstSeq/lastSeq indicate the chunk processed
// zero ledgers — only happens when the chunk's range is empty
// (degenerate splits) or the underlying bucket lacks the range.
type chunkResult struct {
	Idx           int
	From          uint32
	To            uint32
	FirstSeq      uint32
	FirstPrevHash sdkxdr.Hash // PreviousLedgerHash of first ledger seen
	LastSeq       uint32
	LastHash      sdkxdr.Hash
	Verified      int
	Mismatches    int
	CheckpointsOK int
	// CheckpointsMissed counts checkpoints with no file in the
	// cross-anchor mirror INSIDE the span the mirror holds — a hole
	// in the archive, the thing ADR-0017 contract 3 forbids.
	CheckpointsMissed int
	// CheckpointsUnmirrored counts checkpoints the walk reached
	// beyond that span: the mirror's fill job has not delivered them
	// yet. A delivery lag, not a hole — see archiveMirrorCoverage.
	CheckpointsUnmirrored int
}

// stitchChunks validates the boundary between adjacent NON-EMPTY
// chunks: the last hash of the left chunk must equal the first
// PreviousLedgerHash of the right chunk, AND left.LastSeq + 1 must
// equal right.FirstSeq (no gap). Returns nil when every consecutive
// non-empty pair stitches cleanly.
//
// Single-chunk (or single-non-empty-chunk) results have no boundary
// to check; they pass.
//
// Empty chunks (zero ledgers processed — the SDK's stream may
// legitimately yield zero ledgers for ranges before a bucket exists)
// are skipped when choosing WHICH pairs to compare, but never skip
// the check itself (DAT-11): the boundary is re-targeted at the
// nearest non-empty neighbours on each side, so an empty chunk
// sitting between two non-empty chunks — which would mask a genuine
// mid-range hole if the check were skipped outright — still surfaces
// as a seq/hash mismatch between those surrounding chunks. An empty
// chunk cannot silently absorb an interior gap.
//
// A boundary failure increments obs.VerifyArchiveMismatchesTotal
// under the same reason taxonomy verifyChunk uses (a gap is
// "sequence", a hash break is "chain"), labelled with the LEFT
// chunk's index — the boundary belongs to the chunk whose last
// ledger it hangs off. That increment is what makes the failure
// PAGEABLE: the P1 stellarindex_stellar_archive_divergence rule
// selects that counter, so a break landing on one of the ~11
// worker-chunk boundaries (rather than inside a chunk) used to abort
// the run without touching it, surfacing only as the severity-ticket
// stellarindex_verify_archive_unit_failed. Same divergence class,
// same page (#282). The counter reaches Prometheus via
// verify_archive_textfile.go, which reads the live collector on the
// way out and so picks these up automatically.
func stitchChunks(results []chunkResult) error {
	nonEmpty := make([]chunkResult, 0, len(results))
	for _, r := range results {
		if r.Verified > 0 {
			nonEmpty = append(nonEmpty, r)
		}
	}
	if len(nonEmpty) <= 1 {
		return nil
	}
	for i := 0; i < len(nonEmpty)-1; i++ {
		left := nonEmpty[i]
		right := nonEmpty[i+1]
		if left.LastSeq+1 != right.FirstSeq {
			obs.VerifyArchiveMismatchesTotal.WithLabelValues(strconv.Itoa(left.Idx), "sequence").Inc()
			return fmt.Errorf("chunk[%d→%d] boundary gap: chunk[%d].LastSeq=%d, chunk[%d].FirstSeq=%d",
				left.Idx, right.Idx, left.Idx, left.LastSeq, right.Idx, right.FirstSeq)
		}
		if left.LastHash != right.FirstPrevHash {
			obs.VerifyArchiveMismatchesTotal.WithLabelValues(strconv.Itoa(left.Idx), "chain").Inc()
			return fmt.Errorf("chunk[%d→%d] boundary chain break at ledger %d:\n"+
				"  chunk[%d].LastHash         = %s\n"+
				"  chunk[%d].FirstPrevHash    = %s",
				left.Idx, right.Idx, left.LastSeq,
				left.Idx, hashToHex(left.LastHash),
				right.Idx, hashToHex(right.FirstPrevHash))
		}
	}
	return nil
}

// checkpointAnchorOutcome is one checkpoint's cross-anchor verdict.
// A divergence (our hash != the archive's) is not a member: it aborts
// the walk with an error rather than being tallied.
type checkpointAnchorOutcome int

const (
	// checkpointAnchorMatched — the mirror's canonical header hash for
	// this checkpoint equals ours.
	checkpointAnchorMatched checkpointAnchorOutcome = iota
	// checkpointAnchorMissed — no file, INSIDE the span the mirror
	// holds. A hole in the cross-anchor archive (ADR-0017 contract 3).
	checkpointAnchorMissed
	// checkpointAnchorUnmirrored — no file, beyond that span. The
	// mirror's fill job has not delivered this checkpoint yet.
	checkpointAnchorUnmirrored
)

// String is the `outcome` label value on
// obs.VerifyArchiveCheckpointsTotal. "matched"/"missed" are the
// pre-existing values and keep their meaning; "unmirrored" is new.
func (o checkpointAnchorOutcome) String() string {
	switch o {
	case checkpointAnchorMatched:
		return "matched"
	case checkpointAnchorMissed:
		return "missed"
	case checkpointAnchorUnmirrored:
		return "unmirrored"
	}
	return "unknown"
}

// classifyCheckpointAnchor reads the cross-anchor mirror's canonical
// header hash for checkpoint seq and classifies it against ours.
//
// An ABSENT file is only "missed" when the mirror claims to cover
// that checkpoint. Above the mirror's high-water the walk is simply
// ahead of the fill job — on r1 the mirror is filled at 02:2x UTC and
// the tier-B walk runs at 04:38 UTC, so ~23 checkpoints closed in
// between are absent on every single run, by design (F144). Counting
// those as missing archive data is what made ADR-0017's hard
// invariant unenforceable on the deployed path: the flag that would
// enforce it could not be turned on without failing every night.
//
// Errors (read failure, hash divergence) abort the chunk walk; the
// caller bumps the mismatch counter and propagates.
func classifyCheckpointAnchor(archiveRoot string, seq uint32, ourHash sdkxdr.Hash, cov archiveMirrorCoverage) (checkpointAnchorOutcome, error) {
	expected, hit, err := readArchivedLedgerHash(archiveRoot, seq)
	switch {
	case err != nil:
		return checkpointAnchorMissed, fmt.Errorf("ledger %d: archive read failed: %w", seq, err)
	case !hit && cov.outsideCoverage(seq):
		return checkpointAnchorUnmirrored, nil
	case !hit:
		return checkpointAnchorMissed, nil
	case expected != ourHash:
		return checkpointAnchorMissed, fmt.Errorf("checkpoint anchor mismatch at ledger %d:\n"+
			"  our LCM hash          = %s\n"+
			"  archive-signed hash   = %s",
			seq, hashToHex(ourHash), hashToHex(expected))
	}
	return checkpointAnchorMatched, nil
}

// verifyChunk walks one chunk's ledger range and returns the
// counters + boundary hashes the orchestrator needs to stitch
// chunks. Pure walk-logic — no parent-context creation, no flag
// parsing; the caller controls those.
//
// chainCheckInternal: when true, validates ledger N's
// PreviousLedgerHash against ledger N-1's hash within this chunk.
// Cross-chunk boundaries are validated by stitchChunks instead.
//
// Errors abort the chunk's walk; the orchestrator's errgroup
// cancels sibling chunks. The verification semantics match the
// pre-parallel verifyArchiveLCMWalk one-for-one.
//
//nolint:gocognit,funlen,gocyclo // walk-loop linearity beats premature splitting
func verifyChunk(
	ctx context.Context,
	lsCfg ledgerstream.Config,
	chunk opsutil.RangeChunk,
	idx int,
	chainCheckInternal, doCheckpoint bool,
	archiveRoot string,
	mirrorCoverage archiveMirrorCoverage,
	progressMu *sync.Mutex,
	startedAt time.Time,
	progressEvery time.Duration,
	totalVerified *atomic.Int64,
) (chunkResult, error) {
	res := chunkResult{Idx: idx, From: chunk.From, To: chunk.To}
	chunkLabel := strconv.Itoa(idx)

	var (
		prevSeq      uint32
		prevHash     sdkxdr.Hash
		hasPrev      bool
		lastProgress time.Time
	)

	err := ledgerstream.Stream(ctx, lsCfg, chunk.From, chunk.To,
		func(lcm sdkxdr.LedgerCloseMeta) error {
			seq := lcm.LedgerSequence()
			hash := lcm.LedgerHash()
			header, ok := extractLedgerHeader(lcm)
			if !ok {
				return fmt.Errorf("ledger %d: cannot extract LedgerHeader", seq)
			}

			// Capture boundary hashes on first observed ledger.
			if !hasPrev {
				res.FirstSeq = seq
				res.FirstPrevHash = header.PreviousLedgerHash
			}

			if chainCheckInternal && hasPrev {
				if seq != prevSeq+1 {
					res.Mismatches++
					obs.VerifyArchiveMismatchesTotal.WithLabelValues(chunkLabel, "sequence").Inc()
					return fmt.Errorf("chunk[%d] sequence gap: %d → %d (expected %d)",
						idx, prevSeq, seq, prevSeq+1)
				}
				if header.PreviousLedgerHash != prevHash {
					res.Mismatches++
					obs.VerifyArchiveMismatchesTotal.WithLabelValues(chunkLabel, "chain").Inc()
					return fmt.Errorf("chunk[%d] chain break at ledger %d:\n"+
						"  ledger[%d].Hash              = %s\n"+
						"  ledger[%d].PreviousLedgerHash = %s",
						idx, seq, prevSeq, hashToHex(prevHash),
						seq, hashToHex(header.PreviousLedgerHash))
				}
			}

			if doCheckpoint && seq%64 == 63 {
				outcome, cerr := classifyCheckpointAnchor(archiveRoot, seq, hash, mirrorCoverage)
				if cerr != nil {
					res.Mismatches++
					obs.VerifyArchiveMismatchesTotal.WithLabelValues(chunkLabel, "checkpoint").Inc()
					return cerr
				}
				switch outcome {
				case checkpointAnchorMatched:
					res.CheckpointsOK++
				case checkpointAnchorMissed:
					res.CheckpointsMissed++
				case checkpointAnchorUnmirrored:
					res.CheckpointsUnmirrored++
				}
				obs.VerifyArchiveCheckpointsTotal.WithLabelValues(chunkLabel, outcome.String()).Inc()
			}

			prevSeq = seq
			prevHash = hash
			hasPrev = true
			res.Verified++
			res.LastSeq = seq
			res.LastHash = hash
			obs.VerifyArchiveLedgersVerified.WithLabelValues(chunkLabel).Inc()
			obs.VerifyArchiveCurrentLedger.WithLabelValues(chunkLabel).Set(float64(seq))
			// Liveness signal for the systemd watchdog: a walk that
			// stops advancing this stops feeding WATCHDOG=1 (OBS-07).
			verifyArchiveProgress.Ledgers.Add(1)

			if time.Since(lastProgress) >= progressEvery {
				progressMu.Lock()
				// Aggregate verified across all chunks for the
				// progress line — operators want one running total,
				// not N independent counters.
				agg := totalVerified.Load() + int64(res.Verified)
				fmt.Fprintf(os.Stderr, "verify-archive: chunk[%d] ledger %d, agg %d verified, %.0f ledgers/s\n",
					idx, seq, agg, float64(agg)/time.Since(startedAt).Seconds())
				progressMu.Unlock()
				lastProgress = time.Now()
			}
			return nil
		},
	)
	return res, err
}

// chunkOrchestratorOpts carries resume-aware hooks for
// [runVerifyChunks]. The zero value is the legacy single-pass shape:
// chunks are numbered 0..len-1 in the progress log + result array,
// and no per-chunk completion is reported anywhere.
type chunkOrchestratorOpts struct {
	// MirrorCoverage is the checkpoint span the cross-anchor mirror
	// holds, measured once before the walk. The zero value (Known
	// false) treats every absent checkpoint as missed, which is the
	// behaviour every caller had before the span existed.
	MirrorCoverage archiveMirrorCoverage

	// ChunkIdxs maps each position in `chunks` to the chunk's
	// ORIGINAL idx in the parent run's full pre-resume chunk list.
	// nil ⇒ identity (0, 1, …, len(chunks)-1). When resume has
	// filtered already-Done chunks out, this preserves the original
	// numbering so the progress log + state-file row stay
	// consistent across restarts ("chunk[5]" always means the same
	// 5M-ledger slice).
	ChunkIdxs []int

	// OnChunkDone is invoked synchronously after each chunk's
	// walker returns without error, with the chunk's original idx
	// and its terminal chunkResult. nil ⇒ no-op. The caller
	// (verifyArchiveLCMWalk) writes a state-file update marking
	// the chunk Done so a subsequent SIGTERMed run can skip it.
	OnChunkDone func(originalIdx int, res chunkResult)
}

// runVerifyChunks orchestrates parallel chunk verification. Splits
// the range, runs `workers` chunks concurrently via errgroup,
// stitches boundary hashes after all chunks complete.
//
// First chunk error cancels siblings (errgroup semantics) — fail-fast
// matches the serial walk's behaviour where a single mismatch
// aborts the whole verification.
//
// Returns the aggregated chunkResult counters as a single
// chunkResult (Idx=-1, From/To = the input range) for the orchestrator
// to print as the final summary, plus any walk error.
func runVerifyChunks(
	ctx context.Context,
	lsCfg ledgerstream.Config,
	chunks []opsutil.RangeChunk,
	doChain, doCheckpoint bool,
	archiveRoot string,
	startedAt time.Time,
	progressEvery time.Duration,
	opts chunkOrchestratorOpts,
) ([]chunkResult, error) {
	if len(chunks) == 0 {
		return nil, errors.New("verify-archive: empty chunk list")
	}

	// Resolve per-position → original-idx mapping. Default is
	// identity (no resume filtering).
	idxs := opts.ChunkIdxs
	if idxs == nil {
		idxs = make([]int, len(chunks))
		for i := range chunks {
			idxs[i] = i
		}
	}
	if len(idxs) != len(chunks) {
		return nil, fmt.Errorf("verify-archive: ChunkIdxs len %d != chunks len %d", len(idxs), len(chunks))
	}

	results := make([]chunkResult, len(chunks))
	var (
		progressMu    sync.Mutex
		totalVerified atomic.Int64 // cross-chunk running total; atomic — read
		// under progressMu for the progress line, written after each chunk;
		// the two mutexes don't mutually exclude, so the counter must be atomic.
		updateMu sync.Mutex // guards the results[] writes
	)

	g, gctx := errgroup.WithContext(ctx)
	for i, chunk := range chunks {
		i, chunk := i, chunk // capture
		originalIdx := idxs[i]
		g.Go(func() error {
			res, err := verifyChunk(
				gctx, lsCfg, chunk, originalIdx,
				doChain, doCheckpoint, archiveRoot, opts.MirrorCoverage,
				&progressMu, startedAt, progressEvery,
				&totalVerified,
			)
			totalVerified.Add(int64(res.Verified))
			updateMu.Lock()
			results[i] = res
			updateMu.Unlock()
			// Mark Done in the state file only on clean completion
			// — an errored chunk's partial-progress isn't durable
			// (in-flight-chunk resume is a future revision; see
			// ChunkProgress doc in verify_archive_state.go).
			if err == nil && opts.OnChunkDone != nil {
				opts.OnChunkDone(originalIdx, res)
			}
			return err
		})
	}
	walkErr := g.Wait()
	return results, walkErr
}
