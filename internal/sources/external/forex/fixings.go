package forex

import (
	"context"
	"errors"
	"math/big"
	"sort"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// FXFixingWriter is the optional append seam for fx_fixings. It has no read
// method on purpose: the gate is a function of the vendor series alone, so
// two regions fetching the same bars store the same rows.
type FXFixingWriter interface {
	InsertFXFixingBatch(ctx context.Context, bars []FXBar) error
}

const (
	// FixingReferenceWindow and fixingReferenceBars define a bar's gate
	// reference: the median close of the ≤24 most recent raw vendor bars
	// starting in the 96h before it.
	FixingReferenceWindow = 96 * time.Hour
	fixingReferenceBars   = 24

	// FixingSettle keeps the vendor's still-open bar out: a bar is a
	// fixing only once it ended at least this long ago.
	FixingSettle = 5 * time.Minute

	// fixingLiveWindow is how far back each live cycle re-offers bars, so
	// a missed tick or a vendor late-fill still lands.
	fixingLiveWindow = 48 * time.Hour
)

// fixingBandLo and fixingBandHi are [maxRateDeviation]'s band as exact
// ratios: close ÷ reference must sit in [0.5, 1.5].
var (
	fixingBandLo = big.NewRat(1, 2)
	fixingBandHi = big.NewRat(3, 2)
)

// GateFixings scores every candidate bar of series (one ticker, one grain,
// ascending by BarStart) against the median close of the raw bars before
// it. Refused bars stay in the reference of later bars, so the verdict on a
// bar depends only on the vendor series and never on what was stored. A bar
// with no reference (the first bar, or one after a gap longer than the
// window) is accepted.
func GateFixings(series []FXBar, isCandidate func(FXBar) bool) (accepted, refused []FXBar) {
	closes := make([]*big.Rat, len(series))
	for i, b := range series {
		closes[i], _ = new(big.Rat).SetString(b.CloseText)
	}
	for i, b := range series {
		if !isCandidate(b) {
			continue
		}
		if closes[i] == nil || closes[i].Sign() <= 0 {
			refused = append(refused, b)
			continue
		}
		ref := fixingReference(series, closes, i)
		if ref == nil || withinFixingBand(closes[i], ref) {
			accepted = append(accepted, b)
		} else {
			refused = append(refused, b)
		}
	}
	return accepted, refused
}

// fixingReference is the median close of the ≤[fixingReferenceBars] bars
// before series[i] that start within [FixingReferenceWindow] of it; nil when
// there are none.
func fixingReference(series []FXBar, closes []*big.Rat, i int) *big.Rat {
	floor := series[i].BarStart.Add(-FixingReferenceWindow)
	ref := make([]*big.Rat, 0, fixingReferenceBars)
	for j := i - 1; j >= 0 && len(ref) < fixingReferenceBars; j-- {
		if series[j].BarStart.Before(floor) {
			break
		}
		if !series[j].BarStart.Before(series[i].BarStart) || closes[j] == nil || closes[j].Sign() <= 0 {
			continue
		}
		ref = append(ref, closes[j])
	}
	return medianRat(ref)
}

// medianRat is the exact median; an even count averages the middle two.
func medianRat(xs []*big.Rat) *big.Rat {
	if len(xs) == 0 {
		return nil
	}
	s := append([]*big.Rat(nil), xs...)
	sort.Slice(s, func(a, b int) bool { return s[a].Cmp(s[b]) < 0 })
	n := len(s)
	if n%2 == 1 {
		return new(big.Rat).Set(s[n/2])
	}
	sum := new(big.Rat).Add(s[n/2-1], s[n/2])
	return sum.Quo(sum, big.NewRat(2, 1))
}

// withinFixingBand reports whether v ÷ ref ∈ [0.5, 1.5]; ref is positive.
func withinFixingBand(v, ref *big.Rat) bool {
	r := new(big.Rat).Quo(v, ref)
	return r.Cmp(fixingBandLo) >= 0 && r.Cmp(fixingBandHi) <= 0
}

// WithFixingWriter attaches the fx_fixings appender. nil leaves it off.
func (w *Worker) WithFixingWriter(fw FXFixingWriter) *Worker {
	w.fixingWriter = fw
	return w
}

// massiveTickers is the tickers of a raw snapshot the primary vendor itself
// served; a fallback provider's tickers are never queried for bars.
func massiveTickers(raw *Snapshot, source string) []string {
	if raw == nil || source != fxSource {
		return nil
	}
	out := make([]string, 0, len(raw.Currencies))
	for _, c := range raw.Currencies {
		if c.Ticker != anchorTicker {
			out = append(out, c.Ticker)
		}
	}
	return out
}

// guardedDailyRates indexes the served snapshot's guarded rates by ticker and
// UTC day: the current rate on its publication day, the admitted history on
// theirs. An accepted bar is compared with the rate for its own day.
func guardedDailyRates(snap *Snapshot) map[string]map[time.Time]*big.Rat {
	out := map[string]map[time.Time]*big.Rat{}
	if snap == nil {
		return out
	}
	add := func(ticker string, day time.Time, rate float64) {
		r := RateFromFloat(rate)
		if r == nil {
			return
		}
		if out[ticker] == nil {
			out[ticker] = map[time.Time]*big.Rat{}
		}
		out[ticker][day.UTC().Truncate(24*time.Hour)] = r
	}
	for ticker, points := range snap.History7d {
		for _, p := range points {
			add(ticker, p.Date, p.RateUSD)
		}
	}
	for _, c := range snap.Currencies {
		add(c.Ticker, c.UpdateAt, c.RateUSD)
	}
	return out
}

// RateFromFloat converts an in-memory feed rate to an exact Rat through its
// shortest round-trip decimal, so the conversion adds no digits the float did
// not carry. nil for a non-positive or non-finite rate.
func RateFromFloat(f float64) *big.Rat {
	if f <= 0 || !isFiniteFloat(f) {
		return nil
	}
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(f, 'g', -1, 64))
	if !ok {
		return nil
	}
	return r
}

// appendFixings offers the last [fixingLiveWindow] of closed hourly bars for
// each ticker, comparing accepted bars with served's guarded rates. It runs
// after the cache install, so the served snapshot never waits on it. A
// ticker's failure is counted and the cycle continues; a 429 or a dead
// context ends it.
func (w *Worker) appendFixings(ctx context.Context, tickers []string, served *Snapshot) {
	if w.fixingWriter == nil {
		return
	}
	defer obs.FXFixingsLastRefreshUnix.SetToCurrentTime()
	now := w.clock().UTC()
	guarded := guardedDailyRates(served)
	for _, ticker := range tickers {
		if ctx.Err() != nil {
			return
		}
		from := now.Add(-fixingLiveWindow - FixingReferenceWindow)
		series, err := w.client.ListAggBars(ctx, ticker, GrainHour, from, now)
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return
			}
			obs.FXFixingsFetchErrorsTotal.WithLabelValues(ticker).Inc()
			w.logger.Warn("forex: fx_fixings bar fetch failed", "ticker", ticker, "err", err)
			if IsRateLimited(err) {
				return
			}
			continue
		}
		accepted, refused := liveFixingCandidates(series, now)
		w.writeFixings(ctx, ticker, accepted, refused, guarded[ticker])
	}
}

// liveFixingCandidates gates series and keeps the bars a live cycle may
// insert: started within [fixingLiveWindow] and closed [FixingSettle] ago.
// Everything earlier in series is reference only.
func liveFixingCandidates(series []FXBar, now time.Time) (accepted, refused []FXBar) {
	floor := now.Add(-fixingLiveWindow)
	settled := now.Add(-FixingSettle)
	return GateFixings(series, func(b FXBar) bool {
		return !b.BarStart.Before(floor) && !b.BarEnd.After(settled)
	})
}

// writeFixings records the gate's refusals, flags disagreement with the
// guarded daily rate, and inserts the accepted bars.
func (w *Worker) writeFixings(ctx context.Context, ticker string, accepted, refused []FXBar, guarded map[time.Time]*big.Rat) {
	for _, b := range refused {
		obs.FXFixingsBarsRefusedTotal.WithLabelValues(ticker).Inc()
		w.logger.Warn("forex: fx_fixings gate refused bar",
			"ticker", ticker, "bar_start", b.BarStart, "close", b.CloseText)
	}
	disagrees := 0.0
	for _, b := range accepted {
		day := b.BarStart.UTC().Truncate(24 * time.Hour)
		g := guarded[day]
		v, _ := new(big.Rat).SetString(b.CloseText)
		if g != nil && v != nil && !withinFixingBand(v, g) {
			disagrees = 1
			w.logger.Warn("forex: fx_fixings bar disagrees with the guarded daily rate",
				"ticker", ticker, "bar_start", b.BarStart, "close", b.CloseText, "guarded", g.FloatString(8))
		}
	}
	obs.FXFixingsQuoteDisagrees.WithLabelValues(ticker).Set(disagrees)
	if len(accepted) == 0 {
		return
	}
	start := time.Now()
	err := w.fixingWriter.InsertFXFixingBatch(ctx, accepted)
	obs.FXFixingsWriteTxSeconds.Observe(time.Since(start).Seconds())
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			obs.FXFixingsWriteErrorsTotal.Inc()
			w.logger.Warn("forex: fx_fixings insert failed", "ticker", ticker, "bars", len(accepted), "err", err)
		}
		return
	}
	newest := accepted[len(accepted)-1].BarEnd
	w.fixingNewest = laterOf(w.fixingNewest, newest)
	obs.FXFixingsNewestBarEndUnix.Set(float64(w.fixingNewest.Unix()))
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// clock is the worker's time source; tests pin it.
func (w *Worker) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}
