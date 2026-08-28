package aggregate

import (
	"math/big"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Time-local outlier trimming for the published-VWAP path.
//
// Why a second filter (2026-08-28 outlier-trim drift artifact): the
// whole-window [FilterOutliers] scores every print against ONE
// centre — the window median — with ONE scale — 1.4826·MAD of the
// whole window. MAD is the MAJORITY regime's dispersion (~0.1–0.3%
// on XLM/USD), so the band is ~0.6–1.8% at sigma 4 and any AGREED
// move larger than that is trimmed wholesale until it becomes the
// majority of the slice, then the OLD regime is trimmed instead.
// Live on r1: a genuine +2% Kraken step on XLM/GBP (matching the
// XLM/USD × GBP/USD cross) was trimmed for hours, the served window
// VWAP lagged the market by ~d·f and then jumped by d in one tick,
// and `outlier_storm` fired on the re-counted window tail while
// every venue agreed. A step is not an outlier; only a print that
// disagrees with the prints AROUND it is.
//
// The fix scores each print against several robust references and
// keeps it if it sits inside the band of ANY of them:
//
//   - the whole-window centre/scale (the legacy band — so nothing the
//     legacy filter accepted is newly rejected);
//   - its own time bucket (default 1 m), when the bucket holds at
//     least [DefaultOutlierMinBucket] prices;
//   - the nearest non-empty bucket on either side that qualifies the
//     same way (a step that lands MID-bucket leaves the new-regime
//     prints a minority of their own bucket — the next bucket is
//     their honest reference);
//   - when its own bucket is too thin to qualify, the nearest
//     [DefaultOutlierNeighbours] prints on each side in time order,
//     excluding itself (a thin single-source series — 1 print/min
//     SDEX or a quiet fiat cross — has no dense bucket to lean on,
//     and the legacy fallback the design sketched would have kept
//     the drift artifact alive on exactly those pairs).
//
// A print is DROPPED only when it disagrees with every reference —
// the window AND its neighbourhood. That is the shape of a
// fat-finger, a wash print, or a dust-sized spam fill; an agreed
// regime shift agrees with its neighbourhood by definition. The
// local references reuse [robustCentreScale] with a small-sample
// lower bound on the scale ([localScaleRelFloor]): a tight
// neighbourhood still admits ±sigma·0.25% of honest dispersion and
// still rejects a 2× print.
//
// Known trade-off (documented, accepted): a run of more than
// [DefaultOutlierNeighbours] consecutive prints at the SAME wrong
// level in a THIN series validates itself locally. Any time-local
// definition of "outlier" must — six consecutive agreeing prints
// with nothing else around them ARE the local market. The
// unregistered-venue class filter still removes token-farm spam
// from unknown sources ahead of this filter, and the alert on
// venue disagreement (stellarindex_aggregator_outlier_storm) plus
// the trim-fraction alert cover the registered-venue case.
//
// The whole value path is exact *big.Rat (ADR-0003); `Sigma` is a
// config knob converted once to an exact rational.

// Default local-reference geometry. Held as package constants rather
// than config knobs: the bucket matches the closed-bucket serving
// surface (1 m), the qualifying count matches [FilterOutliers]'s
// "fewer than 3 prices is no robust centre" rule, and the
// count-neighbourhood is wide enough that a lone print or a 2–3 print
// burst can never set its own reference in a thin series.
const (
	DefaultOutlierBucket     = time.Minute
	DefaultOutlierMinBucket  = 3
	DefaultOutlierNeighbours = 5
)

// LocalOutlierOptions parameterises [FilterOutliersLocal]. The zero
// value of every field but Sigma selects the package default.
type LocalOutlierOptions struct {
	// Sigma is the σ-equivalent multiplier, with the same meaning as
	// [FilterOutliers]'s sigma: a print survives when it is within
	// Sigma·scale of at least one reference centre. <= 0 disables
	// the filter.
	Sigma float64
	// Bucket is the time-bucket width for the local references.
	Bucket time.Duration
	// MinBucket is the minimum number of usable prices a bucket must
	// hold to serve as a local reference.
	MinBucket int
	// Neighbours is the number of prints on EACH side (in time order)
	// that form the count-neighbourhood reference for a print whose
	// own bucket is too thin to qualify.
	Neighbours int
}

func (o LocalOutlierOptions) withDefaults() LocalOutlierOptions {
	if o.Bucket <= 0 {
		o.Bucket = DefaultOutlierBucket
	}
	if o.MinBucket <= 0 {
		o.MinBucket = DefaultOutlierMinBucket
	}
	if o.Neighbours <= 0 {
		o.Neighbours = DefaultOutlierNeighbours
	}
	return o
}

// FilterOutliersLocal returns a copy of trades with the prints that
// disagree with BOTH the whole window and their time-local
// neighbourhood removed. See the file comment for the rationale and
// the exact reference set. Output preserves input order.
//
// Edge cases match [FilterOutliers]: Sigma <= 0 is a no-op copy;
// fewer than 3 usable prices returns the usable trades unchanged;
// zero-base / zero-quote trades are dropped before the statistics.
func FilterOutliersLocal(trades []canonical.Trade, opts LocalOutlierOptions) []canonical.Trade {
	if opts.Sigma <= 0 || len(trades) < 3 {
		out := make([]canonical.Trade, len(trades))
		copy(out, trades)
		return out
	}
	validIdx, z := outlierScores(trades, opts)
	if z == nil {
		return keepByIndex(trades, validIdx)
	}
	sigmaRat := new(big.Rat).SetFloat64(opts.Sigma)
	if sigmaRat == nil {
		return keepByIndex(trades, validIdx)
	}
	out := make([]canonical.Trade, 0, len(validIdx))
	for k, i := range validIdx {
		if z[k] == nil || z[k].Cmp(sigmaRat) > 0 {
			continue // outlier — disagrees with every reference
		}
		out = append(out, trades[i])
	}
	return out
}

// robustRef is one (centre, scale) reference a print is scored
// against.
type robustRef struct {
	centre, scale *big.Rat
}

// localScaleRelFloor is the LOWER BOUND on a local reference's
// σ-equivalent scale, as a fraction of its centre: 1/400 = 0.25%,
// i.e. a ±1% band at the default sigma 4.
//
// Unlike [zeroScaleRelFloor] (which only replaces a MAD that is
// exactly 0) this applies ALWAYS to the local references. They are
// small samples by construction — a qualifying bucket can be 3–5
// prints — and the MAD of 5 prints is a noisy scale estimate: five
// honest prints that happen to land within 0.02% of each other would
// otherwise reject a sixth honest print 0.15% away. 0.25% is ~4× the
// intra-regime dispersion of a liquid pair and still ~10× below the
// fat-finger / wash prints the filter exists to remove. The window
// reference keeps its exact MAD (and MNY-22 zero-floor), so on tight
// pairs a sub-1% print is still judged by the legacy band.
var localScaleRelFloor = big.NewRat(1, 400)

func newRobustRef(prices []*big.Rat) *robustRef {
	c, s := robustCentreScale(prices)
	return &robustRef{centre: c, scale: s}
}

// newLocalRef is [newRobustRef] with the [localScaleRelFloor] lower
// bound applied to the scale.
func newLocalRef(prices []*big.Rat) *robustRef {
	r := newRobustRef(prices)
	floor := new(big.Rat).Mul(localScaleRelFloor, r.centre)
	floor.Abs(floor)
	if floor.Cmp(r.scale) > 0 {
		r.scale = floor
	}
	return r
}

// score returns |p − centre| / scale. A zero scale (only reachable
// for a zero centre, where no relative floor exists) scores an exact
// match as 0 and anything else as "no finite score" (ok=false).
func (r *robustRef) score(p *big.Rat) (*big.Rat, bool) {
	dev := new(big.Rat).Sub(p, r.centre)
	dev.Abs(dev)
	if r.scale.Sign() == 0 {
		if dev.Sign() == 0 {
			return new(big.Rat), true
		}
		return nil, false
	}
	return dev.Quo(dev, r.scale), true
}

// minScore folds `ref`'s score for p into best (nil = no finite
// score yet).
func minScore(best *big.Rat, ref *robustRef, p *big.Rat) *big.Rat {
	s, ok := ref.score(p)
	if !ok {
		return best
	}
	if best == nil || s.Cmp(best) < 0 {
		return s
	}
	return best
}

// priceBucket is one time bucket of usable prices, in time order.
type priceBucket struct {
	// members are positions into the time-ordered `order` slice.
	members []int
	ref     *robustRef // lazily built; nil when the bucket doesn't qualify
	built   bool
}

// localIndex is the time-ordered view of a window's usable prices
// plus its bucket partition — everything [localIndex.score] needs.
type localIndex struct {
	opts   LocalOutlierOptions
	prices []*big.Rat
	// order[k] = position into prices, sorted by trade time (stable,
	// so equal timestamps keep input order).
	order []int
	// bucketOf[k] is the bucket index of order-position k.
	bucketOf []int
	buckets  []*priceBucket
	window   *robustRef
}

// newLocalIndex sorts the usable prices by trade time and partitions
// them into opts.Bucket-wide buckets.
func newLocalIndex(trades []canonical.Trade, validIdx []int, prices []*big.Rat, opts LocalOutlierOptions) *localIndex {
	ix := &localIndex{opts: opts, prices: prices, window: newRobustRef(prices)}
	ix.order = make([]int, len(prices))
	for k := range ix.order {
		ix.order[k] = k
	}
	sort.SliceStable(ix.order, func(a, b int) bool {
		return trades[validIdx[ix.order[a]]].Timestamp.Before(trades[validIdx[ix.order[b]]].Timestamp)
	})
	ix.bucketOf = make([]int, len(ix.order))
	var lastKey int64
	for k, pos := range ix.order {
		key := trades[validIdx[pos]].Timestamp.Truncate(opts.Bucket).UnixNano()
		if len(ix.buckets) == 0 || key != lastKey {
			ix.buckets = append(ix.buckets, &priceBucket{})
			lastKey = key
		}
		b := ix.buckets[len(ix.buckets)-1]
		b.members = append(b.members, k)
		ix.bucketOf[k] = len(ix.buckets) - 1
	}
	return ix
}

// bucketRef returns bucket bi's local reference, building it on first
// use; nil when bi is out of range or the bucket holds fewer than
// opts.MinBucket prices.
func (ix *localIndex) bucketRef(bi int) *robustRef {
	if bi < 0 || bi >= len(ix.buckets) {
		return nil
	}
	b := ix.buckets[bi]
	if !b.built {
		b.built = true
		if len(b.members) >= ix.opts.MinBucket {
			ps := make([]*big.Rat, len(b.members))
			for j, k := range b.members {
				ps[j] = ix.prices[ix.order[k]]
			}
			b.ref = newLocalRef(ps)
		}
	}
	return b.ref
}

// neighbourhoodRef builds the count-neighbourhood reference for
// order-position k: the nearest opts.Neighbours prices on each side,
// excluding k itself. nil when k has no neighbours at all.
func (ix *localIndex) neighbourhoodRef(k int) *robustRef {
	lo, hi := k-ix.opts.Neighbours, k+ix.opts.Neighbours
	if lo < 0 {
		lo = 0
	}
	if hi > len(ix.order)-1 {
		hi = len(ix.order) - 1
	}
	neigh := make([]*big.Rat, 0, hi-lo)
	for j := lo; j <= hi; j++ {
		if j != k {
			neigh = append(neigh, ix.prices[ix.order[j]])
		}
	}
	if len(neigh) == 0 {
		return nil
	}
	return newLocalRef(neigh)
}

// score returns the SMALLEST σ-equivalent distance from the price at
// order-position k to any of its references: the window, its own
// bucket, the adjacent buckets, and — when its own bucket is too thin
// to qualify — its count-neighbourhood. nil when no reference produced
// a finite score (only possible around a zero centre).
func (ix *localIndex) score(k int) *big.Rat {
	p := ix.prices[ix.order[k]]
	best := minScore(nil, ix.window, p)
	bi := ix.bucketOf[k]
	own := ix.bucketRef(bi)
	for _, ref := range []*robustRef{own, ix.bucketRef(bi - 1), ix.bucketRef(bi + 1)} {
		if ref != nil {
			best = minScore(best, ref, p)
		}
	}
	if own == nil {
		if ref := ix.neighbourhoodRef(k); ref != nil {
			best = minScore(best, ref, p)
		}
	}
	return best
}

// outlierScores returns the indices of the usable-price trades (in
// input order) and, aligned with them, each trade's z — the SMALLEST
// σ-equivalent distance to any reference (see [localIndex.score]). A
// nil z entry means no reference produced a finite score. z is nil as
// a whole when fewer than 3 usable prices exist.
func outlierScores(trades []canonical.Trade, opts LocalOutlierOptions) (validIdx []int, z []*big.Rat) {
	opts = opts.withDefaults()

	prices := make([]*big.Rat, 0, len(trades))
	validIdx = make([]int, 0, len(trades))
	for i := range trades {
		p, ok := priceRat(&trades[i])
		if !ok {
			continue
		}
		prices = append(prices, p)
		validIdx = append(validIdx, i)
	}
	if len(prices) < 3 {
		return validIdx, nil
	}

	ix := newLocalIndex(trades, validIdx, prices, opts)
	z = make([]*big.Rat, len(prices))
	for k, pos := range ix.order {
		z[pos] = ix.score(k)
	}
	return validIdx, z
}
