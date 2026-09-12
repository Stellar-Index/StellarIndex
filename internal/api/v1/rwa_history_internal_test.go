package v1

// Unit tests for the pure arithmetic behind GET /v1/rwa/history.
//
// The endpoint's whole claim is that it never invents a number, so the
// tests that matter are the ones that pin the ABSENCES: a day the oracle
// was silent, a flow log that cannot be trusted, a token that did not
// exist yet. Each of those has to produce nothing rather than something
// plausible.

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func histDay(n int) time.Time {
	return time.Date(2026, 9, n, 0, 0, 0, 0, time.UTC)
}

func flow(contract string, n int, net string) clickhouse.SupplyFlowDay {
	v, _ := new(big.Int).SetString(net, 10)
	return clickhouse.SupplyFlowDay{ContractID: contract, Day: histDay(n), Net: v, Flows: 1}
}

// TestRWACumulateSupply_RunsTheLogForward — a supply level is the
// running total of every flow to date, and a day with no flow inherits
// the level rather than dropping out. That carry is the one this surface
// permits, and it is permitted because the log is complete.
func TestRWACumulateSupply_RunsTheLogForward(t *testing.T) {
	levels, ok := rwaCumulateSupply([]clickhouse.SupplyFlowDay{
		flow("C", 1, "1000"),
		flow("C", 3, "500"),
		flow("C", 5, "-200"),
	})
	if !ok {
		t.Fatal("complete = false on a log that never goes negative")
	}
	want := []string{"1000", "1500", "1300"}
	if len(levels) != len(want) {
		t.Fatalf("levels = %d, want %d", len(levels), len(want))
	}
	for i, w := range want {
		if levels[i].level.String() != w {
			t.Errorf("level[%d] = %s, want %s", i, levels[i].level, w)
		}
	}
	if !levels[1].from.Equal(histDay(3)) {
		t.Errorf("level[1].from = %v, want %v", levels[1].from, histDay(3))
	}
}

// TestRWACumulateSupply_RefusesANegativeExcursion — a running total
// below zero is proof the contract's flows are incompletely seeded, not
// evidence of a negative supply. The WHOLE series is refused, not the
// offending days: a missing mint understates every level after it.
func TestRWACumulateSupply_RefusesANegativeExcursion(t *testing.T) {
	levels, ok := rwaCumulateSupply([]clickhouse.SupplyFlowDay{
		flow("C", 1, "100"),
		flow("C", 2, "-500"), // burns exceed everything ever minted
		flow("C", 3, "9000"), // and a later mint must not rescue it
	})
	if ok {
		t.Fatalf("complete = true on a log that goes negative; levels = %v", levels)
	}
	if levels != nil {
		t.Errorf("levels = %v, want nil — a refused series publishes nothing", levels)
	}
}

// TestRWAHistoryMemberDays_SilentOracleDayIsAGap — the price leg drives
// the series. A day the oracle did not publish produces NO point, and
// emphatically not the previous day's value carried forward.
func TestRWAHistoryMemberDays_SilentOracleDayIsAGap(t *testing.T) {
	levels, _ := rwaCumulateSupply([]clickhouse.SupplyFlowDay{flow("C", 1, "20000000")})
	prices := map[time.Time]*big.Rat{
		histDay(1): big.NewRat(1, 1),
		// day 2: the oracle was silent.
		histDay(3): big.NewRat(2, 1),
	}
	got := rwaHistoryMemberDays(levels, prices, 7)
	if len(got) != 2 {
		t.Fatalf("days = %d, want 2 — the silent day must not be filled", len(got))
	}
	if !got[0].day.Equal(histDay(1)) || !got[1].day.Equal(histDay(3)) {
		t.Fatalf("days = %v, %v — want the 1st and the 3rd", got[0].day, got[1].day)
	}
	// 20000000 / 10^7 = 2 whole tokens.
	if got[0].valueUSD != "2.00" || got[1].valueUSD != "4.00" {
		t.Errorf("values = %q, %q; want 2.00 and 4.00", got[0].valueUSD, got[1].valueUSD)
	}
}

// TestRWAHistoryMemberDays_SupplyCarriesAcrossAQuietDay — the mirror of
// the test above, and the asymmetry the surface rests on: the supply leg
// IS carried across a day with no flow, because the flow log records
// every event that could have moved it.
func TestRWAHistoryMemberDays_SupplyCarriesAcrossAQuietDay(t *testing.T) {
	levels, _ := rwaCumulateSupply([]clickhouse.SupplyFlowDay{flow("C", 1, "10000000")})
	prices := map[time.Time]*big.Rat{histDay(1): big.NewRat(3, 1), histDay(9): big.NewRat(3, 1)}
	got := rwaHistoryMemberDays(levels, prices, 7)
	if len(got) != 2 {
		t.Fatalf("days = %d, want 2", len(got))
	}
	if got[1].valueUSD != "3.00" {
		t.Errorf("value on the quiet day = %q, want 3.00 — the level did not change", got[1].valueUSD)
	}
}

// TestRWAHistoryMemberDays_NothingBeforeTheFirstMint — a price day
// preceding the token's first flow yields nothing. A supply of zero
// times a real net asset value is a perfectly computable dollar figure
// that means nothing at all.
func TestRWAHistoryMemberDays_NothingBeforeTheFirstMint(t *testing.T) {
	levels, _ := rwaCumulateSupply([]clickhouse.SupplyFlowDay{flow("C", 5, "10000000")})
	prices := map[time.Time]*big.Rat{
		histDay(1): big.NewRat(1, 1),
		histDay(2): big.NewRat(1, 1),
		histDay(5): big.NewRat(1, 1),
	}
	got := rwaHistoryMemberDays(levels, prices, 7)
	if len(got) != 1 || !got[0].day.Equal(histDay(5)) {
		t.Fatalf("days = %v, want only the 5th", got)
	}
}

// TestRWAHistoryTotal_DayNobodyCouldBeValuedIsAbsent — the series has
// holes, never zeros. A reader cannot tell a zero that means "worthless"
// from a zero that means "we could not see".
func TestRWAHistoryTotal_DayNobodyCouldBeValuedIsAbsent(t *testing.T) {
	members := []rwaHistoryMember{
		{days: []rwaHistoryDay{{day: histDay(1), valueUSD: "10.00"}, {day: histDay(3), valueUSD: "12.00"}}},
	}
	pts, _ := rwaHistoryTotal(members, 1, time.Time{})
	if len(pts) != 2 {
		t.Fatalf("points = %d, want 2", len(pts))
	}
	for _, p := range pts {
		if p.ValueUSD == "0.00" {
			t.Fatalf("a zero reached the wire at %v", p.T.Time())
		}
	}
	if !pts[1].T.Time().Equal(histDay(3)) {
		t.Errorf("second point = %v, want the 3rd — the 2nd must be skipped, not zeroed", pts[1].T.Time())
	}
}

// TestRWAHistoryTotal_CountsAgainstTheWholeSet — a point's coverage is
// stated against the size of the SET, not against the members that
// happened to be readable. Otherwise every point would claim full
// coverage of whatever it managed to sum.
func TestRWAHistoryTotal_CountsAgainstTheWholeSet(t *testing.T) {
	members := []rwaHistoryMember{
		{days: []rwaHistoryDay{{day: histDay(1), valueUSD: "10.00"}}},
		{days: []rwaHistoryDay{{day: histDay(1), valueUSD: "5.00"}, {day: histDay(2), valueUSD: "6.00"}}},
	}
	pts, _ := rwaHistoryTotal(members, 11, time.Time{})
	if len(pts) != 2 {
		t.Fatalf("points = %d, want 2", len(pts))
	}
	if pts[0].ValueUSD != "15.00" {
		t.Errorf("day 1 = %q, want 15.00 — the sum of what was visible", pts[0].ValueUSD)
	}
	if pts[0].AssetsValued != 2 || pts[0].AssetsUnvalued != 9 {
		t.Errorf("day 1 split = %d/%d, want 2/9 against a set of 11",
			pts[0].AssetsValued, pts[0].AssetsUnvalued)
	}
	if !pts[0].LowerBound || !pts[1].LowerBound {
		t.Error("lower_bound false while members are unvalued")
	}
	if pts[1].AssetsValued != 1 {
		t.Errorf("day 2 valued = %d, want 1 — one member dropped out", pts[1].AssetsValued)
	}
}

// TestRWAHistoryTotal_NoUnvaluedMeansNoLowerBound pins the other side of
// the flag, so a series that really does cover the whole set is not
// mislabelled as a floor.
func TestRWAHistoryTotal_NoUnvaluedMeansNoLowerBound(t *testing.T) {
	members := []rwaHistoryMember{{days: []rwaHistoryDay{{day: histDay(1), valueUSD: "10.00"}}}}
	pts, _ := rwaHistoryTotal(members, 1, time.Time{})
	if len(pts) != 1 || pts[0].LowerBound {
		t.Fatalf("points = %+v, want one point with lower_bound false", pts)
	}
}

func oracleDay(source, feed string, n int, price int64, decimals uint8) timescale.OracleDayPoint {
	return timescale.OracleDayPoint{
		Bucket:       histDay(n),
		Source:       source,
		Asset:        canonical.Asset{Type: canonical.AssetRWA, Code: feed},
		Price:        big.NewInt(price),
		Decimals:     decimals,
		Observations: 1,
	}
}

// TestRWAReduceFeedSeries_PicksOneSourceForTheWholeWindow — two oracles
// pricing one instrument must not alternate down the line, because a
// switch of publisher would render as a price move. The pick is made
// once, on coverage, and is stable.
func TestRWAReduceFeedSeries_PicksOneSourceForTheWholeWindow(t *testing.T) {
	got := rwaReduceFeedSeries([]timescale.OracleDayPoint{
		oracleDay("redstone", "USDY", 1, 100000000, 8),
		oracleDay("redstone", "USDY", 2, 101000000, 8),
		oracleDay("redstone", "USDY", 3, 102000000, 8),
		oracleDay("band", "USDY", 2, 900000000, 8), // one day only
	})
	s := got["USDY"]
	if s == nil {
		t.Fatal("no series for USDY")
	}
	if s.source != "redstone" {
		t.Errorf("source = %q, want redstone — the publisher with the most day buckets", s.source)
	}
	if len(s.days) != 3 {
		t.Errorf("days = %d, want 3 — the losing source's day must not be merged in", len(s.days))
	}
	if got := s.days[histDay(2)].FloatString(2); got != "1.01" {
		t.Errorf("day 2 = %s, want 1.01 — band's 9.00 must not appear", got)
	}
}

// TestRWAReduceFeedSeries_RefusesNonOracleAndNonPositive — R-C from
// rwa_reference.go (an aggregator writing into the same table for
// divergence comparison is not an independent valuation) and the
// non-positive refusal, both carried into the history unchanged.
func TestRWAReduceFeedSeries_RefusesNonOracleAndNonPositive(t *testing.T) {
	got := rwaReduceFeedSeries([]timescale.OracleDayPoint{
		oracleDay("coingecko", "USDY", 1, 100000000, 8), // aggregator, not an oracle
		oracleDay("redstone", "USDY", 2, 0, 8),          // not a valuation of zero: bad data
		oracleDay("redstone", "USDY", 3, -5, 8),
	})
	if len(got) != 0 {
		t.Fatalf("series = %+v, want none", got)
	}
}

// TestRWAReduceFeedSeries_TieBreaksOnSourceName keeps the pick from
// depending on map iteration order when two publishers cover the same
// number of days.
func TestRWAReduceFeedSeries_TieBreaksOnSourceName(t *testing.T) {
	rows := []timescale.OracleDayPoint{
		oracleDay("redstone", "GILTS", 1, 100000000, 8),
		oracleDay("band", "GILTS", 1, 200000000, 8),
	}
	for i := 0; i < 50; i++ {
		if s := rwaReduceFeedSeries(rows)["GILTS"]; s.source != "band" {
			t.Fatalf("source = %q on run %d, want band every time", s.source, i)
		}
	}
}

// TestRWAHistoryCandidates_ScamFlagOutranksTheBinding.
//
// Requirement 3 already refuses a scam-flagged issuer at membership
// time, so this gate is defence in depth against the ONE way a flagged
// issuer reaches a valuation path: it acquires the tag after the set was
// built. The suppression has to run BEFORE the binding lookup, because a
// bound member is exactly the case where a real instrument's net asset
// value is available to be published against an impersonator.
func TestRWAHistoryCandidates_ScamFlagOutranksTheBinding(t *testing.T) {
	const etherfuse = "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
	excluded := map[string]int{}
	got := rwaHistoryCandidates(rwaMembership{members: []rwaMember{
		// USTRY from this issuer IS bound in internal/rwa — so if the
		// flag did not outrank the binding, this row would be valued.
		{code: "USTRY", issuer: etherfuse, dirTags: []string{"issuer", "malicious"}},
	}}, excluded)
	if len(got) != 0 {
		t.Fatalf("candidates = %+v, want none — the flag must suppress a bound member", got)
	}
	if excluded[RWAHistoryExcludedIssuerFlagged] != 1 {
		t.Errorf("excluded = %v, want issuer_flagged×1", excluded)
	}
}

// TestRWAHistoryCandidates_BindsOnThePairAndDerivesTheSAC pins the
// positive path of the same function: identity is (code, issuer), and
// the flow log is keyed by the address derived FROM that identity rather
// than by anything a fixture could assert independently.
func TestRWAHistoryCandidates_BindsOnThePairAndDerivesTheSAC(t *testing.T) {
	const etherfuse = "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
	const impostor = "GAXSPCTVGFIVYGHT7JLJZV57HCN5KUYDJ6DMPLNWUKL7A5A3HKCNW7JW"
	excluded := map[string]int{}
	got := rwaHistoryCandidates(rwaMembership{members: []rwaMember{
		{code: "USTRY", issuer: etherfuse, name: "Etherfuse USTRY", dirName: "Etherfuse"},
		// The same CODE from another account. Nothing binds it, and
		// nothing may hand it the real instrument's valuation.
		{code: "USTRY", issuer: impostor},
	}}, excluded)
	if len(got) != 1 {
		t.Fatalf("candidates = %d, want exactly the bound pair", len(got))
	}
	if got[0].issuer != etherfuse || got[0].feed != "USTRY" {
		t.Errorf("candidate = %s/%s", got[0].issuer, got[0].feed)
	}
	if got[0].decimals != 7 {
		t.Errorf("decimals = %d, want 7 — every classic asset", got[0].decimals)
	}
	want, err := canonical.Asset{Type: canonical.AssetClassic, Code: "USTRY", Issuer: etherfuse}.SacContractID()
	if err != nil {
		t.Fatalf("SacContractID: %v", err)
	}
	if got[0].sac != want {
		t.Errorf("sac = %q, want the derived %q", got[0].sac, want)
	}
	if excluded[RWAHistoryExcludedNotBound] != 1 {
		t.Errorf("excluded = %v, want not_bound×1 for the impostor", excluded)
	}
}

// TestRWAHistoryWindowStart_AllIsOpenEnded — `all` must not become a
// silently-bounded window, which is how a series quietly loses its
// early history.
func TestRWAHistoryWindowStart_AllIsOpenEnded(t *testing.T) {
	if got := rwaHistoryWindowStart("all", histDay(30)); !got.IsZero() {
		t.Errorf("all = %v, want the zero time", got)
	}
	if got := rwaHistoryWindowStart("1w", histDay(30)); !got.Equal(histDay(23)) {
		t.Errorf("1w from the 30th = %v, want the 23rd", got)
	}
}
