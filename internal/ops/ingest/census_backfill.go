package ingest

import (
	"context"
	"errors"
	"flag"
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

// censusBackfill populates the ledger_ingest_log substrate record for
// a historical ledger range (ADR-0033 Phase 2). The live indexer
// writes this record going forward; this subcommand walks Galexie
// metadata for [from, to] and writes the decoder-independent census
// (soroban_event_count + classic_trade_effect_count) plus the
// hash-chain anchors for ledgers that predate live capture.
//
// No decoder runs — this is a pure structural walk (dispatcher.
// CensusLedger), so it is fast and safe to re-run: UpsertLedgerIngestLog
// is ON CONFLICT DO UPDATE, so overlapping ranges and re-runs converge.
//
// Resume: checkpoints into ingestion_cursors as
// (source='census-backfill', sub_source='<from>-<to>'). Re-running the
// same -from/-to resumes from the last processed ledger. Restart-safe.
func censusBackfill(args []string) error { //nolint:gocognit,gocyclo,funlen // linear walk + checkpoint loop; splitting reduces clarity (same as backfillRouter).
	fs := flag.NewFlagSet("census-backfill", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	from := fs.Uint("from", 0, "First ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "Last ledger sequence (inclusive, required)")
	resume := fs.Bool("resume", true, "Resume from saved cursor if a checkpoint exists for this from/to pair")
	bucket := fs.String("bucket", "", "Override storage bucket (default cfg.Storage.S3BucketLive)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" || *from == 0 || *to == 0 || *to < *from {
		return fmt.Errorf("-config, -from, -to are required; -to must be >= -from")
	}

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
	cursorSub := fmt.Sprintf("%d-%d", *from, *to)
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

	streamBucket := cfg.Storage.S3BucketLive
	if *bucket != "" {
		streamBucket = *bucket
	}
	lsCfg := opsutil.NewBoundedLedgerStreamConfig(cfg, streamBucket, 1)
	passphrase := cfg.Stellar.Passphrase()

	fmt.Fprintf(os.Stderr, "census-backfill: streaming ledgers %d..%d from bucket %q\n",
		startLedger, *to, streamBucket)

	var (
		total         int
		skipped       int
		lastProcessed uint32 // last ledger we actually WROTE a row for (logging only)
		// C2-14: `wm` is the durable resume checkpoint — the highest
		// ledger such that EVERY ledger from startLedger through it was
		// persisted (no census error, no skip, no upsert failure). The
		// moment any ledger in the run is left un-persisted the watermark
		// FREEZES, so the checkpoint can never stride PAST a gap. The old
		// code checkpointed `lastProcessed` (the last row written), which
		// advanced right over mid-range skipped ledgers: on resume the
		// cursor sat beyond the gap and the skipped ledgers were never
		// re-read, leaving a permanent substrate hole. Freezing instead
		// re-reads the gap on the next run (idempotent UpsertLedgerIngestLog
		// converges) — and if the ledger is still unreadable the run makes
		// no forward progress, which is a LOUD stall rather than a silent
		// gap (mirrors the C2-1 "durable watermark = last fully-committed"
		// posture).
		wm             contiguousWatermark
		lastCheckpoint = time.Now()
	)
	const checkpointInterval = 30 * time.Second

	checkpoint := func(seq uint32) {
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
			// A ledger we could not fully read (a malformed tx, or a tx
			// whose GetTransactionEvents failed — e.g. an unsupported
			// future meta version) must NOT get an authoritative
			// "complete" substrate row: its SorobanEventCount undercounts,
			// so a projection reconcile against it would falsely pass
			// (G15-06). Skip it; a later re-run on a fixed reader writes
			// the real row. The checkpoint must stay behind the skip so
			// that "later re-run" actually re-reads it (C2-14).
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
			if ierr := store.UpsertLedgerIngestLog(ctx, row); ierr != nil {
				// Row not durably written — freeze the checkpoint here too,
				// otherwise the next successful ledger would checkpoint past
				// this un-persisted one (same stride-past class as a skip).
				wm.gap()
				fmt.Fprintf(os.Stderr, "census-backfill: upsert ledger %d: %v\n", census.LedgerSeq, ierr)
				return nil
			}
			lastProcessed = census.LedgerSeq
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

	// Flush a final checkpoint at the contiguous watermark — the last
	// ledger with NO un-persisted ledger before it — so a resume picks up
	// exactly at the first gap (whether we finished, were interrupted, or
	// hit an archive/read gap). Never past it (C2-14). Use a FRESH bounded
	// context: on a graceful SIGINT the parent ctx is already canceled by
	// the time Stream returns, and checkpointing with it would fail
	// instantly and drop the final resume watermark (F-1318 pattern).
	if wm.seq > 0 {
		fctx, fcancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := store.UpsertCursor(fctx, cursorSrc, cursorSub, wm.seq); err != nil {
			fmt.Fprintf(os.Stderr, "census-backfill: final checkpoint at %d failed: %v\n", wm.seq, err)
		}
		fcancel()
	}

	if walkErr != nil && !errors.Is(walkErr, context.Canceled) {
		return fmt.Errorf("census-backfill stream (last processed %d): %w", lastProcessed, walkErr)
	}
	fmt.Fprintf(os.Stderr, "census-backfill: done — %d ledgers processed, %d skipped, last %d\n",
		total, skipped, lastProcessed)
	return nil
}

// contiguousWatermark tracks the highest ledger sequence such that every
// ledger from the run's start through it has been durably persisted, with
// NO gap (skipped or failed ledger) before it. It is the durability-honest
// resume checkpoint for an in-order ledger walk (C2-14).
//
// Callers report each ledger's outcome in stream order: persisted(seq) for
// a ledger whose row is durably committed, gap() for one that was skipped
// or failed to persist. The first gap FREEZES the watermark — subsequent
// persisted() calls do not advance it — so the checkpoint can never move
// past an un-persisted ledger. `seq` is 0 until the first contiguous
// ledger is persisted (so callers gate the checkpoint on seq > 0).
//
// Correctness depends on in-order delivery (ledgerstream.Stream walks
// [from, to] ascending): once frozen, the watermark holds the last ledger
// before the first gap, which is exactly where a resume must re-read from.
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
