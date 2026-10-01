package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external/forex"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const (
	// fixingFetchWindow keeps each request inside one vendor page: the
	// aggregate limit counts minute bars, and 30 days is under 50,000.
	fixingFetchWindow = 30 * 24 * time.Hour
	fixingCallSpacing = 200 * time.Millisecond
	fixingRetryWait   = 30 * time.Second
	fixingMaxRetries  = 3
	fixingBatchSize   = 5000
)

type barLister interface {
	ListAggBars(ctx context.Context, ticker, grain string, from, to time.Time) ([]forex.FXBar, error)
}

type fixingStore interface {
	InsertFXFixingBatch(ctx context.Context, rows []timescale.FXFixing) (int64, error)
}

// fixingsRun is one --series=fixings pass. sleep is the pacing seam.
type fixingsRun struct {
	logger *slog.Logger
	client barLister
	store  fixingStore // nil on --dry-run
	from   time.Time
	now    time.Time
	sleep  func(context.Context, time.Duration) error
}

// tickerFixings is one ticker's outcome.
type tickerFixings struct {
	h0          time.Time // first hourly bar_start; zero when there is none
	dailyOffset time.Duration
	rows        int64
	refused     int
}

// openFixingsStore opens the store at generation 0 unless a correction
// generation is given: the backfill writes the same vendor bars the live
// appender writes, so it is not a correction and must not outrank them.
func openFixingsStore(ctx context.Context, dsn string, generation int64) (*timescale.Store, error) {
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if generation > 0 {
		store.SetDeriveGeneration(generation)
	}
	return store, nil
}

// runFixings backfills fx_fixings for each ticker: hourly bars from the
// first the vendor has (H0), daily bars below H0. A failed ticker fails the
// run; the others still complete.
func (r fixingsRun) runFixings(ctx context.Context, tickers []string) backfillResult {
	started := time.Now()
	var res backfillResult
	for _, ticker := range tickers {
		if ctx.Err() != nil {
			res.interrupted = true
			break
		}
		res.chunks++
		got, err := r.backfillTicker(ctx, ticker)
		if err != nil {
			res.failedChunks++
			r.logger.Error("fixings ticker failed", "ticker", ticker, "err", err)
			continue
		}
		res.totalRows += int(got.rows)
		r.logger.Info("fixings ticker done", "ticker", ticker, "h0", got.h0,
			"rows", got.rows, "refused", got.refused, "daily_t_offset", got.dailyOffset)
	}
	res.elapsed = time.Since(started).Round(time.Second)
	return res
}

func (r fixingsRun) backfillTicker(ctx context.Context, ticker string) (tickerFixings, error) {
	var out tickerFixings
	hourly, err := r.fetchSeries(ctx, ticker, forex.GrainHour, r.from, r.now)
	if err != nil {
		return out, err
	}
	dailyTo := r.now
	if len(hourly) > 0 {
		out.h0 = hourly[0].BarStart
		dailyTo = out.h0
	}
	daily, err := r.fetchSeries(ctx, ticker, forex.GrainDay, r.from, dailyTo)
	if err != nil {
		return out, err
	}
	if len(daily) > 0 {
		out.dailyOffset = daily[0].BarStart.Sub(daily[0].BarStart.UTC().Truncate(24 * time.Hour))
	}
	settled := r.now.Add(-forex.FixingSettle)
	dailyAcc, dailyRef := forex.GateFixings(daily, func(b forex.FXBar) bool {
		return !b.BarEnd.After(settled) && (out.h0.IsZero() || !b.BarEnd.After(out.h0))
	})
	hourlyAcc, hourlyRef := forex.GateFixings(hourly, func(b forex.FXBar) bool {
		return !b.BarEnd.After(settled)
	})
	out.refused = len(dailyRef) + len(hourlyRef)
	for _, b := range append(dailyRef, hourlyRef...) {
		r.logger.Warn("fixings gate refused bar", "ticker", ticker, "grain", b.Grain,
			"bar_start", b.BarStart, "close", b.CloseText)
	}
	out.rows, err = r.write(ctx, append(dailyAcc, hourlyAcc...))
	return out, err
}

// fetchSeries walks [from, to) in [fixingFetchWindow] steps, ascending, one
// spaced request per step.
func (r fixingsRun) fetchSeries(ctx context.Context, ticker, grain string, from, to time.Time) ([]forex.FXBar, error) {
	var out []forex.FXBar
	for start := from; start.Before(to); start = start.Add(fixingFetchWindow) {
		end := start.Add(fixingFetchWindow)
		if end.After(to) {
			end = to
		}
		bars, err := r.fetchWindow(ctx, ticker, grain, start, end)
		if err != nil {
			return nil, fmt.Errorf("%s %s %s..%s: %w", ticker, grain,
				start.Format(time.RFC3339), end.Format(time.RFC3339), err)
		}
		for _, b := range bars {
			// Window ends are inclusive on the vendor side; keep each bar once.
			if b.BarStart.Before(end) && (len(out) == 0 || b.BarStart.After(out[len(out)-1].BarStart)) {
				out = append(out, b)
			}
		}
	}
	return out, nil
}

// fetchWindow is one request, retried after [fixingRetryWait] on a 429.
func (r fixingsRun) fetchWindow(ctx context.Context, ticker, grain string, from, to time.Time) ([]forex.FXBar, error) {
	for attempt := 0; ; attempt++ {
		if err := r.sleep(ctx, fixingCallSpacing); err != nil {
			return nil, err
		}
		bars, err := r.client.ListAggBars(ctx, ticker, grain, from, to)
		if err == nil || !forex.IsRateLimited(err) || attempt == fixingMaxRetries {
			return bars, err
		}
		if err := r.sleep(ctx, fixingRetryWait); err != nil {
			return nil, err
		}
	}
}

func (r fixingsRun) write(ctx context.Context, bars []forex.FXBar) (int64, error) {
	if r.store == nil {
		return int64(len(bars)), nil
	}
	var total int64
	for len(bars) > 0 {
		n := min(len(bars), fixingBatchSize)
		rows := make([]timescale.FXFixing, n)
		for i, b := range bars[:n] {
			rows[i] = timescale.FXFixing{
				Ticker: b.Ticker, Grain: b.Grain, BarStart: b.BarStart, BarEnd: b.BarEnd,
				RateUSD: b.CloseText, Source: b.Source,
			}
		}
		got, err := r.store.InsertFXFixingBatch(ctx, rows)
		if err != nil {
			return total, err
		}
		total += got
		bars = bars[n:]
	}
	return total, nil
}

// runFixingsMain is run's --series=fixings arm.
func runFixingsMain(ctx context.Context, logger *slog.Logger, cfg backfillConfig) int {
	run := fixingsRun{
		logger: logger,
		client: forex.NewClient(os.Getenv("MASSIVE_API_KEY")),
		from:   cfg.from,
		now:    time.Now().UTC(),
		sleep:  sleepCtx,
	}
	tickers := make([]string, 0, len(cfg.tickerFilter))
	for t := range cfg.tickerFilter {
		tickers = append(tickers, t)
	}
	if !cfg.dryRun || len(tickers) == 0 {
		store, err := openFixingsStore(ctx, cfg.dsn, cfg.generation)
		if err != nil {
			logger.Error("open timescale", "err", err)
			return 1
		}
		defer func() { _ = store.Close() }()
		if !cfg.dryRun {
			run.store = store
		}
		if len(tickers) == 0 {
			if tickers, err = fxQuoteTickers(ctx, store); err != nil {
				logger.Error("list fx_quotes tickers", "err", err)
				return 1
			}
		}
	}
	sort.Strings(tickers)
	logger.Info("fx-history-backfill: start", "source", "massive", "series", seriesFixings,
		"from", cfg.from.Format("2006-01-02"), "tickers", len(tickers),
		"generation", cfg.generation, "dry_run", cfg.dryRun)
	return run.runFixings(ctx, tickers).report(logger)
}

// fxQuoteTickers is every ticker fx_quotes holds, USD (the anchor) excluded.
func fxQuoteTickers(ctx context.Context, store *timescale.Store) ([]string, error) {
	quotes, err := store.LatestFXQuotes(ctx, time.Time{})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(quotes))
	for _, q := range quotes {
		if q.Ticker != "USD" {
			out = append(out, q.Ticker)
		}
	}
	return out, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
