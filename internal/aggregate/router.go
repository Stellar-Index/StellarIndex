package aggregate

import (
	"errors"
	"fmt"
	"math/big"
	"math/bits"
	"sort"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Graph-based cross-rate router.
//
// This is a port of the proven "Rates Engine" pathfinder
// (predecessor system, rate_calculator.go:
// CalculatePathsOfExchange / CalculateCompositeRate /
// OmitOutlierRates / CalculateAverageRate). It answers "what is the
// price of base in quote's terms?" when no direct market exists, by
// chaining the fresh per-pair rates we DO have through intermediary
// hub assets (XLM, USD, BTC, …) and corroborating across every
// independent path of the same (shortest) length.
//
// Two deliberate adaptations from the Rates Engine reference:
//
//   - Types: canonical.Pair / canonical.Asset and exact *big.Rat
//     prices (ADR-0003), NOT the RE `Pair`/float64 `money.go` types.
//     Every price on the value path is an exact rational; a rate's
//     inverse is new(big.Rat).Inv(p), never 1.0/p. No float64 ever
//     touches a served price.
//
//   - Combine statistic: the RE combines surviving composites with a
//     WEIGHTED MEAN (CalculateAverageRate). We combine with the exact
//     MEDIAN instead, to match this repo's masking-resistant
//     robust-statistics posture (robust.go / FilterOutliers /
//     GuardServedVWAP all centre on the median). The median is not
//     dragged by a single divergent survivor the way a mean is. See
//     [CombineRoutes].
//
// Confidence (weakest-link). A cross-rate is only as trustworthy as
// its flimsiest leg: a single dust SDEX print (0.00001 units for $10 →
// an implied ~$1M/unit) must never launder itself into a confident
// valuation by riding through a hub. So every edge carries a
// confidence scalar in [0,1] (populated by Step 2 from real signal —
// USD volume, trade count, source count, dispersion, recency), a
// route's confidence is the MINIMUM confidence across its edges, and
// [CombineRoutes] gates routes on a caller-supplied floor before it
// will treat a composite as confident. The price math stays exact
// *big.Rat; confidence is a float64 quality weight, never a served
// money value.

var (
	// ErrNoRoute is returned by [CombineRoutes] when no path of at most
	// maxHops legs connects base to quote through the available edges.
	ErrNoRoute = errors.New("aggregate: no route from base to quote")

	// ErrEmptyRoute is returned by [CompositeRate] for a zero-length
	// path (no legs to multiply).
	ErrEmptyRoute = errors.New("aggregate: empty route has no composite rate")

	// ErrRouteBroken is returned by [CompositeRate] when consecutive
	// legs do not chain (leg i's From is not leg i-1's To) — mirrors the
	// RE's ErrRatePairMismatch base/quote-chaining guard.
	ErrRouteBroken = errors.New("aggregate: route legs do not chain")

	// ErrLegNonPositive is returned when a leg (or a source quote)
	// carries a zero/negative price — a price is only well-defined for
	// strictly positive rates (mirrors [ErrTriangulateZero]).
	ErrLegNonPositive = errors.New("aggregate: route leg price must be positive")
)

const (
	// routerOutlierPermitPct is the median-relative band, in percent,
	// within which a candidate composite is kept during outlier
	// omission. Ported from the RE's CalculateAverageRate, which calls
	// OmitOutlierRates(rates, 40). A composite further than 40% from the
	// median of the candidates is rejected as an outlier route.
	routerOutlierPermitPct = 40

	// routerDivergenceSpreadPct is the post-omission survivor spread, in
	// percent of the median, above which [CombineRoutes] flags the
	// result diverged even though no single route was rejected — the
	// surviving routes still disagree by more than this much.
	routerDivergenceSpreadPct = 40

	// routerCorroborationAgreePct is the TIGHT pairwise-agreement band, in
	// percent, two surviving routes' composites must fall within of each
	// other before either is counted as CORROBORATING the other for the
	// anomaly-freeze source_count leg (see corroboratingRouteCount /
	// [CombineRoutes]'s corroborationCount return). It is deliberately
	// MUCH tighter than routerOutlierPermitPct (40): merely surviving the
	// loose outlier band is "not an outlier", NOT "agrees". Corroboration
	// is an anti-manipulation control — "N routes agree" must mean the
	// routes actually agree, not just that they are within 40% of the
	// median — so the bar is a few percent. Measured against the SMALLER
	// of the two prices (the conservative, fail-closed denominator).
	// Serving is unaffected; this only tightens what COUNTS as
	// corroboration for the freeze.
	routerCorroborationAgreePct = 3

	// maxCorroborationRoutes caps the brute-force set-packing / clique
	// search corroboratingRouteCount runs over the surviving routes. The
	// shortest-route set for a real target is a handful, so the exact
	// 2^n enumeration is trivial; beyond this many survivors it falls
	// back to a greedy (UNDER-counting, fail-closed) estimate rather than
	// risk a pathological blow-up. Under-counting can only make the
	// freeze fire MORE readily, never suppress it falsely.
	maxCorroborationRoutes = 16
)

// RouteLeg is one directed edge of the exchange graph AND one hop of a
// resolved route (an edge and a leg are the same object): 1 unit of
// From is worth Price units of To, with a per-edge Confidence in [0,1].
//
// A route is a []RouteLeg whose legs chain (leg[i].From == leg[i-1].To)
// from the query base to the query quote.
type RouteLeg struct {
	From canonical.Asset
	To   canonical.Asset
	// Price is the exact rate: 1 From = Price To. Always strictly
	// positive on a well-formed edge (see [BuildEdges]).
	Price *big.Rat
	// Confidence is the trust weight of THIS edge in [0,1]. Step 2
	// populates it from real signal; the router only ever takes minima
	// and maxima of it, so any ordering-consistent scale works.
	Confidence float64
}

// Quote is a directed price observation for a pair: the price of
// Pair.Base in Pair.Quote's terms, with a Confidence in [0,1]. It is
// the input to [BuildEdges]. Step 2 constructs these from fresh VWAPs
// and their computed confidence; this pure core treats Confidence as an
// opaque weakest-link weight.
type Quote struct {
	Pair       canonical.Pair
	Price      *big.Rat
	Confidence float64
}

// BuildEdges expands directed pair quotes into a bidirectional edge
// set. Each quote (A/B)=p contributes BOTH directions: edge A→B at p
// AND edge B→A at the exact rational inverse 1/p
// (new(big.Rat).Inv(p)); both directions inherit the quote's
// confidence. Prices are defensively copied, so the caller may mutate
// its inputs afterwards.
//
// Returns [ErrLegNonPositive] if any quote price is nil or ≤ 0 (an
// inverse of a non-positive rate is meaningless), and skips
// same-asset "pairs" defensively (they cannot be a real market and
// would be a self-loop in the graph). Output is sorted deterministically
// so repeated builds — and any downstream route ordering — are stable.
func BuildEdges(quotes []Quote) ([]RouteLeg, error) {
	edges := make([]RouteLeg, 0, len(quotes)*2)
	for _, q := range quotes {
		if q.Price == nil || q.Price.Sign() <= 0 {
			return nil, fmt.Errorf("%w: pair %s", ErrLegNonPositive, q.Pair)
		}
		if q.Pair.Base.Equal(q.Pair.Quote) {
			continue
		}
		fwd := new(big.Rat).Set(q.Price)
		inv := new(big.Rat).Inv(q.Price)
		edges = append(edges,
			RouteLeg{From: q.Pair.Base, To: q.Pair.Quote, Price: fwd, Confidence: q.Confidence},
			RouteLeg{From: q.Pair.Quote, To: q.Pair.Base, Price: inv, Confidence: q.Confidence},
		)
	}
	sort.SliceStable(edges, func(i, j int) bool {
		if edges[i].From.String() != edges[j].From.String() {
			return edges[i].From.String() < edges[j].From.String()
		}
		return edges[i].To.String() < edges[j].To.String()
	})
	return edges, nil
}

// FindRoutes returns every acyclic path of at most maxHops legs from
// base to quote through edges. maxHops counts LEGS (edges), so a route
// via one hub asset (base→hub→quote) has 2 legs. A path never revisits
// an asset (no cycles), which also bounds the search.
//
// When shortestOnly is true the result is pruned to only the paths of
// the minimum leg-count found — the fewest-hop routes win, matching the
// RE's `shortest` pruning in CalculatePathsOfExchange. Routing is purely
// topological; edge confidence does not influence which paths are found
// (it is scored later, in [CombineRoutes]).
//
// Returns nil for base == quote, maxHops < 1, or a disconnected quote.
func FindRoutes(edges []RouteLeg, base, quote canonical.Asset, maxHops int, shortestOnly bool) [][]RouteLeg {
	if maxHops < 1 || base.Equal(quote) {
		return nil
	}
	adj := buildAdjacency(edges)

	var paths [][]RouteLeg
	var path []RouteLeg
	visited := map[canonical.Asset]bool{base: true}

	var dfs func(current canonical.Asset)
	dfs = func(current canonical.Asset) {
		if len(path) >= maxHops {
			return // no budget for another leg
		}
		for _, e := range adj[current] {
			if visited[e.To] {
				continue // revisiting an asset would form a cycle
			}
			path = append(path, e)
			if e.To.Equal(quote) {
				cp := make([]RouteLeg, len(path))
				copy(cp, path)
				paths = append(paths, cp)
			} else {
				visited[e.To] = true
				dfs(e.To)
				delete(visited, e.To)
			}
			path = path[:len(path)-1]
		}
	}
	dfs(base)

	if shortestOnly {
		paths = keepShortest(paths)
	}
	return paths
}

// buildAdjacency indexes edges by their From asset for O(1) neighbour
// lookup. Self-loops (From == To) are dropped defensively.
func buildAdjacency(edges []RouteLeg) map[canonical.Asset][]RouteLeg {
	adj := make(map[canonical.Asset][]RouteLeg, len(edges))
	for _, e := range edges {
		if e.From.Equal(e.To) {
			continue
		}
		adj[e.From] = append(adj[e.From], e)
	}
	return adj
}

// keepShortest filters paths to only those of the minimum leg-count.
func keepShortest(paths [][]RouteLeg) [][]RouteLeg {
	if len(paths) == 0 {
		return paths
	}
	minLen := len(paths[0])
	for _, p := range paths {
		if len(p) < minLen {
			minLen = len(p)
		}
	}
	out := make([][]RouteLeg, 0, len(paths))
	for _, p := range paths {
		if len(p) == minLen {
			out = append(out, p)
		}
	}
	return out
}

// CompositeRate multiplies the leg rates along one route into that
// route's composite price (base→quote), exactly (*big.Rat). Ported
// from the RE's CalculateCompositeRate, including its base/quote
// chaining validation.
//
// Rejects an empty route ([ErrEmptyRoute]), a route whose legs do not
// chain ([ErrRouteBroken]), and any zero/negative leg
// ([ErrLegNonPositive]) — a broken or non-positive leg would yield a
// plausible-looking but wrong price, so we fail closed rather than
// skip it (cf. [TriangulateChain]). The result is freshly allocated.
func CompositeRate(path []RouteLeg) (*big.Rat, error) {
	if len(path) == 0 {
		return nil, ErrEmptyRoute
	}
	out := new(big.Rat).SetInt64(1)
	for i, leg := range path {
		if leg.Price == nil || leg.Price.Sign() <= 0 {
			return nil, fmt.Errorf("%w: leg %d (%s→%s)", ErrLegNonPositive, i, leg.From, leg.To)
		}
		if i > 0 && !leg.From.Equal(path[i-1].To) {
			return nil, fmt.Errorf("%w: leg %d starts at %s, previous ended at %s",
				ErrRouteBroken, i, leg.From, path[i-1].To)
		}
		out.Mul(out, leg.Price)
	}
	return out, nil
}

// RouteConfidence is the weakest-link confidence of a route: the
// MINIMUM edge confidence along the path. A route is only as
// trustworthy as its flimsiest leg, so one dust-thin edge caps the
// whole route's confidence. An empty route has confidence 0.
func RouteConfidence(path []RouteLeg) float64 {
	if len(path) == 0 {
		return 0
	}
	minConf := path[0].Confidence
	for _, leg := range path[1:] {
		if leg.Confidence < minConf {
			minConf = leg.Confidence
		}
	}
	return minConf
}

// OmitOutliers rejects composites that diverge from the group's median
// by more than permitDivergentPct percent, returning the survivors in
// input order. Ported from the RE's OmitOutlierRates, with two
// deliberate changes: (1) the baseline is the exact MEDIAN, not the
// mean — the median is not dragged by the very outlier we are trying to
// reject (masking-resistant, matching robust.go); (2) the guard fires
// at ≥ 3 values (the RE only filtered at > 3), because a median can
// identify the odd-one-out among three where a mean cannot.
//
// With fewer than 3 values there is no robust way to say which is the
// outlier, so all are kept. If the band would reject EVERYTHING (a
// bimodal even-count with no member near the midpoint), all are kept
// too — we never return an empty survivor set. A zero/undefined median
// disables the relative test (all kept).
func OmitOutliers(rates []*big.Rat, permitDivergentPct int) []*big.Rat {
	keep := omitOutlierIndices(rates, permitDivergentPct)
	out := make([]*big.Rat, 0, len(keep))
	for _, i := range keep {
		out = append(out, rates[i])
	}
	return out
}

// omitOutlierIndices returns the indices of the composites kept by the
// median-relative band. See [OmitOutliers] for the semantics.
func omitOutlierIndices(rates []*big.Rat, permitDivergentPct int) []int {
	all := make([]int, len(rates))
	for i := range rates {
		all[i] = i
	}
	if len(rates) < 3 {
		return all
	}
	med := medianRat(rates)
	medAbs := new(big.Rat).Abs(med)
	if medAbs.Sign() == 0 {
		return all // no relative baseline
	}

	// Keep when |r - med| / |med| * 100 <= permit, i.e.
	//   |r - med| * 100 <= permit * |med|  (exact rational compare).
	hundred := big.NewRat(100, 1)
	rhs := new(big.Rat).Mul(medAbs, big.NewRat(int64(permitDivergentPct), 1))

	keep := make([]int, 0, len(rates))
	for i, r := range rates {
		dev := new(big.Rat).Sub(r, med)
		dev.Abs(dev)
		lhs := new(big.Rat).Mul(dev, hundred)
		if lhs.Cmp(rhs) <= 0 {
			keep = append(keep, i)
		}
	}
	if len(keep) == 0 {
		return all // never reject the entire set
	}
	return keep
}

// scoredRoute pairs a route's exact composite price with its
// weakest-link confidence AND the legs it was computed from, so outlier
// omission, confidence gating, and the edge-disjointness test that backs
// the corroboration count all stay aligned to the same route.
type scoredRoute struct {
	legs       []RouteLeg
	price      *big.Rat
	confidence float64
}

// CombineRoutes is the top-level cross-rate resolver. It finds the
// shortest (fewest-hop) routes from base to quote, computes each
// route's exact composite and weakest-link confidence, gates them on
// minConfidence, rejects outlier composites (median-relative), and
// combines the survivors into one price with the exact MEDIAN.
//
// Returns:
//
//   - composite: the combined base→quote price (exact *big.Rat), the
//     median of the surviving routes' composites. nil only with err.
//   - combinedConfidence: the confidence of the combined price — the
//     MAXIMUM weakest-link confidence among the surviving routes (the
//     best independent path sets the trust floor). Conservative: it
//     never claims more than the single strongest surviving route, even
//     though agreement across routes is itself corroborating.
//   - pathCount: the number of surviving routes that back the SERVED
//     composite (survivors after outlier omission). This is the serving
//     multiplicity carried on the composite meta — NOT the number the
//     freeze trusts. It stays 1 for a single-route target (the whole
//     production config today), byte-identical to the pre-corroboration
//     behaviour.
//   - corroborationCount: the number of INDEPENDENT, TIGHTLY-AGREEING,
//     NON-DIVERGED routes that back the composite — the anti-manipulation
//     count Step 2 feeds into the anomaly-freeze source_count leg. This is
//     STRICTLY tighter than pathCount: it is 0 when the result diverged;
//     0 when no two survivors agree within routerCorroborationAgreePct
//     (loosely-agreeing routes inside the 40% band do NOT corroborate);
//     and, among the tightly-agreeing survivors, the size of the maximum
//     set of PAIRWISE EDGE-DISJOINT routes (two routes that share ANY
//     undirected edge {From,To} are one manipulable market, not two
//     independent confirmations — so an all-through-one-bottleneck set
//     counts as 1). 1 for a single route. See corroboratingRouteCount.
//   - diverged: true if any route was rejected as an outlier OR the
//     surviving routes still spread more than routerDivergenceSpreadPct
//     of their median. A caller should treat a diverged composite as a
//     soft signal, not a clean price.
//   - lowConfidence: true when NO route cleared minConfidence. In that
//     case a best-effort composite is still returned (computed from all
//     routes) so the caller can serve it FLAGGED/stale — it must never
//     be treated as a confident price that feeds market-cap. false when
//     at least one route cleared the floor.
//   - err: [ErrNoRoute] when no path connects base to quote within
//     maxHops; a chaining/positivity error only if an edge is malformed.
//
// minConfidence is the confidence floor: routes at or above it are the
// "confident" set and alone back the combine when any exist; routes
// below it are excluded so a low-confidence path cannot drag a
// high-confidence one. Pass 0 to disable the floor (all routes are
// "confident").
func CombineRoutes(
	edges []RouteLeg, base, quote canonical.Asset, maxHops int, minConfidence float64,
) (composite *big.Rat, combinedConfidence float64, pathCount, corroborationCount int, diverged, lowConfidence bool, err error) {
	routes := FindRoutes(edges, base, quote, maxHops, true)
	if len(routes) == 0 {
		return nil, 0, 0, 0, false, false, ErrNoRoute
	}

	scored, serr := scoreRoutes(routes)
	if serr != nil {
		return nil, 0, 0, 0, false, false, serr
	}

	gated, lowConf := gateByConfidence(scored, minConfidence)

	keep := omitOutlierIndices(pricesOf(gated), routerOutlierPermitPct)
	survivors := make([]scoredRoute, 0, len(keep))
	for _, i := range keep {
		survivors = append(survivors, gated[i])
	}
	survivorPrices := pricesOf(survivors)

	composite = medianRat(survivorPrices)
	combinedConfidence = maxConfidence(survivors)
	pathCount = len(survivors)
	rejected := len(gated) - len(survivors)
	diverged = rejected > 0 || spreadExceeds(survivorPrices, routerDivergenceSpreadPct)
	corroborationCount = corroboratingRouteCount(survivors, diverged)
	return composite, combinedConfidence, pathCount, corroborationCount, diverged, lowConf, nil
}

// scoreRoutes computes each route's exact composite and weakest-link
// confidence. A malformed leg fails the whole call (fail closed).
func scoreRoutes(routes [][]RouteLeg) ([]scoredRoute, error) {
	out := make([]scoredRoute, 0, len(routes))
	for _, r := range routes {
		p, err := CompositeRate(r)
		if err != nil {
			return nil, err
		}
		out = append(out, scoredRoute{legs: r, price: p, confidence: RouteConfidence(r)})
	}
	return out, nil
}

// gateByConfidence keeps only routes at or above the floor. If none
// clear it, it returns ALL routes and reports lowConfidence=true so the
// caller serves a flagged best-effort price rather than nothing.
func gateByConfidence(scored []scoredRoute, minConfidence float64) (kept []scoredRoute, lowConfidence bool) {
	kept = make([]scoredRoute, 0, len(scored))
	for _, s := range scored {
		if s.confidence >= minConfidence {
			kept = append(kept, s)
		}
	}
	if len(kept) == 0 {
		return scored, true
	}
	return kept, false
}

// pricesOf projects the composite prices out of a set of scored routes.
func pricesOf(scored []scoredRoute) []*big.Rat {
	out := make([]*big.Rat, len(scored))
	for i, s := range scored {
		out[i] = s.price
	}
	return out
}

// maxConfidence returns the highest weakest-link confidence in the set
// (0 for an empty set).
func maxConfidence(scored []scoredRoute) float64 {
	best := 0.0
	for i, s := range scored {
		if i == 0 || s.confidence > best {
			best = s.confidence
		}
	}
	return best
}

// spreadExceeds reports whether the max−min spread of vals exceeds pct
// percent of their median (exact rational compare). A set of fewer than
// two values, or a zero median, never "spreads".
func spreadExceeds(vals []*big.Rat, pct int) bool {
	if len(vals) < 2 {
		return false
	}
	med := medianRat(vals)
	medAbs := new(big.Rat).Abs(med)
	if medAbs.Sign() == 0 {
		return false
	}
	lo, hi := vals[0], vals[0]
	for _, v := range vals {
		if v.Cmp(lo) < 0 {
			lo = v
		}
		if v.Cmp(hi) > 0 {
			hi = v
		}
	}
	spread := new(big.Rat).Sub(hi, lo)
	lhs := new(big.Rat).Mul(spread, big.NewRat(100, 1))
	rhs := new(big.Rat).Mul(medAbs, big.NewRat(int64(pct), 1))
	return lhs.Cmp(rhs) > 0
}

// corroboratingRouteCount is the anti-manipulation corroboration count:
// the number of INDEPENDENT, tightly-agreeing, non-diverged routes that
// back the composite. It is what [CombineRoutes] feeds the anomaly-freeze
// source_count leg, and it is deliberately much stricter than the raw
// survivor count so that "N routes agree" means "N genuinely independent
// routes actually agree" — the property a manipulator must not be able to
// fake by wash-trading a single shared leg.
//
// The rules, in order:
//
//   - diverged → 0. If the router saw an outlier route, or the survivors
//     still spread past routerDivergenceSpreadPct, the picture is not
//     clean enough to corroborate anything (fail closed).
//   - 0 or 1 survivor → that count. A single route is the "one unverified
//     path" baseline (1); MAX-ed against the direct source count in the
//     orchestrator it can never raise it, so this is byte-identical to
//     the pre-corroboration behaviour for the single-route production
//     config.
//   - ≥ 2 survivors that do NOT contain a tightly-agreeing pair (no two
//     within routerCorroborationAgreePct) → 0. Two routes that merely
//     fall inside the loose 40% outlier band but actively disagree are
//     evidence of a PROBLEM, not corroboration — drop below the
//     single-path baseline so a disagreeing cross cannot suppress the
//     freeze.
//   - otherwise → the size of the largest set of survivors that pairwise
//     BOTH agree tightly AND are edge-disjoint. Routes sharing any
//     undirected edge collapse to one independent confirmation, so an
//     all-through-one-bottleneck agreeing set scores 1 (not suppressing),
//     while genuinely edge-disjoint agreeing routes score their true
//     multiplicity.
func corroboratingRouteCount(survivors []scoredRoute, diverged bool) int {
	if diverged {
		return 0
	}
	n := len(survivors)
	if n <= 1 {
		return n
	}

	agree := func(i, j int) bool {
		return pricesAgreeWithin(survivors[i].price, survivors[j].price, routerCorroborationAgreePct)
	}
	// Gate: at least two survivors must tightly agree, or nothing counts.
	if maxCliqueSize(n, agree) < 2 {
		return 0
	}

	edgeSets := make([]map[string]struct{}, n)
	for i := range survivors {
		edgeSets[i] = routeEdgeSet(survivors[i].legs)
	}
	agreeAndDisjoint := func(i, j int) bool {
		return agree(i, j) && edgeSetsDisjoint(edgeSets[i], edgeSets[j])
	}
	return maxCliqueSize(n, agreeAndDisjoint)
}

// MaxEdgeDisjointRoutes returns the size of the maximum subset of routes
// that are PAIRWISE edge-disjoint — the count of genuinely independent
// paths in the set. Edge identity is the UNDIRECTED leg {From,To}: an
// edge and its inverse are the same physical market and count as one, so
// two routes that share any leg (in either direction) are NOT
// independent. Shortest-route sets are tiny, so the maximum-set-packing
// search is exact by brute force up to maxCorroborationRoutes routes and
// a fail-closed (under-counting) greedy estimate beyond.
//
// This is the pure independence primitive behind the freeze's
// corroboration count (see corroboratingRouteCount); exported so the
// shared-bottleneck case can be pinned directly.
func MaxEdgeDisjointRoutes(routes [][]RouteLeg) int {
	n := len(routes)
	if n <= 1 {
		return n
	}
	edgeSets := make([]map[string]struct{}, n)
	for i, r := range routes {
		edgeSets[i] = routeEdgeSet(r)
	}
	disjoint := func(i, j int) bool { return edgeSetsDisjoint(edgeSets[i], edgeSets[j]) }
	return maxCliqueSize(n, disjoint)
}

// pricesAgreeWithin reports whether two strictly-positive composite prices
// are within pct percent of each other, measured against the SMALLER of
// the two (the conservative denominator: it yields the larger relative
// gap, so a borderline pair is rejected rather than accepted). Exact
// rational compare — no float ever enters the agreement decision.
func pricesAgreeWithin(a, b *big.Rat, pct int) bool {
	if a == nil || b == nil || a.Sign() <= 0 || b.Sign() <= 0 {
		return false
	}
	smaller := a
	if b.Cmp(a) < 0 {
		smaller = b
	}
	diff := new(big.Rat).Sub(a, b)
	diff.Abs(diff)
	lhs := new(big.Rat).Mul(diff, big.NewRat(100, 1))
	rhs := new(big.Rat).Mul(new(big.Rat).Abs(smaller), big.NewRat(int64(pct), 1))
	return lhs.Cmp(rhs) <= 0
}

// routeEdgeSet is a route's set of UNDIRECTED edge keys (see
// undirectedEdgeKey). Used to test two routes for shared markets.
func routeEdgeSet(route []RouteLeg) map[string]struct{} {
	set := make(map[string]struct{}, len(route))
	for _, leg := range route {
		set[undirectedEdgeKey(leg.From, leg.To)] = struct{}{}
	}
	return set
}

// undirectedEdgeKey is the order-independent identity of the physical
// market between two assets: A→B and B→A hash to the same key, because
// they are the same market quoted in opposite directions. The NUL
// separator cannot appear in a canonical asset string, so the join is
// unambiguous.
func undirectedEdgeKey(a, b canonical.Asset) string {
	sa, sb := a.String(), b.String()
	if sa <= sb {
		return sa + "\x00" + sb
	}
	return sb + "\x00" + sa
}

// edgeSetsDisjoint reports whether two undirected-edge sets share no edge.
func edgeSetsDisjoint(a, b map[string]struct{}) bool {
	// Iterate the smaller set for the cheaper scan.
	if len(b) < len(a) {
		a, b = b, a
	}
	for k := range a {
		if _, ok := b[k]; ok {
			return false
		}
	}
	return true
}

// maxCliqueSize returns the size of the largest subset of {0..n-1} in
// which every pair is connected() — a maximum clique. n is tiny (a
// shortest-route set), so up to maxCorroborationRoutes it enumerates
// every subset exactly; beyond that it returns a greedy clique, which
// can only UNDER-estimate (fail closed: fewer corroborating routes ⇒ the
// freeze fires more readily, never less).
func maxCliqueSize(n int, connected func(i, j int) bool) int {
	if n <= 0 {
		return 0
	}
	if n > maxCorroborationRoutes {
		return greedyCliqueSize(n, connected)
	}
	best := 0
	for mask := 1; mask < (1 << uint(n)); mask++ {
		size := bits.OnesCount(uint(mask))
		if size <= best {
			continue // cannot beat the incumbent
		}
		if maskIsClique(mask, connected) {
			best = size
		}
	}
	return best
}

// maskIsClique reports whether every pair of set bits in mask is
// connected().
func maskIsClique(mask int, connected func(i, j int) bool) bool {
	members := make([]int, 0, bits.OnesCount(uint(mask)))
	for i := 0; mask>>uint(i) != 0; i++ {
		if mask&(1<<uint(i)) != 0 {
			members = append(members, i)
		}
	}
	for a := 0; a < len(members); a++ {
		for b := a + 1; b < len(members); b++ {
			if !connected(members[a], members[b]) {
				return false
			}
		}
	}
	return true
}

// greedyCliqueSize is the fail-closed fallback for absurdly large route
// sets: it grows a single clique greedily, adding a node only if it is
// connected to every node already chosen. The result is a valid clique
// but not necessarily the maximum — it can only under-count, which is the
// safe direction for an anti-manipulation count.
func greedyCliqueSize(n int, connected func(i, j int) bool) int {
	chosen := make([]int, 0, n)
	for i := 0; i < n; i++ {
		ok := true
		for _, j := range chosen {
			if !connected(i, j) {
				ok = false
				break
			}
		}
		if ok {
			chosen = append(chosen, i)
		}
	}
	return len(chosen)
}
