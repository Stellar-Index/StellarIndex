package orchestrator

import (
	"bytes"
	"context"
	"log/slog"
	"math/big"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// burstWindow is a 5m window of sdex prints, six per second in both stored directions, with a price that climbs every second so
// a VWAP over the newest slice differs from the whole window's.
func burstWindow(t *testing.T, pair canonical.Pair, from time.Time) []canonical.Trade {
	t.Helper()
	var out []canonical.Trade
	op := uint32(0)
	for sec := int64(0); sec < 300; sec++ {
		for k := int64(0); k < 6; k++ {
			op++
			base := 1_000_0000 + k*3_1000 + sec*7
			tr := makeTradeOn(t, pair, "sdex", base, base*(1000+sec)/10_000+k, from.Add(time.Duration(sec)*time.Second))
			tr.OpIndex = op
			if op%2 == 0 {
				tr.Pair = pair.Flip()
				tr.BaseAmount, tr.QuoteAmount = tr.QuoteAmount, tr.BaseAmount
			}
			out = append(out, tr)
		}
	}
	return out
}

// sumRatio is Σquote/Σbase over trades oriented to pair: the single-scale VWAP, computed without the aggregator.
func sumRatio(trades []canonical.Trade, pair canonical.Pair) *big.Rat {
	b, q := new(big.Int), new(big.Int)
	for _, tr := range oriented(trades, pair) {
		b.Add(b, tr.BaseAmount.BigInt())
		q.Add(q, tr.QuoteAmount.BigInt())
	}
	return new(big.Rat).SetFrac(q, b)
}

func tradeIDs(trades []canonical.Trade) []string {
	out := make([]string, 0, len(trades))
	for _, tr := range trades {
		out = append(out, tr.ID())
	}
	sort.Strings(out)
	return out
}

// The production burst: the cap leaves a 95 s raw slice and the view trails by 150 s, so the minutes between are neither raw
// nor materialised. They are summed from raw trades and the VWAP covers the whole window.
func TestFillFromMinuteSums_BurstTailIsComputedLive(t *testing.T) {
	pair := mkPair(t, "crypto", "BTC", "fiat", "USD")
	from := time.Date(2026, 11, 27, 2, 22, 0, 0, time.UTC)
	to := from.Add(5 * time.Minute)
	s := &sumsStore{raw: burstWindow(t, pair, from), watermark: to.Add(-150 * time.Second)}
	o := coverageOrch(t, s, 95*6, 0)

	got, weights, coveredFrom, err := o.fetchTradesDetectTruncation(context.Background(), pair, pair, 5*time.Minute, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if !coveredFrom.IsZero() {
		t.Fatalf("coveredFrom = %v; want the whole window covered", coveredFrom)
	}
	if want := [][2]time.Time{{from.Add(2 * time.Minute), from.Add(4 * time.Minute)}}; len(s.liveReads) != 1 || s.liveReads[0] != want[0] {
		t.Errorf("live reads %v, want %v (watermark minute to the raw part)", s.liveReads, want)
	}
	if n := tradeCount(got, weights); n != len(s.raw) {
		t.Errorf("trade count = %d, want %d", n, len(s.raw))
	}
	want := sumRatio(s.raw, pair)
	if have := sumRatio(got, pair); have.Cmp(want) != 0 {
		t.Errorf("VWAP = %s, want %s over every trade", have.FloatString(18), want.FloatString(18))
	}
	if newest := sumRatio(s.raw[len(s.raw)-95*6:], pair); newest.Cmp(want) == 0 {
		t.Fatal("fixture: the newest slice's VWAP equals the whole window's, so truncation would go unseen")
	}
}

// A wide minute in the live tail is re-read raw exactly as a materialised wide minute is: the same rows come out either way.
func TestFillFromMinuteSums_WideTailMinuteMatchesMaterialised(t *testing.T) {
	pair := mkPair(t, "crypto", "BTC", "fiat", "USD")
	from := time.Date(2026, 11, 27, 2, 22, 0, 0, time.UTC)
	to := from.Add(5 * time.Minute)
	raw := burstWindow(t, pair, from)
	evil := makeTradeOn(t, pair, "sdex", 1_0000000, 3_000_000, from.Add(2*time.Minute+7*time.Second))
	evil.OpIndex = 999999
	raw = append(raw, evil)

	run := func(watermark time.Time) []string {
		s := &sumsStore{raw: raw, watermark: watermark}
		o := coverageOrch(t, s, 95*6, 4)
		got, _, coveredFrom, err := o.fetchTradesDetectTruncation(context.Background(), pair, pair, 5*time.Minute, from, to)
		if err != nil {
			t.Fatal(err)
		}
		if !coveredFrom.IsZero() {
			t.Fatalf("watermark %v: coveredFrom = %v, want zero", watermark, coveredFrom)
		}
		for _, tr := range got {
			if tr.Timestamp.Truncate(time.Minute).Equal(from.Add(2*time.Minute)) && strings.HasPrefix(tr.TxHash, minuteSumTxPrefix) {
				t.Errorf("watermark %v: the wide minute carries a summary row %s; want raw rows only", watermark, tr.ID())
			}
		}
		return tradeIDs(got)
	}
	materialised, live := run(to), run(to.Add(-150*time.Second))
	if strings.Join(materialised, ",") != strings.Join(live, ",") {
		t.Errorf("live tail served %d rows, materialised view %d; want the same rows", len(live), len(materialised))
	}
}

// A trade inserted after its minute was materialised lowers the watermark; inside the live-read cap that minute is recomputed
// from raw (late trade counted once), past the cap the leg stays truncated with the reason.
func TestFillFromMinuteSums_PendingInvalidation(t *testing.T) {
	pair := mkPair(t, "crypto", "BTC", "fiat", "USD")
	from := time.Date(2026, 11, 27, 2, 0, 0, 0, time.UTC)

	t.Run("inside the cap is recomputed", func(t *testing.T) {
		to := from.Add(5 * time.Minute)
		raw := burstWindow(t, pair, from)
		late := makeTradeOn(t, pair, "sdex", 5_000_0000, 9_000_000, from.Add(time.Minute+10*time.Second))
		late.OpIndex = 888888
		raw = append(raw, late)
		s := &sumsStore{raw: raw, watermark: to, late: map[string]bool{late.ID(): true}}
		o := coverageOrch(t, s, 95*6, 0)
		got, weights, coveredFrom, err := o.fetchTradesDetectTruncation(context.Background(), pair, pair, 5*time.Minute, from, to)
		if err != nil {
			t.Fatal(err)
		}
		if !coveredFrom.IsZero() {
			t.Fatalf("coveredFrom = %v, want zero", coveredFrom)
		}
		if n := tradeCount(got, weights); n != len(raw) {
			t.Errorf("trade count = %d, want %d (the late trade once)", n, len(raw))
		}
		if have, want := sumRatio(got, pair), sumRatio(raw, pair); have.Cmp(want) != 0 {
			t.Errorf("VWAP = %s, want %s", have.FloatString(18), want.FloatString(18))
		}
	})

	t.Run("past the cap stays truncated", func(t *testing.T) {
		to := from.Add(time.Hour)
		raw := busyWindow(t, pair, from)
		late := makeTradeOn(t, pair, "sdex", 5_000_0000, 9_000_000, from.Add(5*time.Minute+10*time.Second))
		late.OpIndex = 888888
		raw = append(raw, late)
		s := &sumsStore{raw: raw, watermark: to, late: map[string]bool{late.ID(): true}}
		o := coverageOrch(t, s, 100, 0) // raw part covers only the last ~6 minutes
		var logs bytes.Buffer
		o.logger = slog.New(slog.NewTextHandler(&logs, nil))
		got, weights, coveredFrom, err := o.fetchTradesDetectTruncation(context.Background(), pair, pair, time.Hour, from, to)
		if err != nil {
			t.Fatal(err)
		}
		if coveredFrom.IsZero() || len(got) != 100 || weights != nil || len(s.liveReads) != 0 {
			t.Errorf("coveredFrom=%v rows=%d weights=%d live=%v; want the capped raw read", coveredFrom, len(got), len(weights), s.liveReads)
		}
		if !strings.Contains(logs.String(), "beyond the 15m0s cap") {
			t.Errorf("truncation log lacks the cap reason:\n%s", logs.String())
		}
	})
}

// For random trades over a window shorter than the live-read cap, any row cap and any watermark lag, the filled leg carries
// every trade and the same scale-normalised VWAP as the uncapped read.
func TestFillFromMinuteSums_PropertyFilledEqualsWholeWindow(t *testing.T) {
	pair := mkPair(t, "crypto", "XLM", "fiat", "USD")
	from := time.Date(2026, 11, 27, 3, 0, 0, 0, time.UTC)
	to := from.Add(10 * time.Minute)
	rng := rand.New(rand.NewSource(20261127)) //nolint:gosec // G404: deterministic test fixture
	sources := []string{"sdex", "binance", "soroswap"}
	for iter := 0; iter < 200; iter++ {
		n := 20 + rng.Intn(400)
		raw := make([]canonical.Trade, 0, n)
		for i := 0; i < n; i++ {
			ts := from.Add(time.Duration(rng.Int63n(int64(10 * time.Minute))))
			base := 1 + rng.Int63n(1_000_000_000)
			tr := makeTradeOn(t, pair, sources[rng.Intn(len(sources))], base, 1+base/(5+rng.Int63n(20)), ts)
			tr.OpIndex = uint32(i + 1)
			if rng.Intn(2) == 0 {
				tr.Pair = pair.Flip()
				tr.BaseAmount, tr.QuoteAmount = tr.QuoteAmount, tr.BaseAmount
			}
			raw = append(raw, tr)
		}
		watermark := from.Add(time.Duration(rng.Int63n(int64(11 * time.Minute))))
		s := &sumsStore{raw: raw, watermark: watermark}
		o := coverageOrch(t, s, 1+rng.Intn(n), 0)
		got, weights, coveredFrom, err := o.fetchTradesDetectTruncation(context.Background(), pair, pair, 10*time.Minute, from, to)
		if err != nil {
			t.Fatal(err)
		}
		if !coveredFrom.IsZero() {
			t.Fatalf("iter %d: coveredFrom = %v with watermark %v; want whole coverage", iter, coveredFrom, watermark)
		}
		all := oriented(raw, pair)
		if c := tradeCount(got, weights); c != len(all) {
			t.Fatalf("iter %d: trade count %d, want %d", iter, c, len(all))
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
			t.Fatalf("iter %d (watermark %v): VWAP %s, want %s", iter, watermark, have.FloatString(18), want.FloatString(18))
		}
	}
}
