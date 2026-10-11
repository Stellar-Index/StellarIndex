package ingest

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	soroswap_router "github.com/Stellar-Index/StellarIndex/internal/sources/soroswap_router"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// tagRoutedVia is the HISTORICAL half of router attribution (migration 0025
// Phase B). It walks soroswap_router_swaps in ledger windows and back-tags every
// same-(ledger, tx_hash) soroswap `trades` row via timescale.TagTradesRoutedVia,
// the primitive the live sweeper uses, so historical and live tagging policy
// (first-wins, soroswap-scoped, time-bounded, call-path-attributed) cannot drift.
// A row whose router call was a sub_invocation gets its outermost wrapping
// contract's registry name when registered (migrations 0101/0103); otherwise it
// falls back to routed_via='soroswap-router'.
//
// SQL-only. Each window is one UPDATE bounded by the window's router-swap
// close-time span so TimescaleDB prunes trades chunks; windows over compressed
// chunks decompress segments, hence windowed rather than one statement.
//
// Idempotent and resumable: tagged rows never match (routed_via IS NULL), and
// progress checkpoints into ingestion_cursors as (source='tag-routed-via',
// sub_source='<from>-<to>') after each window (-resume defaults to true).
//
// Fail-closed (opsutil.WriteGate): the default run is a DRY RUN; -write applies.
func tagRoutedVia(args []string) error { //nolint:funlen,gocognit,gocyclo // linear windowed pass: flags → bounds → per-window UPDATE + checkpoint
	fs, gate := opsutil.NewMutatingFlagSet("tag-routed-via")
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	from := fs.Uint("from", 0, "First ledger (inclusive). Default: min(ledger) in soroswap_router_swaps.")
	to := fs.Uint("to", 0, "Last ledger (inclusive). Default: max(ledger) in soroswap_router_swaps.")
	window := fs.Uint("window", 500_000, "Ledgers per UPDATE window")
	resume := fs.Bool("resume", true, "Resume from the ingestion_cursors checkpoint of a prior run with the same -from/-to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	if *window == 0 {
		return errors.New("-window must be > 0")
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

	// Default bounds: the full extent of the router-swap record.
	fromLedger, toLedger := uint32(*from), uint32(*to)
	if fromLedger == 0 || toLedger == 0 {
		lo, hi, ok, berr := store.RouterSwapLedgerBounds(ctx)
		if berr != nil {
			return berr
		}
		if !ok {
			fmt.Fprintln(os.Stderr, "tag-routed-via: soroswap_router_swaps is empty — nothing to tag")
			return nil
		}
		if fromLedger == 0 {
			fromLedger = lo
		}
		if toLedger == 0 {
			toLedger = hi
		}
	}
	if toLedger < fromLedger {
		return fmt.Errorf("-to (%d) must be >= -from (%d)", toLedger, fromLedger)
	}

	const cursorSrc = "tag-routed-via"
	start, cursorSub := resumeRangeStart(ctx, store, cursorSrc, fromLedger, toLedger, *resume)
	if start > toLedger {
		fmt.Fprintf(os.Stderr, "tag-routed-via: checkpoint already past -to (%d > %d) — nothing to do\n",
			start, toLedger)
		return nil
	}

	fmt.Fprintf(os.Stderr, "tag-routed-via: ledgers %d..%d, window %d, router=%s scope=%s\n",
		start, toLedger, *window, soroswap_router.SourceName, soroswap.SourceName)

	var totalTagged, windowsPending int64
	for lo := start; lo <= toLedger; {
		hi := lo + uint32(*window) - 1
		if hi > toLedger || hi < lo { // hi<lo guards uint32 overflow
			hi = toLedger
		}
		if ctx.Err() != nil {
			return fmt.Errorf("interrupted at window %d..%d (checkpoint saved through %d)", lo, hi, lo-1)
		}

		minTS, maxTS, ok, berr := store.RouterSwapTimeBounds(ctx, lo, hi)
		if berr != nil {
			return berr
		}
		switch {
		case ok && write:
			// [minTS, maxTS+1s): TagTradesRoutedVia's range is
			// half-open; +1s makes the inclusive max representable.
			tagged, terr := store.TagTradesRoutedVia(ctx,
				soroswap_router.SourceName, soroswap.SourceName,
				minTS, maxTS.Add(time.Second))
			if terr != nil {
				return fmt.Errorf("window %d..%d: %w", lo, hi, terr)
			}
			totalTagged += tagged
			fmt.Fprintf(os.Stderr, "tag-routed-via: window %d..%d tagged %d trades (total %d)\n",
				lo, hi, tagged, totalTagged)
		case ok:
			// Preview: the UPDATE's own predicate decides the row count,
			// and asking for it without running it means a second scan of
			// the very chunks the windowing exists to keep compressed. So
			// the preview reports the WINDOW it would tag, not an estimate.
			windowsPending++
			fmt.Fprintf(os.Stderr, "tag-routed-via: window %d..%d WOULD tag soroswap trades in [%s, %s) (pass -write to apply)\n",
				lo, hi, minTS.UTC().Format(time.RFC3339), maxTS.Add(time.Second).UTC().Format(time.RFC3339))
		}

		// A preview must not advance the resume checkpoint: the next run
		// would skip the windows it only LOOKED at.
		if write {
			if cerr := store.UpsertCursor(ctx, cursorSrc, cursorSub, hi); cerr != nil {
				return fmt.Errorf("checkpoint at ledger %d: %w", hi, cerr)
			}
		}
		if hi == toLedger {
			break
		}
		lo = hi + 1
	}

	if !write {
		fmt.Fprintf(os.Stderr, "tag-routed-via: done. %d window(s) across ledgers %d..%d hold router swaps and WOULD be tagged — pass -write to apply\n",
			windowsPending, start, toLedger)
		return nil
	}
	fmt.Fprintf(os.Stderr, "tag-routed-via: done. %d trades tagged across ledgers %d..%d\n",
		totalTagged, start, toLedger)
	return nil
}
