package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// seedEntryCounts authoritatively recomputes the per-source entry tally
// (source_entry_counts, migration 0035) from a full GROUP BY over every
// decoded-event hypertable (list on [timescale.Store.SeedSourceEntryCounts]),
// overwriting the table.
//
// The writers keep the counter live, but a fresh table only counts entries ingested
// SINCE the counter went live, so this is the one-shot reconciliation for
// pre-counter history and crash-induced increment drift. It SETs, not ADDs, so
// re-running converges.
//
// Run it after the all-time backfill, when ingest only appends at the tip: the
// GROUP BY scans every `trades` chunk in one transaction (within the 4096
// max_locks_per_transaction budget) but is slow and lock-hungry mid-backfill. It is
// not race-free against concurrent tip ingest (<0.01% drift) and self-corrects on
// the next run.
//
// -write is required; a run passing neither -write nor -dry-run is refused so a
// script written before -write fails rather than skipping.
func seedEntryCounts(args []string) error {
	fs, gate := opsutil.NewMutatingFlagSet("seed-entry-counts")
	cfgPath := fs.String("config", "", "path to stellarindex.toml (required)")
	timeout := fs.Duration("timeout", 30*time.Minute, "wall-clock budget for the recount")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config required")
	}
	if err := gate.RequireStatedMode(); err != nil {
		return err
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if !gate.Banner() {
		fmt.Fprintln(os.Stderr, "seed-entry-counts: would recompute source_entry_counts from every decoded-event hypertable")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	defer func() { _ = store.Close() }()

	fmt.Fprintln(os.Stderr, "seed-entry-counts: recomputing source_entry_counts from every decoded-event hypertable (this scans every trades chunk — run post-backfill)…")

	n, err := store.SeedSourceEntryCounts(ctx)
	if err != nil {
		return fmt.Errorf("recount: %w", err)
	}
	fmt.Fprintf(os.Stderr, "seed-entry-counts: %d source rows reconciled\n", n)
	return nil
}
