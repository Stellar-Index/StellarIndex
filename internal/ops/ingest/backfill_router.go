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
	soroswap_router "github.com/Stellar-Index/StellarIndex/internal/sources/soroswap_router"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// backfillRouter walks Galexie ledger metadata for a range and
// reconstructs `soroswap_router_swaps` rows by replaying the
// soroswap-router ContractCallDecoder against every InvokeContract
// op. The decoder is pure (no state), so this is safe to re-run; the
// destination table's PK on (ledger_close_time, ledger, tx_hash,
// op_index) + the generation-guarded upsert make every replay idempotent.
//
// Why this exists despite ADR-0032's "no per-source backfill"
// invariant: that ADR's projector path reads from the
// `soroban_events` landing zone, but the soroswap router emits ZERO
// Soroban events (its work is invoking per-pair contracts). The
// projector therefore cannot rebuild router history — there's no
// landing-zone signal to project from. The only ground truth for
// historical router invocations is the raw ledger metadata in
// Galexie. This subcommand reads it directly via ledgerstream.Stream,
// mirroring `verify-decoders` but writing instead of dry-running.
//
// Resume semantics: progress checkpoints into ingestion_cursors as
// (source='backfill-router', sub_source='<from>-<to>',
// last_ledger=<latest processed>). Re-running the same -from/-to
// resumes from the saved cursor. Restart-safe.
//
// Fail-closed (opsutil.WriteGate): the default run is a DRY RUN that
// walks and decodes the range and reports the rows it WOULD insert,
// writing neither them nor a checkpoint. -write applies.
func backfillRouter(args []string) error { //nolint:funlen,gocognit,gocyclo // linear pipeline, splitting reduces readability
	fs, gate := opsutil.NewMutatingFlagSet("backfill-router")
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	from := fs.Uint("from", 0, "First ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "Last ledger sequence (inclusive, required)")
	resume := fs.Bool("resume", true, "Resume from saved cursor if a checkpoint exists for this from/to pair (default true)")
	bucket := fs.String("bucket", "", "Override bucket (default: s3_bucket_archive, then s3_bucket_live)")
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
	streamBucket, err := opsutil.HistoricReadBucket(cfg, *bucket)
	if err != nil {
		return err
	}

	// Long-lived context for the whole backfill; signal-cancellable
	// so we flush a final checkpoint on SIGTERM/SIGINT.
	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	// Storage handle for inserts + cursor checkpointing.
	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage open: %w", err)
	}
	defer func() { _ = store.Close() }()
	// Re-derive path (INV-3 / migration 0110): stamp a positive
	// derive_generation so a corrected router re-walk (fixed decoder / amount
	// scaling) UPDATEs the stored soroswap_router_swaps rows in place —
	// InsertSoroswapRouterSwap reads the generation from this store — and wins
	// over the live gen-0 values instead of silently no-op'ing. This is the
	// only protocol re-derive entry point that writes a projector table via a
	// DIRECT store call rather than through pipeline.HandleEvent (which
	// projected-rebuild / ch-rebuild already stamp).
	store.SetDeriveGeneration(time.Now().Unix())

	// Minimal dispatcher: just the router decoder. Every other source
	// is irrelevant for this backfill and avoiding their decoders
	// keeps the per-ledger processing tight.
	disp := dispatcher.New()
	disp.AddContractCallDecoder(soroswap_router.NewDecoder(soroswap_router.MainnetRouter))

	cursorSrc := routerCursorSource
	cursorSub := opsutil.RangeCursorKey(uint32(*from), uint32(*to))
	startLedger, done, err := routerWriteStart(ctx, store, cfg, uint32(*from), uint32(*to), *resume, write)
	if err != nil || done {
		return err
	}

	lsCfg := opsutil.NewBoundedLedgerStreamConfig(cfg, streamBucket, 1)

	fmt.Fprintf(os.Stderr, "backfill-router: streaming ledgers %d..%d from bucket %q\n",
		startLedger, *to, streamBucket)

	var (
		totalLedgers   int
		totalRows      int
		insertFailures int
		lastCheckpoint = time.Now()
		// First-write checkpoint: persist immediately so a fresh
		// invocation lays down its presence even if it crashes before
		// the first scheduled checkpoint window.
		firstCheckpoint = true
	)
	const checkpointInterval = 30 * time.Second

	persist := func(ev soroswap_router.Event, ledger uint32, closedAt time.Time) {
		row := timescale.SoroswapRouterSwap{
			Ledger:          ledger,
			LedgerCloseTime: closedAt,
			TxHash:          ev.Swap.TxHash,
			OpIndex:         uint32(ev.Swap.OpIndex),
			ContractID:      ev.Swap.ContractID,
			FunctionName:    ev.Swap.Function,
			OpSource:        ev.Swap.OpSource,
			TxSource:        ev.Swap.TxSource,
			Recipient:       ev.Swap.Recipient,
			Path:            ev.Swap.Path,
			AmountIn:        ev.Swap.AmountIn.String(),
			AmountOut:       ev.Swap.AmountOut.String(),
			CallSig:         ev.Swap.CallSig(),
			// ROADMAP #11 tree-position columns (migration 0101).
			CallPath:  ev.Swap.CallPath,
			CallDepth: ev.Swap.CallDepth,
			CallKind:  ev.Swap.CallKind,
		}
		if !ev.Swap.DeadlineTs.IsZero() {
			row.DeadlineTS = &ev.Swap.DeadlineTs
		}
		if ierr := insertRouterSwap(ctx, store, write, row); ierr != nil {
			insertFailures++
			if insertFailures < 10 {
				fmt.Fprintf(os.Stderr, "backfill-router: insert ledger=%d tx=%s: %v\n",
					ledger, ev.Swap.TxHash, ierr)
			}
			return
		}
		totalRows++
	}

	// lastWalked is the highest ledger the walk actually delivered. The
	// terminal checkpoint MUST use this, not the operator's -to.
	var lastWalked uint32

	checkpoint := func(ledger uint32, force bool) {
		// A preview must not advance the resume checkpoint: -resume
		// defaults to true, so the next run would skip the ledgers this
		// one only decoded and never insert their rows.
		if !write {
			return
		}
		// Never advance the cursor past a ledger whose rows failed to
		// insert. -resume defaults to true and SKIPS checkpointed
		// ledgers, so advancing here loses those rows permanently with
		// no dead-letter record. This mirrors projected-rebuild's
		// checkpointWindow, which withholds for exactly this reason
		// (cold audit 2026-08-04).
		if insertFailures > 0 {
			return
		}
		if !force && time.Since(lastCheckpoint) < checkpointInterval {
			return
		}
		if cerr := store.UpsertCursor(ctx, cursorSrc, cursorSub, ledger); cerr != nil {
			fmt.Fprintf(os.Stderr, "backfill-router: checkpoint at ledger %d failed: %v\n", ledger, cerr)
			return
		}
		lastCheckpoint = time.Now()
	}

	streamErr := ledgerstream.Stream(ctx, lsCfg, startLedger, uint32(*to),
		func(lcm sdkxdr.LedgerCloseMeta) error {
			totalLedgers++
			lastWalked = lcm.LedgerSequence()
			outputs, perr := disp.ProcessLedger(lcm, cfg.Stellar.Passphrase())
			if perr != nil {
				// One-ledger failures are noisy-but-not-fatal for a
				// historical backfill — log + continue rather than
				// abort the whole sweep.
				fmt.Fprintf(os.Stderr, "backfill-router: ledger %d: %v\n",
					lcm.LedgerSequence(), perr)
				return nil
			}
			closedAt := time.Unix(int64(lcm.LedgerCloseTime()), 0).UTC()
			for _, ev := range outputs {
				re, ok := ev.(soroswap_router.Event)
				if !ok {
					continue
				}
				persist(re, lcm.LedgerSequence(), closedAt)
			}
			// Checkpoint periodically + on first ledger.
			if firstCheckpoint {
				checkpoint(lcm.LedgerSequence(), true)
				firstCheckpoint = false
			} else {
				checkpoint(lcm.LedgerSequence(), false)
			}
			// Heartbeat every 10k ledgers so the operator can see
			// progress.
			if totalLedgers%10000 == 0 {
				fmt.Fprintf(os.Stderr, "backfill-router: %d ledgers processed, %d rows inserted (ledger=%d)\n",
					totalLedgers, totalRows, lcm.LedgerSequence())
			}
			return nil
		},
	)

	// Force a final checkpoint with the LAST processed ledger,
	// independent of the periodic timer, so resume always picks up
	// exactly where the run stopped (whether clean exit, signal, or
	// error). The cursor row may not exist if streamErr fired before
	// any ledger was processed — that's fine, GetCursor will
	// ErrNotFound on the next run.
	//
	// lastWalked, NOT *to. This used to checkpoint the operator's
	// declared range top regardless of how far the walk actually got —
	// and UpsertCursor is monotonic-forward, so that write always won
	// and could never be corrected. A run that failed partway (or that
	// returned success with a trailing hole, which
	// TolerateTrailingMissing permits) therefore jumped the cursor to
	// the range top, and the retry printed "cursor already at or past
	// -to — nothing to do" and exited 0 with the remainder permanently
	// unfilled. This is the same defect the indexer's seamed reader
	// documents one layer up (cold audit 2026-08-04).
	if totalLedgers > 0 && streamErr == nil {
		checkpoint(lastWalked, true)
	}

	if streamErr != nil {
		return fmt.Errorf("stream: %w (cursor left at the last clean checkpoint; re-run to continue)", streamErr)
	}

	// Charged against startLedger: a resumed run only owes the ledgers above its cursor.
	if err := rangeWalkCoverage("backfill-router", startLedger, uint32(*to), totalLedgers, streamBucket); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "backfill-router: done. %d ledgers, %d rows %s (%d insert failures)\n",
		totalLedgers, totalRows, writeModeVerb(write, "inserted", "WOULD be inserted (pass -write to apply)"), insertFailures)
	if insertFailures > 0 {
		return fmt.Errorf("%d insert failures — see stderr above", insertFailures)
	}
	return nil
}

// routerCursorSource keys backfill-router's per-range resume cursor.
const routerCursorSource = "backfill-router"

// routerWriteStart resolves the ledger a backfill-router run starts at
// (resuming from its range cursor) and, for a -write run with ledgers left to
// walk, records the dirty window over [from, to] before the first insert.
// done reports a range whose cursor is already at or past to.
func routerWriteStart(ctx context.Context, store dirtyWindowRecorder, cfg config.Config, from, to uint32, resume, write bool) (start uint32, done bool, err error) {
	start = from
	if resume {
		prior, gerr := store.GetCursor(ctx, routerCursorSource, opsutil.RangeCursorKey(from, to))
		if gerr == nil && prior.LastLedger >= from {
			start = prior.LastLedger + 1
			fmt.Fprintf(os.Stderr, "backfill-router: resuming at ledger %d (prior checkpoint last_ledger=%d)\n",
				start, prior.LastLedger)
		} else if gerr != nil && !errors.Is(gerr, timescale.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "backfill-router: read prior cursor failed (%v) — starting from -from\n", gerr)
		}
	}
	if start > to {
		fmt.Fprintf(os.Stderr, "backfill-router: cursor already at or past -to (%d ≥ %d) — nothing to do\n", start, to)
		return start, true, nil
	}
	if err := recordBackfillDirtyWindows(ctx, store, cfg, backfillOpts{
		from: from, to: to, sources: []string{"soroswap-router"}, dryRun: !write,
	}); err != nil {
		return 0, false, err
	}
	return start, false, nil
}

// insertRouterSwap writes one reconstructed router swap, or — in the
// default fail-closed preview — reports success without touching
// soroswap_router_swaps, so the walk still exercises the decoder and the
// zero-ledger bucket guard while writing nothing.
func insertRouterSwap(ctx context.Context, store *timescale.Store, write bool, row timescale.SoroswapRouterSwap) error {
	if !write {
		return nil
	}
	return store.InsertSoroswapRouterSwap(ctx, row)
}

// rangeWalkCoverage fails a bounded walk whose delivered count is not exactly
// the ledgers [from, to] holds. TolerateTrailingMissing ends a walk at a missing object
// with a nil error, so the delivered count is the only sign the range was cut short.
func rangeWalkCoverage(cmd string, from, to uint32, walked int, bucket string) error {
	requested := uint64(to) - uint64(from) + 1
	switch {
	case walked == 0:
		return fmt.Errorf(
			"%s walked 0 of %d ledgers in range [%d,%d] from bucket %q — "+
				"the bucket likely has no files there; historical ranges need the archive bucket, "+
				"and the archive's hourly mirror of live may not yet hold a -to near the tip",
			cmd, requested, from, to, bucket)
	case uint64(walked) < requested:
		return fmt.Errorf(
			"%s walked only %d of %d ledgers in range [%d,%d] from bucket %q — %d trailing ledgers were NOT walked: "+
				"an object is missing and the trailing-missing tolerance ended the walk early (see the ledgerstream "+
				"WARN above). The range is NOT complete; re-run once the objects exist",
			cmd, walked, requested, from, to, bucket, requested-uint64(walked))
	case uint64(walked) > requested:
		return fmt.Errorf(
			"%s walked %d ledgers but range [%d,%d] holds only %d — the delivered count is untrustworthy; "+
				"refusing to report the range complete",
			cmd, walked, from, to, requested)
	}
	return nil
}
