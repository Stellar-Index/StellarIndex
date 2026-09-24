// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// tradesCAGGRefreshVerb re-materialises every continuous aggregate over
// `trades` for a ledger range that was rewritten in place. Their refresh
// policies only look back minutes to months, so a historical rewrite
// (scripts/ops/ch-rebuild-projected.sh) is never picked up on its own.
const tradesCAGGRefreshVerb = "trades-cagg-refresh"

// tradesCAGGRefreshedPrefix starts the success line the command prints.
const tradesCAGGRefreshedPrefix = "trades-cagg-refresh: refreshed"

// tradesCAGGStore is the slice of *timescale.Store the refresh needs.
type tradesCAGGStore interface {
	LedgerRangeToTimeRange(ctx context.Context, fromLedger, toLedger uint32) (time.Time, time.Time, error)
	RefreshContinuousAggregate(ctx context.Context, viewName string, from, to time.Time) error
	RefreshContinuousAggregateForced(ctx context.Context, viewName string, from, to time.Time) error
	Prices1mRetentionArmed(ctx context.Context) (bool, error)
	tradesDriftStore
}

func tradesCAGGRefresh(args []string) error {
	fs := flag.NewFlagSet(tradesCAGGRefreshVerb, flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to stellarindex.toml (required)")
	from := fs.Uint("from", 0, "first ledger of the rewritten range (inclusive, required)")
	to := fs.Uint("to", 0, "last ledger of the rewritten range (inclusive, required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" || *from == 0 || *to < *from || *to > uint(^uint32(0)) {
		return fmt.Errorf("-config, -from and -to are required, with 0 < -from <= -to <= %d", ^uint32(0))
	}
	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}
	ctx, cancel := opsutil.SignalContext()
	defer cancel()
	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	return refreshTradesCAGGsOverLedgers(ctx, store, uint32(*from), uint32(*to), time.Now(), os.Stdout)
}

// refreshTradesCAGGsOverLedgers refreshes every [timescale.TradesCAGGs]
// view, in its order (twap_* after prices_1m), over the time span of the
// trades now stored in [from, to], stopping at the first failure: a later
// view may be built on the one that failed.
//
// No trades in the range is an error, not a no-op: the caller has just
// rewritten it, so an empty range means the time span of whatever was
// deleted cannot be recovered here, and the aggregates may still hold it.
//
// A refresh that returned is not yet proof the aggregates are right, so
// it succeeds only once prices_1m agrees with `trades` over sampled
// windows of the span ([checkTradesPrices1mDrift]).
func refreshTradesCAGGsOverLedgers(ctx context.Context, s tradesCAGGStore, from, to uint32, now time.Time, out io.Writer) error {
	tsFrom, tsTo, err := s.LedgerRangeToTimeRange(ctx, from, to)
	if errors.Is(err, timescale.ErrNotFound) {
		return fmt.Errorf("no trades in ledgers [%d,%d], so the time span to refresh is unknown; if this range was rewritten, refresh the trades continuous aggregates over it by hand", from, to)
	}
	if err != nil {
		return fmt.Errorf("time span of ledgers [%d,%d]: %w", from, to, err)
	}
	armed, err := s.Prices1mRetentionArmed(ctx)
	if err != nil {
		return err
	}
	plan := timescale.PlanCAGGRefresh(timescale.TradesCAGGs, func(c timescale.CAGGSpec) (time.Time, time.Time) {
		return tradesCAGGRefreshWindow(tsFrom, tsTo, c.MinWindow)
	})
	for _, st := range plan {
		if err := timescale.RunCAGGRefreshStep(ctx, s, st, armed); err != nil {
			return fmt.Errorf("refresh %s over ledgers [%d,%d]: %w", st.View, from, to, err)
		}
	}
	checked, err := checkTradesPrices1mDrift(ctx, s, tsFrom, tsTo, now, out)
	if err != nil {
		return fmt.Errorf("ledgers [%d,%d]: %w", from, to, err)
	}
	// drift-windows=0 is a span wholly inside prices_1m's live refresh
	// window, which its own policy owns.
	_, err = fmt.Fprintf(out, "%s [%d,%d] ts=[%s,%s] views=%d drift-windows=%d\n", tradesCAGGRefreshedPrefix,
		from, to, tsFrom.UTC().Format(time.RFC3339), tsTo.UTC().Format(time.RFC3339), len(timescale.TradesCAGGs), checked)
	return err
}

// tradesCAGGRefreshWindow widens [from, to] by half the view's MinWindow
// on each side. Timescale refreshes only the buckets wholly inside the
// window, and MinWindow is at least two buckets, so the half is at least
// one: the buckets holding the first and last rewritten rows — which
// straddle the range's edges — are refreshed too, and the window always
// clears the two-bucket minimum.
func tradesCAGGRefreshWindow(from, to time.Time, minWindow time.Duration) (time.Time, time.Time) {
	pad := minWindow / 2
	return from.Add(-pad), to.Add(pad)
}
