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
