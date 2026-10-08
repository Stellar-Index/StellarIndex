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

// DefaultWindow is ADR-0019's rolling training window: 30 days of 1m bucket VWAPs.
const DefaultWindow = 30 * 24 * time.Hour

// TimedVWAPSource reads a pair's 1m VWAPs over [from, to), oldest first ([SplitByLookback]
// depends on the order).
type TimedVWAPSource interface {
	TimedVWAPsForPair1m(ctx context.Context, pair canonical.Pair, from, to time.Time) ([]TimedVWAP, error)
}

// Sink UPSERTs one pair's [MultiBaseline] so all three windows land atomically. It takes plain
// metadata so the storage adapter implements it, not the other way round.
type Sink interface {
	UpsertBaseline(
		ctx context.Context,
		pair canonical.Pair,
		computedAt, windowStart, windowEnd time.Time,
		m MultiBaseline,
	) error
}

// Refresher recomputes per-pair baselines from one 30-day prices_1m read (split by
// [SplitByLookback]) and writes them through a [Sink], on a slow cadence: they are 30-day stats.
type Refresher struct {
	src         TimedVWAPSource
	sink        Sink
	window      time.Duration
	logger      *slog.Logger
	minuteFloor *big.Rat
}

// NewRefresher constructs a Refresher; window <= 0 means [DefaultWindow]. logger is required.
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

// RefreshOutcome is one pair's refresh result, tallied in [RefreshSummary].
type RefreshOutcome int

const (
	OutcomeOK RefreshOutcome = iota
	OutcomeNotEnoughSamples
	OutcomeReadError
	OutcomeWriteError
	// OutcomeOKPerMinuteFallback: too little USD flow for MinSamples+1 volume bars, so the baseline
	// is per minute rather than leaving a publishable pair with no z-score freeze; Day30.N is clamped.
	OutcomeOKPerMinuteFallback
	// OutcomeOKUnvalued: no USD-valued minute, so the baseline uses every minute to keep the
	// z-score freeze live. Persisted.
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

// RefreshSummary counts [Refresher.RefreshAll] outcomes for metrics.
type RefreshSummary struct {
	OK                  int
	NotEnoughSamples    int
	ReadErrors          int
	WriteErrors         int
	OKPerMinuteFallback int
	OKUnvalued          int
}

// RefreshPair recomputes one pair's [MultiBaseline] from its 30-day series and upserts it. It
// persists nothing and returns [ErrNotEnoughSamples] when even the 30d window is in bootstrap, and
// OutcomeReadError / OutcomeWriteError with the source or sink error.
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
		// Day30.N is also the bootstrap cap's density. Under three bars that
		// density is ~0, but per-minute N is print count and dust can buy it
		// past the gate. Clamping at MinZScoreSamples keeps the 30d
		// window's freeze vote and drops the density to under an hour.
		if multi.Day30 != nil && multi.Day30.N > MinZScoreSamples {
			multi.Day30.N = MinZScoreSamples
		}
	}
	if multi.Day30 == nil {
		// Full bootstrap; the confidence-score loop applies ADR-0019's policy.
		return OutcomeNotEnoughSamples, ErrNotEnoughSamples
	}

	if err := r.sink.UpsertBaseline(ctx, pair, now, windowStart, now, multi); err != nil {
		return OutcomeWriteError, fmt.Errorf("baseline: UpsertBaseline %s: %w", pair.String(), err)
	}
	return okOutcome, nil
}

// volumeBars accumulates consecutive priced minutes into USD-volume bars of at least the minute
// floor, each priced at its USD-weighted mean VWAP, so every point (and unit of Day30.N density) costs
// real flow and dust cannot set the median/MAD. Unpriced minutes and a trailing remainder are dropped;
// valued is false when no minute was USD-valued.
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
		mean, _ := px.Quo(px, usd).Float64() // i128:ok volatility-baseline input (returns/MAD), never a served price
		bars = append(bars, TimedVWAP{VWAP: mean, BucketEnd: t.BucketEnd, USDVolume: new(big.Rat).Set(usd)})
		usd, px = new(big.Rat), new(big.Rat)
	}
	return bars, valued
}

// RefreshAll runs [Refresher.RefreshPair] for pairs, at most concurrency at once (<= 0 means 1;
// keep it within the DB pool). One pair's failure is logged, never aborting the batch.
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
			// A panic unwinds only this pair's goroutine (defers release sem + wg); it is logged with its
			// stack and absent from the summary.
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
