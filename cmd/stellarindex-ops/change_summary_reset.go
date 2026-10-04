package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// changeSummaryReset deletes one change_summary_5m row. The upsert ratchets
// ath/atl with GREATEST/LEAST, so a bad extreme already stored can only be
// cleared by removing the row; the aggregator's change-summary worker
// recreates it from the trailing 30 days on its next pass (≤5 min).
//
// Usage:
//
//	stellarindex-ops change-summary-reset -config /etc/stellarindex.toml \
//	  -entity-type coin -entity-id crypto:XLM -write
//
// Dry-run unless -write. Safe to re-run: deleting an absent row is a no-op.
func changeSummaryReset(args []string) error {
	fs, gate := opsutil.NewMutatingFlagSet("change-summary-reset")
	cfgPath := fs.String("config", "", "path to stellarindex.toml (required)")
	entityType := fs.String("entity-type", "", "change_summary_5m entity_type, e.g. coin | pair (required)")
	entityID := fs.String("entity-id", "", "change_summary_5m entity_id, e.g. crypto:XLM (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	if *entityType == "" || *entityID == "" {
		return errors.New("-entity-type and -entity-id are required")
	}
	gate.Banner()

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

	if gate.DryRun() {
		_, err := store.GetChangeSummary(ctx, *entityType, *entityID)
		switch {
		case err == nil:
			fmt.Fprintf(os.Stderr, "change-summary-reset: dry run — would delete %s/%s\n", *entityType, *entityID)
		case errors.Is(err, sql.ErrNoRows):
			fmt.Fprintf(os.Stderr, "change-summary-reset: dry run — no row for %s/%s\n", *entityType, *entityID)
		default:
			return err
		}
		return nil
	}
	existed, err := store.DeleteChangeSummary(ctx, *entityType, *entityID)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "change-summary-reset: %s/%s deleted=%t\n", *entityType, *entityID, existed)
	return nil
}
