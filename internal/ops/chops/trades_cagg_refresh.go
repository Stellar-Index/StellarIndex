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
	"slices"
	"strings"
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

// tradesCAGGPendingPrefix starts each line -size prints.
const tradesCAGGPendingPrefix = "trades-cagg-refresh: pending"

// tradesCAGGStore is the slice of *timescale.Store the refresh needs.
type tradesCAGGStore interface {
	LedgerRangeToTimeRange(ctx context.Context, fromLedger, toLedger uint32) (time.Time, time.Time, error)
	RefreshContinuousAggregate(ctx context.Context, viewName string, from, to time.Time) error
	RefreshContinuousAggregateForced(ctx context.Context, viewName string, from, to time.Time) error
	Prices1mRetentionArmed(ctx context.Context) (bool, error)
	Prices1mEarliestBucket(ctx context.Context) (time.Time, error)
	twapCoverStore
}

// tradesCAGGSizer is the slice of *timescale.Store -size needs.
type tradesCAGGSizer interface {
	CAGGInvalidationBacklogs(ctx context.Context, views []string, cutoff time.Time) ([]timescale.CAGGInvalidationBacklog, error)
	TradeLedgersInTimeRange(ctx context.Context, from, to time.Time) (uint32, uint32, error)
	Prices1mEarliestBucket(ctx context.Context) (time.Time, error)
	twapCoverStore
}

type tradesCAGGRefreshArgs struct {
	cfgPath  string
	from, to uint32
	force    bool
	size     bool
	write    bool
}

func parseTradesCAGGRefreshArgs(args []string) (tradesCAGGRefreshArgs, error) {
	fs, gate := opsutil.NewMutatingFlagSet(tradesCAGGRefreshVerb)
	cfgPath := fs.String("config", "", "path to stellarindex.toml (required)")
	from := fs.Uint("from", 0, "first ledger of the rewritten range (inclusive, required)")
	to := fs.Uint("to", 0, "last ledger of the rewritten range (inclusive, required)")
	force := fs.Bool("force", true, "recompute every bucket in the window; false re-materialises only the buckets Timescale's invalidation log names")
	size := fs.Bool("size", false, "read-only: print each view's pending invalidation ranges and the ledgers to pass for a -force=false catch-up")
	if err := fs.Parse(args); err != nil {
		return tradesCAGGRefreshArgs{}, err
	}
	// `-force false` sets -force and leaves "false" here, so it would run forced.
	if fs.NArg() > 0 {
		return tradesCAGGRefreshArgs{}, fmt.Errorf("unexpected argument %q; write a boolean flag as -force=false", fs.Arg(0))
	}
	if *cfgPath == "" {
		return tradesCAGGRefreshArgs{}, errors.New("-config is required")
	}
	if *size {
		var extra []string
		fs.Visit(func(f *flag.Flag) {
			if f.Name != "config" && f.Name != "size" {
				extra = append(extra, "-"+f.Name)
			}
		})
		if len(extra) > 0 {
			return tradesCAGGRefreshArgs{}, fmt.Errorf("-size reads every pending range and takes only -config, not %s", strings.Join(extra, " "))
		}
		return tradesCAGGRefreshArgs{cfgPath: *cfgPath, size: true}, nil
	}
	if *from == 0 || *to < *from || *to > uint(^uint32(0)) {
		return tradesCAGGRefreshArgs{}, fmt.Errorf("-from and -to are required, with 0 < -from <= -to <= %d", ^uint32(0))
	}
	// ch-rebuild-projected.sh reads exit 0 as "aggregates rebuilt".
	if err := gate.RequireStatedMode(); err != nil {
		return tradesCAGGRefreshArgs{}, err
	}
	return tradesCAGGRefreshArgs{cfgPath: *cfgPath, from: uint32(*from), to: uint32(*to), force: *force, write: gate.Enabled()}, nil
}

func tradesCAGGRefresh(args []string) error {
	a, err := parseTradesCAGGRefreshArgs(args)
	if err != nil {
		return err
	}
	cfg, err := config.LoadWithEnv(a.cfgPath)
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
	if a.size {
		return sizeTradesCAGGBacklog(ctx, store, time.Now(), os.Stdout)
	}
	if !opsutil.PrintWriteBanner(a.write) {
		return previewTradesCAGGRefresh(ctx, store, a.from, a.to, a.force, time.Now(), os.Stdout)
	}
	return refreshTradesCAGGsOverLedgers(ctx, store, a.from, a.to, a.force, time.Now(), os.Stdout)
}

// sizeTradesCAGGBacklog prints, per [timescale.TradesCAGGs] view, the
// invalidation ranges a non-forced refresh would re-materialise, then
// their hull as the -from/-to ledgers that cover it. A range that starts
// before [tradesCAGGNonForcedFloor] is left out of that hull and printed
// on its own: the non-forced run refuses it ([refuseTwapsBelowPrices1mFloor]).
// So is everything up to a gap in prices_1m the hull's twap buckets would
// read ([refuseUncoveredTwapReads]): the floor rises past it.
func sizeTradesCAGGBacklog(ctx context.Context, s tradesCAGGSizer, now time.Time, out io.Writer) error {
	views := make([]string, len(timescale.TradesCAGGs))
	for i, c := range timescale.TradesCAGGs {
		views[i] = c.Name
	}
	earliest, err := s.Prices1mEarliestBucket(ctx)
	noFloor := errors.Is(err, timescale.ErrNotFound)
	if err != nil && !noFloor {
		return err
	}
	floor := tradesCAGGNonForcedFloor(earliest)
	why := func() string {
		return "starts before " + floor.UTC().Format(time.RFC3339) + ", where twap windows reach below prices_1m's earliest bucket " +
			earliest.UTC().Format(time.RFC3339)
	}
	if noFloor {
		floor = time.Time{}
		why = func() string { return "prices_1m holds no materialised bucket" }
	}
	backlogs, err := s.CAGGInvalidationBacklogs(ctx, views, floor)
	if err != nil {
		return err
	}
	below, catchUp := splitTradesCAGGBacklogs(backlogs, noFloor)
	if !catchUp.lo.IsZero() {
		plan := timescale.PlanCAGGRefresh(timescale.TradesCAGGs, func(c timescale.CAGGSpec) (time.Time, time.Time) {
			return tradesCAGGRefreshWindow(catchUp.lo, catchUp.hi, c.MinWindow)
		})
		gap, _, ok, err := newestUncoveredTwapRead(ctx, s, plan, now.Add(-prices1mSettle))
		if err != nil {
			return err
		}
		if ok {
			floor = tradesCAGGNonForcedFloor(gap[1])
			why = func() string {
				return "starts before " + floor.UTC().Format(time.RFC3339) + ", where twap windows reach the prices_1m gap " + fmtDriftWindow(gap)
			}
			if backlogs, err = s.CAGGInvalidationBacklogs(ctx, views, floor); err != nil {
				return err
			}
			below, catchUp = splitTradesCAGGBacklogs(backlogs, false)
			if _, err := fmt.Fprintf(out, "%s prices_1m gap %s: prices_1m disagrees with trades in minutes a catch-up's twap buckets "+
				"would read, and no pending invalidation rebuilds them, so -force=false refuses it; refresh it with -force=true\n",
				tradesCAGGPendingPrefix, fmtDriftWindow(gap)); err != nil {
				return err
			}
		}
	}
	if err := printTradesCAGGBacklogs(out, backlogs, below, why()); err != nil {
		return err
	}
	return printTradesCAGGCatchUp(ctx, s, out, catchUp, slices.ContainsFunc(below, func(h tradesCAGGHull) bool { return !h.lo.IsZero() }))
}

type tradesCAGGHull struct{ lo, hi time.Time }

func (h *tradesCAGGHull) widen(from, to time.Time) {
	if from.IsZero() {
		return
	}
	if h.lo.IsZero() || from.Before(h.lo) {
		h.lo = from
	}
	if h.hi.IsZero() || to.After(h.hi) {
		h.hi = to
	}
}

// splitTradesCAGGBacklogs returns, per backlog, the hull a non-forced run
// refuses, and the hull of everything it may catch up.
func splitTradesCAGGBacklogs(backlogs []timescale.CAGGInvalidationBacklog, noFloor bool) ([]tradesCAGGHull, tradesCAGGHull) {
	var catchUp tradesCAGGHull
	below := make([]tradesCAGGHull, len(backlogs))
	for i, b := range backlogs {
		if !noFloor {
			below[i].widen(b.BelowFrom, b.BelowTo)
			catchUp.widen(b.AboveFrom, b.AboveTo)
			continue
		}
		// No minute rows at all: nothing is safe to refresh non-forced.
		if b.Ranges > 0 {
			below[i].widen(b.From, b.To)
		}
		if b.SourceRanges > 0 {
			below[i].widen(b.SourceFrom, b.SourceTo)
		}
	}
	return below, catchUp
}

func printTradesCAGGBacklogs(out io.Writer, backlogs []timescale.CAGGInvalidationBacklog, below []tradesCAGGHull, why string) error {
	ts := func(n int64, t time.Time) string {
		if n == 0 {
			return "-"
		}
		return t.UTC().Format(time.RFC3339)
	}
	for _, b := range backlogs {
		if _, err := fmt.Fprintf(out, "%s %s ranges=%d span=%s from=%s to=%s open-ended=%d source-log=%d\n",
			tradesCAGGPendingPrefix, b.View, b.Ranges, b.Span, ts(b.Ranges, b.From), ts(b.Ranges, b.To),
			b.OpenEnded, b.SourceRanges); err != nil {
			return err
		}
	}
	for i, h := range below {
		if h.lo.IsZero() {
			continue
		}
		if _, err := fmt.Fprintf(out, "%s %s below-floor from=%s to=%s: %s, so -force=false refuses it; refresh it with -force=true\n",
			tradesCAGGPendingPrefix, backlogs[i].View, h.lo.UTC().Format(time.RFC3339), h.hi.UTC().Format(time.RFC3339), why); err != nil {
			return err
		}
	}
	return nil
}

func printTradesCAGGCatchUp(ctx context.Context, s tradesCAGGSizer, out io.Writer, catchUp tradesCAGGHull, anyBelow bool) error {
	if catchUp.lo.IsZero() {
		msg := "none: no bounded invalidation range on any trades aggregate"
		if anyBelow {
			msg = "catch-up none: every bounded range is below-floor"
		}
		_, err := fmt.Fprintf(out, "%s %s\n", tradesCAGGPendingPrefix, msg)
		return err
	}
	lo, hi := catchUp.lo, catchUp.hi
	hullStr := "[" + lo.UTC().Format(time.RFC3339) + "," + hi.UTC().Format(time.RFC3339) + "]"
	fromLedger, toLedger, err := s.TradeLedgersInTimeRange(ctx, lo, hi)
	if errors.Is(err, timescale.ErrNotFound) {
		_, err = fmt.Fprintf(out, "%s hull=%s holds no trades; refresh it by ts, not by ledger\n", tradesCAGGPendingPrefix, hullStr)
		return err
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s hull=%s ledgers=[%d,%d] catch-up: -force=false -from %d -to %d -write\n",
		tradesCAGGPendingPrefix, hullStr, fromLedger, toLedger, fromLedger, toLedger)
	return err
}

// tradesCAGGNonForcedFloor is the earliest trade time a -force=false run
// may start at: every twap window it plans ([tradesCAGGRefreshWindow])
// then starts at or after prices_1m's earliest materialised bucket.
func tradesCAGGNonForcedFloor(prices1mEarliest time.Time) time.Time {
	var pad time.Duration
	for _, c := range timescale.TradesCAGGs {
		if slices.Contains(timescale.CAGGsOnPrices1m, c.Name) {
			pad = max(pad, c.MinWindow/2)
		}
	}
	return prices1mEarliest.Add(pad)
}

// refuseTwapsBelowPrices1mFloor refuses a non-forced plan whose twap
// window starts before prices_1m's earliest materialised bucket. Below
// it a past retention drop (migration 0156) emptied the minute rows, even
// once disarmed again, and a non-forced twap refresh recomputes every
// invalidated bucket there from them; the forced plan rebuilds prices_1m
// from trades first.
func refuseTwapsBelowPrices1mFloor(ctx context.Context, s tradesCAGGStore, plan []timescale.CAGGRefreshStep, from, to uint32) error {
	earliest, err := s.Prices1mEarliestBucket(ctx)
	if errors.Is(err, timescale.ErrNotFound) {
		return fmt.Errorf("refused: -force=false over ledgers [%d,%d]: prices_1m holds no materialised bucket, so %s would be "+
			"recomputed from no minute rows; re-run with -force=true, which rebuilds prices_1m from trades first",
			from, to, strings.Join(timescale.CAGGsOnPrices1m, "/"))
	}
	if err != nil {
		return err
	}
	for _, st := range plan {
		if !slices.Contains(timescale.CAGGsOnPrices1m, st.View) || !st.From.Before(earliest) {
			continue
		}
		return fmt.Errorf("refused: -force=false over ledgers [%d,%d]: the %s window [%s, %s) starts before prices_1m's earliest "+
			"materialised bucket %s, and below it the minute rows were dropped by retention or never materialised; a non-forced %s "+
			"refresh would recompute its buckets there from them. Re-run with -force=true, which rebuilds prices_1m from trades "+
			"over the range first (keep its retention disarmed), or narrow -from to a ledger whose first trade is at or after %s",
			from, to, st.View, st.From.UTC().Format(time.RFC3339), st.To.UTC().Format(time.RFC3339),
			earliest.UTC().Format(time.RFC3339), st.View, tradesCAGGNonForcedFloor(earliest).UTC().Format(time.RFC3339))
	}
	return nil
}

// refreshTradesCAGGsOverLedgers refreshes every [timescale.TradesCAGGs]
// view, in its order (twap_* after prices_1m), over the time span of the
// trades now stored in [from, to], stopping at the first failure: a later
// view may be built on the one that failed. Non-forced, each view
// re-materialises only the buckets its invalidation log names.
//
// No trades in the range is an error, not a no-op: the caller has just
// rewritten it, so an empty range means the time span of whatever was
// deleted cannot be recovered here, and the aggregates may still hold it.
//
// A refresh that returned is not yet proof the aggregates are right, so
// it succeeds only once prices_1m agrees with `trades` over sampled
// windows of the span ([checkTradesPrices1mDrift]).
func refreshTradesCAGGsOverLedgers(ctx context.Context, s tradesCAGGStore, from, to uint32, force bool, now time.Time, out io.Writer) error {
	tsFrom, tsTo, plan, err := planTradesCAGGRefresh(ctx, s, from, to, force, now, out)
	if err != nil {
		return err
	}
	armed, err := s.Prices1mRetentionArmed(ctx)
	if err != nil {
		return err
	}
	for _, st := range plan {
		st.Force = st.Force && force
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
	_, err = fmt.Fprintf(out, "%s [%d,%d] ts=[%s,%s] views=%d forced=%t drift-windows=%d\n", tradesCAGGRefreshedPrefix,
		from, to, tsFrom.UTC().Format(time.RFC3339), tsTo.UTC().Format(time.RFC3339), len(timescale.TradesCAGGs), force, checked)
	return err
}

// planTradesCAGGRefresh resolves [from, to] to its trades' time span and
// the refresh steps over it, applying a non-forced run's refusals.
func planTradesCAGGRefresh(ctx context.Context, s tradesCAGGStore, from, to uint32, force bool, now time.Time, out io.Writer) (time.Time, time.Time, []timescale.CAGGRefreshStep, error) {
	tsFrom, tsTo, err := s.LedgerRangeToTimeRange(ctx, from, to)
	if errors.Is(err, timescale.ErrNotFound) {
		return time.Time{}, time.Time{}, nil, fmt.Errorf("no trades in ledgers [%d,%d], so the time span to refresh is unknown; if this range was rewritten, refresh the trades continuous aggregates over it by hand", from, to)
	}
	if err != nil {
		return time.Time{}, time.Time{}, nil, fmt.Errorf("time span of ledgers [%d,%d]: %w", from, to, err)
	}
	plan := timescale.PlanCAGGRefresh(timescale.TradesCAGGs, func(c timescale.CAGGSpec) (time.Time, time.Time) {
		return tradesCAGGRefreshWindow(tsFrom, tsTo, c.MinWindow)
	})
	if !force {
		if err := refuseTwapsBelowPrices1mFloor(ctx, s, plan, from, to); err != nil {
			return time.Time{}, time.Time{}, nil, err
		}
		if err := refuseUncoveredTwapReads(ctx, s, plan, from, to, now, out); err != nil {
			return time.Time{}, time.Time{}, nil, err
		}
	}
	return tsFrom, tsTo, plan, nil
}

// previewTradesCAGGRefresh prints the steps a -write run would take and
// refreshes nothing.
func previewTradesCAGGRefresh(ctx context.Context, s tradesCAGGStore, from, to uint32, force bool, now time.Time, out io.Writer) error {
	_, _, plan, err := planTradesCAGGRefresh(ctx, s, from, to, force, now, out)
	if err != nil {
		return err
	}
	for _, st := range plan {
		if _, err := fmt.Fprintf(out, "trades-cagg-refresh: would refresh %s [%s,%s) forced=%t\n", st.View,
			st.From.UTC().Format(time.RFC3339), st.To.UTC().Format(time.RFC3339), st.Force && force); err != nil {
			return err
		}
	}
	return nil
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
