package v1_test

// GET /v1/rwa/premium — the token's price against the instrument's net
// asset value, over time.
//
// The point of the surface is the ratio; the point of these tests is
// everything the ratio is NOT allowed to do. A premium is built from two
// sampled observations, so unlike its sibling /v1/rwa/history it may not
// carry either leg across a day it was not observed on — and because it
// re-derives a market price rather than reading the served one, it must
// hold that price to a thin-market floor or it becomes a way to publish,
// on a past day, exactly the claim the live surface refuses.
//
// Production wiring: v1.New with Options.OracleHistory = *timescale.Store
// and Options.MarketHistory = *timescale.Store (see
// cmd/stellarindex-api/main.go). The tests below use the SAME
// constructor — there is no test-only server builder — and substitute
// only those two readers.

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ─── stubs ──────────────────────────────────────────────────────────

// stubMarketHistory serves canned market days. It records the base
// assets and the quote spellings it was asked for, so a test can prove
// the handler joined on the exact (code, issuer) and read the dollar
// against every spelling of the dollar.
type stubMarketHistory struct {
	rows        []timescale.MarketDay
	err         error
	askedAssets []string
	askedQuotes []string
}

func (s *stubMarketHistory) DailyMarketDays(
	_ context.Context, assets, quotes []canonical.Asset, _, _ time.Time,
) ([]timescale.MarketDay, error) {
	for _, a := range assets {
		s.askedAssets = append(s.askedAssets, a.String())
	}
	for _, q := range quotes {
		s.askedQuotes = append(s.askedQuotes, q.String())
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.rows, nil
}

// ─── fixture helpers ────────────────────────────────────────────────

// premMarket is a day that comfortably CLEARS the thin-market floor:
// well over the dollar-volume minimum, spread across most of the day.
// A test that wants a thin day says so explicitly with premThinMarket.
func premMarket(assetID string, n int, vwap string) timescale.MarketDay {
	return timescale.MarketDay{
		Day:         histTime(n),
		AssetID:     assetID,
		VWAP:        vwap,
		VolumeUSD:   "18420.55",
		Hours:       9,
		SpanSeconds: int64(11 * time.Hour / time.Second),
		Trades:      37,
	}
}

// premThinMarket is a day with real trades whose activity is below the
// floor — the shape the 2026-08-04 valuation incident had, which the
// serving substance gate exists to withhold.
func premThinMarket(assetID string, n int, vwap string) timescale.MarketDay {
	return timescale.MarketDay{
		Day:         histTime(n),
		AssetID:     assetID,
		VWAP:        vwap,
		VolumeUSD:   "8.57",
		Hours:       1,
		SpanSeconds: 120,
		Trades:      3,
	}
}

func premAssetID(code, issuer string) string { return code + "-" + issuer }

// rwaPremiumServer builds the whole surface: the membership seams
// /v1/rwa/assets uses, plus the two premium legs.
func rwaPremiumServer(
	t *testing.T,
	bound []timescale.Sep1BoundCurrency,
	dir map[string]timescale.DirectoryEntry,
	rows map[string][]timescale.AssetRow,
	oracle v1.RWAOracleHistoryReader,
	market v1.RWAMarketHistoryReader,
) *v1.Server {
	t.Helper()
	return v1.New(v1.Options{
		Sep1Cache: &stubSep1BoundReader{bound: bound},
		Directory: &stubDirectoryReader{entries: dir},
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer:            rows,
			supply:              rwaSupplyFor(rows),
		},
		OracleHistory: oracle,
		MarketHistory: market,
	})
}

func getRWAPremium(t *testing.T, srv *v1.Server, query string) v1.RWAPremiumHistoryView {
	t.Helper()
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/rwa/premium"+query)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.RWAPremiumHistoryView `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	_ = resp.Body.Close()
	return env.Data
}

func premiumExcluded(v v1.RWAPremiumHistoryView, reason string) int {
	for _, e := range v.Excluded {
		if e.Reason == reason {
			return e.Assets
		}
	}
	return 0
}

// ─── parameters ─────────────────────────────────────────────────────

// TestRWAPremium_RefusesASubDailyTimeframe — the series is daily, and
// both legs are day buckets. Answering `24h` with a one-point chart
// would look like an answer to a question the grain cannot address.
func TestRWAPremium_RefusesASubDailyTimeframe(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	srv := rwaPremiumServer(t, bound, dir, rows, &stubOracleHistory{}, &stubMarketHistory{})
	ts := httpTestServer(t, srv)
	for _, tf := range []string{"1h", "24h", "5m", "forever"} {
		resp := mustGet(t, ts.URL+"/v1/rwa/premium?timeframe="+tf)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("timeframe=%s status = %d, want 400", tf, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

// ─── degradation ────────────────────────────────────────────────────

// TestRWAPremium_NoMarketReaderPublishesNoSeriesAndSaysWhy — an empty
// chart here would read as "no token has ever traded away from its
// instrument's value", which is a finding. Without the market leg the
// surface states the absence in `basis` instead.
func TestRWAPremium_NoMarketReaderPublishesNoSeriesAndSaysWhy(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	srv := rwaPremiumServer(t, bound, dir, rows, &stubOracleHistory{}, nil)
	v := getRWAPremium(t, srv, "")
	if len(v.Series) != 0 {
		t.Fatalf("series = %d, want none", len(v.Series))
	}
	if !containsFold(v.Basis, "No premium series is published") {
		t.Errorf("basis = %q — it must state the absence", v.Basis)
	}
}

// TestRWAPremium_AFailedReadIsNotAnEmptySeries — a read that errored
// knows nothing, and must not report its ignorance as a flat chart.
func TestRWAPremium_AFailedReadIsNotAnEmptySeries(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	srv := rwaPremiumServer(t, bound, dir, rows,
		&stubOracleHistory{},
		&stubMarketHistory{err: errors.New("lake unreachable")},
	)
	v := getRWAPremium(t, srv, "")
	if len(v.Series) != 0 {
		t.Fatalf("series = %+v, want none", v.Series)
	}
	if !containsFold(v.Basis, "No premium series is published") {
		t.Errorf("basis = %q — a failed read must say so", v.Basis)
	}
}

// ─── the series ─────────────────────────────────────────────────────

// TestRWAPremium_MeasuresTheDaysVWAPAgainstTheDaysOracleClose is the
// positive path, arithmetic pinned end to end.
//
// Day 1: market 1.0800 against a 1.0700 close → +0.9346% premium.
// Day 2: market 1.0600 against a 1.0700 close → −0.9346% discount.
// The sign is the whole point of the surface, so both directions are
// asserted rather than one.
func TestRWAPremium_MeasuresTheDaysVWAPAgainstTheDaysOracleClose(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	id := premAssetID("USTRY", rwaGoodIssuer)
	oracle := &stubOracleHistory{rows: []timescale.OracleDayPoint{
		histOracle("redstone", "USTRY", 1, 107000000), // 1.07 at 8dp
		histOracle("redstone", "USTRY", 2, 107000000),
	}}
	market := &stubMarketHistory{rows: []timescale.MarketDay{
		premMarket(id, 1, "1.08"),
		premMarket(id, 2, "1.06"),
	}}
	srv := rwaPremiumServer(t, bound, dir, rows, oracle, market)
	v := getRWAPremium(t, srv, "?timeframe=all")

	if len(v.Series) != 1 {
		t.Fatalf("series = %+v, want 1", v.Series)
	}
	s := v.Series[0]
	if len(s.Points) != 2 {
		t.Fatalf("points = %+v, want 2", s.Points)
	}
	if s.Points[0].PremiumPct != "0.9346" {
		t.Errorf("day 1 premium = %q, want 0.9346 ((1.08−1.07)/1.07×100)", s.Points[0].PremiumPct)
	}
	if s.Points[1].PremiumPct != "-0.9346" {
		t.Errorf("day 2 premium = %q, want -0.9346 — a discount must carry its sign", s.Points[1].PremiumPct)
	}
	// Both legs travel beside the ratio: a reader who sees only the
	// quotient cannot tell which side moved.
	if s.Points[0].MarketUSD == "" || s.Points[0].ReferenceUSD == "" {
		t.Errorf("a point served a ratio with no legs: %+v", s.Points[0])
	}
	if s.Points[0].VolumeUSD != "18420.55" || s.Points[0].Trades != 37 {
		t.Errorf("market evidence = %q/%d, want 18420.55/37", s.Points[0].VolumeUSD, s.Points[0].Trades)
	}
	if s.Feed != "rwa:USTRY" || s.Source != "redstone" {
		t.Errorf("feed/source = %q/%q, want rwa:USTRY/redstone", s.Feed, s.Source)
	}
	if v.Granularity != "1d" || v.Quote != "fiat:USD" {
		t.Errorf("granularity/quote = %q/%q", v.Granularity, v.Quote)
	}
	if v.Bound != 1 || v.Members != 1 || v.Assets != 1 {
		t.Errorf("bound/members/assets = %d/%d/%d, want 1/1/1", v.Bound, v.Members, v.Assets)
	}
	if len(v.Sources) != 1 || v.Sources[0] != "redstone" {
		t.Errorf("sources = %v, want [redstone] — the reference leg must name its publisher", v.Sources)
	}
	if v.MembershipAsOf.Time().IsZero() {
		t.Error("membership_as_of is zero — the backwards-applied membership must be dated")
	}
	// The reference leg joins on the FEED; the market leg joins on the
	// exact (code, issuer). Getting either wrong is the code-keyed join
	// internal/rwa exists to refuse.
	if len(oracle.asked) != 1 || oracle.asked[0] != "rwa:USTRY" {
		t.Errorf("asked the oracle for %v, want [rwa:USTRY]", oracle.asked)
	}
	if len(market.askedAssets) != 1 || market.askedAssets[0] != id {
		t.Errorf("asked the market for %v, want [%s]", market.askedAssets, id)
	}
}

// TestRWAPremium_ReadsTheDollarInEverySpellingItIsWrittenAs — a token's
// dollar book is split across spellings of the dollar, and reading one
// of them measures a fraction of the market as though it were all of
// it. The quote set must at minimum carry the synthetic `fiat:USD`.
func TestRWAPremium_ReadsTheDollarInEverySpellingItIsWrittenAs(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	market := &stubMarketHistory{}
	srv := rwaPremiumServer(t, bound, dir, rows,
		&stubOracleHistory{rows: []timescale.OracleDayPoint{
			histOracle("redstone", "USTRY", 1, 107000000),
		}}, market)
	_ = getRWAPremium(t, srv, "?timeframe=all")
	var sawFiatUSD bool
	for _, q := range market.askedQuotes {
		if q == "fiat:USD" {
			sawFiatUSD = true
		}
	}
	if !sawFiatUSD {
		t.Errorf("quote spellings = %v, want fiat:USD among them", market.askedQuotes)
	}
}

// ─── the gaps, which are the endpoint ───────────────────────────────

// TestRWAPremium_SilentOracleDayIsAHoleNotACarriedNAV. The market
// traded on day 2 and the oracle said nothing. Carrying day 1's NAV
// forward would draw a premium against a valuation nobody published —
// the exact fabrication the sibling series is allowed to make on its
// SUPPLY leg and neither series may make on a price.
func TestRWAPremium_SilentOracleDayIsAHoleNotACarriedNAV(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	id := premAssetID("USTRY", rwaGoodIssuer)
	srv := rwaPremiumServer(t, bound, dir, rows,
		&stubOracleHistory{rows: []timescale.OracleDayPoint{
			histOracle("redstone", "USTRY", 1, 107000000),
			// day 2: the oracle is silent.
			histOracle("redstone", "USTRY", 3, 107000000),
		}},
		&stubMarketHistory{rows: []timescale.MarketDay{
			premMarket(id, 1, "1.08"),
			premMarket(id, 2, "1.08"),
			premMarket(id, 3, "1.08"),
		}},
	)
	v := getRWAPremium(t, srv, "?timeframe=all")
	if len(v.Series) != 1 {
		t.Fatalf("series = %+v, want 1", v.Series)
	}
	pts := v.Series[0].Points
	if len(pts) != 2 {
		t.Fatalf("points = %+v, want 2 — the silent oracle day must not be filled", pts)
	}
	for _, p := range pts {
		if p.T.Time().Equal(histTime(2)) {
			t.Fatalf("a premium was published for the day the oracle was silent: %+v", p)
		}
	}
}

// TestRWAPremium_SilentMarketDayIsAHoleNotACarriedPrice. The oracle
// published every day and the token did not trade on day 2. Carrying
// day 1's price forward against a moving NAV would draw a premium
// drifting toward a discount that nobody transacted at.
func TestRWAPremium_SilentMarketDayIsAHoleNotACarriedPrice(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	id := premAssetID("USTRY", rwaGoodIssuer)
	srv := rwaPremiumServer(t, bound, dir, rows,
		&stubOracleHistory{rows: []timescale.OracleDayPoint{
			histOracle("redstone", "USTRY", 1, 107000000),
			histOracle("redstone", "USTRY", 2, 108000000),
			histOracle("redstone", "USTRY", 3, 109000000),
		}},
		&stubMarketHistory{rows: []timescale.MarketDay{
			premMarket(id, 1, "1.08"),
			// day 2: nobody traded.
			premMarket(id, 3, "1.08"),
		}},
	)
	v := getRWAPremium(t, srv, "?timeframe=all")
	if len(v.Series) != 1 {
		t.Fatalf("series = %+v, want 1", v.Series)
	}
	s := v.Series[0]
	if len(s.Points) != 2 {
		t.Fatalf("points = %+v, want 2 — the untraded day must not be filled", s.Points)
	}
	for _, p := range s.Points {
		if p.T.Time().Equal(histTime(2)) {
			t.Fatalf("a premium was published for a day nobody traded: %+v", p)
		}
	}
	// The day is counted, not merely missing: a reader must be able to
	// tell a quiet market from an unread one.
	if s.ReferenceOnlyDays != 1 {
		t.Errorf("reference_only_days = %d, want 1", s.ReferenceOnlyDays)
	}
}

// TestRWAPremium_AThinDayIsWithheldAndCounted. This is the gate the
// endpoint could most easily have routed around. The point-in-time
// premium compares against the SERVED price, which a thin market
// withholds; a history built on the raw daily VWAP would publish, for
// every past day, exactly the claim the live surface refuses.
func TestRWAPremium_AThinDayIsWithheldAndCounted(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	id := premAssetID("USTRY", rwaGoodIssuer)
	srv := rwaPremiumServer(t, bound, dir, rows,
		&stubOracleHistory{rows: []timescale.OracleDayPoint{
			histOracle("redstone", "USTRY", 1, 107000000),
			histOracle("redstone", "USTRY", 2, 107000000),
		}},
		&stubMarketHistory{rows: []timescale.MarketDay{
			premMarket(id, 1, "1.08"),
			// A handful of dust trades in two minutes. Real, and not
			// enough to publish an aggregated price claim from.
			premThinMarket(id, 2, "9.99"),
		}},
	)
	v := getRWAPremium(t, srv, "?timeframe=all")
	if len(v.Series) != 1 {
		t.Fatalf("series = %+v, want 1", v.Series)
	}
	s := v.Series[0]
	if len(s.Points) != 1 {
		t.Fatalf("points = %+v, want 1 — the thin day must not be priced", s.Points)
	}
	if s.Points[0].T.Time().Equal(histTime(2)) {
		t.Fatal("the thin day reached the wire")
	}
	if s.MarketWithheldDays != 1 {
		t.Errorf("market_withheld_days = %d, want 1 — a refused day must be counted, "+
			"because 'we saw trades and refused to price them' and 'there were no "+
			"trades' are opposite findings", s.MarketWithheldDays)
	}
}

// TestRWAPremium_EveryDayThinPublishesNothingAndNamesTheReason — the
// whole-series form of the refusal above.
func TestRWAPremium_EveryDayThinPublishesNothingAndNamesTheReason(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	id := premAssetID("USTRY", rwaGoodIssuer)
	srv := rwaPremiumServer(t, bound, dir, rows,
		&stubOracleHistory{rows: []timescale.OracleDayPoint{
			histOracle("redstone", "USTRY", 1, 107000000),
			histOracle("redstone", "USTRY", 2, 107000000),
		}},
		&stubMarketHistory{rows: []timescale.MarketDay{
			premThinMarket(id, 1, "9.99"),
			premThinMarket(id, 2, "9.99"),
		}},
	)
	v := getRWAPremium(t, srv, "?timeframe=all")
	if len(v.Series) != 0 {
		t.Fatalf("series = %+v, want none", v.Series)
	}
	if premiumExcluded(v, "market_below_floor") != 1 {
		t.Errorf("excluded = %+v, want one market_below_floor", v.Excluded)
	}
}

// TestRWAPremium_TwoLegsThatNeverCoincidePublishNothing — both sides
// exist and never on the same day. Interpolating either onto the
// other's days is exactly what this surface refuses, so the honest
// answer is no series and a stated reason.
func TestRWAPremium_TwoLegsThatNeverCoincidePublishNothing(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	id := premAssetID("USTRY", rwaGoodIssuer)
	srv := rwaPremiumServer(t, bound, dir, rows,
		&stubOracleHistory{rows: []timescale.OracleDayPoint{
			histOracle("redstone", "USTRY", 1, 107000000),
			histOracle("redstone", "USTRY", 3, 107000000),
		}},
		&stubMarketHistory{rows: []timescale.MarketDay{
			premMarket(id, 2, "1.08"),
			premMarket(id, 4, "1.08"),
		}},
	)
	v := getRWAPremium(t, srv, "?timeframe=all")
	if len(v.Series) != 0 {
		t.Fatalf("series = %+v, want none", v.Series)
	}
	if premiumExcluded(v, "no_same_day_observation") != 1 {
		t.Errorf("excluded = %+v, want one no_same_day_observation", v.Excluded)
	}
}

// TestRWAPremium_ATokenThatHasNeverTradedIsNamedNotHidden — the
// ordinary condition of a held-to-maturity instrument. It has a NAV
// every day and a market price on none, and saying so is the finding.
func TestRWAPremium_ATokenThatHasNeverTradedIsNamedNotHidden(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	srv := rwaPremiumServer(t, bound, dir, rows,
		&stubOracleHistory{rows: []timescale.OracleDayPoint{
			histOracle("redstone", "USTRY", 1, 107000000),
		}},
		&stubMarketHistory{},
	)
	v := getRWAPremium(t, srv, "?timeframe=all")
	if len(v.Series) != 0 {
		t.Fatalf("series = %+v, want none", v.Series)
	}
	if premiumExcluded(v, "no_market_history") != 1 {
		t.Errorf("excluded = %+v, want one no_market_history", v.Excluded)
	}
	// Bound is the ceiling on coverage and must be reported even when
	// nothing was published under it.
	if v.Bound != 1 {
		t.Errorf("bound = %d, want 1", v.Bound)
	}
	for _, e := range v.Excluded {
		if e.Detail == "" {
			t.Errorf("exclusion %q carries no detail — a reason nobody can act on is not an account", e.Reason)
		}
	}
}

// ─── suppression, and the order it happens in ───────────────────────

// TestRWAPremium_ScamFlaggedIssuerGetsNoSeries.
//
// The flagged issuer here holds a (code, issuer) the curated table DOES
// bind, and both legs answer for it. Nothing is published, because the
// scam flag excludes at MEMBERSHIP time (rwa.Qualify R3) — the issuer
// never becomes a set member, so no downstream surface can price it.
//
// The endpoint carries its OWN suppression as well, ahead of the
// binding lookup, and that ordering is pinned in
// TestRWAPremiumCandidates_SuppressesAFlaggedIssuerBeforeReadingTheBinding
// — it cannot be reached from here precisely because the upstream gate
// gets there first, which is the correct arrangement and worth
// asserting in both places.
func TestRWAPremium_ScamFlaggedIssuerGetsNoSeries(t *testing.T) {
	bound := []timescale.Sep1BoundCurrency{
		rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
	}
	dir := map[string]timescale.DirectoryEntry{
		rwaGoodIssuer: {
			Address: rwaGoodIssuer, Name: "Etherfuse", Domain: "etherfuse.com",
			Tags: []string{"issuer", "malicious", "unsafe"}, Source: "stellar-expert",
		},
	}
	rows := map[string][]timescale.AssetRow{
		rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312)},
	}
	id := premAssetID("USTRY", rwaGoodIssuer)
	srv := rwaPremiumServer(t, bound, dir, rows,
		&stubOracleHistory{rows: []timescale.OracleDayPoint{
			histOracle("redstone", "USTRY", 1, 107000000),
		}},
		&stubMarketHistory{rows: []timescale.MarketDay{premMarket(id, 1, "1.08")}},
	)
	v := getRWAPremium(t, srv, "?timeframe=all")
	if len(v.Series) != 0 {
		t.Fatalf("a flagged issuer was served a premium series: %+v", v.Series)
	}
	// It is not in the set at all — not "in the set and unpriced".
	if v.Assets != 0 || v.Bound != 0 {
		t.Errorf("assets/bound = %d/%d, want 0/0 — a flagged issuer is refused at membership", v.Assets, v.Bound)
	}
	// And it must not be reported as merely unbound: that would say the
	// curated table has a hole where in fact the issuer is flagged.
	if premiumExcluded(v, "not_bound") != 0 {
		t.Errorf("excluded = %+v — the flagged issuer must not be reported as unbound", v.Excluded)
	}
}

// TestRWAPremium_AnUnboundPairGetsSilenceAndAStatedReason — the
// commonest refusal by far. A token wearing an instrument's ticker that
// no curated binding ties to that instrument is a code collision, and
// pricing it off the code alone is the join internal/rwa refuses.
func TestRWAPremium_AnUnboundPairGetsSilenceAndAStatedReason(t *testing.T) {
	bound := []timescale.Sep1BoundCurrency{
		rwaBound("USTRY", rwaUnknownIssuer, "not-etherfuse.example", "bond"),
	}
	dir := map[string]timescale.DirectoryEntry{
		rwaUnknownIssuer: recognisedIssuer(rwaUnknownIssuer, "Someone Else"),
	}
	rows := map[string][]timescale.AssetRow{
		rwaUnknownIssuer: {rwaRow("USTRY", rwaUnknownIssuer, sptr("0.20"), 4000)},
	}
	id := premAssetID("USTRY", rwaUnknownIssuer)
	srv := rwaPremiumServer(t, bound, dir, rows,
		&stubOracleHistory{rows: []timescale.OracleDayPoint{
			histOracle("redstone", "USTRY", 1, 107000000),
		}},
		&stubMarketHistory{rows: []timescale.MarketDay{premMarket(id, 1, "0.20")}},
	)
	v := getRWAPremium(t, srv, "?timeframe=all")
	if len(v.Series) != 0 {
		t.Fatalf("an unbound pair was priced against a real instrument: %+v", v.Series)
	}
	if premiumExcluded(v, "not_bound") != 1 {
		t.Errorf("excluded = %+v, want one not_bound", v.Excluded)
	}
}

// ─── the honesty fields ─────────────────────────────────────────────

// TestRWAPremium_CoverageIsCountedAgainstTheWholeSet — a point states
// its coverage of the SECTOR, not of whatever happened to be readable.
func TestRWAPremium_CoverageIsCountedAgainstTheWholeSet(t *testing.T) {
	bound := []timescale.Sep1BoundCurrency{
		rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
		rwaBound("CETES", rwaGoodIssuer, "etherfuse.com", "bond"),
	}
	dir := map[string]timescale.DirectoryEntry{
		rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse"),
	}
	rows := map[string][]timescale.AssetRow{
		rwaGoodIssuer: {
			rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312),
			rwaRow("CETES", rwaGoodIssuer, sptr("0.0698"), 512000),
		},
	}
	// Only USTRY trades. CETES holds a NAV and no market — the
	// ordinary state of this asset class.
	id := premAssetID("USTRY", rwaGoodIssuer)
	srv := rwaPremiumServer(t, bound, dir, rows,
		&stubOracleHistory{rows: []timescale.OracleDayPoint{
			histOracle("redstone", "USTRY", 1, 107000000),
			histOracle("redstone", "CETES", 1, 6988900),
		}},
		&stubMarketHistory{rows: []timescale.MarketDay{premMarket(id, 1, "1.08")}},
	)
	v := getRWAPremium(t, srv, "?timeframe=all")
	if v.Assets != 2 || v.Bound != 2 || v.Members != 1 {
		t.Fatalf("assets/bound/members = %d/%d/%d, want 2/2/1", v.Assets, v.Bound, v.Members)
	}
	// The account has to CLOSE: everything in the set is either a
	// member or excluded for a stated reason, and nothing is both.
	total := v.Members
	for _, e := range v.Excluded {
		total += e.Assets
	}
	if total != v.Assets {
		t.Errorf("members + excluded = %d, want assets = %d (excluded = %+v)", total, v.Assets, v.Excluded)
	}
	if len(v.Coverage) != 1 {
		t.Fatalf("coverage = %+v, want 1 day", v.Coverage)
	}
	c := v.Coverage[0]
	if c.AssetsMeasured != 1 || c.AssetsUnmeasured != 1 {
		t.Errorf("coverage = %d measured / %d unmeasured, want 1/1", c.AssetsMeasured, c.AssetsUnmeasured)
	}
	if c.AssetsMeasured+c.AssetsUnmeasured != v.Assets {
		t.Errorf("coverage does not close against the set: %d + %d != %d",
			c.AssetsMeasured, c.AssetsUnmeasured, v.Assets)
	}
}

// TestRWAPremium_MoneyFieldsAreDecimalStringsNeverNumbers — ADR-0003 on
// the wire. A JSON number would round the figure the whole surface
// exists to state exactly.
func TestRWAPremium_MoneyFieldsAreDecimalStringsNeverNumbers(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	id := premAssetID("USTRY", rwaGoodIssuer)
	srv := rwaPremiumServer(t, bound, dir, rows,
		&stubOracleHistory{rows: []timescale.OracleDayPoint{
			histOracle("redstone", "USTRY", 1, 107000000),
		}},
		&stubMarketHistory{rows: []timescale.MarketDay{premMarket(id, 1, "1.08")}},
	)
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/rwa/premium?timeframe=all")
	defer func() { _ = resp.Body.Close() }()
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	body := string(raw["data"])
	for _, field := range []string{`"premium_pct":"`, `"market_usd":"`, `"reference_usd":"`, `"volume_usd":"`} {
		if !strings.Contains(body, field) {
			t.Errorf("money field %s is not a decimal string on the wire", field)
		}
	}
}

// TestRWAPremium_TheZeroDenominatorIsRefusedNotDividedBy — a
// non-positive published value is bad data, never a valuation of zero,
// and nothing is divided by it.
func TestRWAPremium_TheZeroDenominatorIsRefusedNotDividedBy(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	id := premAssetID("USTRY", rwaGoodIssuer)
	zero := histOracle("redstone", "USTRY", 1, 0)
	zero.Price = big.NewInt(0)
	srv := rwaPremiumServer(t, bound, dir, rows,
		&stubOracleHistory{rows: []timescale.OracleDayPoint{
			zero,
			histOracle("redstone", "USTRY", 2, 107000000),
		}},
		&stubMarketHistory{rows: []timescale.MarketDay{
			premMarket(id, 1, "1.08"),
			premMarket(id, 2, "1.08"),
		}},
	)
	v := getRWAPremium(t, srv, "?timeframe=all")
	if len(v.Series) != 1 {
		t.Fatalf("series = %+v, want 1", v.Series)
	}
	if len(v.Series[0].Points) != 1 {
		t.Fatalf("points = %+v, want 1 — nothing may be divided by a zero NAV", v.Series[0].Points)
	}
	if v.Series[0].Points[0].T.Time().Equal(histTime(1)) {
		t.Fatal("a premium was computed against a zero published value")
	}
}
