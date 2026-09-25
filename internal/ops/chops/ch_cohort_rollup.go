package chops

import (
	"errors"
	"flag"
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

// cohortPricesFrom is the first month the cycle prices: the network's
// genesis month. Every USD-quoted month the served tier holds is
// loaded, so a cohort's oldest flow can be valued at its own month.
var cohortPricesFrom = time.Date(2015, time.September, 1, 0, 0, 0, 0, time.UTC)

// closedMonthEdge is the exclusive upper bound the cycle reads
// [Store.MonthlyUSDVWAPs] to: the first instant of now's calendar
// month. prices_1mo's current-month bucket is still accumulating
// trades, so admitting it would serve a partial-month VWAP that
// changes on every rollup cycle — the flicker ADR-0015's closed-bucket
// rule exists to prevent, applied here to the month grain instead of
// the rate endpoints' 30 s one (GH-1058).
func closedMonthEdge(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// sourceAmountDecimals is the source registry's amount scale, which the
// monthly price fold needs to weigh off-chain spellings in whole units.
func sourceAmountDecimals(source string) int {
	return external.Lookup(source).AmountScaleDecimals()
}

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
// shipping the membership to Postgres. The served tier's monthly USD
// VWAPs (prices_1mo, alias-folded) ride along the same way, into
// asset_month_usd_prices, so the flows read can value a month at that
// month's own price. The rest is ClickHouse only:
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

	// The monthly price fold keys every alias spelling of a base onto
	// its canonical form, so the registry the API resolves against has
	// to be installed here too — without it only XLM's three forms fold
	// and a SAC-quoted month of USDC would price under the C… id nothing
	// joins on. Fail-closed on a malformed wrapper, as the API does.
	aliasRegistry, err := canonical.NewAliasRegistry(cfg.Supply.SACWrappers)
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
