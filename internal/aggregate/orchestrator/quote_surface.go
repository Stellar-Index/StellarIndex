package orchestrator

import (
	"math/big"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The quote-surface check compares proxy and own-quote prints sub-bucket by sub-bucket, so a price trend under volume that falls at
// different times of day cannot read as a de-peg while a persistent basis still does. The aligned reading only sees shared slots while
// the served leg is the whole-window VWAP, so it replaces the whole-window reading only when it speaks for nearly all served prints.
const (
	// surfaceSubBucketsPerWindow sets the sub-bucket at window/24 (5m→1m, 1h→2m, 24h→1h), never finer than closedBucket.
	surfaceSubBucketsPerWindow = 24
	// surfaceMinOverlap is the fewest above-floor shared sub-buckets before an aligned reading exists at all.
	surfaceMinOverlap = 3
	// surfaceRecentBuckets is how many of the newest above-floor shared sub-buckets also judge alone (unweighted median), so a de-peg
	// in progress is not out-voted by the older window or one heavy slot.
	surfaceRecentBuckets = 3
	// surfaceCoverageDenom: the aligned reading replaces the whole-window one only when at most 1/20 (5 %) of each surface's volume
	// lies outside above-floor shared slots; a de-peg confined to that remainder moves the served leg by at most 5 % of its size.
	surfaceCoverageDenom = 20
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

// surfacePoint is one shared sub-bucket's signed gap (proxyVWAP - ownVWAP) / ownVWAP, its weight min(proxyBase, ownBase) and the
// cell it came from.
type surfacePoint struct {
	start  time.Time
	gap    *big.Rat
	weight *big.Int
	cell   *surfaceCell
}

// alignedReading is the time-aligned comparison: gap is nil when fewer than surfaceMinOverlap shared slots clear the floor; replaces
// reports whether it speaks for the served prints and may stand instead of the whole-window gap.
type alignedReading struct {
	gap      *big.Rat
	overlap  int
	replaces bool
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

// surfacePoints prices every sub-bucket holding both surfaces, oldest first; ok=false when a sub-bucket VWAP could not be computed
// (the caller fails closed, as for the whole window).
func (o *Orchestrator) surfacePoints(pair canonical.Pair, cells []*surfaceCell) (points []surfacePoint, ok bool) {
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
			return nil, false
		}
		gap := new(big.Rat).Sub(proxyVWAP, ownVWAP)
		gap.Quo(gap, ownVWAP)
		points = append(points, surfacePoint{start: c.start, gap: gap, weight: new(big.Int).Set(weight), cell: c})
	}
	return points, true
}

// aboveFloor keeps the shared slots whose weight is at least half the mean weight over the window's slots (2·slots·w >= Σw), so dust
// on either side neither votes nor counts as covering volume.
func aboveFloor(points []surfacePoint, slots int64) []surfacePoint {
	total := new(big.Int)
	for _, p := range points {
		total.Add(total, p.weight)
	}
	kept := make([]surfacePoint, 0, len(points))
	for _, p := range points {
		if new(big.Int).Mul(p.weight, big.NewInt(2*slots)).Cmp(total) >= 0 {
			kept = append(kept, p)
		}
	}
	return kept
}

// speaksForServed reports whether kept covers all but 1/surfaceCoverageDenom of each surface's volume and holds the newest slot with
// proxy prints, i.e. whether the aligned reading compares nearly every print the served leg VWAP is made of.
func speaksForServed(kept []surfacePoint, cells []*surfaceCell, proxyBase, ownBase *big.Int) bool {
	proxyCovered, ownCovered := new(big.Int), new(big.Int)
	members := make(map[*surfaceCell]bool, len(kept))
	for _, p := range kept {
		proxyCovered.Add(proxyCovered, p.cell.proxyBase)
		ownCovered.Add(ownCovered, p.cell.ownBase)
		members[p.cell] = true
	}
	for i := len(cells) - 1; i >= 0; i-- {
		if len(cells[i].proxy) > 0 {
			if !members[cells[i]] {
				return false
			}
			break
		}
	}
	return mostlyCovered(proxyCovered, proxyBase) && mostlyCovered(ownCovered, ownBase)
}

// mostlyCovered reports denom·(whole - part) <= whole.
func mostlyCovered(part, whole *big.Int) bool {
	rest := new(big.Int).Sub(whole, part)
	return rest.Mul(rest, big.NewInt(surfaceCoverageDenom)).Cmp(whole) <= 0
}

// alignedSurfaceGap is the time-aligned signed surface gap over the above-floor shared slots: the larger in magnitude of their weighted
// median and the unweighted median of the newest surfaceRecentBuckets; ok=false when a sub-bucket VWAP failed.
func (o *Orchestrator) alignedSurfaceGap(
	pair canonical.Pair, trades, scaled []canonical.Trade, proxied map[string]struct{}, window time.Duration,
	proxyBase, ownBase *big.Int,
) (reading alignedReading, ok bool) {
	sub := surfaceSubBucket(window)
	cells := surfaceCells(trades, scaled, proxied, sub)
	points, ok := o.surfacePoints(pair, cells)
	if !ok {
		return alignedReading{}, false
	}
	kept := aboveFloor(points, int64((window+sub-1)/sub))
	if len(kept) < surfaceMinOverlap {
		return alignedReading{overlap: len(kept)}, true
	}
	gap := largerMagnitude(weightedMedianGap(kept), middleOfNewest(kept))
	return alignedReading{gap: gap, overlap: len(kept), replaces: speaksForServed(kept, cells, proxyBase, ownBase)}, true
}

// middleOfNewest is the unweighted median of the newest surfaceRecentBuckets points (points oldest first, len >= surfaceRecentBuckets).
func middleOfNewest(points []surfacePoint) *big.Rat {
	gaps := make([]*big.Rat, 0, surfaceRecentBuckets)
	for _, p := range points[len(points)-surfaceRecentBuckets:] {
		gaps = append(gaps, p.gap)
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].Cmp(gaps[j]) < 0 })
	return new(big.Rat).Set(gaps[surfaceRecentBuckets/2])
}

// largerMagnitude returns whichever of a and b is larger in absolute value (a on a tie).
func largerMagnitude(a, b *big.Rat) *big.Rat {
	if new(big.Rat).Abs(b).Cmp(new(big.Rat).Abs(a)) > 0 {
		return b
	}
	return a
}

// atLeastHalf reports 2*part >= whole.
func atLeastHalf(part, whole *big.Int) bool {
	return new(big.Int).Lsh(part, 1).Cmp(whole) >= 0
}

// weightedMedianGap is the weighted median of points' gaps (ties by sub-bucket start, so the result is a function of the trade set
// alone); on an exact half-weight split it takes the larger magnitude of the two middle values. points must be non-empty.
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
	for i, p := range sorted {
		cum.Add(cum, p.weight)
		if !atLeastHalf(cum, total) {
			continue
		}
		if i+1 < len(sorted) && new(big.Int).Lsh(cum, 1).Cmp(total) == 0 {
			return new(big.Rat).Set(largerMagnitude(p.gap, sorted[i+1].gap))
		}
		return new(big.Rat).Set(p.gap)
	}
	return new(big.Rat).Set(sorted[len(sorted)-1].gap)
}
