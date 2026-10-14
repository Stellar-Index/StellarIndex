package baseline_test

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/confidence"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// stubSource is a baseline.TimedVWAPSource returning a per-pair fixture (or err), built relative to a per-test `now`
// so SplitByLookback's window cutoffs land where expected.
type stubSource struct {
	mu     sync.Mutex
	byPair map[string][]baseline.TimedVWAP
	err    error
}

func newStubSource() *stubSource {
	return &stubSource{byPair: make(map[string][]baseline.TimedVWAP)}
}

func (s *stubSource) set(pair canonical.Pair, vwaps []baseline.TimedVWAP) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byPair[pair.String()] = vwaps
}

func (s *stubSource) TimedVWAPsForPair1m(_ context.Context, pair canonical.Pair, _, _ time.Time) ([]baseline.TimedVWAP, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	return s.byPair[pair.String()], nil
}

// stubSink captures upsert calls for assertion.
type stubSink struct {
	mu     sync.Mutex
	byPair map[string]baseline.MultiBaseline
	err    error
	calls  int
}

func newStubSink() *stubSink {
	return &stubSink{byPair: make(map[string]baseline.MultiBaseline)}
}

func (s *stubSink) UpsertBaseline(_ context.Context, pair canonical.Pair, _, _, _ time.Time, m baseline.MultiBaseline) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return s.err
	}
	s.byPair[pair.String()] = m
	return nil
}

func mustPair(t *testing.T, base, quote string) canonical.Pair {
	t.Helper()
	b, err := canonical.ParseAsset(base)
	if err != nil {
		t.Fatalf("parse %s: %v", base, err)
	}
	q, err := canonical.ParseAsset(quote)
	if err != nil {
		t.Fatalf("parse %s: %v", quote, err)
	}
	p, err := canonical.NewPair(b, q)
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	return p
}

// stableTimedSeries builds `n` evenly-spaced timed VWAPs with bp
// jitter, ending at `now`. Bucket spacing is 1 minute (matching the
// 1m CAGG cadence). Every minute is a priced $100 fill.
func stableTimedSeries(now time.Time, n int) []baseline.TimedVWAP {
	out := make([]baseline.TimedVWAP, n)
	price := 1.0
	for i := 0; i < n; i++ {
		// 0.0001 jitter alternating sign
		shift := 0.0001
		if i%2 == 0 {
			shift = -shift
		}
		price += shift
		out[i] = baseline.TimedVWAP{
			VWAP:      price,
			BucketEnd: now.Add(-time.Duration(n-1-i) * time.Minute),
			USDVolume: big.NewRat(100, 1),
		}
	}
	return out
}

// TestRefresher_HappyPath_30dWindowFills — 30+ days of timed VWAPs
// produces a populated 30d (and 7d, 1d) baseline.
func TestRefresher_HappyPath_30dWindowFills(t *testing.T) {
	pair := mustPair(t, "native", "fiat:USD")
	now := time.Now().UTC()
	src := newStubSource()
	// 30d * 1440 buckets/day = 43,200 — overkill for tests, use a
	// smaller stable series that still spans >7d so all three
	// windows have valid data.
	src.set(pair, stableTimedSeries(now, 8*1440)) // 8 days
	sink := newStubSink()

	r := baseline.NewRefresher(src, sink, 30*24*time.Hour, nil)
	outcome, err := r.RefreshPair(context.Background(), pair)
	if err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}
	if outcome != baseline.OutcomeOK {
		t.Errorf("outcome = %v, want OutcomeOK", outcome)
	}
	if sink.calls != 1 {
		t.Errorf("sink.calls = %d, want 1", sink.calls)
	}
	got := sink.byPair[pair.String()]
	if got.Day30 == nil {
		t.Error("Day30 nil after refresh — should be populated with 8d of data")
	}
	if got.Day7 == nil {
		t.Error("Day7 nil — 8d of data should easily fill 7d")
	}
	if got.Day1 == nil {
		t.Error("Day1 nil — 8d of data covers 1d")
	}
}

// TestRefresher_30dBootstrap_NotEnoughSamples — fewer than 2 buckets
// available → can't compute even one return → OutcomeNotEnoughSamples
// and no write.
func TestRefresher_30dBootstrap_NotEnoughSamples(t *testing.T) {
	pair := mustPair(t, "native", "fiat:USD")
	src := newStubSource()
	// One bucket → 0 returns → no baseline at any window.
	src.set(pair, []baseline.TimedVWAP{
		{VWAP: 1.0, BucketEnd: time.Now().UTC()},
	})
	sink := newStubSink()

	r := baseline.NewRefresher(src, sink, 30*24*time.Hour, nil)
	outcome, err := r.RefreshPair(context.Background(), pair)
	if !errors.Is(err, baseline.ErrNotEnoughSamples) {
		t.Errorf("err = %v, want ErrNotEnoughSamples", err)
	}
	if outcome != baseline.OutcomeNotEnoughSamples {
		t.Errorf("outcome = %v, want OutcomeNotEnoughSamples", outcome)
	}
	if sink.calls != 0 {
		t.Errorf("sink.calls = %d, want 0", sink.calls)
	}
}

// TestRefresher_PartialBootstrap_OnlyDay30Valid — input spans more
// than 1d but bucketing density is too thin for the 1d window to
// have MinSamples. The 30d window upserts; the 1d slot is null.
func TestRefresher_PartialBootstrap_OnlyDay30Valid(t *testing.T) {
	pair := mustPair(t, "native", "fiat:USD")
	now := time.Now().UTC()
	// Three buckets spread over 8 days. Each window subset:
	//   1d  → just the most-recent (1 bucket → 0 returns → bootstrap)
	//   7d  → most-recent two (1 return → still < MinSamples=2 → bootstrap)
	//   30d → all three (2 returns → MinSamples=2 → valid)
	src := newStubSource()
	usd := big.NewRat(100, 1)
	src.set(pair, []baseline.TimedVWAP{
		{VWAP: 1.0, BucketEnd: now.Add(-8 * 24 * time.Hour), USDVolume: usd},
		{VWAP: 1.01, BucketEnd: now.Add(-2 * 24 * time.Hour), USDVolume: usd},
		{VWAP: 1.02, BucketEnd: now.Add(-30 * time.Minute), USDVolume: usd},
	})
	sink := newStubSink()

	r := baseline.NewRefresher(src, sink, 30*24*time.Hour, nil)
	outcome, err := r.RefreshPair(context.Background(), pair)
	if err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}
	if outcome != baseline.OutcomeOK {
		t.Errorf("outcome = %v, want OutcomeOK (Day30 valid)", outcome)
	}
	got := sink.byPair[pair.String()]
	if got.Day30 == nil {
		t.Error("Day30 nil; expected populated")
	}
	if got.Day7 != nil {
		t.Errorf("Day7 expected nil (bootstrap); got %v", got.Day7)
	}
	if got.Day1 != nil {
		t.Errorf("Day1 expected nil (bootstrap); got %v", got.Day1)
	}
}

// TestRefresher_ReadError — TimedVWAPSource fails → no compute,
// no write, OutcomeReadError.
func TestRefresher_ReadError(t *testing.T) {
	pair := mustPair(t, "native", "fiat:USD")
	src := newStubSource()
	src.err = errors.New("hypertable down")
	sink := newStubSink()

	r := baseline.NewRefresher(src, sink, 30*24*time.Hour, nil)
	outcome, err := r.RefreshPair(context.Background(), pair)
	if err == nil {
		t.Error("RefreshPair returned nil err, want non-nil")
	}
	if outcome != baseline.OutcomeReadError {
		t.Errorf("outcome = %v, want OutcomeReadError", outcome)
	}
	if sink.calls != 0 {
		t.Errorf("sink.calls = %d, want 0", sink.calls)
	}
}

// TestRefresher_WriteError — compute succeeds but Sink fails →
// OutcomeWriteError.
func TestRefresher_WriteError(t *testing.T) {
	pair := mustPair(t, "native", "fiat:USD")
	src := newStubSource()
	src.set(pair, stableTimedSeries(time.Now().UTC(), 100))
	sink := newStubSink()
	sink.err = errors.New("disk full")

	r := baseline.NewRefresher(src, sink, 30*24*time.Hour, nil)
	outcome, err := r.RefreshPair(context.Background(), pair)
	if err == nil {
		t.Error("RefreshPair returned nil err, want non-nil")
	}
	if outcome != baseline.OutcomeWriteError {
		t.Errorf("outcome = %v, want OutcomeWriteError", outcome)
	}
}

// TestRefresher_RefreshAll_AggregatesOutcomes — three pairs with
// three different outcomes; the summary counts each.
func TestRefresher_RefreshAll_AggregatesOutcomes(t *testing.T) {
	now := time.Now().UTC()
	pHappy := mustPair(t, "native", "fiat:USD")
	pBootstrap := mustPair(t, "native", "fiat:EUR")
	pReadErr := mustPair(t, "native", "fiat:GBP")

	src := newStubSource()
	src.set(pHappy, stableTimedSeries(now, 100))
	src.set(pBootstrap, []baseline.TimedVWAP{{VWAP: 1.0, BucketEnd: now}}) // not enough
	// pReadErr handled by perPairErrSource below.
	src2 := &perPairErrSource{base: src, errFor: pReadErr.String()}
	sink := newStubSink()

	r := baseline.NewRefresher(src2, sink, 30*24*time.Hour, nil)
	sum := r.RefreshAll(context.Background(),
		[]canonical.Pair{pHappy, pBootstrap, pReadErr}, 2)

	if sum.OK != 1 {
		t.Errorf("OK = %d, want 1", sum.OK)
	}
	if sum.NotEnoughSamples != 1 {
		t.Errorf("NotEnoughSamples = %d, want 1", sum.NotEnoughSamples)
	}
	if sum.ReadErrors != 1 {
		t.Errorf("ReadErrors = %d, want 1", sum.ReadErrors)
	}
	if sum.WriteErrors != 0 {
		t.Errorf("WriteErrors = %d, want 0", sum.WriteErrors)
	}
	if sink.calls != 1 {
		t.Errorf("sink.calls = %d, want 1 (only pHappy upserted)", sink.calls)
	}
}

type perPairErrSource struct {
	base   *stubSource
	errFor string
}

func (s *perPairErrSource) TimedVWAPsForPair1m(ctx context.Context, pair canonical.Pair, from, to time.Time) ([]baseline.TimedVWAP, error) {
	if pair.String() == s.errFor {
		return nil, errors.New("transient db error")
	}
	return s.base.TimedVWAPsForPair1m(ctx, pair, from, to)
}

// panicSource panics for one pair, delegating the rest to a base
// source. Used to simulate a decode/read fault that manifests as a
// panic inside a per-pair RefreshAll goroutine.
type panicSource struct {
	base     *stubSource
	panicFor string
}

func (s *panicSource) TimedVWAPsForPair1m(ctx context.Context, pair canonical.Pair, from, to time.Time) ([]baseline.TimedVWAP, error) {
	if pair.String() == s.panicFor {
		panic("simulated source fault in RefreshPair")
	}
	return s.base.TimedVWAPsForPair1m(ctx, pair, from, to)
}

// TestRefresher_RefreshAll_RecoversPanickingPair proves a panic in one pair's goroutine is contained: the batch completes
// and the healthy pair still upserts (unrecovered, a panic in a detached goroutine kills the process).
func TestRefresher_RefreshAll_RecoversPanickingPair(t *testing.T) {
	now := time.Now().UTC()
	pHappy := mustPair(t, "native", "fiat:USD")
	pPanic := mustPair(t, "native", "fiat:EUR")

	src := newStubSource()
	src.set(pHappy, stableTimedSeries(now, 100))
	psrc := &panicSource{base: src, panicFor: pPanic.String()}
	sink := newStubSink()

	r := baseline.NewRefresher(psrc, sink, 30*24*time.Hour, nil)
	sum := r.RefreshAll(context.Background(),
		[]canonical.Pair{pHappy, pPanic}, 2)

	if sum.OK != 1 {
		t.Errorf("OK = %d, want 1 (the healthy pair still upserts after its sibling panics)", sum.OK)
	}
	if sink.calls != 1 {
		t.Errorf("sink.calls = %d, want 1 (only pHappy upserted; the panicking pair wrote nothing)", sink.calls)
	}
}

// TestRefresher_RefreshAll_ConcurrencyClamp — concurrency <= 0
// falls back to 1 (serial).
func TestRefresher_RefreshAll_ConcurrencyClamp(t *testing.T) {
	pair := mustPair(t, "native", "fiat:USD")
	src := newStubSource()
	src.set(pair, stableTimedSeries(time.Now().UTC(), 100))
	sink := newStubSink()

	r := baseline.NewRefresher(src, sink, 30*24*time.Hour, nil)
	sum := r.RefreshAll(context.Background(),
		[]canonical.Pair{pair, pair, pair}, 0)
	if sum.OK != 3 {
		t.Errorf("OK = %d, want 3", sum.OK)
	}
}

// Two accounts cycling 0.01 USDC every minute for the whole 30-day window
// ($864 of flow) buy one baseline point per ~$6.94, not one per print, so
// the pair stays deep under the bootstrap gate.
func TestRefresher_PennySelfTradesEveryMinuteStayCapped(t *testing.T) {
	pair := mustPair(t, "native", "fiat:USD")
	now := time.Now().UTC()
	src := newStubSource()
	src.set(pair, withUSD(stableTimedSeries(now, 30*1440), big.NewRat(2, 100)))
	sink := newStubSink()

	r := baseline.NewRefresher(src, sink, 30*24*time.Hour, nil).WithMinuteNotionalFloor(productionFloor())
	if outcome, err := r.RefreshPair(context.Background(), pair); err != nil || outcome != baseline.OutcomeOK {
		t.Fatalf("RefreshPair = (%v, %v), want (ok, nil)", outcome, err)
	}
	d30 := sink.byPair[pair.String()].Day30
	// 348 penny-pair minutes ($6.96) per point: 43,200/348 = 124 points.
	if d30 == nil || d30.N != 123 {
		t.Fatalf("Day30 = %+v, want N=123 (one return per $6.94 of flow)", d30)
	}
	if days := float64(d30.N+1) / 1440; days >= confidence.BootstrapDensityDays {
		t.Errorf("density = %.3f days, want < %.1f (bootstrap cap stays engaged)", days, confidence.BootstrapDensityDays)
	}
}

// The issue's pair: mature, ~200 real minutes a day, with a self-trader
// filling every other minute at a penny. The pennies add one point a day
// ($12.40 of flow), so density stays where the real flow puts it.
func TestRefresher_PennyPaddingDoesNotLiftDensity(t *testing.T) {
	pair := mustPair(t, "native", "fiat:USD")
	now := time.Now().UTC()
	series := withUSD(stableTimedSeries(now, 30*1440), big.NewRat(1, 100))
	realMinutes := 0
	for i := range series {
		if i%1440 < 200 {
			series[i].USDVolume = big.NewRat(50, 1)
			realMinutes++
		}
	}
	src := newStubSource()
	src.set(pair, series)
	sink := newStubSink()

	r := baseline.NewRefresher(src, sink, 30*24*time.Hour, nil).WithMinuteNotionalFloor(productionFloor())
	outcome, err := r.RefreshPair(context.Background(), pair)
	if err != nil || outcome != baseline.OutcomeOK {
		t.Fatalf("RefreshPair = (%v, %v), want (ok, nil)", outcome, err)
	}
	d30 := sink.byPair[pair.String()].Day30
	if d30 == nil {
		t.Fatal("Day30 nil")
	}
	if want := realMinutes + 30 - 1; d30.N != want {
		t.Errorf("Day30.N = %d, want %d (real minutes + one penny point a day)", d30.N, want)
	}
	// Same measure orchestrator.baselineAgeDays reads.
	if days := float64(d30.N+1) / 1440; days >= confidence.BootstrapDensityDays {
		t.Errorf("density = %.2f days, want < %.1f (bootstrap cap stays engaged)", days, confidence.BootstrapDensityDays)
	}
}

// Sub-floor minutes merge into one point priced by USD weight, so a dust
// print at a wild price moves it only in proportion to its USD share.
func TestRefresher_VolumeBarPriceIsUSDWeighted(t *testing.T) {
	pair := mustPair(t, "native", "fiat:USD")
	now := time.Now().UTC()
	var series []baseline.TimedVWAP
	for i := 0; i < 10; i++ {
		end := now.Add(-time.Duration(20-2*i) * time.Minute)
		series = append(series,
			baseline.TimedVWAP{VWAP: 1000, BucketEnd: end.Add(-time.Minute), USDVolume: big.NewRat(1, 100)},
			baseline.TimedVWAP{VWAP: 2 + float64(i)*0.001, BucketEnd: end, USDVolume: big.NewRat(9_99, 100)})
	}
	src := newStubSource()
	src.set(pair, series)
	sink := newStubSink()

	r := baseline.NewRefresher(src, sink, 30*24*time.Hour, nil).WithMinuteNotionalFloor(big.NewRat(10, 1))
	if outcome, err := r.RefreshPair(context.Background(), pair); err != nil || outcome != baseline.OutcomeOK {
		t.Fatalf("RefreshPair = (%v, %v), want (ok, nil)", outcome, err)
	}
	d30 := sink.byPair[pair.String()].Day30
	// Each point is (1000*0.01 + p*9.99)/10 ≈ p+1, where an unweighted
	// mean would put it at ~500; the median return stays near zero.
	if d30 == nil || d30.N != 9 || d30.Median > 0.001 {
		t.Errorf("Day30 = %+v, want N=9 with a small median return", d30)
	}
}

// A pair with no USD-valued minute cannot be measured; it keeps a baseline
// over every minute so the z-score freeze stays live, under its own outcome.
func TestRefresher_UnvaluedPairKeepsGuardUnderDistinctOutcome(t *testing.T) {
	pair := mustPair(t, "native", "fiat:EUR")
	now := time.Now().UTC()
	src := newStubSource()
	src.set(pair, withUSD(stableTimedSeries(now, 2*1440), nil))
	sink := newStubSink()

	r := baseline.NewRefresher(src, sink, 30*24*time.Hour, nil).WithMinuteNotionalFloor(productionFloor())
	outcome, err := r.RefreshPair(context.Background(), pair)
	if err != nil || outcome != baseline.OutcomeOKUnvalued {
		t.Fatalf("RefreshPair = (%v, %v), want (ok_unvalued, nil)", outcome, err)
	}
	if d30 := sink.byPair[pair.String()].Day30; d30 == nil || d30.N != 2*1440-1 {
		t.Errorf("Day30 = %+v, want N=%d from every minute", d30, 2*1440-1)
	}
}

// A valued pair's unpriced minutes prove no notional and are dropped; a pair
// too thin for any baseline stays not_enough_samples; a pair with minutes
// but too little flow for three bars falls back to a per-minute baseline.
func TestRefresher_NotionalOutcomesAreDistinct(t *testing.T) {
	now := time.Now().UTC()
	mixed := mustPair(t, "native", "fiat:USD")
	series := stableTimedSeries(now, 1440)
	for i := range series {
		if i%2 == 1 {
			series[i].USDVolume = nil
		}
	}
	thin := mustPair(t, "native", "fiat:GBP")
	dust := mustPair(t, "native", "fiat:JPY")
	src := newStubSource()
	src.set(mixed, series)
	src.set(thin, withUSD(stableTimedSeries(now, 1), big.NewRat(1, 100)))
	src.set(dust, withUSD(stableTimedSeries(now, 100), big.NewRat(1, 1000)))
	sink := newStubSink()

	r := baseline.NewRefresher(src, sink, 30*24*time.Hour, nil).WithMinuteNotionalFloor(productionFloor())
	if outcome, err := r.RefreshPair(context.Background(), mixed); err != nil || outcome != baseline.OutcomeOK {
		t.Fatalf("mixed = (%v, %v), want (ok, nil)", outcome, err)
	}
	if d30 := sink.byPair[mixed.String()].Day30; d30 == nil || d30.N != 720-1 {
		t.Errorf("mixed Day30 = %+v, want N=719 (priced minutes only)", d30)
	}
	if outcome, err := r.RefreshPair(context.Background(), dust); err != nil || outcome != baseline.OutcomeOKPerMinuteFallback {
		t.Errorf("dust = (%v, %v), want ok_per_minute_fallback ($0.10 of flow is under one bar)", outcome, err)
	}
	if d30 := sink.byPair[dust.String()].Day30; d30 == nil || d30.N != baseline.MinZScoreSamples {
		t.Errorf("dust Day30 = %+v, want per-minute stats with N clamped to %d", d30, baseline.MinZScoreSamples)
	}
	sum := r.RefreshAll(context.Background(), []canonical.Pair{mixed, thin, dust}, 2)
	if sum.OK != 1 || sum.NotEnoughSamples != 1 || sum.OKPerMinuteFallback != 1 || sum.OKUnvalued != 0 {
		t.Errorf("summary = %+v, want OK=1 NotEnoughSamples=1 OKPerMinuteFallback=1", sum)
	}
}
