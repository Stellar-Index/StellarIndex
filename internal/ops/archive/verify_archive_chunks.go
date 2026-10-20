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

// startVerifyArchiveMetrics serves /metrics from the obs Registry on addr and returns a stop function (5 s drain).
// A bind failure is an error; scrape failures are only logged so they cannot stall verification.
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

// checkResumeFromHash compares the first chunk's FirstPrevHash with the operator's -resume-from-hash, proving cross-run continuity explicitly.
// A bad hex string or a mismatch is an error naming the expected format or both hashes and the seam ledger.
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
		// A cross-RUN boundary break is divergence like an in-chunk one and counts; the bad-hex returns above are input errors and must not.
		obs.VerifyArchiveMismatchesTotal.WithLabelValues("0", "chain").Inc()
		return fmt.Errorf("resume-from-hash boundary mismatch at ledger %d:\n"+
			"  -resume-from-hash         = %s\n"+
			"  observed FirstPrevHash    = %s",
			firstSeq, expectedHex, hashToHex(firstPrevHash))
	}
	return nil
}

// chunkResult holds a chunk's running counters and boundary hashes for stitching; zero firstSeq/lastSeq means it processed zero ledgers.
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

// stitchChunks validates the boundary between adjacent NON-EMPTY chunks (hash chain and LastSeq+1 == FirstSeq), so an empty chunk cannot absorb a gap.
// A failure increments obs.VerifyArchiveMismatchesTotal ("sequence" or "chain", labelled with the LEFT chunk) so it pages via the P1 divergence rule.
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

// checkpointAnchorOutcome is one checkpoint's cross-anchor verdict; a divergence (our hash != the archive's) is not a member: it aborts
// the walk with an error.
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

// String is the `outcome` label value on obs.VerifyArchiveCheckpointsTotal; "unmirrored" is the newest value.
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

// classifyCheckpointAnchor classifies the mirror's header hash for checkpoint seq against ours. An ABSENT file is "missed" only
// when the mirror covers that checkpoint; above its high-water the walk is ahead of the fill job by design, and counting those
// would make the invariant unenforceable. Read failures and hash divergence abort the chunk walk.
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

// verifyChunk walks one chunk and returns counters and boundary hashes. Every chunk checks PreviousLedgerHash continuity whatever the
// tier, or a checkpoint-only run would have no
// gap detection; cross-chunk boundaries are stitchChunks's job. Errors abort the walk.
//
//nolint:gocognit,funlen,gocyclo // walk-loop linearity beats premature splitting
func verifyChunk(
	ctx context.Context,
	lsCfg ledgerstream.Config,
	chunk opsutil.RangeChunk,
	idx int,
	doCheckpoint bool,
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

			if hasPrev {
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
			// stops advancing this stops feeding WATCHDOG=1.
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

// chunkOrchestratorOpts carries resume-aware hooks for [runVerifyChunks]; the zero value is the single-pass shape (chunks numbered
// 0..len-1, no per-chunk completion reported).
type chunkOrchestratorOpts struct {
	// MirrorCoverage is the checkpoint span the cross-anchor mirror
	// holds, measured once before the walk. The zero value (Known
	// false) treats every absent checkpoint as missed.
	MirrorCoverage archiveMirrorCoverage

	// ChunkIdxs maps each position in chunks to its ORIGINAL idx in the pre-resume list (nil is identity), keeping progress logs and state rows consistent.
	ChunkIdxs []int

	// OnChunkDone is called synchronously after each chunk succeeds (nil is a no-op); the caller marks the chunk Done in the state file.
	OnChunkDone func(originalIdx int, res chunkResult)
}

// runVerifyChunks splits the range, runs chunks concurrently via errgroup (first error cancels siblings), stitches boundaries,
// and returns the aggregated chunkResult (Idx=-1).
func runVerifyChunks(
	ctx context.Context,
	lsCfg ledgerstream.Config,
	chunks []opsutil.RangeChunk,
	doCheckpoint bool,
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
				doCheckpoint, archiveRoot, opts.MirrorCoverage,
				&progressMu, startedAt, progressEvery,
				&totalVerified,
			)
			totalVerified.Add(int64(res.Verified))
			updateMu.Lock()
			results[i] = res
			updateMu.Unlock()
			// Mark Done in the state file only on clean completion — an errored chunk's partial-progress isn't durable
			// (in-flight-chunk resume is a future revision; see ChunkProgress doc in verify_archive_state.go).
			if err == nil && opts.OnChunkDone != nil {
				opts.OnChunkDone(originalIdx, res)
			}
			return err
		})
	}
	walkErr := g.Wait()
	return results, walkErr
}
