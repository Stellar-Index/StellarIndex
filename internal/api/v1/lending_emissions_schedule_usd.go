package v1

import (
	"context"
	"math/big"
	"time"
)

// segmentEnd is where segs[i] stops emitting: its expiration, or the next update if that comes first.
func segmentEnd(segs []emissionSegment, i int) time.Time {
	if i+1 < len(segs) && segs[i+1].start.Before(segs[i].exp) {
		return segs[i+1].start
	}
	return segs[i].exp
}

// emisPrices is a walk's series with every price over one common denominator, parsed once for every schedule line.
type emisPrices struct {
	u      reserveUSDSeries
	scaled []*big.Int // points[i].VWAP x den; nil when it does not parse, which leaves its hours unpriced
	denom  *big.Int   // den x 10^(decimals + scheduleScale): a USD sum is (Σ units x scaled) / denom, exact
}

func newEmisPrices(u reserveUSDSeries) *emisPrices {
	p := &emisPrices{u: u, scaled: make([]*big.Int, len(u.points))}
	rats := make([]*big.Rat, len(u.points))
	den, g := big.NewInt(1), new(big.Int)
	for i, pt := range u.points {
		r, ok := new(big.Rat).SetString(pt.VWAP)
		if !ok {
			continue
		}
		rats[i] = r
		g.GCD(nil, nil, den, r.Denom())
		den.Mul(den, new(big.Int).Quo(r.Denom(), g))
	}
	for i, r := range rats {
		if r != nil {
			p.scaled[i] = new(big.Int).Mul(r.Num(), new(big.Int).Quo(den, r.Denom()))
		}
	}
	p.denom = new(big.Int).Mul(den, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(u.dec+scheduleScale)), nil))
	return p
}

// lineScratch is one line's reused integers, so the hour loop allocates nothing.
type lineScratch struct{ acc, total, run, units, tmp *big.Int }

// line values one schedule line as the shared valuer would hour by hour (each hour at its own price within the bound,
// the same priced, unpriced and carried counts) but multiplies once per run of hours sharing one price bucket:
// Σ uᵢ·p = (Σ uᵢ)·p exactly. The request context is checked once per bucket.
func (p *emisPrices) line(ctx context.Context, segs []emissionSegment, win emissionsHistoryWindow) (emisOneSum, []emisOneSum, error) {
	var total emisOneSum
	if !win.end.After(win.from) {
		return total, nil, nil
	}
	out := make([]emisOneSum, int((win.end.Sub(win.from)+win.width-1)/win.width))
	sc := lineScratch{new(big.Int), new(big.Int), new(big.Int), new(big.Int), new(big.Int)}
	si, pi := 0, 0
	for b := range out {
		if err := ctx.Err(); err != nil {
			return total, out, err
		}
		bs := win.from.Add(time.Duration(b) * win.width)
		p.bucket(&out[b], segs, bs, minTime(bs.Add(win.width), win.end), &si, &pi, sc)
		total.priced, total.unpriced, total.carried = total.priced+out[b].priced, total.unpriced+out[b].unpriced, total.carried+out[b].carried
		for k, n := range out[b].bases {
			if total.bases == nil {
				total.bases = map[string]int{}
			}
			total.bases[k] += n
		}
	}
	if total.priced > 0 {
		total.v[0] = new(big.Rat).SetFrac(sc.total, p.denom)
	}
	return total, out, nil
}

// bucket values the hours of [bs, be) into s; si and pi are the segment and price cursors, which only move forward.
func (p *emisPrices) bucket(s *emisOneSum, segs []emissionSegment, bs, be time.Time, si, pi *int, sc lineScratch) {
	u := p.u
	sc.acc.SetInt64(0)
	runIdx, fiat, stable := -1, 0, 0
	flush := func() {
		if runIdx >= 0 {
			sc.acc.Add(sc.acc, sc.tmp.Mul(sc.run, p.scaled[runIdx]))
		}
	}
	for hs := bs; hs.Before(be); hs = hs.Add(time.Hour) {
		if !hourUnits(segs, si, hs, minTime(hs.Add(time.Hour), be), sc) {
			continue
		}
		idx, ok := p.priceAt(hs, pi)
		if !ok {
			s.unpriced++
			continue
		}
		if idx != runIdx {
			flush()
			runIdx = idx
			sc.run.SetInt64(0)
		}
		sc.run.Add(sc.run, sc.units)
		s.priced++
		if hs.Sub(u.points[idx].Bucket) > u.step {
			s.carried++
		}
		if u.basis == "" && u.stableAt[u.points[idx].Bucket.UTC()] {
			stable++
		} else {
			fiat++
		}
	}
	flush()
	if s.priced == 0 {
		return
	}
	s.v[0] = new(big.Rat).SetFrac(sc.acc, p.denom)
	sc.total.Add(sc.total, sc.acc)
	s.bases = scheduleBases(u.basis, fiat, stable)
}

// hourUnits sets sc.units to the line's eps x nanoseconds over [hs, he) and reports whether any; *si skips the segments
// that ended by hs, which no later hour reaches either.
func hourUnits(segs []emissionSegment, si *int, hs, he time.Time, sc lineScratch) bool {
	for *si < len(segs) && !segmentEnd(segs, *si).After(hs) {
		*si++
	}
	sc.units.SetInt64(0)
	for k := *si; k < len(segs) && segs[k].start.Before(he); k++ {
		if lo, hi := maxTime(segs[k].start, hs), minTime(segmentEnd(segs, k), he); hi.After(lo) {
			sc.units.Add(sc.units, sc.tmp.Mul(sc.tmp.SetInt64(hi.Sub(lo).Nanoseconds()), segs[k].eps))
		}
	}
	return sc.units.Sign() != 0
}

// priceAt is reserveUSDSeries.priceAt with a forward cursor: the index of the newest price at or before hs, if it is
// within the bound and parses.
func (p *emisPrices) priceAt(hs time.Time, pi *int) (int, bool) {
	u := p.u
	for *pi < len(u.points) && !u.points[*pi].Bucket.After(hs) {
		*pi++
	}
	idx := *pi - 1
	return idx, u.reason == "" && idx >= 0 && hs.Sub(u.points[idx].Bucket) <= u.maxAge && p.scaled[idx] != nil
}

// scheduleBases is a bucket's priced hours per basis; nil when all are fiat, which the wire never names.
func scheduleBases(basis string, fiat, stable int) map[string]int {
	if basis == "" && stable == 0 {
		return nil
	}
	out := map[string]int{}
	if fiat > 0 {
		out[basis] = fiat
	}
	if stable > 0 {
		out[priceBasisStablecoinProxy] = stable
	}
	return out
}
