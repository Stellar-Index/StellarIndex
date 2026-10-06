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
	RecordCAGGLateRefreshWindow(ctx context.Context, family string, views []string, from, to time.Time) error
	CAGGLateRefreshWindows(ctx context.Context, family string) ([]timescale.CAGGLateRefreshWindow, error)
	ClearCAGGLateRefreshWindow(ctx context.Context, family, view string, gen int64) (bool, error)
	timescale.CAGGStepRefresher
}

// LateTradeRefresher materialises the [timescale.TradesCAGGs] over trades
// the live writers land older than a view's refresh policy reaches back.
// Each policy only re-aggregates the last start_offset (prices_1m: 15
// minutes), so after a projector or dispatcher outage longer than that the
// resumed writers insert trades no policy ever revisits and the OHLC/TWAP
// views under-report them for good.
//
// Before a late write, its writer records the window durably (migration
// 0211); every flush unions those rows in, and Run flushes once at start,
// so a window survives a crash. After the write commits, the writer
// records any trade the write's own duration made late, then calls
// ObserveTrades, which widens the in-memory window and kicks Run. Run
// refreshes it once per debounce interval, one refresh in flight at a
// time, and each view at most once per its own policy's schedule_interval,
// so a long catch-up costs a refresh per interval rather than per batch. A failed
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

	// inflight counts late writes between beginWrite and their end;
	// entered counts every one begun. A flush clears a durable row only if
	// none was in flight when it read the rows and none has begun since.
	inflight int
	entered  uint64
	// cover is the hour hull recorded durably since coverEpoch last moved;
	// a write inside it skips the upsert. Reset before every clear.
	covered          bool
	coverLo, coverHi time.Time
	coverEpoch       uint64

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
	Logger   *slog.Logger  // nil = slog.Default()
	Debounce time.Duration // 0 = one minute
}

// NewLateTradeRefresher returns a refresher over store; Run drives it.
func NewLateTradeRefresher(store lateTradeCAGGStore, opts LateTradeRefresherOptions) *LateTradeRefresher {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	debounce := opts.Debounce
	if debounce <= 0 {
		debounce = lateTradeRefreshDebounce
	}
	return &LateTradeRefresher{
		store:      store,
		logger:     logger,
		now:        time.Now,
		debounce:   debounce,
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

// ObservingSink wraps a projector sink so a late trade's window is
// recorded before the sink runs and every event it handles without error
// is observed. Nil-safe: a nil r returns sink unchanged.
func (r *LateTradeRefresher) ObservingSink(sink func(context.Context, consumer.Event) error) func(context.Context, consumer.Event) error {
	if r == nil {
		return sink
	}
	return func(ctx context.Context, ev consumer.Event) error {
		t, isTrade := tradeFromEvent(ev)
		if !isTrade {
			return sink(ctx, ev)
		}
		end, err := r.beginWrite(ctx, []canonical.Trade{t})
		if err != nil {
			return err
		}
		defer end()
		if err := sink(ctx, ev); err != nil {
			return err
		}
		return r.committed(ctx, []canonical.Trade{t})
	}
}

// Prime loads the refresh policies so writes before the first flush are
// judged late against them rather than against nothing. Nil-safe.
func (r *LateTradeRefresher) Prime(ctx context.Context) error {
	if r == nil {
		return nil
	}
	return r.loadPolicies(ctx)
}

// lateTradeViewNames are the views every durable trades window is recorded on.
var lateTradeViewNames = func() []string {
	out := make([]string, len(timescale.TradesCAGGs))
	for i, c := range timescale.TradesCAGGs {
		out[i] = c.Name
	}
	return out
}()

// beginWrite durably records the hour hull of the trades already late
// before they are written; call end once the write has returned, and
// committed once it has succeeded. Without a late trade it records nothing
// and end is a no-op.
func (r *LateTradeRefresher) beginWrite(ctx context.Context, trades []canonical.Trade) (end func(), err error) {
	found, err := r.recordLate(ctx, trades, true)
	if !found {
		return func() {}, err
	}
	end = func() {
		r.mu.Lock()
		r.inflight--
		r.mu.Unlock()
	}
	if err != nil {
		end()
		return nil, err
	}
	return end, nil
}

// committed observes a successful write's trades and durably records any
// its own duration made late: they were on time at beginWrite, and memory
// alone dies with the process. A failed record fails the write, so its
// retry, or a restart's redelivery, records them.
func (r *LateTradeRefresher) committed(ctx context.Context, trades []canonical.Trade) error {
	r.ObserveTrades(trades...)
	_, err := r.recordLate(ctx, trades, false)
	return err
}

// recordLate durably records the hour hull of the trades late now, unless a
// record since the last flush already covers it. With inflight, a late
// trade also counts the write as in flight until the caller ends it.
func (r *LateTradeRefresher) recordLate(ctx context.Context, trades []canonical.Trade, inflight bool) (found bool, err error) {
	r.mu.Lock()
	lo, hi, found := r.lateHourHullLocked(trades)
	if !found {
		r.mu.Unlock()
		return false, nil
	}
	if inflight {
		r.inflight++
	}
	r.entered++
	epoch := r.coverEpoch
	covered := r.covered && !lo.Before(r.coverLo) && !hi.After(r.coverHi)
	r.mu.Unlock()
	if covered {
		return true, nil
	}
	if err := r.store.RecordCAGGLateRefreshWindow(ctx, timescale.CAGGLateFamilyTrades, lateTradeViewNames, lo, hi); err != nil {
		return true, fmt.Errorf("record late-trade cagg window: %w", err)
	}
	r.mu.Lock()
	if r.coverEpoch == epoch {
		if !r.covered || lo.Before(r.coverLo) {
			r.coverLo = lo
		}
		if !r.covered || hi.After(r.coverHi) {
			r.coverHi = hi
		}
		r.covered = true
	}
	r.mu.Unlock()
	return true, nil
}

// lateHourHullLocked is the hour-aligned hull of the trades late now. Needs r.mu.
func (r *LateTradeRefresher) lateHourHullLocked(trades []canonical.Trade) (lo, hi time.Time, found bool) {
	cutoff := r.now().Add(-r.lateAfter)
	for i := range trades {
		ts := trades[i].Timestamp
		if !ts.Before(cutoff) {
			continue
		}
		if !found || ts.Before(lo) {
			lo = ts
		}
		if !found || ts.After(hi) {
			hi = ts
		}
		found = true
	}
	if !found {
		return lo, hi, false
	}
	return lo.Truncate(time.Hour), hi.Truncate(time.Hour).Add(time.Hour), true
}

// lateClearGuard is the write-ahead state a flush read its durable rows under.
type lateClearGuard struct {
	idle    bool // no late write was in flight
	entered uint64
}

// resetCoverLocked makes the next late write upsert again. Needs r.mu.
func (r *LateTradeRefresher) resetCoverLocked() {
	r.covered = false
	r.coverEpoch++
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
	r.signal() // refresh what a previous process recorded
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
// Run's context is done. A failure or timeout logs at ERROR each window
// left; the next process refreshes it from its durable row. Nil-safe.
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
	// Before the read: a write begun after it is visible in entered.
	r.mu.Lock()
	r.resetCoverLocked()
	guard := lateClearGuard{idle: r.inflight == 0, entered: r.entered}
	r.mu.Unlock()
	durable, err := r.store.CAGGLateRefreshWindows(ctx, timescale.CAGGLateFamilyTrades)
	if err != nil {
		return 0, fmt.Errorf("load late-trade cagg windows: %w", err)
	}
	if !r.anyPendingLocked() && len(durable) == 0 {
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

	gens, errs := r.adoptDurable(ctx, durable, policies, guard)

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
		case viewSkipped, viewCovered:
		}
		errs = append(errs, r.clearSettled(ctx, c.Name, out, gens, guard))
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

// adoptDurable widens each bounded view's window with its durable row and
// returns the row's gen per view; a row for a view with no bounded policy is
// cleared at once. Needs r.flushMu.
func (r *LateTradeRefresher) adoptDurable(ctx context.Context, durable []timescale.CAGGLateRefreshWindow, policies map[string]timescale.CAGGRefreshWindow, guard lateClearGuard) (gens map[string]int64, errs []error) {
	gens = make(map[string]int64, len(durable))
	for _, d := range durable {
		if w := policies[d.View]; w.HasPolicy && !w.Unbounded {
			r.viewLocked(d.View).widen(d.From, d.To, d.FirstSeen)
			gens[d.View] = d.Gen
			continue
		}
		// Not ours to refresh: nothing would ever clear it.
		errs = append(errs, r.clearDurable(ctx, d.View, d.Gen, guard))
	}
	return gens, errs
}

// clearSettled clears view's durable row when its refresh succeeded or its
// policy covers the window; any other outcome keeps the row, returning nil.
// Needs r.flushMu.
func (r *LateTradeRefresher) clearSettled(ctx context.Context, view string, out viewOutcome, gens map[string]int64, guard lateClearGuard) error {
	gen, ok := gens[view]
	if !ok || (out != viewRefreshed && out != viewCovered) {
		return nil
	}
	return r.clearDurable(ctx, view, gen, guard)
}

// clearDurable deletes view's durable row at gen, unless a late write was in
// flight when the flush read it or has begun since: that write may commit
// after the refresh ran, and only its row would remember it. A kept row is
// unioned into the next flush, which this kicks. Needs r.flushMu.
func (r *LateTradeRefresher) clearDurable(ctx context.Context, view string, gen int64, g lateClearGuard) error {
	r.mu.Lock()
	ok := g.idle && r.entered == g.entered
	if ok {
		r.resetCoverLocked()
	}
	r.mu.Unlock()
	if !ok {
		r.signal()
		return nil
	}
	if _, err := r.store.ClearCAGGLateRefreshWindow(ctx, timescale.CAGGLateFamilyTrades, view, gen); err != nil {
		return fmt.Errorf("clear late-trade cagg window %s: %w", view, err)
	}
	return nil
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
	viewCovered             // its policy reaches the whole window
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
		return viewCovered, 0, nil
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
	const msg = "late-trade cagg refresh left pending at shutdown; the next indexer start refreshes it from cagg_late_refresh_windows"
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

// writer returns store as the sink's trade writer, recording each late write
// before it and reporting each committed one to r when r is non-nil.
func (r *LateTradeRefresher) writer(store *timescale.Store) tradeWriter {
	if r == nil || store == nil {
		return store
	}
	return observingTradeWriter{tradeWriter: store, late: r}
}

// observingTradeWriter records every late trade write before it, and again
// after it if the write ran long enough to make more trades late, and
// reports every one that succeeded. A failed record fails the write, so the
// sink's retry covers it.
type observingTradeWriter struct {
	tradeWriter
	late *LateTradeRefresher
}

func (w observingTradeWriter) BatchInsertTrades(ctx context.Context, trades []canonical.Trade) error {
	end, err := w.late.beginWrite(ctx, trades)
	if err != nil {
		return err
	}
	defer end()
	if err := w.tradeWriter.BatchInsertTrades(ctx, trades); err != nil {
		return err
	}
	return w.late.committed(ctx, trades)
}

func (w observingTradeWriter) InsertTrade(ctx context.Context, t canonical.Trade) error {
	end, err := w.late.beginWrite(ctx, []canonical.Trade{t})
	if err != nil {
		return err
	}
	defer end()
	if err := w.tradeWriter.InsertTrade(ctx, t); err != nil {
		return err
	}
	return w.late.committed(ctx, []canonical.Trade{t})
}
