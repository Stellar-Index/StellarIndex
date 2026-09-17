package chops

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ch-cohort-rollup — what the accounts an address created or sponsored
// went on to hold and do: current holdings, monthly flows, the contracts
// they moved value through, recent activity, and open DeFi positions,
// per root, into the account_cohort_* tables (deploy/clickhouse/
// account_cohort_rollup.sql). Backs /v1/accounts/{g}/graph/cohort.
//
// Two stores, one cycle: the DeFi position snapshot is read from the
// served tier's per-protocol folds (the six behind
// /v1/accounts/{g}/positions) and staged into ClickHouse first, so the
// cohort join happens beside the 25M-row membership instead of
// shipping the membership to Postgres. The rest is ClickHouse only:
// membership from the board rollups' edge tables, one walk over the
// movements archive, and folds.
//
// Runs AFTER the creators and sponsors rollups: membership is their
// edge tables, and the creator floor reads account_creators_rollup.
func chCohortRollup(args []string) error {
	fs := flag.NewFlagSet("ch-cohort-rollup", flag.ContinueOnError)
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	cfgPath := fs.String("config", "", "Path to TOML config file (required — the DeFi position snapshot is read from Postgres)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}
	ctx, cancel := opsutil.SignalContext()
	defer cancel()
	start := time.Now()
	logf := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "ch-cohort-rollup: "+format+"\n", a...)
	}

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return err
	}
	holders, err := store.DeFiPositionHolders(ctx)
	_ = store.Close()
	if err != nil {
		return err
	}
	logf("defi position snapshot: %d rows from the served tier in %s", len(holders), time.Since(start).Round(time.Second))

	if err := clickhouse.RunCohortRollup(ctx, *chAddr, holders, logf); err != nil {
		return err
	}
	logf("cycle complete in %s", time.Since(start).Round(time.Second))
	return nil
}
