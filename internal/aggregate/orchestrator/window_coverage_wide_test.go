package orchestrator

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// priceableCount counts the fills a served set stands for; zero-amount rows weigh in neither VWAP nor the minute sums.
func priceableCount(trades []canonical.Trade, weights map[string]int64) int {
	n := 0
	for i := range trades {
		if w, ok := weights[trades[i].ID()]; ok {
			n += int(w)
		} else if trades[i].BaseAmount.Sign() > 0 && trades[i].QuoteAmount.Sign() > 0 {
			n++
		}
	}
	return n
}

// assertServedIsRaw checks one fetch against the uncapped raw read: the served rows stand for exactly the raw fills in
// [coveredFrom, to), never start later than the plain capped read, and when whole get the uncapped read's outlier verdicts and VWAP.
func assertServedIsRaw(t *testing.T, o *Orchestrator, s *sumsStore, pair canonical.Pair, from, to time.Time) time.Time {
	t.Helper()
	got, weights, cf, err := o.fetchTradesDetectTruncation(context.Background(), pair, pair, to.Sub(from), from, to)
	if err != nil {
		t.Fatal(err)
	}
	capped, err := s.TradesInRange(context.Background(), pair, from, to, o.cfg.MaxTradesPerWindow)
	if err != nil {
		t.Fatal(err)
	}
	if !cf.IsZero() && len(capped) == o.cfg.MaxTradesPerWindow && cf.After(capped[0].Timestamp) {
		t.Fatalf("coveredFrom %v is later than the plain capped read's %v", cf, capped[0].Timestamp)
	}
	var want []canonical.Trade
	for _, tr := range oriented(s.raw, pair) {
		if !tr.Timestamp.Before(cf) && !tr.Timestamp.Before(from) && tr.Timestamp.Before(to) {
			want = append(want, tr)
		}
	}
	for _, tr := range got {
		if tr.Timestamp.Before(cf) {
			t.Fatalf("row %s predates coveredFrom %v", tr.ID(), cf)
		}
	}
	if a, b := priceableCount(got, weights), priceableCount(want, nil); a != b {
		t.Fatalf("served rows stand for %d fills, raw [%v, %v) holds %d", a, cf, to, b)
	}
	wv, err := o.computeNormalizedVWAP(want, pair)
	if err != nil {
		t.Fatal(err)
	}
	hv, err := o.computeNormalizedVWAP(got, pair)
	if err != nil {
		t.Fatal(err)
	}
	if wv.Cmp(hv) != 0 {
		t.Fatalf("VWAP over [%v, %v) = %s, raw gives %s", cf, to, hv.FloatString(18), wv.FloatString(18))
	}
	if !cf.IsZero() {
		return cf
	}
	wantKept := aggregate.FilterOutliersLocal(want, o.outlierOptions(nil))
	gotKept := aggregate.FilterOutliersLocal(got, o.outlierOptions(weights))
	if a, b := priceableCount(gotKept, weights), priceableCount(wantKept, nil); a != b {
		t.Fatalf("outlier filter kept %d fills, uncapped read keeps %d", a, b)
	}
	wv, err = o.computeNormalizedVWAP(wantKept, pair)
	if err != nil {
		t.Fatal(err)
	}
	hv, err = o.computeNormalizedVWAP(gotKept, pair)
	if err != nil {
		t.Fatal(err)
	}
	if wv.Cmp(hv) != 0 {
		t.Fatalf("filtered VWAP = %s, uncapped read gives %s", hv.FloatString(18), wv.FloatString(18))
	}
	return cf
}

// volatileWindow is an hour of sdex and binance prints in both stored directions around 0.1, with zero-amount rows, where each of
// the first `wideUpTo` minutes carries an off-price print: a 3% one (wide, not an outlier) or a 3x one.
func volatileWindow(t *testing.T, rng *rand.Rand, pair canonical.Pair, from time.Time, wideUpTo int) []canonical.Trade {
	t.Helper()
	var out []canonical.Trade
	op := uint32(0)
	add := func(source string, base, quote int64, ts time.Time) {
		op++
		tr := makeTradeOn(t, pair, source, base, quote, ts)
		tr.OpIndex = op
		if rng.Intn(2) == 0 {
			tr.Pair = pair.Flip()
			tr.BaseAmount, tr.QuoteAmount = tr.QuoteAmount, tr.BaseAmount
		}
		out = append(out, tr)
	}
	for m := 0; m < 60; m++ {
		start := from.Add(time.Duration(m) * time.Minute)
		for _, src := range []string{"sdex", "binance"} {
			for k := rng.Intn(12); k >= 0; k-- {
				base := 1_000_0000 + rng.Int63n(5_000_0000)
				quote := base/10 + rng.Int63n(base/2000+1) // within 0.05% of 0.1
				add(src, base, quote, start.Add(time.Duration(rng.Intn(60))*time.Second))
			}
		}
		if rng.Intn(4) == 0 {
			add("sdex", 0, 0, start.Add(time.Duration(rng.Intn(60))*time.Second))
		}
		if m < wideUpTo {
			base := 1_000_0000 + rng.Int63n(1_000_0000)
			quote := base * 103 / 1000
			if rng.Intn(3) == 0 {
				quote = base * 3 / 10
			}
			add([]string{"sdex", "binance"}[rng.Intn(2)], base, quote, start.Add(time.Duration(rng.Intn(60))*time.Second))
		}
	}
	return out
}

// Generated volatile hours, row caps cutting inside minutes and inside the re-reads: whatever the fill serves stands for exactly the
// raw fills from its coveredFrom, and a whole window matches the uncapped read. Most runs carry many times the re-read budget in wide
// minutes and must still come out whole.
func TestFillFromMinuteSums_WideMinutesDifferential(t *testing.T) {
	pair := mkPair(t, "crypto", "XLM", "fiat", "USD")
	from := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	whole := 0
	const runs = 60
	for seed := int64(0); seed < runs; seed++ {
		rng := rand.New(rand.NewSource(seed))
		s := &sumsStore{raw: volatileWindow(t, rng, pair, from, 30+rng.Intn(25)), watermark: to}
		o := coverageOrch(t, s, 40+rng.Intn(260), 4)
		if assertServedIsRaw(t, o, s, pair, from, to).IsZero() {
			whole++
		}
	}
	if whole < runs/2 {
		t.Errorf("%d of %d volatile windows came out whole; the fill must resolve far more than %d wide minutes", whole, runs, minuteRawBudget)
	}
}

// Over 40 wide minutes resolve in at most the re-read budget, and coverage reaches the window start.
func TestFillFromMinuteSums_ManyWideMinutesResolveWithinBudget(t *testing.T) {
	pair := mkPair(t, "crypto", "XLM", "fiat", "USD")
	from := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	raw := busyWindow(t, pair, from)
	for m := 0; m <= 40; m++ {
		e := makeTradeOn(t, pair, "binance", 1_00000000, 30_000_000, from.Add(time.Duration(m)*time.Minute+time.Second))
		e.OpIndex = uint32(900000 + m)
		raw = append(raw, e)
	}
	s := &sumsStore{raw: raw, watermark: to}
	o := coverageOrch(t, s, 100, 4)
	if cf := assertServedIsRaw(t, o, s, pair, from, to); !cf.IsZero() {
		t.Fatalf("coveredFrom = %v; want the whole window", cf)
	}
	if n := len(s.rawReads) - 2; n > minuteRawBudget { // less the capped read and the check's own
		t.Errorf("%d raw re-reads; want at most %d", n, minuteRawBudget)
	}
}

// A late fill lands in an already-resolved wide minute between ticks: the next tick serves it, exactly once.
func TestFillFromMinuteSums_LateInsertBetweenTicks(t *testing.T) {
	pair := mkPair(t, "crypto", "XLM", "fiat", "USD")
	from := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	rng := rand.New(rand.NewSource(7))
	s := &sumsStore{raw: volatileWindow(t, rng, pair, from, 40), watermark: to}
	o := coverageOrch(t, s, 150, 4)
	if cf := assertServedIsRaw(t, o, s, pair, from, to); !cf.IsZero() {
		t.Fatalf("tick 1: coveredFrom = %v; want whole", cf)
	}
	for i, at := range []time.Duration{3*time.Minute + 5*time.Second, 20*time.Minute + 9*time.Second} {
		late := makeTradeOn(t, pair, "binance", 4_000_0000, 4_100_000, from.Add(at))
		late.OpIndex = uint32(800000 + i)
		s.raw = append(s.raw, late)
	}
	if cf := assertServedIsRaw(t, o, s, pair, from, to); !cf.IsZero() {
		t.Fatalf("tick 2: coveredFrom = %v; want whole", cf)
	}
}

// Only a fill ending at an unresolved wide minute counts as one; unusable minute sums do not.
func TestFillFromMinuteSums_WideUnresolvedCounterIsDistinct(t *testing.T) {
	pair := mkPair(t, "crypto", "ETH", "fiat", "USD")
	from := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	wideCtr := obs.AggregatorWindowWideMinutesUnresolvedTotal.WithLabelValues(pair.String(), "1h")
	truncCtr := obs.AggregatorWindowTruncatedTotal.WithLabelValues(pair.String(), "1h")

	s := &sumsStore{raw: busyWindow(t, pair, from), watermark: from.Add(2 * time.Minute)}
	w0, t0 := testutil.ToFloat64(wideCtr), testutil.ToFloat64(truncCtr)
	if _, _, cf, err := coverageOrch(t, s, 100, 4).fetchTradesDetectTruncation(context.Background(), pair, pair, time.Hour, from, to); err != nil || cf.IsZero() {
		t.Fatalf("fixture: want a truncated fetch, got cf=%v err=%v", cf, err)
	}
	if dw, dt := testutil.ToFloat64(wideCtr)-w0, testutil.ToFloat64(truncCtr)-t0; dw != 0 || dt != 1 {
		t.Errorf("stale minute sums: wide counter +%v, truncated +%v; want +0, +1", dw, dt)
	}

	raw := busyWindow(t, pair, from)
	e := makeTradeOn(t, pair, "binance", 1_00000000, 30_000_000, from.Add(time.Second))
	raw = append(raw, e)
	s = &sumsStore{raw: raw, watermark: to, rereadErr: context.DeadlineExceeded}
	w0, t0 = testutil.ToFloat64(wideCtr), testutil.ToFloat64(truncCtr)
	if _, _, cf, err := coverageOrch(t, s, 100, 4).fetchTradesDetectTruncation(context.Background(), pair, pair, time.Hour, from, to); err != nil || cf.IsZero() {
		t.Fatalf("fixture: want a truncated fetch, got cf=%v err=%v", cf, err)
	}
	if dw, dt := testutil.ToFloat64(wideCtr)-w0, testutil.ToFloat64(truncCtr)-t0; dw != 1 || dt != 1 {
		t.Errorf("failed re-read: wide counter +%v, truncated +%v; want +1, +1", dw, dt)
	}
}
