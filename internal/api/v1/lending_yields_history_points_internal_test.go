package v1

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

var yhpD0 = time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)

func yhpState(ledger uint32, at time.Time) *clickhouse.LendingRateState {
	return &clickhouse.LendingRateState{Ledger: ledger, CloseTime: at, LastTime: at}
}

// TestBucketRateStates_PriorIsJudgedPerDayAgainstTheWholeRead: a mid-range day with one state and no base names the state
// before its slack as base_state_before_window, never no_base_state_in_window, whichever earlier day holds that state,
// including the series' first day whose first state did not decode.
func TestBucketRateStates_PriorIsJudgedPerDayAgainstTheWholeRead(t *testing.T) {
	singleOn := func(d time.Time) clickhouse.LendingRateDay {
		return clickhouse.LendingRateDay{Day: d, First: yhpState(50, d.Add(9*time.Hour)), Last: yhpState(50, d.Add(9*time.Hour))}
	}
	d1, d5 := yhpD0.AddDate(0, 0, 1), yhpD0.AddDate(0, 0, 5)
	cases := []struct {
		name  string
		first clickhouse.LendingRateDay
		d     time.Time
	}{
		{"decoded first state", clickhouse.LendingRateDay{Day: yhpD0, First: yhpState(10, yhpD0.Add(time.Hour)), Last: yhpState(11, yhpD0.Add(2*time.Hour))}, d5},
		// the next day: its slack opens at 23:00, so only the decoded last state proves an earlier one
		{"first state undecoded", clickhouse.LendingRateDay{Day: yhpD0, FirstUndecodable: true, Last: yhpState(11, yhpD0.Add(2*time.Hour))}, d1},
		{"no state decoded at all", clickhouse.LendingRateDay{Day: yhpD0, FirstUndecodable: true, LastUndecodable: true}, d5},
	}
	for _, tc := range cases {
		single := singleOn(tc.d)
		days := clickhouse.LendingRateDays{Days: []clickhouse.LendingRateDay{tc.first, single}}
		st, ok := bucketRateStates(days, map[time.Time]clickhouse.LendingRateDay{tc.first.Day: tc.first, tc.d: single}, tc.d)
		if _, _, reason := realisedStatePair(st, ok, tc.d, day24); reason != yieldReasonOldBase {
			t.Errorf("%s: reason %q, want %q", tc.name, reason, yieldReasonOldBase)
		}
	}
	single := singleOn(d5)
	alone := clickhouse.LendingRateDays{Days: []clickhouse.LendingRateDay{single}}
	st, ok := bucketRateStates(alone, map[time.Time]clickhouse.LendingRateDay{d5: single}, d5)
	if _, _, reason := realisedStatePair(st, ok, d5, day24); reason != yieldReasonNoBase {
		t.Errorf("first state ever: reason %q, want %q", reason, yieldReasonNoBase)
	}
}

// TestYieldHistoryEmissions_StoredZeroOverAnUnvaluedSideIsNoRate: a stored day with no row is zero tokens and $0, but its
// APR is a rate only over a valued side; over an empty, dust or unpriced side it is null with the side's reason.
func TestYieldHistoryEmissions_StoredZeroOverAnUnvaluedSideIsNoRate(t *testing.T) {
	e := yieldHistoryEmissions{
		cov:     timescale.LendingEmissionsUSDCoverage{FloorDay: yhpD0, ThroughDay: yhpD0.AddDate(0, 0, 3)},
		rows:    map[time.Time][]timescale.LendingEmissionsUSDDay{},
		history: historyVerified,
	}
	for _, why := range []string{emissionAPRSideEmpty, emissionAPRSideDust, emissionAPRSideUnpriced} {
		var p LendingYieldHistoryPoint
		e.point(&p, "CASSET", clickhouse.EmissionSideSupply, yhpD0, yieldSideUSD{reason: why})
		if !ptrIs(p.EmissionsTokens, "0") || !ptrIs(p.EmissionsUSD, "0.00") || p.EmissionsAPR != nil || !ptrIs(p.EmissionsAPRReason, why) {
			t.Errorf("%s side: tokens %v usd %v apr %v (%v)", why, p.EmissionsTokens, p.EmissionsUSD, p.EmissionsAPR, p.EmissionsAPRReason)
		}
	}
}
