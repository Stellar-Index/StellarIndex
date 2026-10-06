// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// lateTradeSlack widens every view's lateness margin past its policy's
// schedule and bucket width: job-scheduler delay, clock skew, and the
// time prices_1m takes to materialise a minute before the twaps built on
// it can see it.
const lateTradeSlack = 5 * time.Minute

const (
	lateTradeRefreshDebounce   = time.Minute
	lateTradeRefreshMaxBackoff = 10 * time.Minute
)

// LateTradeShutdownFlushBudget bounds the indexer's FlushOnShutdown.
const LateTradeShutdownFlushBudget = 30 * time.Second

// lateTradeCAGGStore is the slice of *timescale.Store the refresher needs.
type lateTradeCAGGStore interface {
	CAGGRefreshWindows(ctx context.Context) ([]timescale.CAGGRefreshWindow, error)
	timescale.CAGGStepRefresher
}

// LateTradeRefresher materialises the [timescale.TradesCAGGs] over trades
// the live writers land older than a view's refresh policy reaches back.
// Each policy only re-aggregates the last start_offset (prices_1m: 15
// minutes), so after a projector or dispatcher outage longer than that the
// resumed writers insert trades no policy ever revisits and the OHLC/TWAP
// views under-report them for good.
//
// Writers call ObserveTrades / ObserveEvent after a write commits; that
// only widens one pending ts window and never blocks. Run refreshes it
// once per debounce interval, one refresh in flight at a time, and each
// view at most once per its own policy's schedule_interval, so a long
// catch-up costs a refresh per interval rather than per batch. A failed
// refresh is logged, counted, kept pending and retried with backoff;
// ingest never waits on it, and one view's failure does not hold the
// others. FlushOnShutdown refreshes what is left once the writers have
// stopped.
type LateTradeRefresher struct {
	store      lateTradeCAGGStore
	logger     *slog.Logger
	now        func() time.Time
	debounce   time.Duration
	maxBackoff time.Duration
	kick       chan struct{}

	mu sync.Mutex
	// policies is nil until loaded; until then lateAfter is 0 and every
	// observed trade is pending, so trades landed right after a restart
	// (the backlog of an indexer outage) are never judged against nothing.
	policies  map[string]timescale.CAGGRefreshWindow
	lateAfter time.Duration
	pending   bool
	lo, hi    time.Time
	since     time.Time // when this window opened
	// viewsDue is when the oldest pending view window fell or falls due,
	// as of the last flush; overdueSince is what was last published.
	viewsDue     time.Time
	overdueSince time.Time

	// flushMu serialises flushes (Run's and FlushOnShutdown's) and guards views.
	flushMu sync.Mutex
	views   map[string]*lateViewState
}

// lateViewState is one view's share of the observed window, kept until a
// refresh of that view succeeds.
type lateViewState struct {
	pending       bool
	lo, hi        time.Time
	since         time.Time // when the oldest unrefreshed observation arrived
	lastRefreshed time.Time
}

// LateTradeRefresherOptions configures [NewLateTradeRefresher].
type LateTradeRefresherOptions struct {
	Logger *slog.Logger // nil = slog.Default()
}

// NewLateTradeRefresher returns a refresher over store; Run drives it.
func NewLateTradeRefresher(store lateTradeCAGGStore, opts LateTradeRefresherOptions) *LateTradeRefresher {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &LateTradeRefresher{
		store:      store,
		logger:     logger,
		now:        time.Now,
		debounce:   lateTradeRefreshDebounce,
		maxBackoff: lateTradeRefreshMaxBackoff,
		kick:       make(chan struct{}, 1),
		views:      make(map[string]*lateViewState, len(timescale.TradesCAGGs)),
	}
}

// ObserveTrades records trades that have just been written. Nil-safe.
func (r *LateTradeRefresher) ObserveTrades(trades ...canonical.Trade) {
	if r == nil || len(trades) == 0 {
		return
	}
	r.mu.Lock()
	cutoff := r.now().Add(-r.lateAfter)
	late := false
	for i := range trades {
		ts := trades[i].Timestamp
		if !ts.Before(cutoff) {
			continue
		}
		late = true
		r.widenLocked(ts, ts)
	}
	r.mu.Unlock()
	if late {
		r.signal()
	}
}

// ObserveEvent is ObserveTrades for a trade-shaped event; others are ignored.
func (r *LateTradeRefresher) ObserveEvent(ev consumer.Event) {
	if t, ok := tradeFromEvent(ev); ok {
		r.ObserveTrades(t)
	}
}

// ObservingSink wraps a projector sink so every event it handles without
// error is observed. Nil-safe: a nil r returns sink unchanged.
func (r *LateTradeRefresher) ObservingSink(sink func(context.Context, consumer.Event) error) func(context.Context, consumer.Event) error {
	if r == nil {
		return sink
	}
	return func(ctx context.Context, ev consumer.Event) error {
		err := sink(ctx, ev)
		if err == nil {
			r.ObserveEvent(ev)
		}
		return err
	}
}

func (r *LateTradeRefresher) widenLocked(lo, hi time.Time) {
	if !r.pending {
		r.lo, r.hi, r.since, r.pending = lo, hi, r.now(), true
		r.publishLocked()
		return
	}
	if lo.Before(r.lo) {
		r.lo = lo
	}
	if hi.After(r.hi) {
		r.hi = hi
	}
}

func (v *lateViewState) widen(lo, hi, since time.Time) {
	if !v.pending {
		v.lo, v.hi, v.since, v.pending = lo, hi, since, true
		return
	}
	if lo.Before(v.lo) {
		v.lo = lo
	}
	if hi.After(v.hi) {
		v.hi = hi
	}
	if since.Before(v.since) {
		v.since = since
	}
}

func (r *LateTradeRefresher) signal() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Run refreshes pending windows until ctx is done. What is still pending
// then is FlushOnShutdown's.
func (r *LateTradeRefresher) Run(ctx context.Context) {
	wait := r.debounce
	var held *time.Timer // fires when a view held by its rate limit falls due
	defer func() {
		if held != nil {
			held.Stop()
		}
	}()
	for {
		var heldC <-chan time.Time
		if held != nil {
			heldC = held.C
		}
		select {
		case <-ctx.Done():
			return
		case <-r.kick:
		case <-heldC:
			held = nil
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		next, err := r.flush(ctx, true)
		if held != nil {
			held.Stop()
			held = nil
		}
		if next > 0 {
			held = time.NewTimer(next)
		}
		if err == nil {
			wait = r.debounce
			continue
		}
		if ctx.Err() != nil {
			return
		}
		obs.LateTradeCAGGRefreshTotal.WithLabelValues("error").Inc()
		wait = min(wait*2, r.maxBackoff)
		r.logger.Error("late-trade cagg refresh failed; window kept for retry", "retry_in", wait, "err", err)
		r.signal()
	}
}

// FlushOnShutdown refreshes everything still pending, ignoring the per-view
// rate limit, under ctx. Call it once the trade writers have stopped and
// Run's context is done. A failure or timeout logs at ERROR each window an
// operator must refresh by hand; it is not counted, since /metrics goes
// down right after and the next process starts from zero. Nil-safe.
func (r *LateTradeRefresher) FlushOnShutdown(ctx context.Context) {
	if r == nil {
		return
	}
	if _, err := r.flush(ctx, false); err != nil {
		r.logAbandoned(err)
	}
}

// flush moves the observed window onto every bounded view, then refreshes
// each view whose policy cannot reach it, in [timescale.TradesCAGGs] order.
// With rateLimited, a view refreshed less than its schedule_interval ago is
// held, and next is how long until the first held view falls due. A view's
// window is dropped only once its refresh succeeds or its policy covers it.
// A failed view does not stop the others, except that the twaps built on
// prices_1m wait out its failure; err joins every failure.
func (r *LateTradeRefresher) flush(ctx context.Context, rateLimited bool) (next time.Duration, err error) {
	r.flushMu.Lock()
	defer r.flushMu.Unlock()
	if !r.anyPendingLocked() {
		return 0, nil
	}
	if err := r.loadPolicies(ctx); err != nil {
		return 0, err
	}
	r.mu.Lock()
	policies := r.policies
	r.distributePendingLocked(policies)
	r.mu.Unlock()
	defer r.publishOverdueLocked(policies)

	var errs []error
	refreshed := 0
	prices1mWaiting := false // held or failed: the twaps built on it wait
	for _, c := range timescale.TradesCAGGs {
		out, wait, err := r.flushView(ctx, c, policies[c.Name], rateLimited, prices1mWaiting)
		switch out {
		case viewHeld:
			if next == 0 || wait < next {
				next = wait
			}
		case viewFailed:
			errs = append(errs, err)
		case viewRefreshed:
			refreshed++
		}
		if out == viewHeld || out == viewFailed {
			prices1mWaiting = prices1mWaiting || c.Name == "prices_1m"
		}
		if out == viewFailed && ctx.Err() != nil {
			break
		}
	}
	if refreshed > 0 {
		obs.LateTradeCAGGRefreshTotal.WithLabelValues("ok").Inc()
	}
	return next, errors.Join(errs...)
}

// distributePendingLocked moves the undistributed window onto every bounded
// view. Needs r.mu.
func (r *LateTradeRefresher) distributePendingLocked(policies map[string]timescale.CAGGRefreshWindow) {
	if !r.pending {
		return
	}
	for _, c := range timescale.TradesCAGGs {
		if w, ok := policies[c.Name]; ok && w.HasPolicy && !w.Unbounded {
			r.viewLocked(c.Name).widen(r.lo, r.hi, r.since)
		}
	}
	r.pending = false
}

type viewOutcome int

const (
	viewSkipped viewOutcome = iota
	viewHeld
	viewFailed
	viewRefreshed
)

// flushView refreshes one view's pending window. wait is how long a held view
// has left on its rate limit. Needs r.flushMu; takes no r.mu.
func (r *LateTradeRefresher) flushView(ctx context.Context, c timescale.CAGGSpec, w timescale.CAGGRefreshWindow, rateLimited, prices1mWaiting bool) (out viewOutcome, wait time.Duration, err error) {
	v := r.views[c.Name]
	if v == nil || !v.pending {
		return viewSkipped, 0, nil
	}
	// Read per view: the twaps' cutoffs are planned after prices_1m's
	// refresh has returned, not before it started.
	now := r.now()
	st, ok := lateTradeRefreshStep(c, w, v.lo, v.hi, now)
	if !ok {
		v.pending = false
		return viewSkipped, 0, nil
	}
	if lateTradeHierarchical[c.Name] && prices1mWaiting {
		return viewSkipped, 0, nil
	}
	if due := v.lastRefreshed.Add(w.ScheduleInterval); rateLimited && now.Before(due) {
		return viewHeld, due.Sub(now), nil
	}
	// Never forced: the invalidation log holds exactly the late rows,
	// and a forced pass would rebuild every bucket in the window.
	if err := timescale.RunCAGGRefreshStep(ctx, r.store, st, false); err != nil {
		return viewFailed, 0, fmt.Errorf("refresh %s over [%s, %s) for trades at [%s, %s]: %w", st.View,
			st.From.UTC().Format(time.RFC3339), st.To.UTC().Format(time.RFC3339),
			v.lo.UTC().Format(time.RFC3339), v.hi.UTC().Format(time.RFC3339), err)
	}
	r.logger.Info("late-trade cagg refresh done", "view", st.View,
		"trades_from", v.lo.UTC().Format(time.RFC3339), "trades_to", v.hi.UTC().Format(time.RFC3339))
	v.pending = false
	v.lastRefreshed = r.now()
	return viewRefreshed, 0, nil
}

// publishOverdueLocked recomputes when the oldest pending view window fell
// (or falls) due: not before it was observed, its view's rate limit
// expires, nor, for a twap, prices_1m's. Needs r.flushMu.
func (r *LateTradeRefresher) publishOverdueLocked(policies map[string]timescale.CAGGRefreshWindow) {
	var oldest, prices1mDue time.Time
	for _, c := range timescale.TradesCAGGs {
		v := r.views[c.Name]
		if v == nil || !v.pending {
			continue
		}
		due := v.since
		if d := v.lastRefreshed.Add(policies[c.Name].ScheduleInterval); d.After(due) {
			due = d
		}
		if c.Name == "prices_1m" {
			prices1mDue = due
		}
		if lateTradeHierarchical[c.Name] && prices1mDue.After(due) {
			due = prices1mDue
		}
		if oldest.IsZero() || due.Before(oldest) {
			oldest = due
		}
	}
	r.mu.Lock()
	r.viewsDue = oldest
	r.publishLocked()
	r.mu.Unlock()
}

// publishLocked publishes the older of viewsDue and the undistributed
// window's open time. Not refreshed mid-flush, so a hung refresh keeps
// the last value and its age keeps growing. Needs r.mu.
func (r *LateTradeRefresher) publishLocked() {
	due := r.viewsDue
	if r.pending && (due.IsZero() || r.since.Before(due)) {
		due = r.since
	}
	r.overdueSince = due
	obs.SetLateTradeCAGGRefreshOverdueSince(due)
}

// lateTradeHierarchical are the [timescale.TradesCAGGs] materialised from
// prices_1m; they wait while a prices_1m refresh is held.
var lateTradeHierarchical = map[string]bool{"twap_1h": true, "twap_1d": true}

// viewLocked needs r.flushMu.
func (r *LateTradeRefresher) viewLocked(name string) *lateViewState {
	v := r.views[name]
	if v == nil {
		v = &lateViewState{}
		r.views[name] = v
	}
	return v
}

// anyPendingLocked needs r.flushMu.
func (r *LateTradeRefresher) anyPendingLocked() bool {
	r.mu.Lock()
	pending := r.pending
	r.mu.Unlock()
	for _, v := range r.views {
		pending = pending || v.pending
	}
	return pending
}

func (r *LateTradeRefresher) loadPolicies(ctx context.Context) error {
	r.mu.Lock()
	loaded := r.policies != nil
	r.mu.Unlock()
	if loaded {
		return nil
	}
	windows, err := r.store.CAGGRefreshWindows(ctx)
	if err != nil {
		return fmt.Errorf("load cagg refresh policies: %w", err)
	}
	policies := make(map[string]timescale.CAGGRefreshWindow, len(windows))
	for _, w := range windows {
		policies[w.View] = w
	}
	lateAfter := time.Duration(-1)
	for _, c := range timescale.TradesCAGGs {
		w, ok := policies[c.Name]
		if !ok || !w.HasPolicy || w.Unbounded {
			r.logger.Warn("late-trade cagg refresh skips a view the database maintains without a bounded policy",
				"view", c.Name, "present", ok, "has_policy", w.HasPolicy, "unbounded", w.Unbounded)
			continue
		}
		if cov := policyCoverage(c, w); lateAfter < 0 || cov < lateAfter {
			lateAfter = cov
		}
	}
	r.mu.Lock()
	r.policies = policies
	r.lateAfter = max(lateAfter, 0)
	r.mu.Unlock()
	return nil
}

// policyCoverage is how far behind a write its trade's bucket can sit and
// still be re-aggregated by view c's policy: the next run starts at most
// one schedule after the write and covers only buckets wholly inside its
// start_offset.
func policyCoverage(c timescale.CAGGSpec, w timescale.CAGGRefreshWindow) time.Duration {
	bucket := c.Bucket
	if bucket == timescale.MonthBucket {
		bucket = 31 * 24 * time.Hour
	}
	return w.StartOffset - w.ScheduleInterval - bucket - lateTradeSlack
}

// lateTradeRefreshStep is a non-forced refresh of view c over trades at
// [lo, hi], padded by half its MinWindow and cut off where its policy takes
// over at now; ok is false when the policy already reaches lo.
func lateTradeRefreshStep(c timescale.CAGGSpec, w timescale.CAGGRefreshWindow, lo, hi, now time.Time) (timescale.CAGGRefreshStep, bool) {
	cutoff := now.Add(-policyCoverage(c, w))
	if !lo.Before(cutoff) {
		return timescale.CAGGRefreshStep{}, false
	}
	pad := c.MinWindow / 2
	from, to := lo.Add(-pad), hi.Add(pad)
	if to.After(cutoff) {
		to = cutoff
	}
	if to.Sub(from) < c.MinWindow {
		from = to.Add(-c.MinWindow)
	}
	return timescale.CAGGRefreshStep{View: c.Name, From: from, To: to, Bucket: c.Bucket, MinWindow: c.MinWindow}, true
}

// logAbandoned logs every window still pending after a failed shutdown flush.
func (r *LateTradeRefresher) logAbandoned(err error) {
	r.flushMu.Lock()
	defer r.flushMu.Unlock()
	const msg = "late-trade cagg refresh abandoned at shutdown; refresh this trades continuous aggregate over this window by hand"
	r.mu.Lock()
	if r.pending {
		r.logger.Error(msg, "view", "all", "trades_from", r.lo.UTC().Format(time.RFC3339),
			"trades_to", r.hi.UTC().Format(time.RFC3339), "err", err)
	}
	r.mu.Unlock()
	for _, c := range timescale.TradesCAGGs {
		if v := r.views[c.Name]; v != nil && v.pending {
			r.logger.Error(msg, "view", c.Name, "trades_from", v.lo.UTC().Format(time.RFC3339),
				"trades_to", v.hi.UTC().Format(time.RFC3339), "err", err)
		}
	}
}

// writer returns store as the sink's trade writer, reporting each committed
// write to r when r is non-nil.
func (r *LateTradeRefresher) writer(store *timescale.Store) tradeWriter {
	if r == nil || store == nil {
		return store
	}
	return observingTradeWriter{tradeWriter: store, late: r}
}

// observingTradeWriter reports every trade write that succeeded.
type observingTradeWriter struct {
	tradeWriter
	late *LateTradeRefresher
}

func (w observingTradeWriter) BatchInsertTrades(ctx context.Context, trades []canonical.Trade) error {
	err := w.tradeWriter.BatchInsertTrades(ctx, trades)
	if err == nil {
		w.late.ObserveTrades(trades...)
	}
	return err
}

func (w observingTradeWriter) InsertTrade(ctx context.Context, t canonical.Trade) error {
	err := w.tradeWriter.InsertTrade(ctx, t)
	if err == nil {
		w.late.ObserveTrades(t)
	}
	return err
}
