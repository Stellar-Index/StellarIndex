package aggregate

import (
	"math/big"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Time-local outlier trimming for the published-VWAP path: a print is kept if inside the band of the whole window, its own time bucket,
// the nearest qualifying buckets, or its nearest neighbours. References are ANCHORED so a wash burst cannot validate itself, and a
// count-majority burst without the base-volume majority withholds the window ([keepIfVolumeMajority]). Exact *big.Rat (ADR-0003).

// Default local-reference geometry, held as constants not config: the 1 m bucket matches the closed-bucket serving surface,
// 3 prices matches FilterOutliers' minimum, and the neighbourhood is wide enough that a short burst cannot set its own reference.
const (
	DefaultOutlierBucket     = time.Minute
	DefaultOutlierMinBucket  = 3
	DefaultOutlierNeighbours = 5
)

// LocalOutlierOptions parameterises [FilterOutliersLocal]; zero values other than Sigma select the package default.
type LocalOutlierOptions struct {
	// Sigma is the sigma-equivalent multiplier, as in [FilterOutliers]; <= 0 disables the filter.
	Sigma float64
	// Bucket is the time-bucket width for the local references.
	Bucket time.Duration
	// MinBucket is the minimum usable prices a bucket needs to serve as a local reference.
	MinBucket int
	// Neighbours is the prints per side forming the count-neighbourhood reference for a print in a thin bucket.
	Neighbours int
	// Weight is how many trades a row stands for (a per-minute summary row); nil counts every row once.
	Weight func(t *canonical.Trade) int64
	// AmountScaleDecimals resolves a source's smallest-unit scale for the volume-majority comparison; nil compares raw amounts (exact only for a single-scale window).
	AmountScaleDecimals func(source string) int
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

// FilterOutliersLocal drops prints that disagree with BOTH the whole window and their time-local neighbourhood, preserving input order; edge cases match [FilterOutliers].
func FilterOutliersLocal(trades []canonical.Trade, opts LocalOutlierOptions) []canonical.Trade {
	if opts.Sigma <= 0 || (len(trades) < 3 && opts.Weight == nil) {
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
	kept := make([]int, 0, len(validIdx))
	for k, i := range validIdx {
		if z[k] == nil || z[k].Cmp(sigmaRat) > 0 {
			continue // outlier — disagrees with every reference
		}
		kept = append(kept, i)
	}
	return keepIfVolumeMajority(trades, validIdx, kept, opts.AmountScaleDecimals)
}

// robustRef is one (centre, scale) reference a print is scored
// against.
type robustRef struct {
	centre, scale *big.Rat
}

// localScaleRelFloor (1/400 = 0.25%, a +-1% band at sigma 4) is the always-applied lower bound on a local reference's scale: the MAD of
// 3-5 prints is noisy, so tight honest prints would otherwise reject a near neighbour. The window reference keeps its exact MAD; [FilterOutliers] shares the floor.
var localScaleRelFloor = big.NewRat(1, 400)

// localScaleRelCeiling (1/100 = 1%, a +-4% band at sigma 4) caps a local reference's scale so a bucket of wash prints with a huge MAD
// cannot admit everything in it; the window reference (exact MAD, no ceiling) still decides genuinely dispersed windows.
var localScaleRelCeiling = big.NewRat(1, 100)

func newRobustRef(prices []*big.Rat, w []int64) *robustRef {
	c, s := weightedCentreScale(prices, w)
	return &robustRef{centre: c, scale: s}
}

// newLocalRef is [newRobustRef] with the scale clamped to
// [localScaleRelFloor, localScaleRelCeiling]·|centre|.
func newLocalRef(prices []*big.Rat, w []int64) *robustRef {
	r := newRobustRef(prices, w)
	floor := new(big.Rat).Mul(localScaleRelFloor, r.centre)
	floor.Abs(floor)
	if floor.Cmp(r.scale) > 0 {
		r.scale = floor
	}
	ceiling := new(big.Rat).Mul(localScaleRelCeiling, r.centre)
	ceiling.Abs(ceiling)
	if ceiling.Sign() > 0 && ceiling.Cmp(r.scale) < 0 {
		r.scale = ceiling
	}
	return r
}

// score returns the ratio-symmetric deviation of p from centre ([symmetricDev]) over scale. A zero scale scores an exact match 0
// and anything else ok=false; so does a non-positive price against a positive centre.
func (r *robustRef) score(p *big.Rat) (*big.Rat, bool) {
	dev := symmetricDev(p, r.centre)
	if dev == nil {
		return nil, false
	}
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
	// ref is the bucket's TRUSTED local reference: nil when the bucket
	// holds fewer than opts.MinBucket prices or its centre is not
	// anchored (see [localIndex.anchorBuckets]).
	ref *robustRef
}

// localIndex is the time-ordered view of a window's usable prices
// plus its bucket partition — everything [localIndex.score] needs.
type localIndex struct {
	opts   LocalOutlierOptions
	prices []*big.Rat
	// weights[pos] is the trade count of prices[pos]; nil means 1 each.
	weights []int64
	// order[k] = position into prices, sorted by trade time (stable,
	// so equal timestamps keep input order).
	order []int
	// bucketOf[k] is the bucket index of order-position k.
	bucketOf []int
	buckets  []*priceBucket
	window   *robustRef
	// anchorTol is sigma · max(window scale, localScaleRelCeiling·|window centre|); a local reference is trusted only when its centre is
	// within it of the window centre or lastAnchored.
	anchorTol *big.Rat
	// lastAnchored is the centre of the most recent trusted local
	// reference in time order (nil before the first) — the chain a
	// step walks along and a wash burst cannot join.
	lastAnchored *big.Rat
}

// TradeOrderLess is the total order for position-sensitive trade slices: close time, ledger, tx_hash, op_index, then source (tx_hash before source keeps it venue-neutral and independent of merge order).
func TradeOrderLess(a, b *canonical.Trade) bool {
	if !a.Timestamp.Equal(b.Timestamp) {
		return a.Timestamp.Before(b.Timestamp)
	}
	if a.Ledger != b.Ledger {
		return a.Ledger < b.Ledger
	}
	if a.TxHash != b.TxHash {
		return a.TxHash < b.TxHash
	}
	if a.OpIndex != b.OpIndex {
		return a.OpIndex < b.OpIndex
	}
	return a.Source < b.Source
}

// newLocalIndex sorts usable prices by trade time (ties by [TradeOrderLess], so order depends on the trade set, not the caller's merge
// order) and partitions them into opts.Bucket-wide buckets.
func newLocalIndex(trades []canonical.Trade, validIdx []int, prices []*big.Rat, weights []int64, opts LocalOutlierOptions) *localIndex {
	ix := &localIndex{opts: opts, prices: prices, weights: weights, window: newRobustRef(prices, weights)}
	ix.order = make([]int, len(prices))
	for k := range ix.order {
		ix.order[k] = k
	}
	sort.SliceStable(ix.order, func(a, b int) bool {
		return TradeOrderLess(&trades[validIdx[ix.order[a]]], &trades[validIdx[ix.order[b]]])
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
	ix.anchorTol = anchorTolerance(ix.window, opts.Sigma)
	ix.anchorBuckets()
	return ix
}

// anchorTolerance returns sigma*max(window.scale, localScaleRelCeiling*|window.centre|): how far a local centre may sit from its
// anchor and stay trusted. The ceiling term keeps a tight window from rejecting a genuine 2% step's own bucket.
func anchorTolerance(window *robustRef, sigma float64) *big.Rat {
	tol := new(big.Rat).Mul(localScaleRelCeiling, window.centre)
	tol.Abs(tol)
	if window.scale.Cmp(tol) > 0 {
		tol.Set(window.scale)
	}
	sigmaRat := new(big.Rat).SetFloat64(sigma)
	if sigmaRat == nil {
		return tol
	}
	return tol.Mul(tol, sigmaRat)
}

// anchored reports whether centre is within ix.anchorTol of the window
// centre or of the previous trusted reference, and on success records
// it as the new chain head.
func (ix *localIndex) anchored(centre *big.Rat) bool {
	ok := withinTol(centre, ix.window.centre, ix.anchorTol) ||
		(ix.lastAnchored != nil && withinTol(centre, ix.lastAnchored, ix.anchorTol))
	if ok {
		ix.lastAnchored = centre
	}
	return ok
}

func withinTol(a, b, tol *big.Rat) bool {
	d := new(big.Rat).Sub(a, b)
	d.Abs(d)
	return d.Cmp(tol) <= 0
}

// anchorBuckets builds each qualifying bucket's reference in time order and keeps only anchored ones; the chain runs over buckets alone,
// so a bucket after a thin stretch still chains from the last dense bucket (or the window).
func (ix *localIndex) anchorBuckets() {
	ix.lastAnchored = nil
	for _, b := range ix.buckets {
		var n int64
		for _, k := range b.members {
			n += ix.weight(ix.order[k])
		}
		if n < int64(ix.opts.MinBucket) {
			continue
		}
		ps := make([]*big.Rat, len(b.members))
		var ws []int64
		if ix.weights != nil {
			ws = make([]int64, len(b.members))
		}
		for j, k := range b.members {
			ps[j] = ix.prices[ix.order[k]]
			if ws != nil {
				ws[j] = ix.weights[ix.order[k]]
			}
		}
		if ref := newLocalRef(ps, ws); ix.anchored(ref.centre) {
			b.ref = ref
		}
	}
	ix.lastAnchored = nil
}

// bucketRef returns bucket bi's trusted local reference; nil when bi
// is out of range or the bucket does not qualify (too thin or
// unanchored).
func (ix *localIndex) bucketRef(bi int) *robustRef {
	if bi < 0 || bi >= len(ix.buckets) {
		return nil
	}
	return ix.buckets[bi].ref
}

// weight returns the trade count of prices[pos].
func (ix *localIndex) weight(pos int) int64 {
	if ix.weights == nil {
		return 1
	}
	return ix.weights[pos]
}

// neighbourhoodRef builds the count-neighbourhood reference for order-position k (nearest opts.Neighbours trades each side, excluding k;
// a weighted row's other trades sit beside it, half each side); nil when k has no neighbours or the centre is not anchored. Call in time
// order: it advances the anchor chain.
func (ix *localIndex) neighbourhoodRef(k int) *robustRef {
	per := int64(ix.opts.Neighbours)
	own := ix.weight(ix.order[k]) - 1
	ownL, ownR := min(own/2, per), min(own-own/2, per)
	var neigh []*big.Rat
	var ws []int64
	add := func(pos int, n int64) {
		neigh = append(neigh, ix.prices[pos])
		ws = append(ws, n)
	}
	if ownL+ownR > 0 {
		add(ix.order[k], ownL+ownR)
	}
	for _, side := range [2]struct {
		step   int
		budget int64
	}{{-1, per - ownL}, {1, per - ownR}} {
		for j := k + side.step; side.budget > 0 && j >= 0 && j < len(ix.order); j += side.step {
			n := min(ix.weight(ix.order[j]), side.budget)
			add(ix.order[j], n)
			side.budget -= n
		}
	}
	if len(neigh) == 0 {
		return nil
	}
	if ix.weights == nil {
		ws = nil
	}
	ref := newLocalRef(neigh, ws)
	if !ix.anchored(ref.centre) {
		return nil
	}
	return ref
}

// score returns the SMALLEST sigma-equivalent distance from order-position k to any trusted reference (window, own bucket, adjacent buckets,
// or the count-neighbourhood if its bucket does not qualify); nil if none gave a finite score. Call in time order (advances the anchor chain).
func (ix *localIndex) score(k int) *big.Rat {
	p := ix.prices[ix.order[k]]
	best := minScore(nil, ix.window, p)
	bi := ix.bucketOf[k]
	own := ix.bucketRef(bi)
	if own != nil {
		ix.lastAnchored = own.centre
	}
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

// outlierScores returns the usable-price trade indices (input order) and each one's z, the SMALLEST sigma-equivalent distance to any
// reference ([localIndex.score]); a nil entry means no finite score, and z is nil with fewer than 3 usable prices.
func outlierScores(trades []canonical.Trade, opts LocalOutlierOptions) (validIdx []int, z []*big.Rat) {
	opts = opts.withDefaults()

	prices := make([]*big.Rat, 0, len(trades))
	validIdx = make([]int, 0, len(trades))
	var weights []int64
	var total int64
	for i := range trades {
		p, ok := priceRat(&trades[i])
		if !ok {
			continue
		}
		prices = append(prices, p)
		validIdx = append(validIdx, i)
		w := int64(1)
		if opts.Weight != nil {
			w = max(opts.Weight(&trades[i]), 1)
			weights = append(weights, w)
		}
		total += w
	}
	if total < 3 {
		return validIdx, nil
	}

	ix := newLocalIndex(trades, validIdx, prices, weights, opts)
	z = make([]*big.Rat, len(prices))
	for k, pos := range ix.order {
		z[pos] = ix.score(k)
	}
	return validIdx, z
}
