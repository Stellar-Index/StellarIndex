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
}

// caggRefreshStep is one refresh_continuous_aggregate call.
type caggRefreshStep struct {
	View     string
	From, To time.Time
	Force    bool
}

// tradesCAGGRefreshPlan orders a refresh of views, each over
// window(view). prices_1m is the input of [timescale.CAGGsOnPrices1m]:
// it is forced over the hull of their windows, and they are forced after
// it, so none of them recomputes a bucket from minute rows a retention
// drop removed (migration 0156). views must list prices_1m before them.
func tradesCAGGRefreshPlan(views []timescale.CAGGSpec, window func(timescale.CAGGSpec) (time.Time, time.Time)) []caggRefreshStep {
	onMinute := make(map[string]bool, len(timescale.CAGGsOnPrices1m))
	for _, v := range timescale.CAGGsOnPrices1m {
		onMinute[v] = true
	}
	plan := make([]caggRefreshStep, 0, len(views))
	minute := -1
	for _, c := range views {
		f, t := window(c)
		st := caggRefreshStep{View: c.Name, From: f, To: t, Force: onMinute[c.Name]}
		switch {
		case c.Name == prices1mView:
			st.Force, minute = true, len(plan)
		case st.Force && minute < 0:
			panic("tradesCAGGRefreshPlan: " + c.Name + " is listed before prices_1m, which it is built on")
		}
		plan = append(plan, st)
	}
	for _, st := range plan {
		if !onMinute[st.View] {
			continue
		}
		if st.From.Before(plan[minute].From) {
			plan[minute].From = st.From
		}
		if st.To.After(plan[minute].To) {
			plan[minute].To = st.To
		}
	}
	return plan
}

// prices1mView is the minute rung the [timescale.CAGGsOnPrices1m] views read.
const prices1mView = "prices_1m"

// refreshTradesCAGGStep runs one planned refresh. A view built on
// prices_1m is refused while that view's retention policy is armed: it
// could drop the minute rows this run just rebuilt before the view reads
// them, and migration 0156 requires the policy disarmed for this refresh.
func refreshTradesCAGGStep(ctx context.Context, s tradesCAGGStore, st caggRefreshStep, prices1mRetentionArmed bool) error {
	if !st.Force {
		return s.RefreshContinuousAggregate(ctx, st.View, st.From, st.To)
	}
	if st.View != prices1mView && prices1mRetentionArmed {
		return fmt.Errorf("refused: prices_1m's retention policy is armed, and %s is materialised from prices_1m; "+
			"disarm it as migrations/0156_prices_1m_retention.up.sql states, confirm, and re-run", st.View)
	}
	return s.RefreshContinuousAggregateForced(ctx, st.View, st.From, st.To)
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
	return refreshTradesCAGGsOverLedgers(ctx, store, uint32(*from), uint32(*to), os.Stdout)
}

// refreshTradesCAGGsOverLedgers refreshes every [timescale.TradesCAGGs]
// view, in its order (twap_* after prices_1m), over the time span of the
// trades now stored in [from, to], stopping at the first failure: a later
// view may be built on the one that failed.
//
// No trades in the range is an error, not a no-op: the caller has just
// rewritten it, so an empty range means the time span of whatever was
// deleted cannot be recovered here, and the aggregates may still hold it.
func refreshTradesCAGGsOverLedgers(ctx context.Context, s tradesCAGGStore, from, to uint32, out io.Writer) error {
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
	plan := tradesCAGGRefreshPlan(timescale.TradesCAGGs, func(c timescale.CAGGSpec) (time.Time, time.Time) {
		return tradesCAGGRefreshWindow(tsFrom, tsTo, c.MinWindow)
	})
	for _, st := range plan {
		if err := refreshTradesCAGGStep(ctx, s, st, armed); err != nil {
			return fmt.Errorf("refresh %s over ledgers [%d,%d]: %w", st.View, from, to, err)
		}
	}
	_, err = fmt.Fprintf(out, "%s [%d,%d] ts=[%s,%s] views=%d\n", tradesCAGGRefreshedPrefix,
		from, to, tsFrom.UTC().Format(time.RFC3339), tsTo.UTC().Format(time.RFC3339), len(timescale.TradesCAGGs))
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
