package v1

// Unit tests for the pure decisions behind GET /v1/rwa/premium.
//
// Three things here cannot be reached from the HTTP surface and are
// exactly the three most worth pinning: the ORDER the pre-read gates
// run in (the scam flag before the binding), the numbers the per-day
// thin-market floor is made of, and the join that refuses to carry
// either leg.

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// etherfuseBoundIssuer is the issuer internal/rwa binds USTRY to. Used
// here so the binding lookup would SUCCEED if it were consulted — a
// suppression test against an unbound pair proves nothing.
const etherfuseBoundIssuer = "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"

func premDay(n int) time.Time { return time.Date(2026, 9, n, 0, 0, 0, 0, time.UTC) }

// premDefaultFloor is the pricingguard-default day floor, for tests
// that exercise rwaPremiumJoin's join logic and don't care about the
// configured-vs-default distinction (see TestRWAPremiumDayFloorTracksServingDefaults
// for that).
func premDefaultFloor() pricingguard.SubstancePolicy {
	return (&Server{}).rwaPremiumDayFloorFor()
}

func premRat(s string) *big.Rat {
	v, ok := new(big.Rat).SetString(s)
	if !ok {
		panic("bad rational fixture: " + s)
	}
	return v
}

// ─── the pre-read gates, and their order ────────────────────────────

// TestRWAPremiumCandidates_SuppressesAFlaggedIssuerBeforeReadingTheBinding.
//
// The member here is BOUND — the curated table ties this exact
// (code, issuer) to the USTRY feed — and it carries a scam-class
// directory tag. If the binding were consulted first, an impersonator
// would be handed a real instrument's net asset value and a premium
// measured against it, which is the attacker-authored-pricing class in
// a new coordinate. The order is the defence, so the order is the test.
func TestRWAPremiumCandidates_SuppressesAFlaggedIssuerBeforeReadingTheBinding(t *testing.T) {
	m := rwaMembership{members: []rwaMember{{
		code:    "USTRY",
		issuer:  etherfuseBoundIssuer,
		dirTags: []string{"issuer", "malicious"},
	}}}
	excluded := map[string]int{}
	cands := rwaPremiumCandidates(m, excluded)
	if len(cands) != 0 {
		t.Fatalf("candidates = %+v, want none — a flagged issuer is never comparable", cands)
	}
	if excluded[RWAHistoryExcludedIssuerFlagged] != 1 {
		t.Errorf("excluded = %+v, want one %s", excluded, RWAHistoryExcludedIssuerFlagged)
	}
	if excluded[RWAHistoryExcludedNotBound] != 0 {
		t.Errorf("excluded = %+v — the refusal must be the flag, not a binding miss", excluded)
	}
}

// TestRWAPremiumCandidates_AnUnboundPairIsRefusedAndCounted — the
// commonest refusal. A code alone is not an asset here.
func TestRWAPremiumCandidates_AnUnboundPairIsRefusedAndCounted(t *testing.T) {
	m := rwaMembership{members: []rwaMember{{
		code:    "USTRY",
		issuer:  "GAXSPCTVGFIVYGHT7JLJZV57HCN5KUYDJ6DMPLNWUKL7A5A3HKCNW7JW",
		dirTags: []string{"issuer"},
	}}}
	excluded := map[string]int{}
	if cands := rwaPremiumCandidates(m, excluded); len(cands) != 0 {
		t.Fatalf("candidates = %+v, want none", cands)
	}
	if excluded[RWAHistoryExcludedNotBound] != 1 {
		t.Errorf("excluded = %+v, want one %s", excluded, RWAHistoryExcludedNotBound)
	}
}

// TestRWAPremiumCandidates_ReadsNoSupply — a premium compares two
// prices. The value series needs a Stellar Asset Contract address to
// key the flow log by and refuses a member without one; this surface
// must not inherit that refusal, because it never reads a supply.
func TestRWAPremiumCandidates_ReadsNoSupply(t *testing.T) {
	m := rwaMembership{members: []rwaMember{{
		code:    "USTRY",
		issuer:  etherfuseBoundIssuer,
		dirTags: []string{"issuer"},
	}}}
	cands := rwaPremiumCandidates(m, map[string]int{})
	if len(cands) != 1 {
		t.Fatalf("candidates = %+v, want 1", cands)
	}
	if cands[0].sac != "" {
		t.Errorf("sac = %q — the premium path must not derive one", cands[0].sac)
	}
	if cands[0].feed != "USTRY" {
		t.Errorf("feed = %q, want USTRY", cands[0].feed)
	}
}

// ─── the floor ──────────────────────────────────────────────────────

// TestRWAPremiumDayFloorTracksServingDefaults.
//
// Two of the day floor's three legs are the serving substance gate's
// own numbers and MUST stay them: both windows are 24 hours long, so
// the dollar-volume minimum means the same thing, and the wall-clock
// span leg is grain-independent. If someone lowers the serving floor,
// this fails rather than letting the two drift apart quietly — a
// history that published a claim the live surface refuses is the
// failure this whole gate exists to prevent.
//
// This compares against an operator-configured policy that DIFFERS
// from the pricingguard package defaults — comparing against the
// defaults themselves would pass unconditionally regardless of whether
// [Server.rwaPremiumDayFloorFor] ever reads the configured value at
// all (RLT-041's finding about the prior version of this test).
//
// The bucket leg is deliberately NOT the serving default and is pinned
// separately below, with the reason.
func TestRWAPremiumDayFloorTracksServingDefaults(t *testing.T) {
	configured := pricingguard.SubstancePolicy{
		MinVolumeUSD: new(big.Rat).SetInt64(pricingguard.DefaultSubstanceMinVolumeUSD * 7),
		MinBuckets:   pricingguard.DefaultSubstanceMinBuckets * 3,
		MinSpan:      pricingguard.DefaultSubstanceMinSpan * 2,
		Window:       24 * time.Hour,
	}
	s := &Server{Options: Options{RWAPremiumSubstance: configured}}
	got := s.rwaPremiumDayFloorFor()

	if got.MinVolumeUSD.Cmp(configured.MinVolumeUSD) != 0 {
		t.Errorf("MinVolumeUSD = %s, want the CONFIGURED value %s (not the pricingguard default)",
			got.MinVolumeUSD.FloatString(2), configured.MinVolumeUSD.FloatString(2))
	}
	if got.MinSpan != configured.MinSpan {
		t.Errorf("MinSpan = %v, want the CONFIGURED value %v (not the pricingguard default)",
			got.MinSpan, configured.MinSpan)
	}
	if got.Window != 24*time.Hour {
		t.Errorf("Window = %v, want 24h — a calendar day", got.Window)
	}
	// The serving gate counts MINUTE buckets, of which a day holds
	// 1440; this counts HOUR buckets, of which a day holds 24, because
	// the hour aggregate is the coarsest-reaching grain that still says
	// when inside a past day the trading happened. Inheriting the
	// configured minute count here would demand more hours than a day
	// has and blank every series, so this leg deliberately stays fixed
	// regardless of what MinBuckets the operator configured.
	if got.MinBuckets != 2 {
		t.Errorf("MinBuckets = %d, want 2 hour buckets", got.MinBuckets)
	}
}

// TestRWAPremiumDayFloorFor_FallsBackToDefaultsWhenUnconfigured pins the
// zero-value Server behaviour: a deployment that never set
// RWAPremiumSubstance (or a test double) gets the pricingguard package
// defaults, not a nil *big.Rat panic.
func TestRWAPremiumDayFloorFor_FallsBackToDefaultsWhenUnconfigured(t *testing.T) {
	s := &Server{}
	got := s.rwaPremiumDayFloorFor()
	want := new(big.Rat).SetInt64(pricingguard.DefaultSubstanceMinVolumeUSD)
	if got.MinVolumeUSD.Cmp(want) != 0 {
		t.Errorf("MinVolumeUSD = %s, want the pricingguard default %s", got.MinVolumeUSD.FloatString(2), want.FloatString(2))
	}
	if got.MinSpan != pricingguard.DefaultSubstanceMinSpan {
		t.Errorf("MinSpan = %v, want the pricingguard default %v", got.MinSpan, pricingguard.DefaultSubstanceMinSpan)
	}
}

// ─── the join ───────────────────────────────────────────────────────

func premCand() rwaHistoryCandidate {
	return rwaHistoryCandidate{
		assetID: "USTRY-" + etherfuseBoundIssuer,
		code:    "USTRY",
		issuer:  etherfuseBoundIssuer,
		feed:    "USTRY",
	}
}

func premGoodDay(n int, vwap string) timescale.MarketDay {
	return timescale.MarketDay{
		Day:         premDay(n),
		AssetID:     "USTRY-" + etherfuseBoundIssuer,
		VWAP:        vwap,
		VolumeUSD:   "5000.00",
		Hours:       8,
		SpanSeconds: int64(10 * time.Hour / time.Second),
		Trades:      40,
	}
}

// TestRWAPremiumJoin_PublishesOnlyDaysBothLegsWereObserved is the
// endpoint in one function. Day 1 has both legs; day 2 has a market and
// no published value; day 3 has a published value and no market.
// Exactly one point may come out, and the two absences must be
// distinguishable afterwards.
func TestRWAPremiumJoin_PublishesOnlyDaysBothLegsWereObserved(t *testing.T) {
	ref := &rwaFeedSeries{source: "redstone", days: map[time.Time]*big.Rat{
		premDay(1): premRat("107/100"),
		premDay(3): premRat("107/100"),
	}}
	got := rwaPremiumJoin(premCand(), ref, []timescale.MarketDay{
		premGoodDay(1, "1.08"),
		premGoodDay(2, "1.08"),
	}, premDefaultFloor())
	if len(got.days) != 1 {
		t.Fatalf("days = %+v, want exactly the one day both legs answered", got.days)
	}
	if !got.days[0].day.Equal(premDay(1)) {
		t.Errorf("day = %v, want %v", got.days[0].day, premDay(1))
	}
	if len(got.refOnly) != 1 || !got.refOnly[0].Equal(premDay(3)) {
		t.Errorf("refOnly = %v, want [%v] — the untraded day must be countable", got.refOnly, premDay(3))
	}
	if len(got.withheld) != 0 {
		t.Errorf("withheld = %v — day 2 had no NAV, it was not refused for thinness", got.withheld)
	}
}

// TestRWAPremiumJoin_AThinDayIsWithheldNotPriced — and it is withheld
// BEFORE the NAV is looked at, so a thin day is reported as thin even
// on a day the oracle also happened to be silent.
func TestRWAPremiumJoin_AThinDayIsWithheldNotPriced(t *testing.T) {
	ref := &rwaFeedSeries{source: "redstone", days: map[time.Time]*big.Rat{
		premDay(1): premRat("107/100"),
	}}
	thin := premGoodDay(1, "9.99")
	thin.VolumeUSD, thin.Hours, thin.SpanSeconds = "8.57", 1, 120
	got := rwaPremiumJoin(premCand(), ref, []timescale.MarketDay{thin}, premDefaultFloor())
	if len(got.days) != 0 {
		t.Fatalf("days = %+v, want none — a dust day may not carry a price claim", got.days)
	}
	if len(got.withheld) != 1 {
		t.Errorf("withheld = %v, want the thin day counted", got.withheld)
	}
}

// TestRWAPremiumJoin_CountsNoUntradedDaysBeforeTheFirstObservedMarket.
//
// The oracle has priced this instrument since day 1; the token's first
// observed market day is day 4. Days 1–3 are NOT reported as days it
// did not trade, because from here "did not trade", "did not exist
// yet" and "the hour aggregate is not materialised that far back" are
// one thing — and migrations 0115/0147 dropped the price aggregates
// and left re-materialisation to the operator, so the third is a live
// possibility rather than a hypothetical.
func TestRWAPremiumJoin_CountsNoUntradedDaysBeforeTheFirstObservedMarket(t *testing.T) {
	ref := &rwaFeedSeries{source: "redstone", days: map[time.Time]*big.Rat{
		premDay(1): premRat("107/100"),
		premDay(2): premRat("107/100"),
		premDay(3): premRat("107/100"),
		premDay(4): premRat("107/100"),
		premDay(5): premRat("107/100"),
	}}
	got := rwaPremiumJoin(premCand(), ref, []timescale.MarketDay{
		premGoodDay(4, "1.08"),
	}, premDefaultFloor())
	if len(got.days) != 1 {
		t.Fatalf("days = %+v, want 1", got.days)
	}
	// Only day 5 — after the first observation, and silent.
	if len(got.refOnly) != 1 || !got.refOnly[0].Equal(premDay(5)) {
		t.Errorf("refOnly = %v, want [%v] — days before the first observed "+
			"market may not be reported as days the token did not trade", got.refOnly, premDay(5))
	}
}

// TestRWAPremiumJoin_NoMarketDaysCountsNothing — a member with no
// observed market at all is reported by the caller as
// no_market_history, and must not additionally accrue a tally of days
// it "did not trade" spanning the oracle's whole publication history.
func TestRWAPremiumJoin_NoMarketDaysCountsNothing(t *testing.T) {
	ref := &rwaFeedSeries{source: "redstone", days: map[time.Time]*big.Rat{
		premDay(1): premRat("107/100"),
		premDay(2): premRat("107/100"),
	}}
	got := rwaPremiumJoin(premCand(), ref, nil, premDefaultFloor())
	if len(got.days) != 0 || len(got.refOnly) != 0 || len(got.withheld) != 0 {
		t.Errorf("got days=%v refOnly=%v withheld=%v, want all empty",
			got.days, got.refOnly, got.withheld)
	}
}

// TestRWAPremiumJoin_AWithheldDayIsNotAlsoCountedAsUntraded — a day the
// market existed and was refused, and a day nobody traded, are opposite
// findings. Counting one day as both would let a reader add the two
// tallies and get more days than the window holds.
func TestRWAPremiumJoin_AWithheldDayIsNotAlsoCountedAsUntraded(t *testing.T) {
	ref := &rwaFeedSeries{source: "redstone", days: map[time.Time]*big.Rat{
		premDay(1): premRat("107/100"),
		premDay(2): premRat("107/100"),
	}}
	thin := premGoodDay(2, "9.99")
	thin.VolumeUSD, thin.Hours, thin.SpanSeconds = "8.57", 1, 120
	got := rwaPremiumJoin(premCand(), ref, []timescale.MarketDay{
		premGoodDay(1, "1.08"),
		thin,
	}, premDefaultFloor())
	if len(got.days) != 1 {
		t.Fatalf("days = %+v, want 1", got.days)
	}
	if len(got.withheld) != 1 || !got.withheld[0].Equal(premDay(2)) {
		t.Fatalf("withheld = %v, want [%v]", got.withheld, premDay(2))
	}
	if len(got.refOnly) != 0 {
		t.Errorf("refOnly = %v — the withheld day must not be counted twice", got.refOnly)
	}
}

// TestRWAPremiumJoin_AnUnparseableVolumeFailsClosed — the floor is what
// licenses the price claim, so a volume that cannot be checked against
// it is treated as zero, exactly as pricingguard.SubstanceOK does.
func TestRWAPremiumJoin_AnUnparseableVolumeFailsClosed(t *testing.T) {
	ref := &rwaFeedSeries{source: "redstone", days: map[time.Time]*big.Rat{
		premDay(1): premRat("107/100"),
	}}
	bad := premGoodDay(1, "1.08")
	bad.VolumeUSD = "not-a-number"
	got := rwaPremiumJoin(premCand(), ref, []timescale.MarketDay{bad}, premDefaultFloor())
	if len(got.days) != 0 {
		t.Fatalf("days = %+v, want none — an unverifiable volume must fail closed", got.days)
	}
}

// TestRWAPremiumJoin_SignAndScale — the sign is the whole point of the
// measurement, and the scale matches `premium.pct` on /v1/rwa/assets so
// the two surfaces cannot round one quantity two ways.
func TestRWAPremiumJoin_SignAndScale(t *testing.T) {
	for _, tc := range []struct {
		name     string
		market   string
		ref      string
		wantPct  string
		wantSign int
	}{
		{"premium", "1.08", "107/100", "0.9346", +1},
		{"discount", "1.06", "107/100", "-0.9346", -1},
		{"at par", "1.07", "107/100", "0.0000", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := &rwaFeedSeries{source: "redstone", days: map[time.Time]*big.Rat{
				premDay(1): premRat(tc.ref),
			}}
			got := rwaPremiumJoin(premCand(), ref, []timescale.MarketDay{premGoodDay(1, tc.market)}, premDefaultFloor())
			if len(got.days) != 1 {
				t.Fatalf("days = %+v, want 1", got.days)
			}
			if got.days[0].pct != tc.wantPct {
				t.Errorf("pct = %q, want %q", got.days[0].pct, tc.wantPct)
			}
			v := premRat(got.days[0].pct)
			if v.Sign() != tc.wantSign {
				t.Errorf("sign = %d, want %d", v.Sign(), tc.wantSign)
			}
		})
	}
}

// TestRWAPremiumCoverage_ClosesAgainstTheWholeSet — measured plus
// unmeasured is the size of the sector, never the size of what was
// readable.
func TestRWAPremiumCoverage_ClosesAgainstTheWholeSet(t *testing.T) {
	series := []RWAPremiumSeries{{
		AssetID: "A",
		Points: []RWAPremiumHistoryPoint{
			{T: WireTime(premDay(1))},
			{T: WireTime(premDay(2))},
		},
	}, {
		AssetID: "B",
		Points:  []RWAPremiumHistoryPoint{{T: WireTime(premDay(2))}},
	}}
	cov := rwaPremiumCoverage(series, 11)
	if len(cov) != 2 {
		t.Fatalf("coverage = %+v, want 2 days", cov)
	}
	if cov[0].AssetsMeasured != 1 || cov[0].AssetsUnmeasured != 10 {
		t.Errorf("day 1 = %d/%d, want 1/10", cov[0].AssetsMeasured, cov[0].AssetsUnmeasured)
	}
	if cov[1].AssetsMeasured != 2 || cov[1].AssetsUnmeasured != 9 {
		t.Errorf("day 2 = %d/%d, want 2/9", cov[1].AssetsMeasured, cov[1].AssetsUnmeasured)
	}
	for _, c := range cov {
		if c.AssetsMeasured+c.AssetsUnmeasured != 11 {
			t.Errorf("coverage does not close: %+v", c)
		}
	}
}

// TestRWAPremiumSeriesRows_OrdersByAbsoluteDispersion — a 4% discount
// and a 4% premium are the same size of finding, and a signed sort
// would bury one of them at the bottom of the legend.
func TestRWAPremiumSeriesRows_OrdersByAbsoluteDispersion(t *testing.T) {
	members := []rwaPremiumMember{
		{assetID: "SMALL", feed: "A", days: []rwaPremiumDay{{day: premDay(1), pct: "0.5000"}}},
		{assetID: "BIGDISCOUNT", feed: "B", days: []rwaPremiumDay{{day: premDay(1), pct: "-4.0000"}}},
		{assetID: "BIGPREMIUM", feed: "C", days: []rwaPremiumDay{{day: premDay(1), pct: "3.0000"}}},
	}
	got, truncated := rwaPremiumSeriesRows(members, time.Time{})
	if truncated {
		t.Error("truncated = true on three points")
	}
	want := []string{"BIGDISCOUNT", "BIGPREMIUM", "SMALL"}
	for i, w := range want {
		if got[i].AssetID != w {
			t.Fatalf("order = %v, want %v", []string{got[0].AssetID, got[1].AssetID, got[2].AssetID}, want)
		}
	}
}

// TestCachedRWAPremiumHistory_RecoversFromAPanicInsteadOfWedgingTheFlight.
//
// A bound candidate with s.OracleHistory left nil drives buildRWAPremiumHistory
// straight into a nil-interface panic in rwaPremiumReferenceDays. Before the
// recover was added, that panic escaped cachedRWAPremiumHistory with
// s.rwaPremFlight still pointing at a channel nobody would ever close — every
// later caller queued on it and /v1/rwa/premium never served again for the
// life of the process (RLT-098, same class as GH-587). This proves the
// single-flight gate clears and a following call is served rather than
// hanging.
func TestCachedRWAPremiumHistory_RecoversFromAPanicInsteadOfWedgingTheFlight(t *testing.T) {
	s := &Server{
		rwaCache: &rwaMembership{
			available: true,
			members: []rwaMember{{
				code:   "USTRY",
				issuer: etherfuseBoundIssuer,
			}},
		},
		rwaAt: time.Now(),
		// oracleHistory left nil: buildRWAPremiumHistory calls
		// s.OracleHistory.DailyOraclePrices on it once the bound candidate
		// clears the membership/scam gates, panicking on the nil interface.
	}

	first := s.cachedRWAPremiumHistory(context.Background())
	if first.available {
		t.Fatalf("first call: available = true on a nil oracleHistory, want unavailable")
	}

	s.rwaPremMu.Lock()
	flight := s.rwaPremFlight
	s.rwaPremMu.Unlock()
	if flight != nil {
		t.Fatalf("rwaPremFlight left set after a panicking build — every future caller would block on it forever")
	}

	done := make(chan rwaPremiumHistory, 1)
	go func() { done <- s.cachedRWAPremiumHistory(context.Background()) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("second call did not return — the single-flight gate is wedged")
	}
}
