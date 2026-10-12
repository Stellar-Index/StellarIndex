package chops

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// cohortPricesFrom is the network's genesis month: every USD-quoted month the served tier holds is loaded, so a
// cohort's oldest flow is valued at its own month.
var cohortPricesFrom = time.Date(2015, time.September, 1, 0, 0, 0, 0, time.UTC)

// closedMonthEdge is the exclusive upper bound for [Store.MonthlyUSDVWAPs]: now's month start, since admitting the
// accumulating month would serve a flickering partial VWAP (ADR-0015 closed-bucket rule).
func closedMonthEdge(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// sourceAmountDecimals is the source registry's amount scale, which the
// monthly price fold needs to weigh off-chain spellings in whole units.
func sourceAmountDecimals(source string) int {
	return external.Lookup(source).AmountScaleDecimals()
}

// cohortRollupLockPath is chCohortRollup's exclusive advisory lock, in the same state directory as the other rollup
// locks (see holdersRollupLockPath).
const cohortRollupLockPath = "/var/lib/stellarindex/ch-cohort-rollup.lock"

// ch-cohort-rollup rolls up what the accounts an address created or sponsored went on to hold and do into the account_cohort_* tables
// behind /v1/accounts/{g}/graph/cohort. Runs AFTER the creators and sponsors rollups (it reads their edge tables) and takes the lock at
// acquireRollupLock.
func chCohortRollup(args []string) error {
	fs, gate := opsutil.NewMutatingFlagSet("ch-cohort-rollup")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	cfgPath := fs.String("config", "", "Path to TOML config file (required — the DeFi position snapshot is read from Postgres)")
	lockPath := fs.String("lock-file", cohortRollupLockPath,
		"path to the exclusive advisory lock serializing this run against the timer or a second concurrent invocation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	if err := gate.RequireStatedMode(); err != nil {
		return fmt.Errorf("ch-cohort-rollup: %w", err)
	}
	if !gate.Banner() {
		fmt.Fprintln(os.Stderr, "ch-cohort-rollup: DRY RUN — would recompute the creator and sponsor cohort tables and EXCHANGE them live; pass -write to apply")
		return nil
	}

	unlock, err := acquireRollupLock("ch-cohort-rollup", *lockPath)
	if err != nil {
		return err
	}
	defer unlock()

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

	// The monthly price fold needs the API's alias registry: without it only XLM's three forms
	// fold and a SAC-quoted month prices under an id nothing joins on. Fail closed on a bad wrapper.
	aliasRegistry, err := canonical.NewAliasRegistry(cfg.Stellar.Passphrase(), cfg.Supply.SACWrappers)
	if err != nil {
		return fmt.Errorf("alias registry: %w", err)
	}
	canonical.InstallAliasRegistry(aliasRegistry)

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return err
	}
	holders, err := store.DeFiPositionHolders(ctx)
	if err != nil {
		_ = store.Close()
		return err
	}
	logf("defi position snapshot: %d rows from the served tier in %s", len(holders), time.Since(start).Round(time.Second))
	prices, err := store.MonthlyUSDVWAPs(ctx, cohortPricesFrom, closedMonthEdge(time.Now()), sourceAmountDecimals)
	_ = store.Close()
	if err != nil {
		return err
	}
	logf("monthly usd prices: %d (asset, month) rows from the served tier in %s", len(prices), time.Since(start).Round(time.Second))

	if err := clickhouse.RunCohortRollup(ctx, *chAddr, holders, prices, logf); err != nil {
		return err
	}
	logf("cycle complete in %s", time.Since(start).Round(time.Second))
	return nil
}
