// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
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
// ingest never waits on it. FlushOnShutdown refreshes what is left once
// the writers have stopped.
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

	// flushMu serialises flushes (Run's and FlushOnShutdown's) and guards views.
	flushMu sync.Mutex
	views   map[string]*lateViewState
}

// lateViewState is one view's share of the observed window, kept until a
// refresh of that view succeeds.
type lateViewState struct {
	pending       bool
	lo, hi        time.Time
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
	if !r.pending || lo.Before(r.lo) {
		r.lo = lo
	}
	if !r.pending || hi.After(r.hi) {
		r.hi = hi
	}
	r.pending = true
}

func (v *lateViewState) widen(lo, hi time.Time) {
	if !v.pending || lo.Before(v.lo) {
		v.lo = lo
	}
	if !v.pending || hi.After(v.hi) {
		v.hi = hi
	}
	v.pending = true
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
// Run's context is done. A failure or timeout counts as outcome="abandoned"
// and logs the windows an operator must refresh by hand. Nil-safe.
func (r *LateTradeRefresher) FlushOnShutdown(ctx context.Context) {
	if r == nil {
		return
	}
	if _, err := r.flush(ctx, false); err != nil {
		obs.LateTradeCAGGRefreshTotal.WithLabelValues("abandoned").Inc()
		r.logAbandoned(err)
	}
}

// flush moves the observed window onto every bounded view, then refreshes
// each view whose policy cannot reach it, in [timescale.TradesCAGGs] order.
// With rateLimited, a view refreshed less than its schedule_interval ago is
// held, and next is how long until the first held view falls due. A view's
// window is dropped only once its refresh succeeds or its policy covers it.
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
	if r.pending {
		for _, c := range timescale.TradesCAGGs {
			if w, ok := policies[c.Name]; ok && w.HasPolicy && !w.Unbounded {
				r.viewLocked(c.Name).widen(r.lo, r.hi)
			}
		}
		r.pending = false
	}
	r.mu.Unlock()

	refreshed := 0
	prices1mHeld := false
	for _, c := range timescale.TradesCAGGs {
		v := r.views[c.Name]
		if v == nil || !v.pending {
			continue
		}
		w := policies[c.Name]
		// Read per view: the twaps' cutoffs are planned after prices_1m's
		// refresh has returned, not before it started.
		now := r.now()
		st, ok := lateTradeRefreshStep(c, w, v.lo, v.hi, now)
		if !ok {
			v.pending = false
			continue
		}
		if lateTradeHierarchical[c.Name] && prices1mHeld {
			continue
		}
		if due := v.lastRefreshed.Add(w.ScheduleInterval); rateLimited && now.Before(due) {
			if wait := due.Sub(now); next == 0 || wait < next {
				next = wait
			}
			prices1mHeld = prices1mHeld || c.Name == "prices_1m"
			continue
		}
		// Never forced: the invalidation log holds exactly the late rows,
		// and a forced pass would rebuild every bucket in the window.
		if err := timescale.RunCAGGRefreshStep(ctx, r.store, st, false); err != nil {
			return 0, fmt.Errorf("refresh %s over [%s, %s) for trades at [%s, %s]: %w", st.View,
				st.From.UTC().Format(time.RFC3339), st.To.UTC().Format(time.RFC3339),
				v.lo.UTC().Format(time.RFC3339), v.hi.UTC().Format(time.RFC3339), err)
		}
		r.logger.Info("late-trade cagg refresh done", "view", st.View,
			"trades_from", v.lo.UTC().Format(time.RFC3339), "trades_to", v.hi.UTC().Format(time.RFC3339))
		v.pending = false
		v.lastRefreshed = r.now()
		refreshed++
	}
	if refreshed > 0 {
		obs.LateTradeCAGGRefreshTotal.WithLabelValues("ok").Inc()
	}
	return next, nil
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
