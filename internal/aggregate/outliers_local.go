package aggregate

import (
	"math/big"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Time-local outlier trimming for the published-VWAP path.
//
// The whole-window [FilterOutliers] trims an AGREED move larger than ~1%
// wholesale until it becomes the majority. A step is not an outlier; only a
// print that disagrees with the prints AROUND it is.
//
// A print is kept if it sits inside the band of ANY of: the whole-window
// centre/scale; its own time bucket (default 1 m) holding at least
// [DefaultOutlierMinBucket] prices; the nearest qualifying bucket either side;
// or, when its own is too thin, the nearest [DefaultOutlierNeighbours] prints.
//
// Local references are ANCHORED so a wash burst cannot validate itself: the local
// scale is CLAMPED to [localScaleRelFloor, localScaleRelCeiling]·centre, and a
// reference is TRUSTED only when its centre lies within sigma·max(window scale,
// ceiling·centre) of the window median OR the previous trusted reference.
//
// A burst holding the COUNT majority but not the base-volume majority withholds
// the window ([keepIfVolumeMajority]). Exact *big.Rat on the value path (ADR-0003).

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
	// AmountScaleDecimals resolves a trade source's smallest-unit scale
	// so the survivors' base volume is compared at one scale
	// ([keepIfVolumeMajority]). nil compares raw amounts, which is exact
	// only for a single-scale window.
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

// FilterOutliersLocal returns a copy of trades with the prints that
// disagree with BOTH the whole window and their time-local
// neighbourhood removed. See the file comment for the rationale and
// the exact reference set. Output preserves input order.
//
// Edge cases match [FilterOutliers]: Sigma <= 0 is a no-op copy;
// fewer than 3 usable prices returns the usable trades unchanged;
// zero-base / zero-quote trades are dropped before the statistics; a
// trim that keeps less base volume than it drops returns empty.
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
// reference keeps its exact MAD (and zero-MAD floor), so on tight
// pairs a sub-1% print is still judged by the legacy band.
var localScaleRelFloor = big.NewRat(1, 400)

// localScaleRelCeiling is the UPPER BOUND on a local reference's
// σ-equivalent scale, as a fraction of its centre: 1/100 = 1 %, i.e.
// a ±4 % band at the default sigma 4. A local reference's MAD is its
// own neighbourhood's dispersion — for a bucket of wash prints that
// gap 25–37 % between fills it is enormous, and an uncapped scale
// would let the bucket admit everything in it. Honest intra-minute
// dispersion on any served pair is far below 1 %, so the cap costs
// nothing on real data and the window reference (exact MAD, no
// ceiling) still decides genuinely dispersed windows.
var localScaleRelCeiling = big.NewRat(1, 100)

func newRobustRef(prices []*big.Rat) *robustRef {
	c, s := robustCentreScale(prices)
	return &robustRef{centre: c, scale: s}
}

// newLocalRef is [newRobustRef] with the scale clamped to
// [localScaleRelFloor, localScaleRelCeiling]·|centre|.
func newLocalRef(prices []*big.Rat) *robustRef {
	r := newRobustRef(prices)
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

// score returns the ratio-symmetric deviation of p from centre
// ([symmetricDev]) divided by scale — the σ-equivalent distance
// a caller compares against sigma. A zero scale (only reachable for a
// zero centre, where no relative floor exists) scores an exact match as
// 0 and anything else as "no finite score" (ok=false); so does a
// non-positive price, which has no finite ratio deviation from a
// positive centre.
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
	// order[k] = position into prices, sorted by trade time (stable,
	// so equal timestamps keep input order).
	order []int
	// bucketOf[k] is the bucket index of order-position k.
	bucketOf []int
	buckets  []*priceBucket
	window   *robustRef
	// anchorTol is the anchor tolerance: sigma · max(window scale,
	// localScaleRelCeiling·|window centre|). A local reference is
	// trusted only when its centre is within anchorTol of the window
	// centre or of lastAnchored.
	anchorTol *big.Rat
	// lastAnchored is the centre of the most recent trusted local
	// reference in time order (nil before the first) — the chain a
	// step walks along and a wash burst cannot join.
	lastAnchored *big.Rat
}

// TradeOrderLess is the total order trades are arranged in wherever slice
// position carries weight: close time, ledger, tx_hash, op_index, and the
// source name last. The local index and
// internal/api/v1.sortTradesChronological both sort with it.
//
// The tie-break is NOT decorative. Ledger-close
// timestamps are shared by every trade in the ledger, so same-timestamp
// prints are the common case, and the index references a print's
// neighbours BY POSITION ([localIndex.neighbourhoodRef]) and walks the
// anchor chain in this order. A merge-order-preserving sort would let the
// window's input assembly decide those positions, and the same window
// could produce different trim decisions tick to tick.
//
// tx_hash comes before source so the order is neutral as well as
// deterministic: a transaction hash carries no venue identity, so inside
// one ledger close no venue takes neighbourhood position or first claim
// on the anchor chain by the alphabetical rank of its registered name.
// Source only separates rows that share a transaction and operation.
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

// newLocalIndex sorts the usable prices by trade time (ties broken by
// [TradeOrderLess], so the arrangement is a function of the trade set
// and not of the caller's merge order) and partitions them into
// opts.Bucket-wide buckets.
func newLocalIndex(trades []canonical.Trade, validIdx []int, prices []*big.Rat, opts LocalOutlierOptions) *localIndex {
	ix := &localIndex{opts: opts, prices: prices, window: newRobustRef(prices)}
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

// anchorTolerance returns sigma · max(window.scale,
// localScaleRelCeiling·|window.centre|): how far a local reference's
// centre may sit from its anchor and still be trusted. The ceiling
// term keeps the tolerance meaningful on a tight window (a 0.2 % MAD
// would otherwise reject a genuine 2 % step's own bucket); the
// window-scale term keeps it consistent with the legacy band on a
// genuinely dispersed window.
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

// anchorBuckets builds each qualifying bucket's reference in time
// order and keeps only the anchored ones. The chain runs over
// buckets alone here: a bucket after a thin stretch still chains from
// the last dense bucket before it (or from the window).
func (ix *localIndex) anchorBuckets() {
	ix.lastAnchored = nil
	for _, b := range ix.buckets {
		if len(b.members) < ix.opts.MinBucket {
			continue
		}
		ps := make([]*big.Rat, len(b.members))
		for j, k := range b.members {
			ps[j] = ix.prices[ix.order[k]]
		}
		if ref := newLocalRef(ps); ix.anchored(ref.centre) {
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

// neighbourhoodRef builds the count-neighbourhood reference for
// order-position k: the nearest opts.Neighbours prices on each side,
// excluding k itself. nil when k has no neighbours at all or the
// neighbourhood's centre is not anchored. Must be called in time
// order (it advances the anchor chain).
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
	ref := newLocalRef(neigh)
	if !ix.anchored(ref.centre) {
		return nil
	}
	return ref
}

// score returns the SMALLEST σ-equivalent distance from the price at
// order-position k to any of its trusted references: the window, its
// own bucket, the adjacent buckets, and — when its own bucket does not
// qualify — its count-neighbourhood. nil when no reference produced a
// finite score (only possible around a zero centre). Must be called in
// time order: an own-bucket reference advances the anchor chain the
// neighbourhood references continue from.
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
