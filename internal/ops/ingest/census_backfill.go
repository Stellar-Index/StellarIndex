package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// censusBackfill writes the decoder-independent ledger_ingest_log census and hash-chain anchors for a historical range (ADR-0033).
// Re-runs converge and resume from the ingestion_cursors checkpoint. Fail-closed (opsutil.WriteGate): default is a DRY RUN.
func censusBackfill(args []string) error { //nolint:gocognit,gocyclo,funlen // linear walk + checkpoint loop; splitting reduces clarity (same as backfillRouter).
	fs, gate := opsutil.NewMutatingFlagSet("census-backfill")
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	from := fs.Uint("from", 0, "First ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "Last ledger sequence (inclusive, required)")
	resume := fs.Bool("resume", true, "Resume from saved cursor if a checkpoint exists for this from/to pair")
	bucket := fs.String("bucket", "", "galexie bucket override. Default: the range vs ingestion.live_seam_ledger picks archive-or-live; with no seam configured it stays cfg.Storage.S3BucketLive, which does NOT hold historic ranges — pass the archive bucket for those (see opsutil.ResolveStreamBucket)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" || *from == 0 || *to == 0 || *to < *from {
		return fmt.Errorf("-config, -from, -to are required; -to must be >= -from")
	}
	write := gate.Banner()

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}

	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage open: %w", err)
	}
	defer func() { _ = store.Close() }()

	cursorSrc := "census-backfill"
	cursorSub := opsutil.RangeCursorKey(uint32(*from), uint32(*to))
	startLedger := uint32(*from)
	if *resume {
		prior, gerr := store.GetCursor(ctx, cursorSrc, cursorSub)
		if gerr == nil && prior.LastLedger >= uint32(*from) {
			startLedger = prior.LastLedger + 1
			fmt.Fprintf(os.Stderr, "census-backfill: resuming at ledger %d (prior last_ledger=%d)\n",
				startLedger, prior.LastLedger)
		} else if gerr != nil && !errors.Is(gerr, timescale.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "census-backfill: read prior cursor failed (%v) — starting from -from\n", gerr)
		}
	}
	if startLedger > uint32(*to) {
		fmt.Fprintf(os.Stderr, "census-backfill: cursor already at or past -to (%d ≥ %d) — nothing to do\n",
			startLedger, *to)
		return nil
	}

	// Resolve on the range actually about to be WALKED (startLedger, not
	// -from): a resumed run may begin above a seam its original -from sat
	// below, and the bucket must match the ledgers this process will read.
	streamBucket, berr := censusBucket(cfg, *bucket, startLedger, uint32(*to))
	if berr != nil {
		return fmt.Errorf("census-backfill: %w", berr)
	}
	lsCfg := opsutil.NewBoundedLedgerStreamConfig(cfg, streamBucket, 1)
	passphrase := cfg.Stellar.Passphrase()

	fmt.Fprintf(os.Stderr, "census-backfill: streaming ledgers %d..%d from bucket %q\n",
		startLedger, *to, streamBucket)

	var (
		total         int
		skipped       int
		persisted     int    // ledgers whose substrate row is durably committed
		lastProcessed uint32 // last ledger we actually WROTE a row for (logging only)
		// wm is the resume checkpoint: the highest ledger with EVERY ledger up to it persisted. It freezes at the first
		// un-persisted ledger so the checkpoint never strides past a gap; a stuck ledger stalls loudly, not silently.
		wm             contiguousWatermark
		lastCheckpoint = time.Now()
	)
	const checkpointInterval = 30 * time.Second

	// A preview must not advance the resume checkpoint: the next run
	// would skip the ledgers it only LOOKED at and leave a substrate hole
	// behind a cursor that claims they are done (the stride-past
	// failure, with the write never having happened at all).
	checkpoint := func(seq uint32) {
		if !write {
			return
		}
		if err := store.UpsertCursor(ctx, cursorSrc, cursorSub, seq); err != nil {
			fmt.Fprintf(os.Stderr, "census-backfill: checkpoint at %d failed: %v\n", seq, err)
		}
	}

	walkErr := ledgerstream.Stream(ctx, lsCfg, startLedger, uint32(*to),
		func(lcm sdkxdr.LedgerCloseMeta) error {
			total++
			census, cerr := dispatcher.CensusLedger(lcm, passphrase)
			if cerr != nil {
				// Ledger not persisted — freeze the checkpoint so resume
				// re-reads from here rather than striding past it.
				wm.gap()
				fmt.Fprintf(os.Stderr, "census-backfill: ledger %d census: %v\n", lcm.LedgerSequence(), cerr)
				return nil
			}
			// A ledger not fully read (malformed tx, failed GetTransactionEvents) must NOT get an authoritative "complete" row:
			// its SorobanEventCount undercounts. Skip it; the checkpoint stays behind so a re-run re-reads it.
			if census.TxReadErrors > 0 || census.TxEventReadErrors > 0 {
				skipped++
				wm.gap()
				fmt.Fprintf(os.Stderr, "census-backfill: ledger %d had %d tx read errors + %d tx-event read errors; skipping substrate row\n",
					census.LedgerSeq, census.TxReadErrors, census.TxEventReadErrors)
				return nil
			}
			row := timescale.LedgerIngestRow{
				LedgerSeq:               census.LedgerSeq,
				LedgerCloseTime:         census.LedgerCloseTime,
				LedgerHash:              census.LedgerHash[:],
				PrevLedgerHash:          census.PrevLedgerHash[:],
				SorobanEventCount:       census.SorobanEventCount,
				ClassicTradeEffectCount: census.ClassicTradeEffectCount,
			}
			if ierr := upsertCensusRow(ctx, store, write, row); ierr != nil {
				// Row not durably written — freeze the checkpoint here too,
				// otherwise the next successful ledger would checkpoint past
				// this un-persisted one (same stride-past class as a skip).
				wm.gap()
				fmt.Fprintf(os.Stderr, "census-backfill: upsert ledger %d: %v\n", census.LedgerSeq, ierr)
				return nil
			}
			lastProcessed = census.LedgerSeq
			persisted++
			wm.persisted(census.LedgerSeq)
			if wm.seq > 0 && time.Since(lastCheckpoint) >= checkpointInterval {
				checkpoint(wm.seq)
				lastCheckpoint = time.Now()
				fmt.Fprintf(os.Stderr, "census-backfill: %d ledgers processed (at %d, checkpoint %d, %d skipped)\n",
					total, lastProcessed, wm.seq, skipped)
			}
			return nil
		},
	)

	// Flush the final checkpoint at the contiguous watermark, never past it. Use a FRESH bounded context:
	// on SIGINT the parent ctx is already canceled and would drop the final watermark.
	if wm.seq > 0 && write {
		fctx, fcancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := store.UpsertCursor(fctx, cursorSrc, cursorSub, wm.seq); err != nil {
			fmt.Fprintf(os.Stderr, "census-backfill: final checkpoint at %d failed: %v\n", wm.seq, err)
		}
		fcancel()
	}

	if walkErr != nil && !errors.Is(walkErr, context.Canceled) {
		return fmt.Errorf("census-backfill stream (last processed %d): %w", lastProcessed, walkErr)
	}
	fmt.Fprintf(os.Stderr, "census-backfill: %d ledgers streamed, %d %s, %d skipped, last %d\n",
		total, persisted, writeModeVerb(write, "persisted", "WOULD be persisted"), skipped, lastProcessed)
	// A run that did not persist its whole range must NOT exit 0 — see
	// censusCoverage.
	if cerr := censusCoverage(startLedger, uint32(*to), persisted, skipped, streamBucket, ctx.Err() != nil); cerr != nil {
		return fmt.Errorf("census-backfill: %w", cerr)
	}
	if !write {
		fmt.Fprintf(os.Stderr, "census-backfill: done — [%d,%d] censused from %q (DRY RUN, nothing written; pass -write to apply)\n",
			startLedger, *to, streamBucket)
		return nil
	}
	fmt.Fprintf(os.Stderr, "census-backfill: done — [%d,%d] complete from %q\n", startLedger, *to, streamBucket)
	return nil
}

// censusBucket resolves the galexie bucket a census-backfill walk reads. The live bucket cannot hold pre-live ranges
// and TolerateTrailingMissing would make that look like a clean walk (ADR-0033); policy lives in opsutil.ResolveStreamBucket.
func censusBucket(cfg config.Config, override string, from, to uint32) (string, error) {
	return opsutil.ResolveStreamBucket(cfg, override, from, to)
}

// censusCoverage fails a run that did not persist its whole range, naming the bucket, because the reconcile measures completeness
// against a ledger_ingest_log row per ledger. `from` is post-resume, so ledgers banked by an earlier run are not re-charged.
func censusCoverage(from, to uint32, persisted, skipped int, bucket string, interrupted bool) error {
	want := int64(to) - int64(from) + 1
	switch {
	case interrupted:
		return fmt.Errorf("INTERRUPTED after persisting %d of %d ledgers in [%d,%d] from %q — the range is NOT complete; re-run it (the cursor resumes at the first gap)",
			persisted, want, from, to, bucket)
	case persisted == 0:
		return fmt.Errorf("persisted ZERO of %d ledgers for [%d,%d] from %q — that bucket does not hold this range (wrong bucket for a historic range is the usual cause: pass -bucket <archive bucket>); NOT recording this range as backfilled",
			want, from, to, bucket)
	case int64(persisted) < want:
		return fmt.Errorf("persisted only %d of %d ledgers for [%d,%d] from %q — %d ledgers are missing from the bucket, failed their census, or were skipped for tx read errors (%d skipped; see the per-ledger errors above); the substrate range is NOT complete",
			persisted, want, from, to, bucket, want-int64(persisted), skipped)
	}
	return nil
}

// contiguousWatermark tracks the highest ledger such that every ledger from the run's start through it is durably persisted.
// The first gap() freezes it; it relies on in-order delivery. seq is 0 until the first persisted ledger.
type contiguousWatermark struct {
	seq    uint32
	frozen bool
}

func (w *contiguousWatermark) persisted(seq uint32) {
	if w.frozen {
		return
	}
	w.seq = seq
}

func (w *contiguousWatermark) gap() {
	w.frozen = true
}

// upsertCensusRow writes one ledger's substrate row, or in the default preview reports success without writing, so the walk still
// exercises the read + census path and the coverage check.
func upsertCensusRow(ctx context.Context, store *timescale.Store, write bool, row timescale.LedgerIngestRow) error {
	if !write {
		return nil
	}
	return store.UpsertLedgerIngestLog(ctx, row)
}
