package aggregate

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/bits"
	"sort"

	"github.com/Stellar-Index/StellarIndex/internal/robuststat"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/confidence"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Graph-based cross-rate router (port of the Rates Engine pathfinder): prices base in quote's terms by chaining fresh per-pair rates through hubs,
// corroborated across same-length paths. Prices are exact *big.Rat (ADR-0003), inverses are Inv(p). The combine statistic is the MEMBER MEDIAN of the
// highest-confidence tier, never a blend; confidence is weakest-link (min edge), a float64 quality weight and never a served money value.

var (
	// ErrNoRoute is returned by [CombineRoutes] when no path within maxHops connects base to quote.
	ErrNoRoute = errors.New("aggregate: no route from base to quote")

	// ErrEmptyRoute is returned by [CompositeRate] for a zero-length path.
	ErrEmptyRoute = errors.New("aggregate: empty route has no composite rate")

	// ErrRouteBroken is returned by [CompositeRate] when consecutive legs do not chain.
	ErrRouteBroken = errors.New("aggregate: route legs do not chain")

	// ErrLegNonPositive is returned when a leg or source quote has a zero or negative price.
	ErrLegNonPositive = errors.New("aggregate: route leg price must be positive")
)

const (
	// routerOutlierPermitPct is the median-relative band in percent within which a composite survives outlier omission (40, as in the Rates Engine).
	routerOutlierPermitPct = 40

	// routerDivergenceSpreadPct is the post-omission survivor spread (percent of the median) above which [CombineRoutes] flags divergence
	// even if no single route was rejected.
	routerDivergenceSpreadPct = 40

	// routerCorroborationAgreePct is the TIGHT pairwise band, in percent, within which two surviving routes count as corroborating each other, measured
	// against the SMALLER price (fail-closed). Much tighter than routerOutlierPermitPct: surviving the loose band is not agreement. Serving is unaffected.
	routerCorroborationAgreePct = 3

	// maxCorroborationRoutes caps the exact 2^n corroboration search; beyond it a greedy estimate runs that can only UNDER-count (fail-closed), never overstate.
	maxCorroborationRoutes = 16
)

// RouteTrustFloor is the bootstrap confidence cap; a route must score STRICTLY above it to publish as a reroute or count toward corroboration, since every unscored edge carries exactly the cap.
const RouteTrustFloor = confidence.BootstrapConfidenceCap

// ClearsRouteTrustFloor reports whether weakestLink is strictly above [RouteTrustFloor].
func ClearsRouteTrustFloor(weakestLink float64) bool {
	return weakestLink > RouteTrustFloor
}

// RouteLeg is one directed edge of the exchange graph and one hop of a route: 1 unit of From is worth Price units of To, with a per-edge Confidence in [0,1].
type RouteLeg struct {
	From canonical.Asset
	To   canonical.Asset
	// Price is the exact rate (1 From = Price To), strictly positive on a well-formed edge.
	Price *big.Rat
	// Confidence is this edge's trust weight in [0,1]; the router only takes minima and maxima of it.
	Confidence float64
	// Provenance names underlying data rows beyond the nominal pair (e.g. a shared fx_quotes row), folded into edge identity by [corroboratingRouteCount]; nil for an organic market edge.
	Provenance []string
}

// Quote is a directed price observation (Pair.Base in Pair.Quote terms) with a Confidence in [0,1], the input to [BuildEdges]; Provenance carries to both directions.
type Quote struct {
	Pair       canonical.Pair
	Price      *big.Rat
	Confidence float64
	Provenance []string
}

// BuildEdges expands quotes into bidirectional edges (A to B at p, B to A at exact 1/p), deduped by undirected market, with non-finite confidence clamped to 0; a nil or non-positive price returns [ErrLegNonPositive].
func BuildEdges(quotes []Quote) ([]RouteLeg, error) {
	edges := make([]RouteLeg, 0, len(quotes)*2)
	seen := make(map[string]struct{}, len(quotes))
	// Sort by pair string first: quotes come from a map range, so the first-seen quote per undirected market would vary per tick and diverge
	// across regions, violating the all-regions-serve-the-same-rate invariant (ADR-0018). Safe in place: the caller passes a fresh slice.
	sort.SliceStable(quotes, func(i, j int) bool {
		return quotes[i].Pair.String() < quotes[j].Pair.String()
	})
	for _, q := range quotes {
		if q.Price == nil || q.Price.Sign() <= 0 {
			return nil, fmt.Errorf("%w: pair %s", ErrLegNonPositive, q.Pair)
		}
		if q.Pair.Base.Equal(q.Pair.Quote) {
			continue
		}
		// Dedup by UNDIRECTED market key: XLM/USD and USD/XLM are one market, and counting both would inflate pathCount.
		// The first quote seen derives both directions.
		mkey := undirectedEdgeKey(q.Pair.Base, q.Pair.Quote)
		if _, dup := seen[mkey]; dup {
			continue
		}
		seen[mkey] = struct{}{}
		fwd := new(big.Rat).Set(q.Price)
		inv := new(big.Rat).Inv(q.Price)
		conf := sanitizeConfidence(q.Confidence)
		edges = append(edges,
			RouteLeg{From: q.Pair.Base, To: q.Pair.Quote, Price: fwd, Confidence: conf, Provenance: q.Provenance},
			RouteLeg{From: q.Pair.Quote, To: q.Pair.Base, Price: inv, Confidence: conf, Provenance: q.Provenance},
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

// FindRoutes returns every acyclic path of at most maxHops legs from base to quote; shortestOnly prunes to the fewest-hop paths, and the result is nil for base == quote, maxHops < 1 or no connection.
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

// CompositeRate multiplies leg rates along a route exactly (*big.Rat), failing closed with [ErrEmptyRoute], [ErrRouteBroken] or [ErrLegNonPositive] rather than skipping a bad leg.
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

// RouteConfidence is the weakest-link confidence of a route: the minimum edge confidence, 0 for an empty route.
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

// sanitizeConfidence clamps NaN/Inf to 0 (fail-closed): a NaN never matches confidence == best in [highestConfidencePrice], emptying the top tier
// and panicking the served-price median. Applied at both [BuildEdges] and [scoreRoutes].
func sanitizeConfidence(c float64) float64 {
	if math.IsNaN(c) || math.IsInf(c, 0) {
		return 0
	}
	return c
}

// OmitOutliers rejects composites more than permitDivergentPct percent from the exact median, keeping input order; with fewer than 3 values, or if every value would be rejected, all are kept.
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
	med := robuststat.Median(rates)
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

// scoredRoute pairs a route's exact composite price with its weakest-link confidence and its legs, keeping outlier omission, confidence
// gating and the edge-disjointness corroboration count aligned to the same route.
type scoredRoute struct {
	legs       []RouteLeg
	price      *big.Rat
	confidence float64
}

// CombineRoutes serves the member median of the top-confidence gated tier (chosen before outlier omission); lowConfidence means no route cleared minConfidence and must never feed market-cap, and corroborationCount is an audit signal, not a source count.
func CombineRoutes(
	edges []RouteLeg, base, quote canonical.Asset, maxHops int, minConfidence float64,
) (composite *big.Rat, combinedConfidence float64, servedRouteCount, pathCount, corroborationCount int, diverged, lowConfidence bool, err error) {
	all := FindRoutes(edges, base, quote, maxHops, false)
	routes := keepShortest(all)
	if len(routes) == 0 {
		return nil, 0, 0, 0, 0, false, false, ErrNoRoute
	}

	scored, serr := scoreRoutes(routes)
	if serr != nil {
		return nil, 0, 0, 0, 0, false, false, serr
	}

	gated, lowConf := gateByConfidence(scored, minConfidence)

	// SERVING ANCHOR: the composite is the highest-confidence tier of the GATED routes, chosen BEFORE price-median omission, so a thin divergent
	// majority cannot evict the best route and set the price. servedRouteCount is that tier's size, separate from pathCount (the sets can be disjoint).
	composite, servedRouteCount = highestConfidencePrice(gated)
	combinedConfidence = maxConfidence(gated)

	// DIVERGENCE and CORROBORATION use the full gated set with the loose outlier band: a lower-confidence route can trip diverged or corroborate but
	// cannot move the served price. pathCount is serving multiplicity, not independence (corroborationCount) nor what produced the value (servedRouteCount).
	keep := omitOutlierIndices(pricesOf(gated), routerOutlierPermitPct)
	survivors := make([]scoredRoute, 0, len(keep))
	for _, i := range keep {
		survivors = append(survivors, gated[i])
	}
	pathCount = len(survivors)
	rejected := len(gated) - len(survivors)
	diverged = rejected > 0 || spreadExceeds(pricesOf(survivors), routerDivergenceSpreadPct)
	if !diverged {
		// keepShortest drops every longer route before scoring, so a thin
		// shortest route would otherwise read cleaner than the longer set
		// it displaced; disagreeing with that set is divergence.
		longer, lerr := nextTierDisagrees(composite, all, len(routes[0]), minConfidence)
		if lerr != nil {
			return nil, 0, 0, 0, 0, false, false, lerr
		}
		diverged = longer
	}
	corroborationCount = corroboratingRouteCount(survivors, diverged)
	return composite, combinedConfidence, servedRouteCount, pathCount, corroborationCount, diverged, lowConf, nil
}

// nextTierDisagrees reports whether the composite differs by more than routerDivergenceSpreadPct from the member median of the next-longer
// route tier (gated and outlier-omitted like the served tier); false if there is no longer tier.
func nextTierDisagrees(composite *big.Rat, all [][]RouteLeg, shortest int, minConfidence float64) (bool, error) {
	next := 0
	for _, r := range all {
		if len(r) > shortest && (next == 0 || len(r) < next) {
			next = len(r)
		}
	}
	if next == 0 {
		return false, nil
	}
	var tier [][]RouteLeg
	for _, r := range all {
		if len(r) == next {
			tier = append(tier, r)
		}
	}
	scored, err := scoreRoutes(tier)
	if err != nil {
		return false, err
	}
	gated, _ := gateByConfidence(scored, minConfidence)
	prices := pricesOf(gated)
	kept := make([]*big.Rat, 0, len(prices))
	for _, i := range omitOutlierIndices(prices, routerOutlierPermitPct) {
		kept = append(kept, prices[i])
	}
	if len(kept) == 0 {
		return false, nil
	}
	return spreadExceeds([]*big.Rat{composite, robuststat.MedianMember(kept)}, routerDivergenceSpreadPct), nil
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
		// sanitizeConfidence guards against a route whose weakest edge carries
		// a non-finite confidence (e.g. an edge built directly rather than via
		// BuildEdges) reaching the serving tier as NaN and emptying it (L5).
		out = append(out, scoredRoute{legs: r, price: p, confidence: sanitizeConfidence(RouteConfidence(r))})
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

// highestConfidencePrice is the SERVED composite: the member median of only the top weakest-link-confidence tier of the GATED set, computed BEFORE outlier omission,
// so a thin low-confidence route trips divergence flags but never moves the price. Co-equal routes give a price one actually produced, never an averaged midpoint;
// a divergent minority (>=3 only) is dropped first. Also returns the tier's size after omission (servedRouteCount, distinct from pathCount).
func highestConfidencePrice(routes []scoredRoute) (*big.Rat, int) {
	best := maxConfidence(routes)
	top := make([]*big.Rat, 0, len(routes))
	for _, s := range routes {
		if s.confidence == best {
			top = append(top, s.price)
		}
	}
	// Within the co-equal top tier: drop a divergent minority for clean
	// blending, then serve a value a route ACTUALLY produced (member median,
	// never an averaged midpoint of two disagreeing routes).
	top = OmitOutliers(top, routerOutlierPermitPct)
	return robuststat.MedianMember(top), len(top)
}

// spreadExceeds reports whether the max−min spread of vals exceeds pct
// percent of their median (exact rational compare). A set of fewer than
// two values, or a zero median, never "spreads".
func spreadExceeds(vals []*big.Rat, pct int) bool {
	if len(vals) < 2 {
		return false
	}
	med := robuststat.Median(vals)
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

// corroboratingRouteCount is the anti-manipulation count of INDEPENDENT, tightly-agreeing, non-diverged routes: diverged is 0; 0-1 survivors is that count; 2+ with no pair
// within routerCorroborationAgreePct is 0; otherwise the largest set of survivors clearing [RouteTrustFloor] that agree pairwise AND are edge-disjoint (a shared edge or
// [RouteLeg.Provenance] collapses to one confirmation), so wash-trading one shared leg cannot fake it.
func corroboratingRouteCount(survivors []scoredRoute, diverged bool) int {
	if diverged {
		return 0
	}
	n := len(survivors)
	if n <= 1 {
		return n
	}

	// A route too thin to trust must not corroborate: only routes whose weakest-link confidence clears the floor may join a
	// corroborating pair, so a thin route that merely agrees and is edge-disjoint counts for nothing.
	confident := func(i int) bool {
		return ClearsRouteTrustFloor(survivors[i].confidence)
	}
	agree := func(i, j int) bool {
		return confident(i) && confident(j) &&
			pricesAgreeWithin(survivors[i].price, survivors[j].price, routerCorroborationAgreePct)
	}
	// Gate: at least two confidence-clearing survivors must tightly agree, or
	// nothing counts.
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

// MaxEdgeDisjointRoutes returns the largest pairwise edge-disjoint subset of routes (edge = undirected {From,To} plus Provenance), exact up to maxCorroborationRoutes and a fail-closed under-counting greedy beyond.
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

// pricesAgreeWithin reports whether two positive prices are within pct percent, measured against the SMALLER (conservative: a borderline pair
// is rejected). Exact rational compare, no float.
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
		for _, p := range leg.Provenance {
			set[provenanceEdgeKey(p)] = struct{}{}
		}
	}
	return set
}

// provenanceEdgeKey namespaces a leg's [RouteLeg.Provenance] so it never collides with an [undirectedEdgeKey] market key: routes sharing
// a provenance key are as non-independent as routes sharing a market edge.
func provenanceEdgeKey(p string) string {
	return "provenance\x00" + p
}

// undirectedEdgeKey is the order-independent identity of the market between two assets (A-to-B and B-to-A match);
// the NUL separator cannot appear in a canonical asset string.
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

// maxCliqueSize returns the largest subset of {0..n-1} whose pairs are all connected(); exact up to maxCorroborationRoutes,
// a greedy clique beyond, which only UNDER-estimates (fail-closed).
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

// greedyCliqueSize is the fail-closed fallback for huge route sets: a greedy clique that is valid but may under-count, the safe direction for an anti-manipulation count.
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
