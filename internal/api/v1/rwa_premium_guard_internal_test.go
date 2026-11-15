// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func premRefDays(n int) *rwaFeedSeries {
	ref := &rwaFeedSeries{source: "redstone", days: map[time.Time]*big.Rat{}}
	for d := 1; d <= n; d++ {
		ref.days[premDay(d)] = premRat("107/100")
	}
	return ref
}

func premPublished(m rwaPremiumMember, n int) bool {
	_, ok := m.published[premDay(n)]
	return ok
}

// TestRWAPremiumJoin_AnOffMarketDayIsWithheld: a day that clears the substance floor at more than 3x the market's own
// preceding days is refused, as the live price path refuses it; the days around it publish.
func TestRWAPremiumJoin_AnOffMarketDayIsWithheld(t *testing.T) {
	market := []timescale.MarketDay{}
	for d := 1; d <= 6; d++ {
		market = append(market, premGoodDay(d, "1.08"))
	}
	market = append(market, premGoodDay(7, "4.00"), premGoodDay(8, "1.08"))

	got := rwaPremiumJoin(premCand(), premRefDays(8), market, premDefaultFloor())
	if premPublished(got, 7) {
		t.Fatal("day 7 published a premium from a price 3.7x off the six days before it")
	}
	if len(got.withheld) != 1 || !got.withheld[0].Equal(premDay(7)) {
		t.Errorf("withheld = %v, want day 7 counted", got.withheld)
	}
	if !premPublished(got, 6) || !premPublished(got, 8) {
		t.Errorf("published = %v, want the days either side of the refused one", got.published)
	}
}

// TestRWAPremiumJoin_FirstDayIsJudgedAgainstThePublishedValue: with no earlier day, the published value is the centre and
// the band is the wide one, so a real premium publishes and an order-of-magnitude print does not.
func TestRWAPremiumJoin_FirstDayIsJudgedAgainstThePublishedValue(t *testing.T) {
	for _, tc := range []struct {
		vwap      string
		published bool
	}{
		{"2.00", true},
		{"12.00", false},
		{"0.09", false},
	} {
		got := rwaPremiumJoin(premCand(), premRefDays(1), []timescale.MarketDay{premGoodDay(1, tc.vwap)}, premDefaultFloor())
		if premPublished(got, 1) != tc.published {
			t.Errorf("vwap %s: published = %v, want %v", tc.vwap, !tc.published, tc.published)
		}
		if !tc.published && len(got.withheld) != 1 {
			t.Errorf("vwap %s: withheld = %v, want the refused day counted", tc.vwap, got.withheld)
		}
	}
}

// TestRWAPremiumJoin_ALastingRepricingBecomesTheCentre: a market that moves and stays there is published again once
// enough refused days in a row agree with each other, rather than refused forever.
func TestRWAPremiumJoin_ALastingRepricingBecomesTheCentre(t *testing.T) {
	market := []timescale.MarketDay{}
	for d := 1; d <= 5; d++ {
		market = append(market, premGoodDay(d, "1.00"))
	}
	for d := 6; d <= 13; d++ {
		market = append(market, premGoodDay(d, "4.00"))
	}
	got := rwaPremiumJoin(premCand(), premRefDays(13), market, premDefaultFloor())
	if premPublished(got, 6) {
		t.Error("the first day of the move published against five days at a quarter of its price")
	}
	if !premPublished(got, 13) {
		t.Errorf("day 13 still refused after eight days at the new price: withheld = %v", got.withheld)
	}
}

// TestRWAPremiumJoin_ARefusedPrintIsNotTheNextDaysBaseline: a refused day never becomes the centre, so the same print on
// the following days is refused too, for as long as the history is thin. That holds when the first day had no published
// value to be judged against, and after a weekend.
func TestRWAPremiumJoin_ARefusedPrintIsNotTheNextDaysBaseline(t *testing.T) {
	withoutDay := func(n, missing int) *rwaFeedSeries {
		ref := premRefDays(n)
		delete(ref.days, premDay(missing))
		return ref
	}
	for _, vwap := range []string{"12.00", "0.09"} {
		for days := 1; days < rwaPremiumRecentreDays; days++ {
			market := []timescale.MarketDay{}
			for d := 1; d <= days; d++ {
				market = append(market, premGoodDay(d, vwap))
			}
			for name, ref := range map[string]*rwaFeedSeries{
				"every day published":   premRefDays(days),
				"first day unpublished": withoutDay(days, 1),
			} {
				got := rwaPremiumJoin(premCand(), ref, market, premDefaultFloor())
				if len(got.published) != 0 {
					t.Errorf("%d day(s) at %s, %s: published %v, want none", days, vwap, name, got.published)
				}
			}
		}
	}

	// Two honest days, a weekend with no published value, then the print: Friday's value still anchors it.
	market := []timescale.MarketDay{premGoodDay(1, "1.08"), premGoodDay(2, "1.08"), premGoodDay(3, "12.00"), premGoodDay(4, "12.00")}
	got := rwaPremiumJoin(premCand(), withoutDay(4, 3), market, premDefaultFloor())
	if premPublished(got, 3) || premPublished(got, 4) {
		t.Errorf("published = %v, want days 3 and 4 refused", got.published)
	}
}

// TestRWAPremiumJoin_AMixedRefusedRunIsNotARepricing: refused days re-centre only when they agree with each other and
// are at least as many as the accepted days they replace.
func TestRWAPremiumJoin_AMixedRefusedRunIsNotARepricing(t *testing.T) {
	for _, seq := range [][]string{
		{"50", "0.05", "50", "0.05", "50", "50"},
		{"12.00", "12.00", "12.00", "0.09", "0.09", "12.00"},
	} {
		market := []timescale.MarketDay{}
		for d := 1; d <= 6; d++ {
			market = append(market, premGoodDay(d, "1.08"))
		}
		for i, v := range seq {
			market = append(market, premGoodDay(7+i, v))
		}
		got := rwaPremiumJoin(premCand(), premRefDays(12), market, premDefaultFloor())
		for d := 7; d <= 12; d++ {
			if premPublished(got, d) {
				t.Errorf("%v: day %d published", seq, d)
			}
		}
	}
}

// TestRWAPremiumJoin_AThinHistoryCannotRatchetAwayFromThePublishedValue: while the published value anchors the centre,
// each day must sit inside the band around the published value alone, not only around a median the prints have moved.
func TestRWAPremiumJoin_AThinHistoryCannotRatchetAwayFromThePublishedValue(t *testing.T) {
	market := []timescale.MarketDay{}
	for i, v := range []string{"9.00", "40.00", "85.00", "240.00", "600.00"} {
		market = append(market, premGoodDay(1+i, v))
	}
	got := rwaPremiumJoin(premCand(), premRefDays(5), market, premDefaultFloor())
	for d := 2; d <= 5; d++ {
		if premPublished(got, d) {
			t.Errorf("day %d published, more than 10x the published value", d)
		}
	}
}

// TestRWAPremiumJoin_ARepricingSurvivesAPrintAtTheOldLevel: an accepted day in between does not reset the refused run.
func TestRWAPremiumJoin_ARepricingSurvivesAPrintAtTheOldLevel(t *testing.T) {
	market := []timescale.MarketDay{}
	for d := 1; d <= 25; d++ {
		v := "4.00"
		if d <= 5 || d%5 == 0 {
			v = "1.00"
		}
		market = append(market, premGoodDay(d, v))
	}
	got := rwaPremiumJoin(premCand(), premRefDays(25), market, premDefaultFloor())
	if premPublished(got, 6) || !premPublished(got, 24) {
		t.Errorf("published = %v, want day 6 refused and day 24 published", got.published)
	}
}

// TestRWAPremiumJoin_AFatFingerDayDoesNotRefuseTheNextHonestDay: the mirror of the case above.
func TestRWAPremiumJoin_AFatFingerDayDoesNotRefuseTheNextHonestDay(t *testing.T) {
	market := []timescale.MarketDay{premGoodDay(1, "12.00"), premGoodDay(2, "1.08"), premGoodDay(3, "1.09")}
	got := rwaPremiumJoin(premCand(), premRefDays(3), market, premDefaultFloor())
	if premPublished(got, 1) || !premPublished(got, 2) || !premPublished(got, 3) {
		t.Errorf("published = %v, want days 2 and 3 only", got.published)
	}
	if len(got.withheld) != 1 || !got.withheld[0].Equal(premDay(1)) {
		t.Errorf("withheld = %v, want day 1 only", got.withheld)
	}
}

// TestDailyMarketDaysHasOneGuardedCaller: the daily prices_1h VWAP carries no anomaly filter of its own, so a second reader
// would publish it unjudged. A new caller must add its own guard and be named here.
func TestDailyMarketDaysHasOneGuardedCaller(t *testing.T) {
	guarded := map[string]bool{filepath.Join("internal", "api", "v1", "rwa_premium_history.go"): true}
	root := filepath.Join("..", "..", "..")
	callers := 0
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !strings.Contains(string(src), ".DailyMarketDays(") {
				return nil
			}
			callers++
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			if !guarded[rel] {
				t.Errorf("%s calls DailyMarketDays: judge its days with rwaPremiumBaseline.offMarket (or an equal guard) and list it here", rel)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	if callers == 0 {
		t.Fatal("found no DailyMarketDays caller: the scan is not reading the sources")
	}
}

// TestRWAPremiumJoin_AStaleBaselineGivesWayToThePublishedValue: days older than the age bound are no centre for today, so a
// market that comes back at the published value is not refused for being far from what it traded at two months ago.
func TestRWAPremiumJoin_AStaleBaselineGivesWayToThePublishedValue(t *testing.T) {
	market := []timescale.MarketDay{}
	for d := 1; d <= 6; d++ {
		market = append(market, premGoodDay(d, "0.30"))
	}
	market = append(market, premGoodDay(60, "1.08"), premGoodDay(61, "12.00"))

	got := rwaPremiumJoin(premCand(), premRefDays(61), market, premDefaultFloor())
	if !premPublished(got, 60) {
		t.Errorf("day 60 at the published value was refused against days 54+ days old; withheld = %v", got.withheld)
	}
	if premPublished(got, 61) {
		t.Error("day 61 published 11x off the only fresh day before it")
	}
}
