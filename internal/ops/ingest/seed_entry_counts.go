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

// seedEntryCounts recomputes source_entry_counts with a full GROUP BY, SETting (not adding) so re-runs converge.
// Run it at the tip after the all-time backfill: the scan is lock-hungry and not race-free against live ingest.
// -write is required; a run with neither -write nor -dry-run is refused.
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
