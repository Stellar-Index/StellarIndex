package aggregate_test

import (
	"errors"
	"math"
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// A real, well-formed issuer G-address so the classic "obscure" test
// assets are valid canonical.Assets (the router treats every asset as
// an opaque comparable key, but valid inputs keep the tests honest).
const testIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// Hub + obscure test assets. XLM/USD/BTC are hubs; OBSCURE / obsAsset /
// obsFiat stand in for thinly-quoted assets that only reach a quote
// currency through a hub.
var (
	xlm      = canonical.NativeAsset()
	usd      = canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}
	gbp      = canonical.Asset{Type: canonical.AssetFiat, Code: "GBP"}
	btc      = canonical.Asset{Type: canonical.AssetCrypto, Code: "BTC"}
	obscure  = canonical.Asset{Type: canonical.AssetClassic, Code: "OBSCURE", Issuer: testIssuer}
	obsAsset = canonical.Asset{Type: canonical.AssetClassic, Code: "OBSA", Issuer: testIssuer}
	obsFiat  = canonical.Asset{Type: canonical.AssetFiat, Code: "NGN"}
)

// ── helpers ─────────────────────────────────────────────────────────

func rq(base, quote canonical.Asset, num, den int64, conf float64) aggregate.Quote {
	return aggregate.Quote{
		Pair:       canonical.Pair{Base: base, Quote: quote},
		Price:      big.NewRat(num, den),
		Confidence: conf,
	}
}

func rl(from, to canonical.Asset, num, den int64, conf float64) aggregate.RouteLeg {
	return aggregate.RouteLeg{From: from, To: to, Price: big.NewRat(num, den), Confidence: conf}
}

func mustEdges(t *testing.T, quotes ...aggregate.Quote) []aggregate.RouteLeg {
	t.Helper()
	edges, err := aggregate.BuildEdges(quotes)
	if err != nil {
		t.Fatalf("BuildEdges: %v", err)
	}
	return edges
}

func eqRat(t *testing.T, got, want *big.Rat, ctx string) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: got nil *big.Rat, want %s", ctx, want.RatString())
	}
	if got.Cmp(want) != 0 {
		t.Errorf("%s: got %s, want %s", ctx, got.RatString(), want.RatString())
	}
}

// ── topology ────────────────────────────────────────────────────────

// 2-hop via XLM hub: OBSCURE→XLM→GBP composite = product; pathCount=1.
func TestRouter_TwoHopViaXLMHub(t *testing.T) {
	edges := mustEdges(t,
		rq(obscure, xlm, 2, 1, 0.9), // 1 OBSCURE = 2 XLM
		rq(xlm, gbp, 3, 10, 0.9),    // 1 XLM = 0.3 GBP
	)

	routes := aggregate.FindRoutes(edges, obscure, gbp, 2, true)
	if len(routes) != 1 {
		t.Fatalf("FindRoutes: got %d routes, want 1", len(routes))
	}
	if len(routes[0]) != 2 {
		t.Fatalf("route length = %d, want 2 legs", len(routes[0]))
	}

	composite, conf, count, _, diverged, low, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0)
	if err != nil {
		t.Fatalf("CombineRoutes: %v", err)
	}
	eqRat(t, composite, big.NewRat(3, 5), "composite") // 2 × 3/10 = 3/5
	if count != 1 {
		t.Errorf("pathCount = %d, want 1", count)
	}
	if diverged {
		t.Error("diverged = true, want false (single clean route)")
	}
	if low {
		t.Error("lowConfidence = true, want false")
	}
	if conf != 0.9 {
		t.Errorf("combinedConfidence = %v, want 0.9", conf)
	}
}

// shortest-preference: with BOTH a 2-hop and a 3-hop route to the same
// target, shortestOnly returns ONLY the 2-hop route.
func TestRouter_ShortestPreference(t *testing.T) {
	edges := mustEdges(t,
		rq(obscure, xlm, 2, 1, 0.9),
		rq(xlm, gbp, 3, 10, 0.9), // enables 2-hop OBSCURE→XLM→GBP
		rq(xlm, usd, 5, 1, 0.9),
		rq(usd, gbp, 4, 5, 0.9), // enables 3-hop OBSCURE→XLM→USD→GBP
	)

	// shortestOnly: exactly the one 2-hop route.
	short := aggregate.FindRoutes(edges, obscure, gbp, 3, true)
	if len(short) != 1 {
		t.Fatalf("shortestOnly: got %d routes, want 1", len(short))
	}
	if len(short[0]) != 2 {
		t.Fatalf("shortestOnly route length = %d, want 2", len(short[0]))
	}
	if !short[0][len(short[0])-1].To.Equal(gbp) || !short[0][len(short[0])-1].From.Equal(xlm) {
		t.Errorf("shortest route final leg = %s→%s, want XLM→GBP",
			short[0][len(short[0])-1].From, short[0][len(short[0])-1].To)
	}

	// Without the shortest filter both the 2-hop and 3-hop appear.
	all := aggregate.FindRoutes(edges, obscure, gbp, 3, false)
	if len(all) != 2 {
		t.Fatalf("all routes: got %d, want 2 (a 2-hop and a 3-hop)", len(all))
	}
	lengths := map[int]bool{}
	for _, r := range all {
		lengths[len(r)] = true
	}
	if !lengths[2] || !lengths[3] {
		t.Errorf("route lengths = %v, want both 2 and 3", lengths)
	}
}

// multi-path corroboration: two DISTINCT 2-hop routes that AGREE →
// combined = median, pathCount=2, diverged=false.
func TestRouter_MultiPathCorroboration(t *testing.T) {
	edges := mustEdges(t,
		rq(obscure, xlm, 2, 1, 0.8), rq(xlm, gbp, 3, 10, 0.8), // A: 2×3/10 = 3/5
		rq(obscure, usd, 3, 1, 0.8), rq(usd, gbp, 1, 5, 0.8), //  B: 3×1/5 = 3/5
	)

	routes := aggregate.FindRoutes(edges, obscure, gbp, 2, true)
	if len(routes) != 2 {
		t.Fatalf("FindRoutes: got %d shortest routes, want 2", len(routes))
	}

	composite, conf, count, _, diverged, low, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0.5)
	if err != nil {
		t.Fatalf("CombineRoutes: %v", err)
	}
	eqRat(t, composite, big.NewRat(3, 5), "composite") // median of two 3/5 = 3/5
	if count != 2 {
		t.Errorf("pathCount = %d, want 2 (corroborated)", count)
	}
	if diverged {
		t.Error("diverged = true, want false (routes agree)")
	}
	if low {
		t.Error("lowConfidence = true, want false")
	}
	if conf != 0.8 {
		t.Errorf("combinedConfidence = %v, want 0.8", conf)
	}
}

// outlier rejection: three same-length routes, one wildly off → outlier
// omitted, composite = median of the 2 good, pathCount=2, diverged=true.
func TestRouter_OutlierRejection(t *testing.T) {
	edges := mustEdges(t,
		rq(obscure, xlm, 2, 1, 0.8), rq(xlm, gbp, 3, 10, 0.8), // 3/5
		rq(obscure, usd, 3, 1, 0.8), rq(usd, gbp, 1, 5, 0.8), //  3/5
		rq(obscure, btc, 1, 1, 0.8), rq(btc, gbp, 6, 1, 0.8), //  6/1  (10× off)
	)

	routes := aggregate.FindRoutes(edges, obscure, gbp, 2, true)
	if len(routes) != 3 {
		t.Fatalf("FindRoutes: got %d shortest routes, want 3", len(routes))
	}

	composite, _, count, _, diverged, low, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0.5)
	if err != nil {
		t.Fatalf("CombineRoutes: %v", err)
	}
	eqRat(t, composite, big.NewRat(3, 5), "composite") // median of the 2 good 3/5
	if count != 2 {
		t.Errorf("pathCount = %d, want 2 (outlier dropped)", count)
	}
	if !diverged {
		t.Error("diverged = false, want true (a route was rejected)")
	}
	if low {
		t.Error("lowConfidence = true, want false")
	}
}

// 4-hop obscure×obscure: resolves at maxHops=4 but NOT at maxHops=3 —
// proves the cap and that the worst case is reachable.
func TestRouter_FourHopObscureToObscure(t *testing.T) {
	edges := mustEdges(t,
		rq(obsAsset, xlm, 2, 1, 0.7),  // OBSA→XLM
		rq(xlm, btc, 1, 1000, 0.7),    // XLM→BTC
		rq(btc, usd, 50000, 1, 0.7),   // BTC→USD
		rq(usd, obsFiat, 100, 1, 0.7), // USD→OBS_FIAT
	)

	// 3 hops cannot reach it.
	if got := aggregate.FindRoutes(edges, obsAsset, obsFiat, 3, true); got != nil {
		t.Fatalf("FindRoutes maxHops=3: got %d routes, want none", len(got))
	}
	if _, _, _, _, _, _, err := aggregate.CombineRoutes(edges, obsAsset, obsFiat, 3, 0); !errors.Is(err, aggregate.ErrNoRoute) {
		t.Fatalf("CombineRoutes maxHops=3: err = %v, want ErrNoRoute", err)
	}

	// 4 hops reaches it.
	routes := aggregate.FindRoutes(edges, obsAsset, obsFiat, 4, true)
	if len(routes) != 1 || len(routes[0]) != 4 {
		t.Fatalf("FindRoutes maxHops=4: got %d routes (first len %d), want 1 route of 4 legs",
			len(routes), routeLen(routes))
	}
	composite, _, count, _, _, _, err := aggregate.CombineRoutes(edges, obsAsset, obsFiat, 4, 0)
	if err != nil {
		t.Fatalf("CombineRoutes maxHops=4: %v", err)
	}
	// 2 × 1/1000 × 50000 × 100 = 10000
	eqRat(t, composite, big.NewRat(10000, 1), "composite")
	if count != 1 {
		t.Errorf("pathCount = %d, want 1", count)
	}
}

func routeLen(routes [][]aggregate.RouteLeg) int {
	if len(routes) == 0 {
		return 0
	}
	return len(routes[0])
}

// inverse edges: only XLM/OBSCURE is priced (not OBSCURE/XLM) — routing
// must still work off the exact rational inverse.
func TestRouter_InverseEdges(t *testing.T) {
	edges := mustEdges(t,
		rq(xlm, obscure, 1, 2, 0.9), // 1 XLM = 0.5 OBSCURE  ⇒ OBSCURE→XLM = 2
		rq(xlm, gbp, 3, 10, 0.9),
	)
	composite, _, count, _, _, _, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0)
	if err != nil {
		t.Fatalf("CombineRoutes: %v", err)
	}
	eqRat(t, composite, big.NewRat(3, 5), "composite") // inv(1/2)=2, ×3/10 = 3/5
	if count != 1 {
		t.Errorf("pathCount = %d, want 1", count)
	}
}

// no-cycle / no-route: a fully cyclic graph plus a disconnected quote
// must terminate and return no route (never loop forever).
func TestRouter_NoCycleNoRoute(t *testing.T) {
	edges := mustEdges(t,
		rq(obscure, xlm, 2, 1, 0.9), // cycle: OBSCURE→XLM→USD→OBSCURE
		rq(xlm, usd, 5, 1, 0.9),
		rq(usd, obscure, 1, 10, 0.9),
	)
	// GBP is not in the graph at all.
	if got := aggregate.FindRoutes(edges, obscure, gbp, 5, true); got != nil {
		t.Fatalf("FindRoutes to disconnected quote: got %d routes, want none", len(got))
	}
	if _, _, _, _, _, _, err := aggregate.CombineRoutes(edges, obscure, gbp, 5, 0); !errors.Is(err, aggregate.ErrNoRoute) {
		t.Fatalf("CombineRoutes to disconnected quote: err = %v, want ErrNoRoute", err)
	}
	// A reachable quote inside the cycle still resolves (proves the walk
	// terminated because it found the target, not because it gave up).
	if got := aggregate.FindRoutes(edges, obscure, usd, 5, true); len(got) == 0 {
		t.Error("FindRoutes to a reachable in-cycle quote returned nothing")
	}
}

// ── exact arithmetic (ADR-0003) ─────────────────────────────────────

// 1/3 chains stay exact — no float rounding may creep into a composite.
func TestRouter_ExactRationalNoFloatDrift(t *testing.T) {
	third := func(from, to canonical.Asset) aggregate.RouteLeg {
		return rl(from, to, 1, 3, 1.0)
	}
	two, err := aggregate.CompositeRate([]aggregate.RouteLeg{third(obscure, xlm), third(xlm, gbp)})
	if err != nil {
		t.Fatalf("CompositeRate 2-leg: %v", err)
	}
	eqRat(t, two, big.NewRat(1, 9), "1/3 × 1/3")

	three, err := aggregate.CompositeRate([]aggregate.RouteLeg{
		third(obscure, xlm), third(xlm, usd), third(usd, gbp),
	})
	if err != nil {
		t.Fatalf("CompositeRate 3-leg: %v", err)
	}
	eqRat(t, three, big.NewRat(1, 27), "1/3 × 1/3 × 1/3")

	// Through the full CombineRoutes path as well.
	edges := mustEdges(t, rq(obscure, xlm, 1, 3, 1.0), rq(xlm, gbp, 1, 3, 1.0))
	composite, _, _, _, _, _, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0)
	if err != nil {
		t.Fatalf("CombineRoutes: %v", err)
	}
	eqRat(t, composite, big.NewRat(1, 9), "combined 1/3 × 1/3")
}

// ── confidence (weakest-link) ───────────────────────────────────────

// (a) A route through ONE low-confidence edge has route confidence ==
// that low value, and CombineRoutes flags lowConfidence when it is the
// only route (and it sits below the floor).
func TestRouter_WeakestLinkConfidence(t *testing.T) {
	edges := mustEdges(t,
		rq(obscure, xlm, 2, 1, 0.9), // strong leg
		rq(xlm, gbp, 3, 10, 0.1),    // flimsy leg — caps the route
	)
	routes := aggregate.FindRoutes(edges, obscure, gbp, 2, true)
	if len(routes) != 1 {
		t.Fatalf("FindRoutes: got %d routes, want 1", len(routes))
	}
	if rc := aggregate.RouteConfidence(routes[0]); rc != 0.1 {
		t.Errorf("RouteConfidence = %v, want 0.1 (weakest link)", rc)
	}

	composite, conf, count, _, _, low, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0.5)
	if err != nil {
		t.Fatalf("CombineRoutes: %v", err)
	}
	eqRat(t, composite, big.NewRat(3, 5), "best-effort composite") // still computed
	if !low {
		t.Error("lowConfidence = false, want true (only route is below the floor)")
	}
	if conf != 0.1 {
		t.Errorf("combinedConfidence = %v, want 0.1", conf)
	}
	if count != 1 {
		t.Errorf("pathCount = %d, want 1", count)
	}
}

// (b) Two same-length routes, one high-conf and one low-conf: the
// low-conf route is EXCLUDED by the floor and does not drag the
// composite toward its (bad, dust-inflated) value.
func TestRouter_LowConfidenceRouteExcluded(t *testing.T) {
	edges := mustEdges(t,
		rq(obscure, xlm, 2, 1, 0.9), rq(xlm, gbp, 3, 10, 0.9), // A: 3/5, conf 0.9
		rq(obscure, usd, 6, 1, 0.1), rq(usd, gbp, 1, 1, 0.9), //  B: 6/1, conf 0.1
	)

	// With the floor, only the trustworthy route backs the composite.
	composite, conf, count, _, diverged, low, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0.5)
	if err != nil {
		t.Fatalf("CombineRoutes (floor 0.5): %v", err)
	}
	eqRat(t, composite, big.NewRat(3, 5), "gated composite") // NOT dragged toward 6
	if count != 1 {
		t.Errorf("pathCount = %d, want 1 (low-conf route excluded)", count)
	}
	if conf != 0.9 {
		t.Errorf("combinedConfidence = %v, want 0.9", conf)
	}
	if low {
		t.Error("lowConfidence = true, want false (a route cleared the floor)")
	}
	if diverged {
		t.Error("diverged = true, want false (single trusted survivor)")
	}

	// Control: even WITHOUT the confidence floor the bad route can no longer
	// drag the served price — highestConfidencePrice serves the MAX-confidence
	// survivor (0.9 → 3/5), independently of the floor. The dust route still
	// counts toward pathCount and sets diverged (so the composite is served
	// FLAGGED), but it does not move the value. Two independent protections:
	// the floor EXCLUDES it, confidence-weighted serving REFUSES to blend it.
	served, _, count0, _, diverged0, _, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0)
	if err != nil {
		t.Fatalf("CombineRoutes (floor 0): %v", err)
	}
	// NOT median(3/5, 6) = 33/10: served = the 0.9-confidence route alone.
	eqRat(t, served, big.NewRat(3, 5), "un-gated composite")
	if count0 != 2 {
		t.Errorf("un-gated pathCount = %d, want 2 (both routes survive)", count0)
	}
	if !diverged0 {
		t.Error("un-gated diverged = false, want true (routes disagree widely)")
	}
}

// (c) All routes below the floor → a best-effort composite is still
// returned, but lowConfidence=true so the caller serves it flagged.
func TestRouter_AllRoutesBelowFloor(t *testing.T) {
	edges := mustEdges(t,
		rq(obscure, xlm, 2, 1, 0.10), rq(xlm, gbp, 3, 10, 0.10), // A: 3/5, conf 0.10
		rq(obscure, usd, 3, 1, 0.15), rq(usd, gbp, 1, 5, 0.15), //  B: 3/5, conf 0.15
	)
	composite, conf, count, _, diverged, low, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0.5)
	if err != nil {
		t.Fatalf("CombineRoutes: %v", err)
	}
	eqRat(t, composite, big.NewRat(3, 5), "best-effort composite")
	if !low {
		t.Error("lowConfidence = false, want true (no route cleared the floor)")
	}
	if count != 2 {
		t.Errorf("pathCount = %d, want 2 (both used best-effort)", count)
	}
	if conf != 0.15 {
		t.Errorf("combinedConfidence = %v, want 0.15 (best of the weak routes)", conf)
	}
	if diverged {
		t.Error("diverged = true, want false (weak but agreeing)")
	}
}

// ── unit-level guards ───────────────────────────────────────────────

func TestBuildEdges_RejectsNonPositivePrice(t *testing.T) {
	for name, price := range map[string]*big.Rat{
		"nil":  nil,
		"zero": new(big.Rat),
		"neg":  big.NewRat(-1, 1),
	} {
		_, err := aggregate.BuildEdges([]aggregate.Quote{{
			Pair: canonical.Pair{Base: obscure, Quote: xlm}, Price: price, Confidence: 1,
		}})
		if !errors.Is(err, aggregate.ErrLegNonPositive) {
			t.Errorf("%s: err = %v, want ErrLegNonPositive", name, err)
		}
	}
}

func TestBuildEdges_AddsBothDirections(t *testing.T) {
	edges := mustEdges(t, rq(obscure, xlm, 2, 1, 0.5))
	if len(edges) != 2 {
		t.Fatalf("got %d edges, want 2 (both directions)", len(edges))
	}
	var fwd, inv *big.Rat
	for _, e := range edges {
		switch {
		case e.From.Equal(obscure) && e.To.Equal(xlm):
			fwd = e.Price
		case e.From.Equal(xlm) && e.To.Equal(obscure):
			inv = e.Price
		}
	}
	eqRat(t, fwd, big.NewRat(2, 1), "forward edge")
	eqRat(t, inv, big.NewRat(1, 2), "inverse edge") // exact 1/p
}

func TestCompositeRate_Errors(t *testing.T) {
	if _, err := aggregate.CompositeRate(nil); !errors.Is(err, aggregate.ErrEmptyRoute) {
		t.Errorf("empty: err = %v, want ErrEmptyRoute", err)
	}
	// legs that do not chain: obscure→xlm then usd→gbp (usd != xlm)
	broken := []aggregate.RouteLeg{rl(obscure, xlm, 2, 1, 1), rl(usd, gbp, 3, 1, 1)}
	if _, err := aggregate.CompositeRate(broken); !errors.Is(err, aggregate.ErrRouteBroken) {
		t.Errorf("broken chain: err = %v, want ErrRouteBroken", err)
	}
	// non-positive leg
	badleg := []aggregate.RouteLeg{{From: obscure, To: xlm, Price: new(big.Rat), Confidence: 1}}
	if _, err := aggregate.CompositeRate(badleg); !errors.Is(err, aggregate.ErrLegNonPositive) {
		t.Errorf("non-positive leg: err = %v, want ErrLegNonPositive", err)
	}
}

func TestOmitOutliers_Semantics(t *testing.T) {
	rats := func(ns ...int64) []*big.Rat {
		out := make([]*big.Rat, len(ns))
		for i, n := range ns {
			out[i] = big.NewRat(n, 1)
		}
		return out
	}

	// Fewer than 3 → all kept (can't robustly pick an outlier).
	if got := aggregate.OmitOutliers(rats(10, 1000), 40); len(got) != 2 {
		t.Errorf("n=2: kept %d, want 2 (passthrough)", len(got))
	}
	// 3 values, one far → the far one is dropped (median baseline).
	got := aggregate.OmitOutliers(rats(100, 102, 500), 40)
	if len(got) != 2 {
		t.Fatalf("n=3 outlier: kept %d, want 2", len(got))
	}
	for _, r := range got {
		if r.Cmp(big.NewRat(200, 1)) > 0 {
			t.Errorf("outlier not dropped: %s", r.RatString())
		}
	}
	// Tight cluster → all kept.
	if got := aggregate.OmitOutliers(rats(100, 101, 99, 100), 40); len(got) != 4 {
		t.Errorf("tight cluster: kept %d, want 4", len(got))
	}
}

func TestRouteConfidence_Empty(t *testing.T) {
	if c := aggregate.RouteConfidence(nil); c != 0 {
		t.Errorf("RouteConfidence(nil) = %v, want 0", c)
	}
}

// ── corroboration integrity (R1/R2) ─────────────────────────────────
//
// The corroboration count CombineRoutes returns is the number fed to the
// anomaly-freeze source_count leg. It must count only INDEPENDENT +
// TIGHTLY-AGREEING + NON-DIVERGED routes, or a manipulator games the
// freeze. These pin each of those three properties.

// R2 core: the maximum edge-disjoint subset counts two routes that share
// ANY undirected edge as ONE independent confirmation.
func TestRouter_MaxEdgeDisjointRoutes(t *testing.T) {
	// Two 3-hop routes that converge on the SAME final edge USD→GBP (the
	// shared bottleneck): a single wash-traded USD/GBP market backs both,
	// so there is really one independent confirmation, not two.
	bottleneckA := []aggregate.RouteLeg{
		rl(obscure, xlm, 2, 1, 0.9), rl(xlm, usd, 5, 1, 0.9), rl(usd, gbp, 3, 10, 0.9),
	}
	bottleneckB := []aggregate.RouteLeg{
		rl(obscure, btc, 1, 1, 0.9), rl(btc, usd, 10, 1, 0.9), rl(usd, gbp, 3, 10, 0.9),
	}
	if got := aggregate.MaxEdgeDisjointRoutes([][]aggregate.RouteLeg{bottleneckA, bottleneckB}); got != 1 {
		t.Errorf("shared-bottleneck routes: MaxEdgeDisjointRoutes = %d, want 1 "+
			"(both funnel through USD→GBP — one manipulable market, not two)", got)
	}

	// Two 2-hop routes through DISTINCT hubs share no edge → 2 independent.
	disjointA := []aggregate.RouteLeg{rl(obscure, xlm, 2, 1, 0.9), rl(xlm, gbp, 3, 10, 0.9)}
	disjointB := []aggregate.RouteLeg{rl(obscure, usd, 3, 1, 0.9), rl(usd, gbp, 1, 5, 0.9)}
	if got := aggregate.MaxEdgeDisjointRoutes([][]aggregate.RouteLeg{disjointA, disjointB}); got != 2 {
		t.Errorf("edge-disjoint routes: MaxEdgeDisjointRoutes = %d, want 2", got)
	}

	// Undirected edge identity: A→B and B→A are the SAME market. A route
	// using XLM→USD and one using USD→XLM are NOT independent.
	fwdLeg := []aggregate.RouteLeg{rl(obscure, xlm, 2, 1, 0.9), rl(xlm, usd, 5, 1, 0.9)}
	invLeg := []aggregate.RouteLeg{rl(gbp, usd, 4, 1, 0.9), rl(usd, xlm, 1, 5, 0.9)}
	if got := aggregate.MaxEdgeDisjointRoutes([][]aggregate.RouteLeg{fwdLeg, invLeg}); got != 1 {
		t.Errorf("inverse shared edge {XLM,USD}: MaxEdgeDisjointRoutes = %d, want 1 "+
			"(an edge and its inverse are one physical market)", got)
	}

	// A single route is trivially 1; an empty set is 0.
	if got := aggregate.MaxEdgeDisjointRoutes([][]aggregate.RouteLeg{disjointA}); got != 1 {
		t.Errorf("single route: MaxEdgeDisjointRoutes = %d, want 1", got)
	}
	if got := aggregate.MaxEdgeDisjointRoutes(nil); got != 0 {
		t.Errorf("empty set: MaxEdgeDisjointRoutes = %d, want 0", got)
	}
}

// R1: two routes that survive the LOOSE 40% outlier band but disagree by
// more than the tight corroboration band do NOT corroborate — the
// corroboration count is 0 even though pathCount (the serving
// multiplicity) is 2 and nothing diverged.
func TestRouter_CorroborationRequiresTightAgreement(t *testing.T) {
	edges := mustEdges(t,
		rq(obscure, xlm, 10, 1, 0.9), rq(xlm, gbp, 10, 1, 0.9), // A: 100
		rq(obscure, usd, 10, 1, 0.9), rq(usd, gbp, 23, 2, 0.9), //  B: 115  (+15%)
	)
	composite, _, pathCount, corroboration, diverged, low, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0)
	if err != nil {
		t.Fatalf("CombineRoutes: %v", err)
	}
	if low {
		t.Fatal("lowConfidence=true, want false (both routes clear the 0 floor)")
	}
	if diverged {
		t.Error("diverged=true, want false (100 vs 115 is inside the 40% loose band)")
	}
	if pathCount != 2 {
		t.Errorf("pathCount=%d, want 2 (both routes still serve the median)", pathCount)
	}
	if corroboration != 0 {
		t.Errorf("corroborationCount=%d, want 0 — routes 15%% apart are inside the loose "+
			"band but NOT tightly agreeing, so neither corroborates the other", corroboration)
	}
	// Both routes are co-equal top confidence (0.9) but 15% apart — the
	// bimodal/co-equal case. The served composite is a value a route ACTUALLY
	// produced (the lower cluster, 100), NOT the unproduced midpoint 107.5 an
	// averaging median would blend across two disagreeing routes (H1).
	eqRat(t, composite, big.NewRat(100, 1), "served member = a produced value, not the blended midpoint")
}

// R1: a DIVERGED combine (an outlier route was rejected) never
// corroborates, even though two clean survivors remain and agree.
func TestRouter_CorroborationZeroWhenDiverged(t *testing.T) {
	edges := mustEdges(t,
		rq(obscure, xlm, 2, 1, 0.9), rq(xlm, gbp, 3, 10, 0.9), // 3/5
		rq(obscure, usd, 3, 1, 0.9), rq(usd, gbp, 1, 5, 0.9), //  3/5
		rq(obscure, btc, 1, 1, 0.9), rq(btc, gbp, 6, 1, 0.9), //  6/1  (outlier)
	)
	_, _, pathCount, corroboration, diverged, _, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0)
	if err != nil {
		t.Fatalf("CombineRoutes: %v", err)
	}
	if !diverged {
		t.Fatal("diverged=false, want true (the 6/1 route is an outlier)")
	}
	if pathCount != 2 {
		t.Errorf("pathCount=%d, want 2 (two clean survivors)", pathCount)
	}
	if corroboration != 0 {
		t.Errorf("corroborationCount=%d, want 0 — a diverged combine is a manipulation "+
			"signal and must not suppress the freeze even with agreeing survivors", corroboration)
	}
}

// R2: two SHORTEST routes that agree tightly but share a bottleneck edge
// count as ONE independent confirmation (corroborationCount=1), while
// pathCount is 2. The shipped freeze needs ≥2 INDEPENDENT to suppress.
func TestRouter_CorroborationSharedBottleneck(t *testing.T) {
	// Only 3-hop routes reach GBP, and both funnel through USD→GBP.
	edges := mustEdges(t,
		rq(obscure, xlm, 2, 1, 0.9), rq(xlm, usd, 5, 1, 0.9), // obscure→USD via XLM = 10
		rq(obscure, btc, 1, 1, 0.9), rq(btc, usd, 10, 1, 0.9), // obscure→USD via BTC = 10
		rq(usd, gbp, 3, 10, 0.9), // the shared bottleneck into GBP
	)
	composite, _, pathCount, corroboration, diverged, _, err := aggregate.CombineRoutes(edges, obscure, gbp, 3, 0)
	if err != nil {
		t.Fatalf("CombineRoutes: %v", err)
	}
	eqRat(t, composite, big.NewRat(3, 1), "composite") // 10 × 3/10 = 3, both routes
	if diverged {
		t.Error("diverged=true, want false (routes agree exactly)")
	}
	if pathCount != 2 {
		t.Errorf("pathCount=%d, want 2 (two surviving routes)", pathCount)
	}
	if corroboration != 1 {
		t.Errorf("corroborationCount=%d, want 1 — both routes share the USD→GBP edge, so "+
			"they are one independent confirmation, not two", corroboration)
	}
}

// R2: two genuinely edge-disjoint, tightly-agreeing routes DO corroborate
// (corroborationCount=2). Companion to the bottleneck case.
func TestRouter_CorroborationEdgeDisjointAgreeing(t *testing.T) {
	edges := mustEdges(t,
		rq(obscure, xlm, 2, 1, 0.9), rq(xlm, gbp, 3, 10, 0.9), // A: 3/5
		rq(obscure, usd, 3, 1, 0.9), rq(usd, gbp, 1, 5, 0.9), //  B: 3/5
	)
	_, _, pathCount, corroboration, diverged, _, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0)
	if err != nil {
		t.Fatalf("CombineRoutes: %v", err)
	}
	if diverged {
		t.Error("diverged=true, want false")
	}
	if pathCount != 2 || corroboration != 2 {
		t.Errorf("(pathCount, corroborationCount) = (%d, %d), want (2, 2) — two edge-disjoint "+
			"agreeing routes are two independent confirmations", pathCount, corroboration)
	}
}

// A single route corroborates nothing but reports 1 (the baseline that,
// MAX-ed against the direct source count, can never raise it) — keeps the
// single-route shipped config byte-identical.
func TestRouter_CorroborationSingleRoute(t *testing.T) {
	edges := mustEdges(t, rq(obscure, xlm, 2, 1, 0.9), rq(xlm, gbp, 3, 10, 0.9))
	_, _, pathCount, corroboration, _, _, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0)
	if err != nil {
		t.Fatalf("CombineRoutes: %v", err)
	}
	if pathCount != 1 || corroboration != 1 {
		t.Errorf("(pathCount, corroborationCount) = (%d, %d), want (1, 1)", pathCount, corroboration)
	}
}

// Step 3.5 — a lower-confidence route CORROBORATES but never moves the served
// price. This is what makes enabling a thin, USD-volume-floor-EXEMPT bridge
// (XLM→BTC→GBP, where the crypto-quoted XLM/BTC leg escapes dropForMinUSDVolume)
// safe: it can widen the freeze source_count when it agrees, but a manipulator
// wash-trading the thin leg cannot drag the served cross — the deep route sets
// the price.
func TestRouter_ServesHighestConfidenceRoute(t *testing.T) {
	// Deep route A (conf 0.95) prices obscure/GBP at 3/5 = 0.6; thin bridge
	// route B (conf 0.20) is 20% higher at 18/25 = 0.72 — inside the loose 40%
	// outlier band, so both survive omission.
	edges := mustEdges(t,
		rq(obscure, xlm, 2, 1, 0.95), rq(xlm, gbp, 3, 10, 0.95), // A: 3/5,  conf 0.95
		rq(obscure, usd, 3, 1, 0.20), rq(usd, gbp, 6, 25, 0.20), //  B: 18/25, conf 0.20
	)
	composite, conf, pathCount, corroboration, _, _, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0)
	if err != nil {
		t.Fatalf("CombineRoutes: %v", err)
	}
	// Served = the deep route's price, NOT median(3/5, 18/25). The thin route
	// cannot move it.
	eqRat(t, composite, big.NewRat(3, 5), "served = highest-confidence route")
	if conf != 0.95 {
		t.Errorf("combinedConfidence = %v, want 0.95 (the served route's confidence)", conf)
	}
	if pathCount != 2 {
		t.Errorf("pathCount = %d, want 2 (both routes survive)", pathCount)
	}
	// 20% apart → outside the 3% tight band → they do not corroborate either.
	if corroboration != 0 {
		t.Errorf("corroborationCount = %d, want 0 (20%% apart, not tightly agreeing)", corroboration)
	}

	// Manipulation check: move the THIN route's price (as a manipulator
	// wash-trading the unguarded bridge would). While it stays inside the band
	// the served price is UNCHANGED — the deep route alone sets it.
	moved := mustEdges(t,
		rq(obscure, xlm, 2, 1, 0.95), rq(xlm, gbp, 3, 10, 0.95), // A: 3/5, conf 0.95
		rq(obscure, usd, 3, 1, 0.20), rq(usd, gbp, 11, 50, 0.20), // B moved to 33/50 = 0.66
	)
	movedComposite, _, _, _, _, _, err := aggregate.CombineRoutes(moved, obscure, gbp, 2, 0)
	if err != nil {
		t.Fatalf("CombineRoutes (moved): %v", err)
	}
	eqRat(t, movedComposite, big.NewRat(3, 5), "served price unmoved by thin-route manipulation")
}

// ── serving anchor (H1) ─────────────────────────────────────────────

// H1 (a): a deep, high-confidence route must set the served price even when
// a thin low-confidence MAJORITY would evict it as a price-median outlier. At
// n≥3 the median-relative outlier filter is confidence-blind, so before the
// fix the two thin routes (the majority) evicted the deep route and served
// their own 0.3; the served composite must instead be the deep route's 0.6.
func TestRouter_DeepRouteProtectedFromThinMajority(t *testing.T) {
	edges := mustEdges(t,
		rq(obscure, xlm, 2, 1, 0.9), rq(xlm, gbp, 3, 10, 0.9), // A: 0.6, conf 0.9 (deep)
		rq(obscure, usd, 3, 1, 0.1), rq(usd, gbp, 1, 10, 0.9), //  B: 0.3, conf 0.1 (thin)
		rq(obscure, btc, 3, 1, 0.1), rq(btc, gbp, 1, 10, 0.9), //  C: 0.3, conf 0.1 (thin)
	)
	composite, conf, pathCount, corroboration, diverged, low, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0)
	if err != nil {
		t.Fatalf("CombineRoutes: %v", err)
	}
	// Served = the DEEP route's price (0.6), not the thin majority's 0.3.
	eqRat(t, composite, big.NewRat(3, 5), "served = deep high-confidence route")
	if conf != 0.9 {
		t.Errorf("combinedConfidence = %v, want 0.9 (the served deep route's confidence, not the "+
			"thin survivors' 0.1)", conf)
	}
	if low {
		t.Error("lowConfidence = true, want false (the deep route clears the floor)")
	}
	// The thin majority still shapes the divergence/serving-population signals:
	// the deep route is the median-relative outlier among the three, so it is
	// dropped from survivors → pathCount 2, diverged true.
	if pathCount != 2 {
		t.Errorf("pathCount = %d, want 2 (two thin survivors after the deep route is omitted)", pathCount)
	}
	if !diverged {
		t.Error("diverged = false, want true (the deep route diverges from the thin majority)")
	}
	// Thin routes (conf 0.1) are below the corroboration floor AND the combine
	// diverged, so nothing corroborates.
	if corroboration != 0 {
		t.Errorf("corroborationCount = %d, want 0 (thin + diverged)", corroboration)
	}
}

// H1 (b): two genuinely co-equal top-confidence routes that DISAGREE (two
// clusters >40% apart) must serve a price ONE cluster actually produced, not
// the averaged midpoint. Before the fix medianRat blended them into an
// unproduced value.
func TestRouter_BimodalCoEqualServesProducedValue(t *testing.T) {
	edges := mustEdges(t,
		rq(obscure, xlm, 2, 1, 0.9), rq(xlm, gbp, 3, 10, 0.9), // A: 0.6, conf 0.9
		rq(obscure, usd, 2, 1, 0.9), rq(usd, gbp, 1, 2, 0.9), //  B: 1.0, conf 0.9
	)
	composite, conf, _, _, _, _, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0)
	if err != nil {
		t.Fatalf("CombineRoutes: %v", err)
	}
	// Served = the lower produced cluster (0.6), NOT median(0.6, 1.0) = 0.8.
	eqRat(t, composite, big.NewRat(3, 5), "served = a produced cluster, not the midpoint")
	if composite.Cmp(big.NewRat(4, 5)) == 0 {
		t.Error("served the unproduced midpoint 0.8 — a blend of two disagreeing co-equal routes (H1)")
	}
	if conf != 0.9 {
		t.Errorf("combinedConfidence = %v, want 0.9", conf)
	}
}

// ── corroboration confidence floor (M2) ─────────────────────────────

// M2: a thin route (weakest-link confidence below the corroboration floor)
// that agrees tightly AND is edge-disjoint must NOT raise the corroboration
// count. With min_route_confidence shipped at 0 the thin route still serves +
// survives, but a route too thin to trust cannot be an independent
// confirmation the freeze relies on.
func TestRouter_CorroborationRequiresConfidenceFloor(t *testing.T) {
	edges := mustEdges(t,
		rq(obscure, xlm, 2, 1, 0.9), rq(xlm, gbp, 3, 10, 0.9), // A: 3/5, conf 0.9
		rq(obscure, usd, 3, 1, 0.2), rq(usd, gbp, 1, 5, 0.9), //  B: 3/5, conf 0.2 (thin)
	)
	_, _, pathCount, corroboration, diverged, low, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0)
	if err != nil {
		t.Fatalf("CombineRoutes: %v", err)
	}
	if low {
		t.Fatal("lowConfidence = true, want false (both routes clear the 0 floor)")
	}
	if diverged {
		t.Error("diverged = true, want false (both routes are 3/5)")
	}
	if pathCount != 2 {
		t.Errorf("pathCount = %d, want 2 (both routes still serve)", pathCount)
	}
	if corroboration != 0 {
		t.Errorf("corroborationCount = %d, want 0 — route B's weakest leg (conf 0.2) is below the "+
			"corroboration confidence floor, so a thin route must not count as an independent "+
			"confirmation even though it agrees exactly and is edge-disjoint", corroboration)
	}
}

// ── reverse-oriented market dedup (M3) ──────────────────────────────

// M3: a market quoted in BOTH orientations (XLM/OBSCURE and OBSCURE/XLM) is
// ONE physical market. The dedup must yield the same edge set + route set as
// one orientation alone — not a double-counted market that becomes two routes.
func TestBuildEdges_DedupsReverseOrientedMarket(t *testing.T) {
	oneOrientation := mustEdges(t,
		rq(obscure, xlm, 2, 1, 0.9),
		rq(xlm, gbp, 3, 10, 0.9),
	)
	bothOrientations := mustEdges(t,
		rq(obscure, xlm, 2, 1, 0.9),
		rq(xlm, obscure, 1, 2, 0.9), // reverse of the SAME market — exact inverse
		rq(xlm, gbp, 3, 10, 0.9),
	)
	if len(bothOrientations) != len(oneOrientation) {
		t.Errorf("both-orientation graph has %d edges, want %d — a market quoted both ways is one "+
			"market, not two (M3)", len(bothOrientations), len(oneOrientation))
	}
	// One 2-hop route to GBP, not two duplicate routes off the doubled edge.
	routes := aggregate.FindRoutes(bothOrientations, obscure, gbp, 2, true)
	if len(routes) != 1 {
		t.Errorf("both-orientation graph yields %d routes to GBP, want 1 (the reverse-oriented "+
			"duplicate must not become a second route)", len(routes))
	}
	_, _, pathCount, _, _, _, err := aggregate.CombineRoutes(bothOrientations, obscure, gbp, 2, 0)
	if err != nil {
		t.Fatalf("CombineRoutes: %v", err)
	}
	if pathCount != 1 {
		t.Errorf("pathCount = %d, want 1 (the deduped market backs one route)", pathCount)
	}
}

// ── non-finite confidence guard (L5) ────────────────────────────────

// L5: a quote whose confidence is NaN must not panic the router. Pre-fix the
// NaN propagated through RouteConfidence → maxConfidence, never matched
// `== best`, emptied the served top tier and panicked the median. Post-fix
// the confidence is clamped to 0 (fail-closed) and a sensible price is served.
func TestRouter_NonFiniteConfidenceNoPanic(t *testing.T) {
	edges := mustEdges(t,
		aggregate.Quote{Pair: canonical.Pair{Base: obscure, Quote: xlm}, Price: big.NewRat(2, 1), Confidence: math.NaN()},
		rq(xlm, gbp, 3, 10, 0.9),
	)
	composite, conf, _, _, _, _, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0)
	if err != nil {
		t.Fatalf("CombineRoutes: %v", err)
	}
	// Route confidence = min(clamped 0, 0.9) = 0; still serves the exact price.
	eqRat(t, composite, big.NewRat(3, 5), "served composite despite a NaN edge confidence")
	if math.IsNaN(conf) || math.IsInf(conf, 0) {
		t.Errorf("combinedConfidence = %v, want a finite value (a NaN confidence must be clamped)", conf)
	}
}

// ── direct-market corroboration (D1, 2026-08-28) ────────────────────

// A structurally single-venue target (XLM/GBP: one venue) has exactly ONE
// hub route once its direct edge is excluded from routing, so the router
// alone can never count above 1 and the freeze's source_count widening was
// dead in production. CombineRoutesWithDirect enters the direct print as
// the second, edge-disjoint opinion: a confident hub route that tightly
// agrees with it corroborates it (2). The SERVED composite, pathCount and
// flags are byte-identical to CombineRoutes — only the count moves.
func TestRouter_DirectMarketCorroboratedBySingleHubRoute(t *testing.T) {
	edges := mustEdges(t,
		rq(xlm, usd, 1, 10, 0.9), // XLM/USD 0.10, deep multi-venue leg
		rq(usd, gbp, 4, 5, 1.0),  //  USD/GBP 0.80, institutional FX snap
	) // caller has already excluded the direct XLM/GBP edge
	// Direct XLM/GBP 0.0805 — 0.6% off the 0.08 composite, well inside the
	// 3% band. Thin (conf 0.3): the direct print is the price UNDER TEST,
	// not a confirming route, so its own confidence is not floor-gated.
	direct := rq(xlm, gbp, 805, 10000, 0.3)

	composite, conf, pathCount, corroboration, diverged, low, err := aggregate.CombineRoutesWithDirect(
		edges, xlm, gbp, 2, 0, direct)
	if err != nil {
		t.Fatalf("CombineRoutesWithDirect: %v", err)
	}
	// Serving unchanged: the composite is the hub route's product, never
	// the direct print, never a blend.
	eqRat(t, composite, big.NewRat(2, 25), "composite (XLM→USD→GBP)")
	if conf != 0.9 || pathCount != 1 || diverged || low {
		t.Errorf("serving fields moved: conf=%v pathCount=%d diverged=%v low=%v, want 0.9/1/false/false",
			conf, pathCount, diverged, low)
	}
	if corroboration != 2 {
		t.Errorf("corroborationCount = %d, want 2 — the confident XLM→USD→GBP route reproduces "+
			"the single-venue direct print within the tight band; direct + hub route are "+
			"edge-disjoint and both count", corroboration)
	}

	// Baseline: the same graph WITHOUT the direct print is still 1.
	_, _, _, routerOnly, _, _, err := aggregate.CombineRoutes(edges, xlm, gbp, 2, 0)
	if err != nil {
		t.Fatalf("CombineRoutes: %v", err)
	}
	if routerOnly != 1 {
		t.Errorf("router-only corroborationCount = %d, want 1", routerOnly)
	}
}

// A DIVERGENT composite must not corroborate the direct print, and a thin
// hub route must not either: in both cases the count falls back to the
// router-only answer (1), so the freeze's source_count leg sees the raw
// trade-source count. Also pins that a direct print the caller failed to
// exclude from routing collapses with its own 1-hop route (shared edge).
func TestRouter_DirectMarketNotCorroboratedWhenDivergentOrThin(t *testing.T) {
	deep := mustEdges(t, rq(xlm, usd, 1, 10, 0.9), rq(usd, gbp, 4, 5, 1.0)) // composite 0.08

	// Direct 0.085 vs composite 0.08: 6.25% against the smaller — outside
	// the 3% corroboration band (the divergent-composite case).
	_, _, _, divergent, _, _, err := aggregate.CombineRoutesWithDirect(
		deep, xlm, gbp, 2, 0, rq(xlm, gbp, 85, 1000, 0.9))
	if err != nil {
		t.Fatalf("divergent: %v", err)
	}
	if divergent != 1 {
		t.Errorf("divergent direct: corroborationCount = %d, want 1 — a composite 6.25%% away "+
			"from the direct print confirms nothing", divergent)
	}

	// Borderline: exactly 3% (0.0824 vs 0.08 → 3.0% of the smaller) is
	// still inside the closed band; 3.1% is not.
	_, _, _, atBand, _, _, _ := aggregate.CombineRoutesWithDirect(deep, xlm, gbp, 2, 0, rq(xlm, gbp, 824, 10000, 0.9))
	_, _, _, pastBand, _, _, _ := aggregate.CombineRoutesWithDirect(deep, xlm, gbp, 2, 0, rq(xlm, gbp, 8248, 100000, 0.9))
	if atBand != 2 || pastBand != 1 {
		t.Errorf("band edge: at 3.0%% = %d (want 2), at 3.1%% = %d (want 1)", atBand, pastBand)
	}

	// Thin hub route (XLM/USD conf 0.2 < corroborationMinConfidence): the
	// CONFIRMING route is floor-gated even though the direct print agrees.
	thin := mustEdges(t, rq(xlm, usd, 1, 10, 0.2), rq(usd, gbp, 4, 5, 1.0))
	_, _, _, thinCount, _, _, err := aggregate.CombineRoutesWithDirect(
		thin, xlm, gbp, 2, 0, rq(xlm, gbp, 2, 25, 0.9))
	if err != nil {
		t.Fatalf("thin: %v", err)
	}
	if thinCount != 1 {
		t.Errorf("thin hub route: corroborationCount = %d, want 1 — a route below the "+
			"corroboration floor cannot confirm the direct print", thinCount)
	}

	// Direct edge NOT excluded from routing: the 1-hop route IS the direct
	// market, shares its undirected edge, and so collapses to one.
	withDirectEdge := mustEdges(t, rq(xlm, gbp, 2, 25, 0.9), rq(xlm, usd, 1, 10, 0.9), rq(usd, gbp, 4, 5, 1.0))
	_, _, _, collapsed, _, _, err := aggregate.CombineRoutesWithDirect(
		withDirectEdge, xlm, gbp, 2, 0, rq(xlm, gbp, 2, 25, 0.9))
	if err != nil {
		t.Fatalf("unexcluded direct: %v", err)
	}
	if collapsed != 1 {
		t.Errorf("unexcluded direct edge: corroborationCount = %d, want 1 — the 1-hop route "+
			"and the direct print are the same market", collapsed)
	}

	// R1 preserved: two hub routes 12.5% apart (0.08 vs 0.09) survive the
	// loose band but contradict each other → router-only 0. The direct
	// print siding with one of them must NOT rescue the count.
	disagreeing := mustEdges(t,
		rq(xlm, usd, 1, 10, 0.9), rq(usd, gbp, 4, 5, 1.0), // 0.08
		rq(xlm, canonical.Asset{Type: canonical.AssetFiat, Code: "EUR"}, 1, 10, 0.9),
		rq(canonical.Asset{Type: canonical.AssetFiat, Code: "EUR"}, gbp, 9, 10, 1.0), // 0.09
	)
	_, _, _, contradicted, _, _, err := aggregate.CombineRoutesWithDirect(
		disagreeing, xlm, gbp, 2, 0, rq(xlm, gbp, 2, 25, 0.9))
	if err != nil {
		t.Fatalf("disagreeing: %v", err)
	}
	if contradicted != 0 {
		t.Errorf("disagreeing hub routes + agreeing direct: corroborationCount = %d, want 0 — a "+
			"self-contradicting route set is a problem signal the direct print must not paper over", contradicted)
	}

	// A nil / non-positive direct price is ignored (== CombineRoutes).
	_, _, _, noDirect, _, _, err := aggregate.CombineRoutesWithDirect(deep, xlm, gbp, 2, 0, aggregate.Quote{})
	if err != nil || noDirect != 1 {
		t.Errorf("nil direct: count=%d err=%v, want 1/nil", noDirect, err)
	}
}
