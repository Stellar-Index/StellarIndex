package orchestrator

import (
	"math/big"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The quote-surface check compares proxy and own-quote prints sub-bucket by sub-bucket, so a price trend under volume that falls at
// different times of day cannot read as a de-peg while a persistent basis still does.
const (
	// surfaceSubBucketsPerWindow sets the sub-bucket at window/24 (5m→1m, 1h→2m, 24h→1h), never finer than closedBucket.
	surfaceSubBucketsPerWindow = 24
	// surfaceMinOverlap is the fewest sub-buckets holding both surfaces before their median is trusted over the whole-window compare.
	surfaceMinOverlap = 3
	// surfaceRecentBuckets is how many of the newest overlapping sub-buckets also judge alone, so a de-peg in progress is not
	// out-voted by the older half of the window.
	surfaceRecentBuckets = 3
)

// surfaceSubBucket is the time-alignment grain for window.
func surfaceSubBucket(window time.Duration) time.Duration {
	sub := (window / surfaceSubBucketsPerWindow).Truncate(closedBucket)
	if sub < closedBucket {
		return closedBucket
	}
	return sub
}

// surfaceCell holds one sub-bucket's prints per surface and their base volume at the window's common scale.
type surfaceCell struct {
	start              time.Time
	proxy, own         []canonical.Trade
	proxyBase, ownBase *big.Int
}

// surfacePoint is one overlapping sub-bucket's signed gap (proxyVWAP - ownVWAP) / ownVWAP and its weight min(proxyBase, ownBase).
type surfacePoint struct {
	start  time.Time
	gap    *big.Rat
	weight *big.Int
}

// surfaceCells buckets trades by sub-bucket start (absolute-time aligned, so input order cannot move a boundary), oldest first; scaled
// is trades at the common amount scale, index-aligned.
func surfaceCells(trades, scaled []canonical.Trade, proxied map[string]struct{}, sub time.Duration) []*surfaceCell {
	byStart := make(map[int64]*surfaceCell)
	for i := range trades {
		start := trades[i].Timestamp.UTC().Truncate(sub)
		c, ok := byStart[start.UnixNano()]
		if !ok {
			c = &surfaceCell{start: start, proxyBase: new(big.Int), ownBase: new(big.Int)}
			byStart[start.UnixNano()] = c
		}
		if _, isProxy := proxied[trades[i].ID()]; isProxy {
			c.proxy = append(c.proxy, trades[i])
			c.proxyBase.Add(c.proxyBase, scaled[i].BaseAmount.BigInt())
			continue
		}
		c.own = append(c.own, trades[i])
		c.ownBase.Add(c.ownBase, scaled[i].BaseAmount.BigInt())
	}
	cells := make([]*surfaceCell, 0, len(byStart))
	for _, c := range byStart {
		cells = append(cells, c)
	}
	sort.Slice(cells, func(i, j int) bool { return cells[i].start.Before(cells[j].start) })
	return cells
}

// surfacePoints prices every sub-bucket holding both surfaces and returns them oldest first with each surface's covered base volume;
// ok=false when a sub-bucket VWAP could not be computed (the caller fails closed, as for the whole window).
func (o *Orchestrator) surfacePoints(
	pair canonical.Pair, cells []*surfaceCell,
) (points []surfacePoint, proxyCovered, ownCovered *big.Int, ok bool) {
	proxyCovered, ownCovered = new(big.Int), new(big.Int)
	for _, c := range cells {
		if len(c.proxy) == 0 || len(c.own) == 0 {
			continue
		}
		weight := c.proxyBase
		if c.ownBase.Cmp(weight) < 0 {
			weight = c.ownBase
		}
		if weight.Sign() <= 0 {
			continue
		}
		proxyVWAP, perr := o.computeNormalizedVWAP(c.proxy, pair)
		ownVWAP, oerr := o.computeNormalizedVWAP(c.own, pair)
		if perr != nil || oerr != nil || proxyVWAP.Sign() <= 0 || ownVWAP.Sign() <= 0 {
			return nil, nil, nil, false
		}
		gap := new(big.Rat).Sub(proxyVWAP, ownVWAP)
		gap.Quo(gap, ownVWAP)
		points = append(points, surfacePoint{start: c.start, gap: gap, weight: new(big.Int).Set(weight)})
		proxyCovered.Add(proxyCovered, c.proxyBase)
		ownCovered.Add(ownCovered, c.ownBase)
	}
	return points, proxyCovered, ownCovered, true
}

// alignedSurfaceGap is the time-aligned signed surface gap: the larger in magnitude of the weighted median over every overlapping
// sub-bucket and over the newest surfaceRecentBuckets of them. aligned=false (gap nil) when the overlap is under surfaceMinOverlap
// sub-buckets or covers under half of either surface's base volume; ok=false when a sub-bucket VWAP failed.
func (o *Orchestrator) alignedSurfaceGap(
	pair canonical.Pair, trades, scaled []canonical.Trade, proxied map[string]struct{}, window time.Duration,
	proxyBase, ownBase *big.Int,
) (gap *big.Rat, overlap int, aligned, ok bool) {
	points, proxyCovered, ownCovered, ok := o.surfacePoints(pair, surfaceCells(trades, scaled, proxied, surfaceSubBucket(window)))
	if !ok {
		return nil, 0, false, false
	}
	if len(points) < surfaceMinOverlap || !atLeastHalf(proxyCovered, proxyBase) || !atLeastHalf(ownCovered, ownBase) {
		return nil, len(points), false, true
	}
	gap = weightedMedianGap(points)
	if recent := weightedMedianGap(points[len(points)-surfaceRecentBuckets:]); new(big.Rat).Abs(recent).Cmp(new(big.Rat).Abs(gap)) > 0 {
		gap = recent
	}
	return gap, len(points), true, true
}

// atLeastHalf reports 2*part >= whole.
func atLeastHalf(part, whole *big.Int) bool {
	return new(big.Int).Lsh(part, 1).Cmp(whole) >= 0
}

// weightedMedianGap is the lower weighted median of points' gaps (ties by sub-bucket start, so the result is a function of the trade
// set alone); points must be non-empty with positive weights.
func weightedMedianGap(points []surfacePoint) *big.Rat {
	sorted := append([]surfacePoint(nil), points...)
	sort.Slice(sorted, func(i, j int) bool {
		if c := sorted[i].gap.Cmp(sorted[j].gap); c != 0 {
			return c < 0
		}
		return sorted[i].start.Before(sorted[j].start)
	})
	total := new(big.Int)
	for _, p := range sorted {
		total.Add(total, p.weight)
	}
	cum := new(big.Int)
	for _, p := range sorted {
		cum.Add(cum, p.weight)
		if atLeastHalf(cum, total) {
			return new(big.Rat).Set(p.gap)
		}
	}
	return new(big.Rat).Set(sorted[len(sorted)-1].gap)
}
