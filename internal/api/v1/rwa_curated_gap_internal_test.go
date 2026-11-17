package v1

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestRWACuratedGapIsSameDay pins gap_vs_verified_usd to the verified total ON the curator's month-end: never today's total,
// never a lower-bound day.
func TestRWACuratedGapIsSameDay(t *testing.T) {
	monthEnd := time.Date(2025, 8, 31, 0, 0, 0, 0, time.UTC)
	member := func(days ...rwaHistoryDay) rwaHistoryMember { return rwaHistoryMember{assetID: "A", days: days} }
	exact := rwaValueHistory{available: true, setAssets: 1, members: []rwaHistoryMember{member(
		rwaHistoryDay{day: monthEnd.AddDate(0, 0, -1), valueUSD: "2900000000.00"},
		rwaHistoryDay{day: monthEnd, valueUSD: "3000000000.00"},
		rwaHistoryDay{day: monthEnd.AddDate(0, 2, 0), valueUSD: "5000000000.00"},
	)}}
	floor := exact
	floor.setAssets = 2 // a second member is unvalued that day
	incomplete := exact
	incomplete.setIncomplete = true
	noDay := rwaValueHistory{available: true, setAssets: 1, members: []rwaHistoryMember{member(
		rwaHistoryDay{day: monthEnd.AddDate(0, 0, 1), valueUSD: "3000000000.00"},
	)}}

	for name, tc := range map[string]struct {
		hist rwaValueHistory
		want string
	}{
		"same-day exact total":       {exact, "1004795860.00"},
		"month-end is a lower bound": {floor, ""},
		"set incomplete":             {incomplete, ""},
		"no point on the month-end":  {noDay, ""},
		"history never built":        {rwaValueHistory{}, ""},
	} {
		hist := tc.hist
		executed := monthEnd.AddDate(0, 0, 5)
		pub := &timescale.CuratedRWAPublished{MonthEnd: monthEnd, TotalUSD: "4004795860", ExecutedAt: executed, SourceQuery: 1}
		p := rwaCuratedPublishedBlock(pub, func(d time.Time) *big.Rat { return rwaVerifiedTotalOn(hist, d) }, executed.Add(time.Hour))
		if p == nil {
			t.Fatalf("%s: published block withheld", name)
		}
		got := ""
		if p.GapVsVerifiedUSD != nil {
			got = *p.GapVsVerifiedUSD
		}
		if got != tc.want {
			t.Errorf("%s: gap_vs_verified_usd = %q, want %q", name, got, tc.want)
		}
	}
}
