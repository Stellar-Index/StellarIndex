package baseline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// DefaultWindow is the size of the rolling training window per
// ADR-0019: 30 days of 1m bucket VWAPs.
const DefaultWindow = 30 * 24 * time.Hour

// TimedVWAPSource reads time-stamped 1m VWAPs for a pair over a
// half-open window [from, to). Implementations are expected to
// return values in chronological order (oldest first); the
// downstream [SplitByLookback] depends on that ordering.
//
// Production wiring: a thin adapter around
// `timescale.Store.TimedVWAPsForPair1m`.
type TimedVWAPSource interface {
	TimedVWAPsForPair1m(ctx context.Context, pair canonical.Pair, from, to time.Time) ([]TimedVWAP, error)
}

// Sink persists a freshly-computed multi-window baseline.
// Implementations are expected to be UPSERT — one row per pair,
// the latest computation wins.
//
// The interface takes a [MultiBaseline] so the refresher's three-
// window output (1d / 7d / 30d) lands atomically in storage —
// either all three windows update together or the upsert fails as
// a unit.
//
// The interface takes the metadata fields directly rather than a
// pre-built struct so the refresher doesn't depend on the storage
// package — keeps the dep direction clean (storage adapter
// implements `Sink`, not the other way around).
type Sink interface {
	UpsertBaseline(
		ctx context.Context,
		pair canonical.Pair,
		computedAt, windowStart, windowEnd time.Time,
		m MultiBaseline,
	) error
}

// Refresher recomputes per-pair baselines from the prices_1m CAGG
// and writes them through a [Sink]. Designed to run as a separate
// goroutine in the aggregator binary on a slow cadence (e.g.
// hourly) — baselines are 30-day rolling stats, so refreshing at
// the 1m bucket cadence would be wasted work.
//
// On each per-pair refresh, the Refresher pulls the full 30-day
// VWAP series and uses [SplitByLookback] to derive the 1d / 7d
// sub-windows, then computes a [MultiBaseline] and persists it
// atomically — one read of the hypertable produces all three
// windows.
type Refresher struct {
	src         TimedVWAPSource
	sink        Sink
	window      time.Duration
	logger      *slog.Logger
	minuteFloor *big.Rat
}

// NewRefresher constructs a Refresher. Pass `window <= 0` to use
// [DefaultWindow]. Logger is required (use slog.Default() if you
// don't have one).
func NewRefresher(src TimedVWAPSource, sink Sink, window time.Duration, logger *slog.Logger) *Refresher {
	if window <= 0 {
		window = DefaultWindow
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Refresher{src: src, sink: sink, window: window, logger: logger, minuteFloor: new(big.Rat)}
}

// WithMinuteNotionalFloor sets the USD notional one baseline sample point
// must carry; see [Refresher.volumeBars] and [MinuteNotionalFloor]. nil or
// negative means 0 (every priced minute is its own point).
func (r *Refresher) WithMinuteNotionalFloor(floor *big.Rat) *Refresher {
	if floor == nil || floor.Sign() < 0 {
		floor = new(big.Rat)
	}
	r.minuteFloor = new(big.Rat).Set(floor)
	return r
}

// MinuteNotionalFloor pro-rates the publish floor (min_usd_volume per
// window) to one minute of the LONGEST published window, so a pair trading
// at exactly the publish floor yields one baseline point per minute and the
// bootstrap cap's density measures sustained USD flow, not print count.
// minUSDVolume <= 0 (floor disabled) yields 0.
func MinuteNotionalFloor(minUSDVolume float64, longestWindow time.Duration) *big.Rat {
	minutes := int64(longestWindow / time.Minute)
	if minUSDVolume <= 0 || minutes <= 0 {
		return new(big.Rat)
	}
	f := new(big.Rat).SetFloat64(minUSDVolume)
	if f == nil {
		return new(big.Rat)
	}
	return f.Quo(f, new(big.Rat).SetInt64(minutes))
}

// RefreshOutcome describes the per-pair outcome of one refresh
// attempt — used by [RefreshSummary] to give callers a structured
// breakdown of what happened across a batch.
type RefreshOutcome int

const (
	OutcomeOK RefreshOutcome = iota
	OutcomeNotEnoughSamples
	OutcomeReadError
	OutcomeWriteError
	// OutcomeOKPerMinuteFallback: the window carried too little USD flow
	// for MinSamples+1 volume bars, so the baseline was built one point per
	// minute, as before volume bars, rather than leaving a publishable pair
	// with no z-score freeze. Its density is print-count and dust-buyable.
	OutcomeOKPerMinuteFallback
	// OutcomeOKUnvalued: no minute in the window carried a USD valuation,
	// so notional is unmeasurable and the baseline was built from every
	// minute to keep the z-score freeze live. Persisted, like OutcomeOK.
	OutcomeOKUnvalued
)

func (o RefreshOutcome) String() string {
	switch o {
	case OutcomeOK:
		return "ok"
	case OutcomeNotEnoughSamples:
		return "not_enough_samples"
	case OutcomeReadError:
		return "read_error"
	case OutcomeWriteError:
		return "write_error"
	case OutcomeOKPerMinuteFallback:
		return "ok_per_minute_fallback"
	case OutcomeOKUnvalued:
		return "ok_unvalued"
	default:
		return "unknown"
	}
}

// RefreshSummary aggregates the outcomes of a [Refresher.RefreshAll]
// run. Counts per outcome let the caller emit metrics in one place
// without scanning per-pair errors.
type RefreshSummary struct {
	OK                  int
	NotEnoughSamples    int
	ReadErrors          int
	WriteErrors         int
	OKPerMinuteFallback int
	OKUnvalued          int
}

// RefreshPair recomputes the baseline for one pair and writes it.
// Reads the pair's full 30-day timed VWAP series, splits into 1d /
// 7d / 30d sub-windows, computes a [MultiBaseline] (each window
// independently bootstraps if it doesn't have enough samples), and
// upserts atomically.
//
// Returns:
//
//   - (OutcomeOK, nil) on a successful upsert (Day30 valid; the
//     1d/7d windows may still be in bootstrap on this scale)
//   - (OutcomeNotEnoughSamples, [ErrNotEnoughSamples]) when even
//     the 30d window has fewer than [MinSamples] returns — the
//     pair is in full bootstrap and nothing is persisted
//   - (OutcomeOKPerMinuteFallback, nil) on a successful upsert of the
//     per-minute baseline for a pair with too little USD flow for bars
//   - (OutcomeOKUnvalued, nil) on a successful upsert for a pair with no
//     USD-valued minute in the window
//   - (OutcomeReadError, err) on a [TimedVWAPSource] failure
//   - (OutcomeWriteError, err) on a [Sink] failure
func (r *Refresher) RefreshPair(ctx context.Context, pair canonical.Pair) (RefreshOutcome, error) {
	now := time.Now().UTC()
	windowStart := now.Add(-r.window)

	timed, err := r.src.TimedVWAPsForPair1m(ctx, pair, windowStart, now)
	if err != nil {
		return OutcomeReadError, fmt.Errorf("baseline: TimedVWAPsForPair1m %s: %w", pair.String(), err)
	}

	sample, valued := r.volumeBars(timed)
	okOutcome := OutcomeOK
	if !valued {
		sample = timed
		okOutcome = OutcomeOKUnvalued
	}

	multi := NewMultiBaseline(SplitByLookback(sample, now))
	if multi.Day30 == nil && valued {
		multi = NewMultiBaseline(SplitByLookback(timed, now))
		okOutcome = OutcomeOKPerMinuteFallback
	}
	if multi.Day30 == nil {
		// Even the long window is in bootstrap; persist nothing.
		// Caller's confidence-score loop applies ADR-0019 bootstrap
		// policy.
		return OutcomeNotEnoughSamples, ErrNotEnoughSamples
	}

	if err := r.sink.UpsertBaseline(ctx, pair, now, windowStart, now, multi); err != nil {
		return OutcomeWriteError, fmt.Errorf("baseline: UpsertBaseline %s: %w", pair.String(), err)
	}
	return okOutcome, nil
}

// volumeBars turns a valued pair's minutes into USD-volume bars: consecutive
// priced minutes accumulate until their summed notional reaches the minute
// floor, then emit one point at the last minute's bucket end, priced at the
// USD-weighted mean of their VWAPs. Every point, and so every return and
// every unit of Day30.N density, costs a floor's worth of USD flow; a dust
// minute contributes usd/floor of a point and that share of its price, so
// real flow sets the median/MAD. A minute at or above the floor is its own
// point at its exact VWAP. Unpriced minutes prove no notional and are
// skipped; a trailing sub-floor remainder is dropped. valued is false when
// no minute carried a USD valuation, and the caller falls back to every
// minute.
func (r *Refresher) volumeBars(timed []TimedVWAP) (bars []TimedVWAP, valued bool) {
	bars = make([]TimedVWAP, 0, len(timed))
	usd, px := new(big.Rat), new(big.Rat)
	for i := range timed {
		t := timed[i]
		if t.USDVolume == nil || t.USDVolume.Sign() <= 0 {
			continue
		}
		v := new(big.Rat).SetFloat64(t.VWAP)
		if v == nil {
			continue
		}
		valued = true
		usd.Add(usd, t.USDVolume)
		px.Add(px, v.Mul(v, t.USDVolume))
		if usd.Cmp(r.minuteFloor) < 0 {
			continue
		}
		mean, _ := px.Quo(px, usd).Float64()
		bars = append(bars, TimedVWAP{VWAP: mean, BucketEnd: t.BucketEnd, USDVolume: new(big.Rat).Set(usd)})
		usd, px = new(big.Rat), new(big.Rat)
	}
	return bars, valued
}

// RefreshAll runs [Refresher.RefreshPair] for every pair in
// `pairs` with up to `concurrency` in flight at once. Per-pair
// failures are logged but don't abort the batch — a transient
// failure on one pair shouldn't starve the others. Returns a
// summary of outcomes across the batch.
//
// concurrency <= 0 falls back to 1 (serial). Use a value at or
// below your DB connection-pool size to avoid pool exhaustion.
func (r *Refresher) RefreshAll(ctx context.Context, pairs []canonical.Pair, concurrency int) RefreshSummary {
	if concurrency < 1 {
		concurrency = 1
	}

	type result struct {
		outcome RefreshOutcome
	}
	results := make(chan result, len(pairs))

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
loop:
	for _, pair := range pairs {
		select {
		case <-ctx.Done():
			break loop // exit the outer for, not just this select
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(pair canonical.Pair) {
			// A panic in one pair's refresh must not crash the whole
			// aggregator process — it unwinds this per-pair goroutine,
			// releasing sem + wg via the defers below; the pair is simply
			// absent from the summary (logged at Error with its stack).
			defer worker.Recover(r.logger, "baseline-refresh:"+pair.String())
			defer wg.Done()
			defer func() { <-sem }()

			outcome, err := r.RefreshPair(ctx, pair)
			if err != nil && !errors.Is(err, ErrNotEnoughSamples) && ctx.Err() == nil {
				r.logger.Warn("baseline refresh failed",
					"pair", pair.String(), "outcome", outcome.String(), "err", err)
			}
			results <- result{outcome: outcome}
		}(pair)
	}
	wg.Wait()
	close(results)

	var sum RefreshSummary
	for res := range results {
		switch res.outcome {
		case OutcomeOK:
			sum.OK++
		case OutcomeNotEnoughSamples:
			sum.NotEnoughSamples++
		case OutcomeReadError:
			sum.ReadErrors++
		case OutcomeWriteError:
			sum.WriteErrors++
		case OutcomeOKPerMinuteFallback:
			sum.OKPerMinuteFallback++
		case OutcomeOKUnvalued:
			sum.OKUnvalued++
		}
	}
	return sum
}
