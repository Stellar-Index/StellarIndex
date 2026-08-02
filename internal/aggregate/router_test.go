package aggregate_test

import (
	"errors"
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

	composite, conf, count, diverged, low, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0)
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

	composite, conf, count, diverged, low, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0.5)
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

	composite, _, count, diverged, low, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0.5)
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
	if _, _, _, _, _, err := aggregate.CombineRoutes(edges, obsAsset, obsFiat, 3, 0); !errors.Is(err, aggregate.ErrNoRoute) {
		t.Fatalf("CombineRoutes maxHops=3: err = %v, want ErrNoRoute", err)
	}

	// 4 hops reaches it.
	routes := aggregate.FindRoutes(edges, obsAsset, obsFiat, 4, true)
	if len(routes) != 1 || len(routes[0]) != 4 {
		t.Fatalf("FindRoutes maxHops=4: got %d routes (first len %d), want 1 route of 4 legs",
			len(routes), routeLen(routes))
	}
	composite, _, count, _, _, err := aggregate.CombineRoutes(edges, obsAsset, obsFiat, 4, 0)
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
	composite, _, count, _, _, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0)
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
	if _, _, _, _, _, err := aggregate.CombineRoutes(edges, obscure, gbp, 5, 0); !errors.Is(err, aggregate.ErrNoRoute) {
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
	composite, _, _, _, _, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0)
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

	composite, conf, count, _, low, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0.5)
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
	composite, conf, count, diverged, low, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0.5)
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

	// Control: WITHOUT the floor the bad route drags the median to a
	// fantasy value — this is exactly what the floor prevents.
	dragged, _, count0, diverged0, _, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0)
	if err != nil {
		t.Fatalf("CombineRoutes (floor 0): %v", err)
	}
	eqRat(t, dragged, big.NewRat(33, 10), "un-gated composite") // median(3/5, 6) = 33/10
	if count0 != 2 {
		t.Errorf("un-gated pathCount = %d, want 2", count0)
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
	composite, conf, count, diverged, low, err := aggregate.CombineRoutes(edges, obscure, gbp, 2, 0.5)
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
