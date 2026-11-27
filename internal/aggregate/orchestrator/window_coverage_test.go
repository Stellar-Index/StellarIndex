package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

var _ TradeSumsStore = (*timescale.Store)(nil)

// sumsStore serves raw trades and the trade_sums_1m rows derived from them, as migration 0217 defines the view.
type sumsStore struct {
	raw       []canonical.Trade // stored direction in each trade's Pair
	watermark time.Time
	late      map[string]bool // Trade.IDs inserted after their minute was materialised
	rawReads  []time.Time     // from of every TradesInRange call
	liveReads [][2]time.Time  // [from, to) of every TradeSums1mLive call
}

func (s *sumsStore) TradesInRange(_ context.Context, p canonical.Pair, from, to time.Time, limit int) ([]canonical.Trade, error) {
	s.rawReads = append(s.rawReads, from)
	var out []canonical.Trade
	for _, t := range s.raw {
		if t.Timestamp.Before(from) || !t.Timestamp.Before(to) {
			continue
		}
		switch {
		case t.Pair.Equal(p):
			out = append(out, t)
		case t.Pair.Equal(p.Flip()):
			t.Pair = p
			t.BaseAmount, t.QuoteAmount = t.QuoteAmount, t.BaseAmount
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return tradeIsBefore(out[i], out[j]) })
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func tradeIsBefore(a, b canonical.Trade) bool {
	if !a.Timestamp.Equal(b.Timestamp) {
		return a.Timestamp.Before(b.Timestamp)
	}
	return a.OpIndex < b.OpIndex
}

// TradeSums1mInRange serves the view as materialised: minutes past the watermark absent, late trades missing from their minute,
// and the watermark lowered to the earliest late trade's minute, as the store does for a pending invalidation.
func (s *sumsStore) TradeSums1mInRange(_ context.Context, p canonical.Pair, from, to time.Time) ([]timescale.TradeMinuteSum, time.Time, error) {
	wm := s.watermark
	for _, t := range s.raw {
		if s.late[t.ID()] && !t.Timestamp.Before(from) && t.Timestamp.Before(wm) {
			wm = t.Timestamp.Truncate(time.Minute)
		}
	}
	end := to
	if s.watermark.Before(end) {
		end = s.watermark
	}
	return minuteSumsOf(s.raw, s.late, p, from, end), wm, nil
}

func (s *sumsStore) TradeSums1mLive(_ context.Context, p canonical.Pair, from, to time.Time) ([]timescale.TradeMinuteSum, error) {
	s.liveReads = append(s.liveReads, [2]time.Time{from, to})
	if to.Sub(from) > timescale.TradeSums1mLiveMaxSpan {
		return nil, fmt.Errorf("live span %v over the cap", to.Sub(from))
	}
	return minuteSumsOf(s.raw, nil, p, from, to), nil
}

// minuteSumsOf is migration 0217's SELECT over trades, minus the skip set, for buckets in [from, to).
func minuteSumsOf(raw []canonical.Trade, skip map[string]bool, p canonical.Pair, from, to time.Time) []timescale.TradeMinuteSum {
	idx := map[string]*timescale.TradeMinuteSum{}
	var keys []string
	for _, t := range raw {
		b := t.Timestamp.Truncate(time.Minute)
		forward := t.Pair.Equal(p)
		if (!forward && !t.Pair.Equal(p.Flip())) || b.Before(from) || !b.Before(to) || skip[t.ID()] {
			continue
		}
		if t.BaseAmount.Sign() <= 0 || t.QuoteAmount.Sign() <= 0 {
			continue
		}
		key := fmt.Sprintf("%s|%v|%v", t.Source, b, forward)
		price := new(big.Rat).SetFrac(t.QuoteAmount.BigInt(), t.BaseAmount.BigInt())
		r, ok := idx[key]
		if !ok {
			r = &timescale.TradeMinuteSum{
				Source: t.Source, Bucket: b, BaseSum: canonical.NewAmount(new(big.Int)), QuoteSum: canonical.NewAmount(new(big.Int)),
				MinPrice: new(big.Rat).Set(price), MaxPrice: new(big.Rat).Set(price),
			}
			idx[key] = r
			keys = append(keys, key)
		}
		r.BaseSum = canonical.NewAmount(new(big.Int).Add(r.BaseSum.BigInt(), t.BaseAmount.BigInt()))
		r.QuoteSum = canonical.NewAmount(new(big.Int).Add(r.QuoteSum.BigInt(), t.QuoteAmount.BigInt()))
		r.Count++
		if price.Cmp(r.MinPrice) < 0 {
			r.MinPrice.Set(price)
		}
		if price.Cmp(r.MaxPrice) > 0 {
			r.MaxPrice.Set(price)
		}
	}
	out := make([]timescale.TradeMinuteSum, 0, len(keys))
	for _, k := range keys {
		r := *idx[k]
		if !r.Bucket.IsZero() && !keyForward(k) {
			r.BaseSum, r.QuoteSum = r.QuoteSum, r.BaseSum
			r.MinPrice, r.MaxPrice = new(big.Rat).Inv(r.MaxPrice), new(big.Rat).Inv(r.MinPrice)
		}
		out = append(out, r)
	}
	return out
}

func keyForward(k string) bool { return k[len(k)-4:] == "true" }

// busyWindow builds a 1h window of sdex and binance trades in both stored directions, priced around 0.1 with a slow drift, three
// prints per source per 20 s on shared timestamps so a row cap cuts inside a timestamp.
func busyWindow(t *testing.T, pair canonical.Pair, from time.Time) []canonical.Trade {
	t.Helper()
	var out []canonical.Trade
	op := uint32(0)
	for sec := 0; sec < 3600; sec += 20 {
		ts := from.Add(time.Duration(sec) * time.Second)
		for k := int64(0); k < 3; k++ {
			op++
			base := 1_000_0000 + k*7_0000 + int64(sec)
			quote := base/10 + int64(sec)*3
			sd := makeTradeOn(t, pair, "sdex", base, quote, ts)
			sd.OpIndex = op
			if op%2 == 0 {
				sd.Pair = pair.Flip()
				sd.BaseAmount, sd.QuoteAmount = sd.QuoteAmount, sd.BaseAmount
			}
			bn := makeTradeOn(t, pair, "binance", base*10, quote*10+int64(k), ts)
			bn.OpIndex = op + 100000
			out = append(out, sd, bn)
		}
	}
	return out
}

func coverageOrch(t *testing.T, s *sumsStore, maxTrades int, sigma float64) *Orchestrator {
	t.Helper()
	return New(s, nil, Config{MaxTradesPerWindow: maxTrades, OutlierSigmaThreshold: sigma, Windows: []time.Duration{time.Hour}})
}

func oriented(trades []canonical.Trade, p canonical.Pair) []canonical.Trade {
	out := make([]canonical.Trade, 0, len(trades))
	for _, t := range trades {
		if t.Pair.Equal(p.Flip()) {
			t.Pair = p
			t.BaseAmount, t.QuoteAmount = t.QuoteAmount, t.BaseAmount
		}
		out = append(out, t)
	}
	return out
}

// A capped leg filled from minute sums yields the VWAP and trade count of the whole raw window, with the cap cutting inside a
// minute and inside a timestamp.
func TestFillFromMinuteSums_ReproducesTheWholeWindow(t *testing.T) {
	pair := mkPair(t, "crypto", "XLM", "fiat", "USD")
	from := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	s := &sumsStore{raw: busyWindow(t, pair, from), watermark: to}
	o := coverageOrch(t, s, 1001, 4) // 1001 = mid-timestamp; 6 rows per timestamp

	got, weights, coveredFrom, err := o.fetchTradesDetectTruncation(context.Background(), pair, pair, time.Hour, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if !coveredFrom.IsZero() || len(weights) == 0 {
		t.Fatalf("coveredFrom = %v, %d minute rows; want the whole window covered from minute sums", coveredFrom, len(weights))
	}
	all := oriented(s.raw, pair)
	if n := tradeCount(got, weights); n != len(all) {
		t.Errorf("trade count = %d, want %d", n, len(all))
	}
	want, err := o.computeNormalizedVWAP(all, pair)
	if err != nil {
		t.Fatal(err)
	}
	have, err := o.computeNormalizedVWAP(got, pair)
	if err != nil {
		t.Fatal(err)
	}
	if want.Cmp(have) != 0 {
		t.Errorf("VWAP = %s, want %s", have.FloatString(18), want.FloatString(18))
	}
	ids := map[string]bool{}
	for _, tr := range got {
		if ids[tr.ID()] {
			t.Fatalf("duplicate Trade.ID %s", tr.ID())
		}
		ids[tr.ID()] = true
	}
}

// With the outlier filter on, a regime shift inside the window gets the same verdicts and VWAP from a capped leg plus minute sums
// as from the uncapped read: summary rows weigh as their trades, so the recent raw rows cannot outvote the older majority.
func TestFillFromMinuteSums_OutlierFilterMatchesUncappedRead(t *testing.T) {
	pair := mkPair(t, "crypto", "XLM", "fiat", "USD")
	from := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	for _, step := range []int64{103, 110} { // +3% (chains as a regime shift) and +10% (a minority burst)
		var raw []canonical.Trade
		op := uint32(0)
		for sec := 0; sec < 3600; sec += 10 {
			level := int64(100)
			if sec >= 3000 {
				level = step
			}
			for k := int64(0); k < 3; k++ {
				op++
				base := 1_000_0000 + k*1_000 + int64(sec)
				tr := makeTradeOn(t, pair, "sdex", base, base*level/1000+k, from.Add(time.Duration(sec)*time.Second))
				tr.OpIndex = op
				raw = append(raw, tr)
			}
		}
		s := &sumsStore{raw: raw, watermark: to}
		o := coverageOrch(t, s, 200, 4) // raw rows cover the last ~11 minutes

		got, weights, coveredFrom, err := o.fetchTradesDetectTruncation(context.Background(), pair, pair, time.Hour, from, to)
		if err != nil {
			t.Fatal(err)
		}
		if !coveredFrom.IsZero() || len(weights) == 0 {
			t.Fatalf("step %d: coveredFrom = %v, %d minute rows; want full coverage from minute sums", step, coveredFrom, len(weights))
		}
		wantKept := aggregate.FilterOutliersLocal(raw, o.outlierOptions(nil))
		gotKept := aggregate.FilterOutliersLocal(got, o.outlierOptions(weights))
		if a, b := tradeCount(gotKept, weights), len(wantKept); a != b {
			t.Fatalf("step %d: filter kept %d trades, uncapped read keeps %d", step, a, b)
		}
		if len(wantKept) == 0 {
			continue
		}
		want, err := o.computeNormalizedVWAP(wantKept, pair)
		if err != nil {
			t.Fatal(err)
		}
		have, err := o.computeNormalizedVWAP(gotKept, pair)
		if err != nil {
			t.Fatal(err)
		}
		if want.Cmp(have) != 0 {
			t.Errorf("step %d: VWAP = %s, uncapped read gives %s", step, have.FloatString(18), want.FloatString(18))
		}
	}
}

// An off-price print inside honest volume makes its minute too wide to summarise: the minute is re-read raw so the print stays a
// row of its own for the outlier filter.
func TestFillFromMinuteSums_WideMinuteIsReadRaw(t *testing.T) {
	pair := mkPair(t, "crypto", "XLM", "fiat", "USD")
	from := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	raw := busyWindow(t, pair, from)
	evil := makeTradeOn(t, pair, "sdex", 1_0000000, 3_000_000, from.Add(5*time.Minute+7*time.Second))
	evil.OpIndex = 999999
	raw = append(raw, evil)
	s := &sumsStore{raw: raw, watermark: to}
	o := coverageOrch(t, s, 1000, 4)

	got, _, coveredFrom, err := o.fetchTradesDetectTruncation(context.Background(), pair, pair, time.Hour, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if !coveredFrom.IsZero() {
		t.Fatalf("coveredFrom = %v, want zero", coveredFrom)
	}
	found := false
	for _, tr := range got {
		if tr.ID() == evil.ID() {
			found = true
		}
		if tr.Timestamp.Truncate(time.Minute).Equal(from.Add(5*time.Minute)) && tr.TxHash != evil.TxHash {
			t.Errorf("minute 12:05 carries a summary row %s; want raw rows only", tr.ID())
		}
	}
	if !found {
		t.Error("the off-price print was folded into a minute row instead of kept as a raw row")
	}
}

// More wide minutes than the raw budget: coverage ends after the newest one left unread, and nothing older is served.
func TestFillFromMinuteSums_BudgetExhaustedEndsCoverage(t *testing.T) {
	pair := mkPair(t, "crypto", "XLM", "fiat", "USD")
	from := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	raw := busyWindow(t, pair, from)
	for m := 0; m <= minuteRawBudget; m++ {
		e := makeTradeOn(t, pair, "binance", 1_00000000, 30_000_000, from.Add(time.Duration(m)*time.Minute+time.Second))
		e.OpIndex = uint32(900000 + m)
		raw = append(raw, e)
	}
	s := &sumsStore{raw: raw, watermark: to}
	o := coverageOrch(t, s, 100, 4) // raw part covers only the last ~6 minutes
	var logs bytes.Buffer
	o.logger = slog.New(slog.NewTextHandler(&logs, nil))
	counter := obs.AggregatorWindowTruncatedTotal.WithLabelValues(pair.String(), "1h")
	before := testutil.ToFloat64(counter)

	got, _, coveredFrom, err := o.fetchTradesDetectTruncation(context.Background(), pair, pair, time.Hour, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if want := from.Add(time.Minute); !coveredFrom.Equal(want) {
		t.Fatalf("coveredFrom = %v, want %v (end of the oldest wide minute, the one past the budget)", coveredFrom, want)
	}
	if d := testutil.ToFloat64(counter) - before; d != 1 {
		t.Errorf("truncated counter {pair=%s, window=1h} advanced by %v, want 1", pair, d)
	}
	// The operator acts on this line: it must say which served pair and window lost coverage, from where, and over how many rows.
	for _, want := range []string{
		" pair=" + pair.String(), " window=1h", "covered_from=" + coveredFrom.UTC().Format("2006-01-02T15:04:05"), fmt.Sprintf("rows=%d", len(got)),
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("truncation log lacks %q:\n%s", want, logs.String())
		}
	}
	for _, tr := range got {
		if tr.Timestamp.Before(coveredFrom) {
			t.Errorf("row %s predates coveredFrom", tr.ID())
		}
	}
}

// The sums are not used, and the leg stays truncated at its raw read with the refusal named, when the unmaterialised tail between
// the view's watermark and the raw part is longer than the live-read cap, or the window does not start on a minute.
func TestFillFromMinuteSums_FallsBackToTruncated(t *testing.T) {
	pair := mkPair(t, "crypto", "XLM", "fiat", "USD")
	from := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	for name, tc := range map[string]struct {
		from      time.Time
		watermark time.Time
		reason    string
	}{
		"tail beyond the live-read cap": {from: from, watermark: from.Add(2 * time.Minute), reason: "beyond the 15m0s cap"},
		"window not minute-aligned":     {from: from.Add(30 * time.Second), watermark: to, reason: "not minute-aligned"},
	} {
		t.Run(name, func(t *testing.T) {
			s := &sumsStore{raw: busyWindow(t, pair, from), watermark: tc.watermark}
			o := coverageOrch(t, s, 100, 4) // raw part covers only the last ~6 minutes
			var logs bytes.Buffer
			o.logger = slog.New(slog.NewTextHandler(&logs, nil))
			got, weights, coveredFrom, err := o.fetchTradesDetectTruncation(context.Background(), pair, pair, to.Sub(tc.from), tc.from, to)
			if err != nil {
				t.Fatal(err)
			}
			if coveredFrom.IsZero() || len(got) != 100 || weights != nil {
				t.Errorf("coveredFrom=%v rows=%d weights=%d; want the capped raw read, truncated", coveredFrom, len(got), len(weights))
			}
			if len(s.liveReads) != 0 {
				t.Errorf("live tail reads %v; want none past the cap", s.liveReads)
			}
			if !strings.Contains(logs.String(), tc.reason) {
				t.Errorf("truncation log lacks reason %q:\n%s", tc.reason, logs.String())
			}
		})
	}
}
