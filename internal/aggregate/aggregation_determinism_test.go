package aggregate

import (
	"flag"
	"fmt"
	"maps"
	"math/big"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

var updateDeterminismGolden = flag.Bool("update-determinism", false, "rewrite testdata/aggregation_determinism.golden")

const determinismShuffles = 50

var detT0 = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

// detDecimals mirrors the registry's split: on-chain DEX at 7, a CEX at 8, so NormalizeAmountScale lifts one source.
func detDecimals(source string) int {
	if source == "binance" {
		return 8
	}
	return 7
}

func detAmount(s string) canonical.Amount {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("bad amount " + s)
	}
	return canonical.NewAmount(v)
}

func detTrade(source string, ledger uint32, tx string, op uint32, sec int, base, quote string) canonical.Trade {
	return canonical.Trade{
		Source: source, Ledger: ledger, TxHash: strings.Repeat(tx, 64/len(tx)), OpIndex: op,
		Timestamp:  detT0.Add(time.Duration(sec) * time.Second),
		BaseAmount: detAmount(base), QuoteAmount: detAmount(quote),
	}
}

// detTradeSets are the inputs: shared ledger-close timestamps across sources, equal prices, amounts above 2^63 and 2^100, a zero
// leg, an outlier, and a single trade.
func detTradeSets() map[string][]canonical.Trade {
	return map[string][]canonical.Trade{
		"mixed": {
			detTrade("sdex", 100, "a1", 0, 0, "10000000", "25000000"),
			detTrade("sdex", 100, "a2", 0, 0, "10000000", "25000000"), // same ts, same price
			detTrade("soroswap", 100, "a0", 1, 0, "20000000", "50100000"),
			detTrade("aquarius", 100, "a3", 0, 0, "30000000", "74900000"),
			detTrade("sdex", 101, "b1", 0, 5, "9223372036854775809", "23058430092136939520"),                            // base > 2^63
			detTrade("soroswap", 101, "b2", 2, 5, "1267650600228229401496703205377", "3169126500570573503741758013440"), // > 2^100
			detTrade("aquarius", 101, "b3", 0, 5, "0", "5000000"),                                                       // zero base: unpriceable
			detTrade("binance", 0, "c1", 0, 5, "100000000", "250000000"),
			detTrade("binance", 0, "c2", 0, 30, "100000000", "251000000"),
			detTrade("sdex", 102, "d1", 0, 30, "10000000", "250000000"), // 10x outlier
			detTrade("soroswap", 103, "e1", 0, 61, "15000000", "37600000"),
			detTrade("aquarius", 103, "e2", 0, 61, "15000000", "37400000"),
			detTrade("sdex", 104, "f1", 0, 90, "40000000", "100000000"),
			detTrade("sdex", 104, "f2", 1, 90, "40000000", "100000000"),
		},
		"single": {detTrade("sdex", 200, "99", 0, 0, "3", "7")},
	}
}

// detBuckets feeds TWAPFromBuckets: a carry-in bucket, buckets on both window edges, an idle gap and an i128-scale VWAP.
func detBuckets() []ClosedBucket {
	at := func(m int) time.Time { return detT0.Add(time.Duration(m) * time.Minute) }
	return []ClosedBucket{
		{Close: at(-3), VWAP: "2.4999999"},
		{Close: at(0), VWAP: "2.5000001"},
		{Close: at(4), VWAP: "2.51"},
		{Close: at(17), VWAP: "170141183460469231731687303715884105727.0000001"},
		{Close: at(18), VWAP: "0.0000001"},
		{Close: at(30), VWAP: "99"},
		{Close: at(31), VWAP: "1000"},
	}
}

func ratText(r *big.Rat) string {
	if r == nil {
		return "<nil>"
	}
	return r.FloatString(30) + " (" + r.RatString() + ")"
}

func detSortedIDs(trades []canonical.Trade) string {
	ids := make([]string, len(trades))
	for i := range trades {
		ids[i] = trades[i].Source + "/" + trades[i].TxHash[:2] + "/" + fmt.Sprint(trades[i].OpIndex)
	}
	slices.Sort(ids)
	return strings.Join(ids, ",")
}

func detVWAP(trades []canonical.Trade) string {
	v, err := VWAP(NormalizeAmountScale(trades, detDecimals))
	if err != nil {
		return "err " + err.Error()
	}
	return ratText(v)
}

// renderAggregates runs every trade-consuming entry point over trades and renders each result as decimal text. Position-sensitive
// entry points (TWAP, OHLC) get the TradeOrderLess order every production caller sorts into first.
func renderAggregates(trades []canonical.Trade) string {
	var b strings.Builder
	line := func(k, v string) { b.WriteString(k + " = " + v + "\n") }
	w := NormalizeAmountScale(trades, detDecimals)
	line("vwap", detVWAP(trades))
	line("base_volume", TotalBaseVolume(w.Trades()).String())
	line("quote_volume", TotalQuoteVolume(w.Trades()).String())
	for _, c := range SourceContributions(w.Trades()) {
		line("contribution "+c.Source, fmt.Sprintf("weight=%s base=%s quote=%s trades=%d",
			ratText(c.Weight), c.BaseVolume, c.QuoteVolume, c.TradeCount))
	}
	kept := FilterOutliers(trades, 4)
	line("filter_outliers kept", detSortedIDs(kept))
	line("filter_outliers vwap", detVWAP(kept))
	sw := w.FilterOutliers(4)
	if v, err := VWAP(sw); err == nil {
		line("scaled_filter_outliers vwap", ratText(v))
	}
	local := FilterOutliersLocal(trades, LocalOutlierOptions{Sigma: 4, AmountScaleDecimals: detDecimals})
	line("filter_outliers_local kept", detSortedIDs(local))
	line("filter_outliers_local vwap", detVWAP(local))

	ordered := slices.Clone(w.Trades())
	slices.SortFunc(ordered, func(a, c canonical.Trade) int {
		if TradeOrderLess(&a, &c) {
			return -1
		}
		if TradeOrderLess(&c, &a) {
			return 1
		}
		return 0
	})
	twap, n, err := TWAPWithCount(ordered, detT0.Add(2*time.Minute))
	line("twap", fmt.Sprintf("%s weighted=%d err=%v", ratText(twap), n, err))
	if bar, err := ComputeOHLC(ordered); err == nil {
		line("ohlc", fmt.Sprintf("o=%s h=%s l=%s c=%s base=%s quote=%s n=%d", ratText(bar.Open), ratText(bar.High),
			ratText(bar.Low), ratText(bar.Close), bar.BaseVolume, bar.QuoteVolume, bar.TradeCount))
	}
	return b.String()
}

func renderBucketTWAP(buckets []ClosedBucket) string {
	res, ok, err := TWAPFromBuckets(buckets, detT0, detT0.Add(30*time.Minute))
	return fmt.Sprintf("twap_from_buckets = %s ok=%v buckets=%d newest=%s err=%v\n",
		ratText(res.Price), ok, res.Buckets, res.Newest.Format(time.RFC3339), err)
}

// TestAggregation_ByteIdenticalAcrossInputOrder pins that every aggregate is a function of the trade SET: 50 seeded shuffles render
// byte-identical text, equal to the checked-in golden (regenerate with -update-determinism).
func TestAggregation_ByteIdenticalAcrossInputOrder(t *testing.T) {
	sets := detTradeSets()
	names := slices.Sorted(maps.Keys(sets))
	var golden strings.Builder
	for _, name := range names {
		set := sets[name]
		want := renderAggregates(set)
		for seed := int64(1); seed <= determinismShuffles; seed++ {
			shuffled := slices.Clone(set)
			rng := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic test data
			rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			if got := renderAggregates(shuffled); got != want {
				t.Fatalf("set %q, shuffle seed %d: aggregates depend on input order\n got:\n%s\nwant:\n%s", name, seed, got, want)
			}
		}
		golden.WriteString("[" + name + "]\n" + want)
	}
	buckets := detBuckets()
	want := renderBucketTWAP(buckets)
	for seed := int64(1); seed <= determinismShuffles; seed++ {
		shuffled := slices.Clone(buckets)
		rng := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic test data
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		if got := renderBucketTWAP(shuffled); got != want {
			t.Fatalf("bucket shuffle seed %d: got %s want %s", seed, got, want)
		}
	}
	golden.WriteString("[buckets]\n" + want)

	path := filepath.Join("testdata", "aggregation_determinism.golden")
	if *updateDeterminismGolden {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(golden.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gold, err := os.ReadFile(path) // #nosec G304 -- fixed testdata path
	if err != nil {
		t.Fatalf("read golden (regenerate with -update-determinism): %v", err)
	}
	if string(gold) != golden.String() {
		t.Fatalf("aggregates drifted from %s (regenerate with -update-determinism after review)\n got:\n%s", path, golden.String())
	}
}
