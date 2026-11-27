package v1

import (
	"math/big"
	"time"
)

// usdLegs is a valuer's fixed legs as an array type L, so a zero sum still has every leg; P is *L, seen as a slice.
type usdLegs[L any] interface {
	*L
	slice() []*big.Rat
}

// usdSum is exact USD per leg; a leg is nil until an hour is valued. bases counts priced hours per basis, "" fiat.
type usdSum[L any, P usdLegs[L]] struct {
	v                         L
	priced, unpriced, carried int
	bases                     map[string]int
}

// usdAmountHour is one hour's exact base-unit amounts, one per leg in the valuer's leg order.
type usdAmountHour struct {
	start   time.Time
	amounts []*big.Int
}

// usdDay is one asset's USD for one UTC day; reason is the price data's own reason no hour could be valued.
type usdDay[L any, P usdLegs[L]] struct {
	sum     usdSum[L, P]
	reason  string
	outcome revenueDayOutcome
}

// usdWire is a sum's served facts; legs are rounded once, and nil together when reason is set.
type usdWire struct {
	legs                      []*string
	priced, unpriced, carried int
	basis                     string
	basisHours                map[string]int
	lowerBound                bool
	reason                    *string
}

func (t *usdSum[L, P]) legs() []*big.Rat { return P(&t.v).slice() }

func (t *usdSum[L, P]) add(o *usdSum[L, P]) {
	dst := t.legs()
	for i, x := range o.legs() {
		if x == nil {
			continue
		}
		if dst[i] == nil {
			dst[i] = new(big.Rat)
		}
		dst[i].Add(dst[i], x)
	}
	t.priced, t.unpriced, t.carried = t.priced+o.priced, t.unpriced+o.unpriced, t.carried+o.carried
	for b, n := range o.bases {
		if t.bases == nil {
			t.bases = map[string]int{}
		}
		t.bases[b] += n
	}
}

// valueHour adds one hour's amounts at that hour's price; an hour without a price within the bound is counted unpriced.
func (t *usdSum[L, P]) valueHour(u reserveUSDSeries, at time.Time, amounts []*big.Int) {
	p, ok := u.priceAt(at)
	if u.reason != "" || !ok {
		t.unpriced++
		return
	}
	basis := u.basis
	if basis == "" && u.stableAt[p.Bucket.UTC()] {
		basis = priceBasisStablecoinProxy
	}
	h := usdSum[L, P]{priced: 1, bases: map[string]int{basis: 1}}
	legs := h.legs()
	for i, a := range amounts {
		v, err := usdValueRat(a, p.VWAP, u.dec)
		if err != nil {
			t.unpriced++
			return
		}
		legs[i] = v
	}
	if at.Sub(p.Bucket) > u.step {
		h.carried = 1
	}
	t.add(&h)
}

// wire rounds once, here; reason (or no hour valued against an unpriced one) nulls every leg.
func (t *usdSum[L, P]) wire(reason string, lowerBound bool) usdWire {
	out := usdWire{priced: t.priced, unpriced: t.unpriced, carried: t.carried, lowerBound: lowerBound || t.unpriced > 0}
	if reason == "" && t.priced == 0 && t.unpriced > 0 {
		reason = lendingUSDNoPriceInBound
	}
	if reason != "" {
		out.reason = strRef(reason)
		return out
	}
	for _, x := range t.legs() {
		if x == nil {
			x = new(big.Rat)
		}
		out.legs = append(out.legs, strRef(ratString(x, 2)))
	}
	for b, n := range t.bases {
		switch {
		case b == "":
		case n == t.priced:
			out.basis = b
		default:
			if out.basisHours == nil {
				out.basisHours = map[string]int{}
			}
			out.basisHours[b] = n
		}
	}
	return out
}

func amountsNonZero(amounts []*big.Int) bool {
	for _, a := range amounts {
		if a.Sign() != 0 {
			return true
		}
	}
	return false
}

func zeroAmounts(n int) []*big.Int {
	out := make([]*big.Int, n)
	for i := range out {
		out[i] = new(big.Int)
	}
	return out
}

// valueUSDBucket values the bucket at start, whose amounts are total, from hours[h:] that fall inside it and returns the
// index past them; whatever of total those hours do not account for counts as one unpriced hour.
func valueUSDBucket[L any, P usdLegs[L]](start time.Time, total []*big.Int, hours []usdAmountHour, h int, u reserveUSDSeries, width time.Duration) (usdSum[L, P], int) {
	var sum usdSum[L, P]
	seen := zeroAmounts(len(total))
	for ; h < len(hours) && hours[h].start.Before(start.Add(width)); h++ {
		if hours[h].start.Before(start) {
			continue
		}
		for i, a := range hours[h].amounts {
			seen[i].Add(seen[i], a)
		}
		if amountsNonZero(hours[h].amounts) {
			sum.valueHour(u, hours[h].start, hours[h].amounts)
		}
	}
	for i := range seen {
		if seen[i].Cmp(total[i]) != 0 {
			sum.unpriced++
			break
		}
	}
	return sum, h
}

// valueUSDDay values one UTC day's hours with walk's read. A day with nothing to value is zero without a read; walk is
// called only when one is needed, and a refused or failed read values nothing.
func valueUSDDay[L any, P usdLegs[L]](day time.Time, hours []usdAmountHour, walk func() reserveUSDRead) usdDay[L, P] {
	var d usdDay[L, P]
	total := zeroAmounts(len(d.sum.legs()))
	amountHours := 0
	for _, h := range hours {
		for i, a := range h.amounts {
			total[i].Add(total[i], a)
		}
		if amountsNonZero(h.amounts) {
			amountHours++
		}
	}
	if amountHours == 0 {
		return d
	}
	r := walk()
	switch revenueReadRefusal(r) {
	case exclusionPriceWithheld:
		// Every hour carrying an amount is unpriced, so the stored row's legs are NULL: a refusal is never a zero.
		d.outcome, d.reason, d.sum.unpriced = revenueDayRefused, r.series.reason, amountHours
		return d
	case exclusionPriceReadFailed:
		d.outcome = revenueDayDegraded
		return d
	}
	d.sum, _ = valueUSDBucket[L, P](day, total, hours, 0, r.series, day24)
	d.reason = r.series.reason
	return d
}

// storedLegs is the day's legs as exact decimals, all nil when no hour was valued against an unpriced one; ok is false
// (legs cut short) when a leg is not a finite decimal.
func (d *usdDay[L, P]) storedLegs() ([]*string, bool) {
	if d.sum.priced == 0 && d.sum.unpriced > 0 {
		return nil, true
	}
	src := d.sum.legs()
	out := make([]*string, len(src))
	for i, x := range src {
		if x == nil {
			x = new(big.Rat)
		}
		s, ok := ratExactDecimal(x)
		if !ok {
			return out, false
		}
		out[i] = &s
	}
	return out, true
}

// storedBasisHours is the stored form of a sum's priced hours per basis, fiat named.
func storedBasisHours(bases map[string]int) map[string]int {
	out := map[string]int{}
	for b, n := range bases {
		if b == "" {
			b = "fiat"
		}
		out[b] = n
	}
	return out
}
