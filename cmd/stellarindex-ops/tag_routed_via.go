package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/StellarIndex/stellar-index/internal/config"
	"github.com/StellarIndex/stellar-index/internal/sources/soroswap"
	soroswap_router "github.com/StellarIndex/stellar-index/internal/sources/soroswap_router"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

// tagRoutedVia is the HISTORICAL half of router attribution
// (migration 0025 Phase B / BACKLOG #29). It walks the persisted
// router-invocation record (soroswap_router_swaps) in ledger windows
// and back-tags every same-(ledger, tx_hash) soroswap `trades` row
// with routed_via='soroswap-router' via the same
// timescale.TagTradesRoutedVia primitive the live sweeper uses — so
// the historical and live tagging policies (first-wins, soroswap-
// scoped, time-bounded) cannot drift.
//
// SQL-only: no Galexie walk, no decoders. Both join sides are
// already in Postgres. Each window is one UPDATE whose predicate is
// bounded by the window's router-swap close-time span, so
// TimescaleDB prunes trades chunks per window. Windows over trades
// chunks older than the compression horizon decompress the affected
// segments — that is why this runs windowed rather than as one
// statement over all history.
//
// Idempotent + resumable: already-tagged rows never match
// (routed_via IS NULL), and progress checkpoints into
// ingestion_cursors as (source='tag-routed-via',
// sub_source='soroswap-router') after each completed window.
// Re-running resumes past the last completed window; -resume=false
// re-sweeps from the start (harmless, just slower).
func tagRoutedVia(args []string) error { //nolint:funlen,gocognit,gocyclo // linear windowed pass: flags → bounds → per-window UPDATE + checkpoint
	fs := flag.NewFlagSet("tag-routed-via", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	from := fs.Uint("from", 0, "First ledger (inclusive). Default: min(ledger) in soroswap_router_swaps.")
	to := fs.Uint("to", 0, "Last ledger (inclusive). Default: max(ledger) in soroswap_router_swaps.")
	window := fs.Uint("window", 500_000, "Ledgers per UPDATE window")
	resume := fs.Bool("resume", true, "Resume from the saved ingestion_cursors checkpoint")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	if *window == 0 {
		return errors.New("-window must be > 0")
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}

	ctx, cancel := signalContext()
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

	const (
		cursorSrc = "tag-routed-via"
		cursorSub = soroswap_router.SourceName
	)
	start := fromLedger
	if *resume {
		prior, gerr := store.GetCursor(ctx, cursorSrc, cursorSub)
		switch {
		case gerr == nil && prior.LastLedger >= fromLedger:
			start = prior.LastLedger + 1
			fmt.Fprintf(os.Stderr, "tag-routed-via: resuming at ledger %d (checkpoint last_ledger=%d)\n",
				start, prior.LastLedger)
		case gerr != nil && !errors.Is(gerr, timescale.ErrNotFound):
			fmt.Fprintf(os.Stderr, "tag-routed-via: read cursor failed (%v) — starting from -from\n", gerr)
		}
	}
	if start > toLedger {
		fmt.Fprintf(os.Stderr, "tag-routed-via: checkpoint already past -to (%d > %d) — nothing to do\n",
			start, toLedger)
		return nil
	}

	fmt.Fprintf(os.Stderr, "tag-routed-via: ledgers %d..%d, window %d, router=%s scope=%s\n",
		start, toLedger, *window, soroswap_router.SourceName, soroswap.SourceName)

	var totalTagged int64
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
		if ok {
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
		}

		if cerr := store.UpsertCursor(ctx, cursorSrc, cursorSub, hi); cerr != nil {
			return fmt.Errorf("checkpoint at ledger %d: %w", hi, cerr)
		}
		if hi == toLedger {
			break
		}
		lo = hi + 1
	}

	fmt.Fprintf(os.Stderr, "tag-routed-via: done. %d trades tagged across ledgers %d..%d\n",
		totalTagged, start, toLedger)
	return nil
}
