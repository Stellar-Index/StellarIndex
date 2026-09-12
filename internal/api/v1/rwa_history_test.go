package v1_test

// GET /v1/rwa/history — the RWA set valued over time (#352).
//
// The surface exists because /v1/rwa/assets is a snapshot and the
// question everyone actually asks is "and is it growing". What makes it
// hard is that answering it honestly means refusing to answer on most of
// the days: a set member is worth something on a day only if BOTH the
// lake knows its supply on that day and an oracle published a value for
// its instrument on that day. These tests pin the refusals, because a
// chart that quietly fills its gaps is the failure mode this whole
// endpoint is written against.
//
// Production wiring: v1.New with Options.OracleHistory = *timescale.Store
// and Options.TokenSupply = *clickhouse.SupplyReader (see
// cmd/stellarindex-api/main.go). The tests below use the SAME
// constructor — there is no test-only server builder — and substitute
// only those two readers.

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ─── stubs ──────────────────────────────────────────────────────────

// stubOracleHistory serves canned day buckets. It records the assets it
// was asked about so a test can prove the handler joined on the FEED
// rather than on the Stellar asset code.
type stubOracleHistory struct {
	rows  []timescale.OracleDayPoint
	err   error
	asked []string
}

func (s *stubOracleHistory) DailyOraclePrices(
	_ context.Context, assets []canonical.Asset, _ canonical.Asset, _, _ time.Time,
) ([]timescale.OracleDayPoint, error) {
	for _, a := range assets {
		s.asked = append(s.asked, a.String())
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.rows, nil
}

// stubFlowSupply is a TokenSupplyReader that ALSO answers the daily
// flow-history seam — the shape *clickhouse.SupplyReader has in
// production. A test wanting to prove the optional seam degrades cleanly
// wires stubTokenSupplies instead, which does not implement it.
type stubFlowSupply struct {
	days []clickhouse.SupplyFlowDay
	err  error
}

func (s *stubFlowSupply) TokenSupply(_ context.Context, id string) (clickhouse.TokenSupply, error) {
	return clickhouse.TokenSupply{ContractID: id}, nil
}

func (s *stubFlowSupply) NativeTotalCoins(context.Context) (int64, uint32, error) {
	return 0, 0, errors.New("the RWA history must not read the native total supply")
}

func (s *stubFlowSupply) DailySupplyFlowsForContracts(
	_ context.Context, contractIDs []string,
) ([]clickhouse.SupplyFlowDay, error) {
	if s.err != nil {
		return nil, s.err
	}
	keep := map[string]struct{}{}
	for _, c := range contractIDs {
		keep[c] = struct{}{}
	}
	out := []clickhouse.SupplyFlowDay{}
	for _, d := range s.days {
		if _, ok := keep[d.ContractID]; ok {
			out = append(out, d)
		}
	}
	return out, nil
}

// ─── fixture helpers ────────────────────────────────────────────────

// rwaHistSAC is the deterministic Stellar Asset Contract address the
// flow log is keyed by. Derived rather than hardcoded: the handler
// derives it the same way, and a hardcoded fixture would let the two
// drift apart without a test noticing.
func rwaHistSAC(t *testing.T, code, issuer string) string {
	t.Helper()
	sac, err := canonical.Asset{Type: canonical.AssetClassic, Code: code, Issuer: issuer}.SacContractID()
	if err != nil {
		t.Fatalf("SacContractID(%s-%s): %v", code, issuer, err)
	}
	return sac
}

func histTime(n int) time.Time { return time.Date(2026, 9, n, 0, 0, 0, 0, time.UTC) }

func histFlow(contract string, n int, net string) clickhouse.SupplyFlowDay {
	v, _ := new(big.Int).SetString(net, 10)
	return clickhouse.SupplyFlowDay{ContractID: contract, Day: histTime(n), Net: v, Flows: 1}
}

func histOracle(source, feed string, n int, price int64) timescale.OracleDayPoint {
	return timescale.OracleDayPoint{
		Bucket:       histTime(n),
		Source:       source,
		Asset:        canonical.Asset{Type: canonical.AssetRWA, Code: feed},
		Price:        big.NewInt(price),
		Decimals:     8,
		Observations: 12,
	}
}

// rwaHistoryServer builds the whole surface: the membership seams
// /v1/rwa/assets uses, plus the two history legs.
func rwaHistoryServer(
	t *testing.T,
	bound []timescale.Sep1BoundCurrency,
	dir map[string]timescale.DirectoryEntry,
	rows map[string][]timescale.AssetRow,
	supply v1.TokenSupplyReader,
	oracle v1.RWAOracleHistoryReader,
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
		TokenSupply:   supply,
		OracleHistory: oracle,
	})
}

func getRWAHistory(t *testing.T, srv *v1.Server, query string) v1.RWAHistoryView {
	t.Helper()
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/rwa/history"+query)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.RWAHistoryView `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return env.Data
}

// oneBoundMember wires the single positive fixture every test below
// varies: USTRY from the Etherfuse issuer, which internal/rwa binds to
// the `rwa:USTRY` feed.
func oneBoundMember() ([]timescale.Sep1BoundCurrency, map[string]timescale.DirectoryEntry, map[string][]timescale.AssetRow) {
	return []timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{
			rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312)},
		}
}

// ─── parameters ─────────────────────────────────────────────────────

// TestRWAHistory_RefusesASubDailyTimeframe — the series is daily.
// Answering `24h` with a one- or two-point chart would look like an
// answer to a question the grain cannot address.
func TestRWAHistory_RefusesASubDailyTimeframe(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	srv := rwaHistoryServer(t, bound, dir, rows, &stubFlowSupply{}, &stubOracleHistory{})
	ts := httpTestServer(t, srv)
	for _, tf := range []string{"1h", "24h", "5m", "forever"} {
		resp := mustGet(t, ts.URL+"/v1/rwa/history?timeframe="+tf)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("timeframe=%s status = %d, want 400", tf, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

func TestRWAHistory_RefusesAnUnknownGroupBy(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	srv := rwaHistoryServer(t, bound, dir, rows, &stubFlowSupply{}, &stubOracleHistory{})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/rwa/history?group_by=anchor_class")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// ─── degradation ────────────────────────────────────────────────────

// TestRWAHistory_NoPriceReaderPublishesNoSeriesAndSaysWhy — an empty
// chart is a claim about the sector. Without the price leg the surface
// states the absence in `basis` rather than serving a flat nothing that
// reads as "the sector is worth zero".
func TestRWAHistory_NoPriceReaderPublishesNoSeriesAndSaysWhy(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	srv := rwaHistoryServer(t, bound, dir, rows, &stubFlowSupply{}, nil)
	v := getRWAHistory(t, srv, "")
	if len(v.Points) != 0 {
		t.Fatalf("points = %d, want none", len(v.Points))
	}
	if v.Basis == "" || !containsFold(v.Basis, "No series is published") {
		t.Errorf("basis = %q — it must state the absence", v.Basis)
	}
}

// TestRWAHistory_NoSupplySeamPublishesNoSeries — the supply leg is
// type-asserted off the token-supply reader, so a deployment whose
// reader predates the seam must degrade to no series rather than to a
// series valued at a supply of zero.
func TestRWAHistory_NoSupplySeamPublishesNoSeries(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	sac := rwaHistSAC(t, "USTRY", rwaGoodIssuer)
	srv := rwaHistoryServer(t, bound, dir, rows,
		// Implements TokenSupplyReader but NOT the daily-flow seam.
		&stubTokenSupplies{byID: map[string]string{sac: "10000000"}},
		&stubOracleHistory{rows: []timescale.OracleDayPoint{histOracle("redstone", "USTRY", 1, 107000000)}},
	)
	v := getRWAHistory(t, srv, "")
	if len(v.Points) != 0 {
		t.Fatalf("points = %+v, want none", v.Points)
	}
	if !containsFold(v.Basis, "No series is published") {
		t.Errorf("basis = %q — it must state the absence", v.Basis)
	}
}

// ─── the series ─────────────────────────────────────────────────────

// TestRWAHistory_ValuesSupplyTimesTheDaysOracleClose is the positive
// path, arithmetic pinned end to end: 20 whole tokens at 1.07 dollars.
func TestRWAHistory_ValuesSupplyTimesTheDaysOracleClose(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	sac := rwaHistSAC(t, "USTRY", rwaGoodIssuer)
	oracle := &stubOracleHistory{rows: []timescale.OracleDayPoint{
		histOracle("redstone", "USTRY", 1, 107000000), // 1.07 at 8dp
		histOracle("redstone", "USTRY", 2, 108000000),
	}}
	srv := rwaHistoryServer(t, bound, dir, rows,
		&stubFlowSupply{days: []clickhouse.SupplyFlowDay{
			histFlow(sac, 1, "200000000"), // 20 whole tokens at 7dp
		}},
		oracle,
	)
	v := getRWAHistory(t, srv, "?timeframe=all")
	if len(v.Points) != 2 {
		t.Fatalf("points = %+v, want 2", v.Points)
	}
	if v.Points[0].ValueUSD != "21.40" {
		t.Errorf("day 1 = %q, want 21.40 (20 × 1.07)", v.Points[0].ValueUSD)
	}
	if v.Points[1].ValueUSD != "21.60" {
		t.Errorf("day 2 = %q, want 21.60 (20 × 1.08)", v.Points[1].ValueUSD)
	}
	if v.Members != 1 || v.Assets != 1 {
		t.Errorf("members/assets = %d/%d, want 1/1", v.Members, v.Assets)
	}
	if v.Granularity != "1d" || v.Quote != "fiat:USD" {
		t.Errorf("granularity/quote = %q/%q", v.Granularity, v.Quote)
	}
	if len(v.Sources) != 1 || v.Sources[0] != "redstone" {
		t.Errorf("sources = %v, want [redstone] — a dollar figure must name its publisher", v.Sources)
	}
	if v.MembershipAsOf.Time().IsZero() {
		t.Error("membership_as_of is zero — the backwards-applied membership must be dated")
	}
	// The join must run on the FEED, never on the Stellar asset code.
	if len(oracle.asked) != 1 || oracle.asked[0] != "rwa:USTRY" {
		t.Errorf("asked the oracle for %v, want [rwa:USTRY]", oracle.asked)
	}
}

// TestRWAHistory_SilentOracleDayIsAHoleNotAZero is the endpoint's whole
// reason for existing in this shape. The supply is perfectly well known
// on day 2; the oracle said nothing, so there is no point.
func TestRWAHistory_SilentOracleDayIsAHoleNotAZero(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	sac := rwaHistSAC(t, "USTRY", rwaGoodIssuer)
	srv := rwaHistoryServer(t, bound, dir, rows,
		&stubFlowSupply{days: []clickhouse.SupplyFlowDay{histFlow(sac, 1, "200000000")}},
		&stubOracleHistory{rows: []timescale.OracleDayPoint{
			histOracle("redstone", "USTRY", 1, 107000000),
			// day 2: silent.
			histOracle("redstone", "USTRY", 3, 107000000),
		}},
	)
	v := getRWAHistory(t, srv, "?timeframe=all")
	if len(v.Points) != 2 {
		t.Fatalf("points = %+v, want 2 — the silent day must not be filled", v.Points)
	}
	for _, p := range v.Points {
		if p.T.Time().Equal(histTime(2)) {
			t.Fatalf("a point was published for the day the oracle was silent: %+v", p)
		}
		if p.ValueUSD == "0.00" {
			t.Fatalf("a zero reached the wire at %v", p.T.Time())
		}
	}
}

// TestRWAHistory_NothingBeforeTheFirstMint — the oracle has priced the
// instrument since before the token existed. Those days must not be
// valued at a supply of zero, which would draw a real line along the
// axis and read as "the sector was worth nothing then".
func TestRWAHistory_NothingBeforeTheFirstMint(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	sac := rwaHistSAC(t, "USTRY", rwaGoodIssuer)
	srv := rwaHistoryServer(t, bound, dir, rows,
		&stubFlowSupply{days: []clickhouse.SupplyFlowDay{histFlow(sac, 5, "200000000")}},
		&stubOracleHistory{rows: []timescale.OracleDayPoint{
			histOracle("redstone", "USTRY", 1, 107000000),
			histOracle("redstone", "USTRY", 2, 107000000),
			histOracle("redstone", "USTRY", 5, 107000000),
		}},
	)
	v := getRWAHistory(t, srv, "?timeframe=all")
	if len(v.Points) != 1 || !v.Points[0].T.Time().Equal(histTime(5)) {
		t.Fatalf("points = %+v, want only the 5th", v.Points)
	}
}

// TestRWAHistory_IncompleteFlowLogRefusesTheMember — a running total
// that goes negative means the lake's seeding is incomplete, not that
// the token has negative supply. The member is refused with a reason a
// reader (and an operator) can act on.
func TestRWAHistory_IncompleteFlowLogRefusesTheMember(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	sac := rwaHistSAC(t, "USTRY", rwaGoodIssuer)
	srv := rwaHistoryServer(t, bound, dir, rows,
		&stubFlowSupply{days: []clickhouse.SupplyFlowDay{
			histFlow(sac, 1, "-200000000"), // a burn with no matching mint
			histFlow(sac, 2, "900000000"),
		}},
		&stubOracleHistory{rows: []timescale.OracleDayPoint{histOracle("redstone", "USTRY", 2, 107000000)}},
	)
	v := getRWAHistory(t, srv, "?timeframe=all")
	if len(v.Points) != 0 {
		t.Fatalf("points = %+v, want none — an incompletely seeded log values nothing", v.Points)
	}
	if !hasExclusion(v, "supply_incomplete", 1) {
		t.Fatalf("excluded = %+v, want supply_incomplete×1", v.Excluded)
	}
	if d := exclusionDetail(v, "supply_incomplete"); d == "" {
		t.Error("the exclusion carries no detail — a bare count cannot say who can move it")
	}
}

// TestRWAHistory_NoFlowsAtAllIsItsOwnReason keeps "the lake has nothing
// for this contract" apart from "the lake has something and it is
// wrong". They are opposite findings with different owners.
func TestRWAHistory_NoFlowsAtAllIsItsOwnReason(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	srv := rwaHistoryServer(t, bound, dir, rows,
		&stubFlowSupply{}, // the lake knows nothing about this contract
		&stubOracleHistory{rows: []timescale.OracleDayPoint{histOracle("redstone", "USTRY", 1, 107000000)}},
	)
	v := getRWAHistory(t, srv, "?timeframe=all")
	if len(v.Points) != 0 {
		t.Fatalf("points = %+v, want none", v.Points)
	}
	if !hasExclusion(v, "no_supply_history", 1) {
		t.Fatalf("excluded = %+v, want no_supply_history×1", v.Excluded)
	}
}

// TestRWAHistory_UnboundMemberIsExcludedNotDropped — a set member with
// no curated (code, issuer) → feed binding cannot be valued, and the
// total must say so rather than quietly become a smaller claim under the
// same label.
func TestRWAHistory_UnboundMemberIsExcludedNotDropped(t *testing.T) {
	sac := rwaHistSAC(t, "USTRY", rwaGoodIssuer)
	srv := rwaHistoryServer(t,
		[]timescale.Sep1BoundCurrency{
			rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
			// Same recognised issuer, a code nothing binds to a feed.
			rwaBound("NOTAFEED", rwaGoodIssuer, "etherfuse.com", "bond"),
		},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{
			rwaGoodIssuer: {
				rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312),
				rwaRow("NOTAFEED", rwaGoodIssuer, sptr("1.0000"), 12),
			},
		},
		&stubFlowSupply{days: []clickhouse.SupplyFlowDay{histFlow(sac, 1, "200000000")}},
		&stubOracleHistory{rows: []timescale.OracleDayPoint{histOracle("redstone", "USTRY", 1, 107000000)}},
	)
	v := getRWAHistory(t, srv, "?timeframe=all")
	if v.Assets != 2 || v.Members != 1 {
		t.Fatalf("assets/members = %d/%d, want 2/1", v.Assets, v.Members)
	}
	if !hasExclusion(v, "not_bound", 1) {
		t.Fatalf("excluded = %+v, want not_bound×1", v.Excluded)
	}
	if len(v.Points) != 1 {
		t.Fatalf("points = %+v, want 1", v.Points)
	}
	p := v.Points[0]
	if p.AssetsValued != 1 || p.AssetsUnvalued != 1 || !p.LowerBound {
		t.Errorf("point = %+v; want 1 valued / 1 unvalued / lower_bound true against a set of 2", p)
	}
}

// TestRWAHistory_GroupByAssetNamesTheFeedAndTheOracle — a decomposed
// series still has to carry its provenance per line, or the reader gets
// a stack of anonymous dollar curves.
func TestRWAHistory_GroupByAssetNamesTheFeedAndTheOracle(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	sac := rwaHistSAC(t, "USTRY", rwaGoodIssuer)
	srv := rwaHistoryServer(t, bound, dir, rows,
		&stubFlowSupply{days: []clickhouse.SupplyFlowDay{histFlow(sac, 1, "200000000")}},
		&stubOracleHistory{rows: []timescale.OracleDayPoint{histOracle("redstone", "USTRY", 1, 107000000)}},
	)
	v := getRWAHistory(t, srv, "?timeframe=all&group_by=asset")
	if len(v.Groups) != 1 {
		t.Fatalf("groups = %+v, want 1", v.Groups)
	}
	g := v.Groups[0]
	if g.Key != "USTRY-"+rwaGoodIssuer || g.Code != "USTRY" || g.Issuer != rwaGoodIssuer {
		t.Errorf("group identity = %q/%q/%q — identity is (code, issuer), never the code", g.Key, g.Code, g.Issuer)
	}
	if g.Feed != "rwa:USTRY" || g.Source != "redstone" {
		t.Errorf("group feed/source = %q/%q, want rwa:USTRY/redstone", g.Feed, g.Source)
	}
	if len(g.Points) != 1 || g.Points[0].ValueUSD != "21.40" {
		t.Errorf("group points = %+v", g.Points)
	}
}

// TestRWAHistory_GroupByIssuerMergesTheIssuersAssets — and carries no
// feed or source, because an issuer group may span several of each.
func TestRWAHistory_GroupByIssuerMergesTheIssuersAssets(t *testing.T) {
	ustry := rwaHistSAC(t, "USTRY", rwaGoodIssuer)
	cetes := rwaHistSAC(t, "CETES", rwaGoodIssuer)
	srv := rwaHistoryServer(t,
		[]timescale.Sep1BoundCurrency{
			rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
			rwaBound("CETES", rwaGoodIssuer, "etherfuse.com", "bond"),
		},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{
			rwaGoodIssuer: {
				rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312),
				rwaRow("CETES", rwaGoodIssuer, sptr("0.0698"), 41231),
			},
		},
		&stubFlowSupply{days: []clickhouse.SupplyFlowDay{
			histFlow(ustry, 1, "200000000"),
			histFlow(cetes, 1, "100000000"),
		}},
		&stubOracleHistory{rows: []timescale.OracleDayPoint{
			histOracle("redstone", "USTRY", 1, 107000000), // 20 × 1.07 = 21.40
			histOracle("redstone", "CETES", 1, 700000),    // 10 × 0.007 = 0.07
		}},
	)
	v := getRWAHistory(t, srv, "?timeframe=all&group_by=issuer")
	if len(v.Groups) != 1 {
		t.Fatalf("groups = %+v, want one issuer", v.Groups)
	}
	g := v.Groups[0]
	if g.Key != rwaGoodIssuer || g.Members != 2 {
		t.Errorf("group = %q with %d members, want the issuer with 2", g.Key, g.Members)
	}
	if g.Feed != "" || g.Source != "" {
		t.Errorf("issuer group carries feed %q / source %q — it may span several of each", g.Feed, g.Source)
	}
	if len(g.Points) != 1 || g.Points[0].ValueUSD != "21.47" {
		t.Fatalf("group points = %+v, want 21.47", g.Points)
	}
	if len(v.Points) != 1 || v.Points[0].ValueUSD != "21.47" {
		t.Errorf("total = %+v; the groups must add up to it", v.Points)
	}
}

// TestRWAHistory_TimeframeWindowsTheSeries — and `all` does not.
func TestRWAHistory_TimeframeWindowsTheSeries(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	sac := rwaHistSAC(t, "USTRY", rwaGoodIssuer)
	old := time.Now().UTC().AddDate(0, 0, -400).Truncate(24 * time.Hour)
	recent := time.Now().UTC().AddDate(0, 0, -2).Truncate(24 * time.Hour)
	mint, _ := new(big.Int).SetString("200000000", 10)
	srv := rwaHistoryServer(t, bound, dir, rows,
		&stubFlowSupply{days: []clickhouse.SupplyFlowDay{
			{ContractID: sac, Day: old, Net: mint, Flows: 1},
		}},
		&stubOracleHistory{rows: []timescale.OracleDayPoint{
			{Bucket: old, Source: "redstone", Asset: canonical.Asset{Type: canonical.AssetRWA, Code: "USTRY"}, Price: big.NewInt(107000000), Decimals: 8},
			{Bucket: recent, Source: "redstone", Asset: canonical.Asset{Type: canonical.AssetRWA, Code: "USTRY"}, Price: big.NewInt(108000000), Decimals: 8},
		}},
	)
	if got := getRWAHistory(t, srv, "?timeframe=all"); len(got.Points) != 2 {
		t.Fatalf("timeframe=all points = %d, want 2", len(got.Points))
	}
	got := getRWAHistory(t, srv, "?timeframe=1mo")
	if len(got.Points) != 1 {
		t.Fatalf("timeframe=1mo points = %+v, want only the recent one", got.Points)
	}
	if got.Timeframe != "1mo" {
		t.Errorf("timeframe echoed as %q", got.Timeframe)
	}
}

// ─── small helpers ──────────────────────────────────────────────────

func hasExclusion(v v1.RWAHistoryView, reason string, n int) bool {
	for _, e := range v.Excluded {
		if e.Reason == reason && e.Assets == n {
			return true
		}
	}
	return false
}

func exclusionDetail(v v1.RWAHistoryView, reason string) string {
	for _, e := range v.Excluded {
		if e.Reason == reason {
			return e.Detail
		}
	}
	return ""
}
