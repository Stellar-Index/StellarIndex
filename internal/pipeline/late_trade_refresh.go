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
// once per debounce interval, one refresh in flight at a time, so a long
// catch-up costs a refresh per interval rather than per batch. A failed
// refresh is logged, counted, kept pending and retried with backoff;
// ingest never waits on it.
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

func (r *LateTradeRefresher) widenLocked(lo, hi time.Time) {
	if !r.pending || lo.Before(r.lo) {
		r.lo = lo
	}
	if !r.pending || hi.After(r.hi) {
		r.hi = hi
	}
	r.pending = true
}

func (r *LateTradeRefresher) signal() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Run refreshes pending windows until ctx is done.
func (r *LateTradeRefresher) Run(ctx context.Context) {
	wait := r.debounce
	for {
		select {
		case <-ctx.Done():
			r.logAbandoned()
			return
		case <-r.kick:
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			r.logAbandoned()
			return
		case <-t.C:
		}
		err := r.flush(ctx)
		if err == nil {
			wait = r.debounce
			continue
		}
		if ctx.Err() != nil {
			r.logAbandoned()
			return
		}
		obs.LateTradeCAGGRefreshTotal.WithLabelValues("error").Inc()
		wait = min(wait*2, r.maxBackoff)
		r.logger.Error("late-trade cagg refresh failed; window kept for retry", "retry_in", wait, "err", err)
		r.signal()
	}
}

// flush refreshes the pending window once. On failure the window is
// pending again.
func (r *LateTradeRefresher) flush(ctx context.Context) error {
	if err := r.loadPolicies(ctx); err != nil {
		return err
	}
	r.mu.Lock()
	if !r.pending {
		r.mu.Unlock()
		return nil
	}
	lo, hi := r.lo, r.hi
	r.pending = false
	policies := r.policies
	r.mu.Unlock()

	steps := lateTradeRefreshPlan(policies, lo, hi, r.now())
	for _, st := range steps {
		// Never forced: the invalidation log holds exactly the late rows,
		// and a forced pass would rebuild every bucket in the window.
		if err := timescale.RunCAGGRefreshStep(ctx, r.store, st, false); err != nil {
			r.mu.Lock()
			r.widenLocked(lo, hi)
			r.mu.Unlock()
			return fmt.Errorf("refresh %s over [%s, %s) for trades at [%s, %s]: %w", st.View,
				st.From.UTC().Format(time.RFC3339), st.To.UTC().Format(time.RFC3339),
				lo.UTC().Format(time.RFC3339), hi.UTC().Format(time.RFC3339), err)
		}
	}
	if len(steps) > 0 {
		obs.LateTradeCAGGRefreshTotal.WithLabelValues("ok").Inc()
		r.logger.Info("late-trade cagg refresh done", "views", len(steps),
			"trades_from", lo.UTC().Format(time.RFC3339), "trades_to", hi.UTC().Format(time.RFC3339))
	}
	return nil
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

// lateTradeRefreshPlan returns, in [timescale.TradesCAGGs] order (prices_1m
// before the twaps built on it), a non-forced refresh of every view whose
// policy cannot reach trades at lo, over [lo, hi] padded by half the
// view's MinWindow and cut off where its policy takes over.
func lateTradeRefreshPlan(policies map[string]timescale.CAGGRefreshWindow, lo, hi, now time.Time) []timescale.CAGGRefreshStep {
	var steps []timescale.CAGGRefreshStep
	for _, c := range timescale.TradesCAGGs {
		w, ok := policies[c.Name]
		if !ok || !w.HasPolicy || w.Unbounded {
			continue
		}
		cutoff := now.Add(-policyCoverage(c, w))
		if !lo.Before(cutoff) {
			continue
		}
		pad := c.MinWindow / 2
		from, to := lo.Add(-pad), hi.Add(pad)
		if to.After(cutoff) {
			to = cutoff
		}
		if to.Sub(from) < c.MinWindow {
			from = to.Add(-c.MinWindow)
		}
		steps = append(steps, timescale.CAGGRefreshStep{View: c.Name, From: from, To: to, Bucket: c.Bucket, MinWindow: c.MinWindow})
	}
	return steps
}

func (r *LateTradeRefresher) logAbandoned() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.pending {
		return
	}
	r.logger.Error("late-trade cagg refresh abandoned at shutdown; refresh the trades continuous aggregates over this window by hand",
		"trades_from", r.lo.UTC().Format(time.RFC3339), "trades_to", r.hi.UTC().Format(time.RFC3339))
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
