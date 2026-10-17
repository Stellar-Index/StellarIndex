package aggregate

import (
	"fmt"
	"math/big"
	"math/rand"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

func TestWeightedMedianRat_MatchesExpandedMultiset(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for trial := 0; trial < 500; trial++ {
		n := 1 + rng.Intn(8)
		vals := make([]*big.Rat, n)
		w := make([]int64, n)
		var expanded []*big.Rat
		for i := range vals {
			vals[i] = big.NewRat(int64(rng.Intn(20)), int64(1+rng.Intn(3)))
			w[i] = int64(1 + rng.Intn(4))
			for j := int64(0); j < w[i]; j++ {
				expanded = append(expanded, vals[i])
			}
		}
		if got, want := weightedMedianRat(vals, w), medianRat(expanded); got.Cmp(want) != 0 {
			t.Fatalf("trial %d: weighted median %s, expanded median %s", trial, got.RatString(), want.RatString())
		}
	}
}

// A summary row of c trades at one price must get the verdict its c trades would get as separate rows: the filter's centre, spread
// and bucket thresholds count trades, not rows. Every minute carries at least MinBucket trades, so verdicts come from bucket and
// window references alone and the expanded copies of a row agree with each other.
func TestFilterOutliersLocal_WeightedRowsMatchExpandedTrades(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	t0 := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	for trial := 0; trial < 200; trial++ {
		var rows, expanded []canonical.Trade
		weight := map[string]int64{}
		origin := map[string]string{}
		centre := int64(1_000_000)
		if trial%2 == 1 {
			centre = 1_100_000 // regime shift on alternate trials, applied half-way below
		}
		for m := 0; m < 40; m++ {
			ts := t0.Add(time.Duration(m) * time.Minute)
			level := int64(1_000_000)
			if m >= 30 {
				level = centre
			}
			price := func() int64 {
				p := level + int64(rng.Intn(4001)) - 2000
				if rng.Intn(25) == 0 {
					p = level * int64(2+rng.Intn(3)) // gross outlier
				}
				return p
			}
			if m < 30 && rng.Intn(3) > 0 {
				c := int64(3 + rng.Intn(60))
				p := price()
				r := localTrade("sdex", p*c, ts)
				r.BaseAmount = canonical.NewAmount(big.NewInt(localFixtureBase * c))
				r.TxHash = fmt.Sprintf("sum-%d-%d", trial, m)
				rows = append(rows, r)
				weight[r.ID()] = c
				for j := int64(0); j < c; j++ {
					e := localTrade("sdex", p, ts)
					e.TxHash, e.OpIndex = r.TxHash, uint32(j)
					expanded = append(expanded, e)
					origin[e.ID()] = r.ID()
				}
				continue
			}
			for j := 0; j < 3+rng.Intn(5); j++ {
				r := localTrade("binance", price(), ts.Add(time.Duration(j)*time.Second))
				r.TxHash = fmt.Sprintf("raw-%d-%d-%d", trial, m, j)
				rows = append(rows, r)
				expanded = append(expanded, r)
				origin[r.ID()] = r.ID()
			}
		}
		opts := LocalOutlierOptions{Sigma: 4}
		want := map[string]int{}
		for _, e := range FilterOutliersLocal(expanded, opts) {
			want[origin[e.ID()]]++
		}
		opts.Weight = func(tr *canonical.Trade) int64 {
			if w, ok := weight[tr.ID()]; ok {
				return w
			}
			return 1
		}
		got := FilterOutliersLocal(rows, opts)
		kept := map[string]bool{}
		for _, r := range got {
			kept[r.ID()] = true
		}
		for _, r := range rows {
			c := weight[r.ID()]
			if c == 0 {
				c = 1
			}
			if n := want[r.ID()]; (n == int(c)) != kept[r.ID()] || (n != 0 && n != int(c)) {
				t.Fatalf("trial %d row %s (weight %d): weighted kept=%v, expanded kept %d of %d copies", trial, r.ID(), c, kept[r.ID()], n, c)
			}
		}
	}
}

func weightedLocalIndex(t *testing.T, trades []canonical.Trade, weights []int64) *localIndex {
	t.Helper()
	validIdx := make([]int, len(trades))
	prices := make([]*big.Rat, len(trades))
	for i := range trades {
		validIdx[i] = i
		p, ok := priceRat(&trades[i])
		if !ok {
			t.Fatalf("price for %+v", trades[i])
		}
		prices[i] = p
	}
	return newLocalIndex(trades, validIdx, prices, weights, LocalOutlierOptions{Sigma: 4}.withDefaults())
}

// A minute holding one summary row of 60 trades is a dense bucket with its own reference, as its 60 trades would be.
func TestLocalIndex_WeightedBucketCountsTrades(t *testing.T) {
	t0 := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	var trades []canonical.Trade
	for m := 0; m < 3; m++ {
		trades = append(trades, localTrade("sdex", 1_000_000+int64(m)*100, t0.Add(time.Duration(m)*time.Minute)))
	}
	weighted := weightedLocalIndex(t, trades, []int64{60, 60, 60})
	single := weightedLocalIndex(t, trades, nil)
	for bi := range weighted.buckets {
		if weighted.bucketRef(bi) == nil {
			t.Errorf("bucket %d of one 60-trade row has no reference", bi)
		}
		if single.bucketRef(bi) != nil {
			t.Errorf("bucket %d of one unweighted row has a reference", bi)
		}
	}
}

// A row of w trades in a thin stretch sees the count-neighbourhood of its middle trade: (w-1)/2 of its other trades on the left.
func TestLocalIndex_WeightedNeighbourhoodMatchesMiddleTrade(t *testing.T) {
	rng := rand.New(rand.NewSource(13))
	t0 := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	for trial := 0; trial < 300; trial++ {
		var rows, expanded []canonical.Trade
		var weights []int64
		for i := 0; i < 4+rng.Intn(10); i++ {
			ts := t0.Add(time.Duration(i) * time.Minute)
			w := int64(1 + rng.Intn(4))
			p := int64(1_000_000 + rng.Intn(8001) - 4000)
			r := localTrade("sdex", p*w, ts)
			r.BaseAmount = canonical.NewAmount(big.NewInt(localFixtureBase * w))
			r.TxHash = fmt.Sprintf("row-%d", i)
			rows, weights = append(rows, r), append(weights, w)
			for j := int64(0); j < w; j++ {
				e := localTrade("sdex", p, ts)
				e.TxHash, e.OpIndex = r.TxHash, uint32(j)
				expanded = append(expanded, e)
			}
		}
		wix := weightedLocalIndex(t, rows, weights)
		eix := weightedLocalIndex(t, expanded, nil)
		for k, pos := range wix.order {
			tx, w := rows[pos].TxHash, weights[pos]
			seen := int64(0)
			for ek, epos := range eix.order {
				if expanded[epos].TxHash != tx {
					continue
				}
				if seen == (w-1)/2 {
					wix.lastAnchored, eix.lastAnchored = nil, nil
					got, want := wix.neighbourhoodRef(k), eix.neighbourhoodRef(ek)
					if (got == nil) != (want == nil) || got != nil && (got.centre.Cmp(want.centre) != 0 || got.scale.Cmp(want.scale) != 0) {
						t.Fatalf("trial %d row %s (w=%d): weighted ref %+v, middle trade's ref %+v", trial, tx, w, got, want)
					}
					break
				}
				seen++
			}
		}
	}
}
