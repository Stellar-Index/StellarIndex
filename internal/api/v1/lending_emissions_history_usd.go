package v1

import (
	"context"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The emissions history's USD reasons, beyond the price series' and the identity gate's own.
const (
	emisUSDReadsDisagree = "reads_disagree"     // a 1d bucket's hours do not add up to it, so none of it is valued
	emisUSDHoursUnread   = "hourly_read_failed" // the hourly read behind 1d buckets failed; the days are served in tokens
)

// emisUSDOne and emisUSDTwo are the emission valuer's legs: one amount, or an allocation's backstop and pool shares.
type (
	emisUSDOne [1]*big.Rat
	emisUSDTwo [2]*big.Rat
)

func (a *emisUSDOne) slice() []*big.Rat { return a[:] }
func (a *emisUSDTwo) slice() []*big.Rat { return a[:] }

type (
	emisOneSum = usdSum[emisUSDOne, *emisUSDOne]
	emisTwoSum = usdSum[emisUSDTwo, *emisUSDTwo]
)

// lendingEmissionUSDCoverage is how much of a figure's emission tokens were valued, as on /revenue's usd.
type lendingEmissionUSDCoverage struct {
	// UnpricedHours carried an amount but no price within the bound: left out, so the figure is a lower bound.
	PricedHours       int            `json:"priced_hours"`
	UnpricedHours     int            `json:"unpriced_hours"`
	PriceCarriedHours int            `json:"price_carried_hours"`
	PriceBasis        string         `json:"price_basis,omitempty"`
	PriceBasisHours   map[string]int `json:"price_basis_hours,omitempty"`
	LowerBound        bool           `json:"lower_bound"`
	USDReason         *string        `json:"usd_reason"`
}

// LendingEmissionUSD is an emission-token amount valued hour by hour at the token's hourly price, rounded once; null with
// usd_reason when nothing of it could be valued.
type LendingEmissionUSD struct {
	AmountUSD *string `json:"amount_usd"`
	// Approximate: the tokens valued are an approximate schedule's (BLNT's, rebuilt to dust); not a lower bound.
	Approximate bool `json:"approximate,omitempty"`
	lendingEmissionUSDCoverage
	exact *big.Rat // AmountUSD before its one rounding, which the emissions rollup stores
}

// LendingEmissionAllocUSD is a backstop allocation's two shares valued as LendingEmissionUSD, null together.
type LendingEmissionAllocUSD struct {
	BackstopEmissionsUSD *string `json:"backstop_emissions_usd"`
	PoolEmissionsUSD     *string `json:"pool_emissions_usd"`
	lendingEmissionUSDCoverage
	exact [2]*big.Rat
}

// exactLegs is a sum's legs unrounded, zero for a leg no hour valued; nil when w served none.
func exactLegs[L any, P usdLegs[L]](t *usdSum[L, P], w usdWire) []*big.Rat {
	if w.legs == nil {
		return nil
	}
	out := make([]*big.Rat, 0, len(w.legs))
	for _, x := range t.legs() {
		if x == nil {
			x = new(big.Rat)
		}
		out = append(out, x)
	}
	return out
}

func emisUSDCoverage(w usdWire) lendingEmissionUSDCoverage {
	return lendingEmissionUSDCoverage{
		PricedHours: w.priced, UnpricedHours: w.unpriced, PriceCarriedHours: w.carried, PriceBasis: w.basis,
		PriceBasisHours: w.basisHours, LowerBound: w.lowerBound, USDReason: w.reason,
	}
}

// emisWire is t on the wire. A sum with nothing to value is zero (no tokens, no USD); one that valued nothing because
// its hours disagree with it says so.
func emisWire[L any, P usdLegs[L]](t *usdSum[L, P], reason string, disagreed, lowerBound bool) usdWire {
	switch {
	case t.priced+t.unpriced == 0:
		reason = ""
	case reason == "" && t.priced == 0 && disagreed:
		reason = emisUSDReadsDisagree
	}
	return t.wire(reason, lowerBound)
}

func emisOneUSD(t *emisOneSum, reason string, disagreed, lowerBound bool) LendingEmissionUSD {
	w := emisWire(t, reason, disagreed, lowerBound)
	out := LendingEmissionUSD{lendingEmissionUSDCoverage: emisUSDCoverage(w)}
	if x := exactLegs(t, w); x != nil {
		out.AmountUSD, out.exact = w.legs[0], x[0]
	}
	return out
}

func emisTwoUSD(t *emisTwoSum, reason string, disagreed, lowerBound bool) LendingEmissionAllocUSD {
	w := emisWire(t, reason, disagreed, lowerBound)
	out := LendingEmissionAllocUSD{lendingEmissionUSDCoverage: emisUSDCoverage(w)}
	if x := exactLegs(t, w); x != nil {
		out.BackstopEmissionsUSD, out.PoolEmissionsUSD = w.legs[0], w.legs[1]
		out.exact = [2]*big.Rat{x[0], x[1]}
	}
	return out
}

// emisCount is a bucket's events and how many of their amounts were not read.
type emisCount struct{ n, unread int64 }

// markUnread: a figure none of whose amounts were read is null (amount_unread), never "0.00"; one missing some is a
// lower bound.
func markUnread(c *lendingEmissionUSDCoverage, nullify func(), k emisCount) {
	switch {
	case k.unread == 0:
	case k.unread >= k.n:
		nullify()
		c.USDReason, c.LowerBound = strRef(emissionsReasonAmountUnread), true
	default:
		c.LowerBound = true
	}
}

func markOneUnread(u *LendingEmissionUSD, k emisCount) {
	markUnread(&u.lendingEmissionUSDCoverage, func() { u.AmountUSD, u.exact = nil, nil }, k)
}

func markTwoUnread(u *LendingEmissionAllocUSD, k emisCount) {
	markUnread(&u.lendingEmissionUSDCoverage, func() {
		u.BackstopEmissionsUSD, u.PoolEmissionsUSD, u.exact = nil, nil, [2]*big.Rat{}
	}, k)
}

// emisRows is one stream's buckets (b, with their counts c) and the hours they are valued from (h).
type emisRows struct {
	b, h []usdAmountHour
	c    []emisCount
}

func (r emisRows) total() emisCount {
	var t emisCount
	for _, k := range r.c {
		t.n, t.unread = t.n+k.n, t.unread+k.unread
	}
	return t
}

// hoursDisagree: a leg's hours carry more than its bucket, or none of a bucket that has some. Fewer is a lower bound.
func hoursDisagree(seen, total []*big.Int) bool {
	for i := range seen {
		if seen[i].Cmp(total[i]) > 0 || (seen[i].Sign() == 0 && total[i].Sign() != 0) {
			return true
		}
	}
	return false
}

// valueEmisBuckets values each bucket from the hours inside it through the shared valuer. A bucket its hours disagree
// with values nothing and is marked, so a figure never exceeds what its served tokens support.
func valueEmisBuckets[L any, P usdLegs[L]](buckets, hours []usdAmountHour, u reserveUSDSeries, width time.Duration) (usdSum[L, P], []usdSum[L, P], []bool) {
	var total usdSum[L, P]
	out, bad := make([]usdSum[L, P], len(buckets)), make([]bool, len(buckets))
	h := 0
	for j, b := range buckets {
		for h < len(hours) && hours[h].start.Before(b.start) {
			h++
		}
		seen, k, n := zeroAmounts(len(b.amounts)), h, 0
		for ; k < len(hours) && hours[k].start.Before(b.start.Add(width)); k++ {
			for i, a := range hours[k].amounts {
				seen[i].Add(seen[i], a)
			}
			if amountsNonZero(hours[k].amounts) {
				n++
			}
		}
		if hoursDisagree(seen, b.amounts) {
			out[j], bad[j], h = usdSum[L, P]{unpriced: max(n, 1)}, true, k
		} else {
			out[j], h = valueUSDBucket[L, P](b.start, b.amounts, hours, h, u, width)
		}
		total.add(&out[j])
	}
	return total, out, bad
}

func anyTrue(bs []bool) bool {
	for _, b := range bs {
		if b {
			return true
		}
	}
	return false
}

func oneAmount(start time.Time, a *big.Int) usdAmountHour {
	return usdAmountHour{start: start, amounts: []*big.Int{a}}
}

// emisUSDInput is every stream's amounts. Schedule lines are valued from their timelines, not from rows.
type emisUSDInput struct {
	segs  [][]emissionSegment
	pool  emisRows
	res   map[uint32]*emisRows
	alloc emisRows
	tier  map[string]*emisRows
}

func claimRows(day, hour []timescale.EmissionClaimBucket) (emisRows, map[uint32]*emisRows) {
	var pool emisRows
	res := map[uint32]*emisRows{}
	line := func(id uint32) *emisRows {
		if res[id] == nil {
			res[id] = &emisRows{}
		}
		return res[id]
	}
	for _, r := range day {
		k, a := emisCount{r.Claims, r.AmountUnread}, oneAmount(r.Start, r.Amount.BigInt())
		switch {
		case r.Scope == timescale.EmissionClaimScopePool:
			pool.b, pool.c = append(pool.b, a), append(pool.c, k)
		case r.Scope == timescale.EmissionClaimScopeSingle && r.TokenID != nil:
			l := line(*r.TokenID)
			l.b, l.c = append(l.b, a), append(l.c, k)
		}
	}
	for _, r := range hour {
		a := oneAmount(r.Start, r.Amount.BigInt())
		switch {
		case r.Scope == timescale.EmissionClaimScopePool:
			pool.h = append(pool.h, a)
		case r.Scope == timescale.EmissionClaimScopeSingle && r.TokenID != nil:
			l := line(*r.TokenID)
			l.h = append(l.h, a)
		}
	}
	return pool, res
}

func allocRows(day, hour []timescale.EmissionAllocBucket) emisRows {
	amounts := func(r timescale.EmissionAllocBucket) usdAmountHour {
		return usdAmountHour{start: r.Start, amounts: []*big.Int{r.BackstopShare.BigInt(), r.PoolShare.BigInt()}}
	}
	var out emisRows
	for _, r := range day {
		out.b, out.c = append(out.b, amounts(r)), append(out.c, emisCount{r.Events, r.Unread})
	}
	for _, r := range hour {
		out.h = append(out.h, amounts(r))
	}
	return out
}

func tierRows(day, hour []timescale.EmissionBackstopClaimBucket) map[string]*emisRows {
	out := map[string]*emisRows{}
	tier := func(t string) *emisRows {
		if out[t] == nil {
			out[t] = &emisRows{}
		}
		return out[t]
	}
	for _, r := range day {
		t := tier(r.Tier)
		t.b, t.c = append(t.b, oneAmount(r.Start, r.BLNT.BigInt())), append(t.c, emisCount{r.Claims, r.Unread})
	}
	for _, r := range hour {
		t := tier(r.Tier)
		t.h = append(t.h, oneAmount(r.Start, r.BLNT.BigInt()))
	}
	return out
}

// emisUSDInputs gathers what the view's available streams serve: buckets from h, hours from hourly (h itself at 1h).
// The schedule is skipped when refused: it would only be counted unpriced.
func emisUSDInputs(v *LendingEmissionsHistoryView, h, hourly timescale.LendingEmissionsHistory, refused bool) emisUSDInput {
	var in emisUSDInput
	if v.Reserves.Available && !refused {
		timelines, ids := emissionTimelines(h.Schedule)
		for _, id := range ids {
			in.segs = append(in.segs, timelines[id].segments)
		}
	}
	in.pool, in.res = claimRows(h.Claims, hourly.Claims)
	if v.BackstopAlloc.Available {
		in.alloc = allocRows(h.Alloc, hourly.Alloc)
	}
	if v.BackstopClaims.Available {
		in.tier = tierRows(h.BackstopClaims, hourly.BackstopClaims)
	}
	return in
}

// span is the first and last hour carrying an amount in any stream: the one price walk covers only those.
func (in emisUSDInput) span(win emissionsHistoryWindow) (first, last time.Time, ok bool) {
	see := func(lo, hi time.Time) {
		if !ok || lo.Before(first) {
			first = lo
		}
		if !ok || hi.After(last) {
			last = hi
		}
		ok = true
	}
	rows := []emisRows{in.pool, in.alloc}
	for _, r := range in.res {
		rows = append(rows, *r)
	}
	for _, r := range in.tier {
		rows = append(rows, *r)
	}
	for _, r := range rows {
		for _, h := range r.h {
			if amountsNonZero(h.amounts) {
				see(h.start, h.start)
			}
		}
	}
	for _, segs := range in.segs {
		scheduleSpan(segs, win, see)
	}
	return first, last, ok
}

// scheduleSpan calls see with the first and last hour of each segment's emitting span inside the window.
func scheduleSpan(segs []emissionSegment, win emissionsHistoryWindow, see func(lo, hi time.Time)) {
	for i, s := range segs {
		lo, hi := maxTime(s.start, win.from), minTime(segmentEnd(segs, i), win.end)
		if s.eps.Sign() != 0 && hi.After(lo) {
			see(lo.Truncate(time.Hour), hi.Add(-time.Nanosecond).Truncate(time.Hour))
		}
	}
}

// emissionsHistoryUSD values every available stream in USD through revenue's walk: one read of the emission token's hourly
// series over the hours carrying an amount, once the token passes the protocol-token identity gate (reason ""). At 1d
// the rows are valued from h.Hours, read in the same snapshot; hoursFailed means that read failed. walkFailed: the price
// walk failed or was cut short.
func (s *Server) emissionsHistoryUSD(ctx context.Context, v *LendingEmissionsHistoryView, h timescale.LendingEmissionsHistory, b emissionsBounds, reason string, hoursFailed bool, flags *Flags) (walkFailed bool, err error) {
	v.PriceMaxAgeHours = int(lendingUSDHourlyPriceMaxAge / time.Hour)
	hourly := h
	if b.pool.width != time.Hour {
		hourly = timescale.LendingEmissionsHistory{}
		if h.Hours != nil {
			hourly = *h.Hours
		}
	}
	in := emisUSDInputs(v, h, hourly, reason != "")
	u := reserveUSDSeries{reason: reason}
	if first, last, ok := in.span(b.pool); ok && reason == "" {
		dec, known := 0, v.EmissionTokenDecimals != nil
		if known {
			dec = *v.EmissionTokenDecimals
		}
		u = s.reserveUSDWalk(ctx, *v.EmissionToken, dec, known, clickhouse.LendingHistoryQuery{From: first, To: last}, 0).series
		*flags = u.usdFlags(*flags)
	}
	rowSeries := u
	if hoursFailed && rowSeries.reason == "" {
		rowSeries.reason = emisUSDHoursUnread
	}
	walkFailed = u.degraded || u.reason == exclusionPriceReadFailed
	if err := valueEmissionSchedule(ctx, v, in, u, b.pool); err != nil {
		return walkFailed, err
	}
	valueEmissionRows(v, in, rowSeries, b.pool.width)
	return walkFailed, nil
}

// valueEmissionSchedule sets each schedule line's usd and its points'. A refused token values no line.
func valueEmissionSchedule(ctx context.Context, v *LendingEmissionsHistoryView, in emisUSDInput, u reserveUSDSeries, win emissionsHistoryWindow) error {
	var prices *emisPrices
	if in.segs != nil {
		prices = newEmisPrices(u)
	}
	for i := range v.Reserves.Streams {
		line := &v.Reserves.Streams[i]
		approx := v.Reserves.Approximate || line.Approximate
		if prices == nil {
			total := emisOneSum{unpriced: len(line.Points)}
			line.USD = emisOneUSD(&total, u.reason, false, v.Reserves.LowerBound)
			for j := range line.Points {
				line.Points[j].USD = emisOneUSD(&emisOneSum{unpriced: 1}, u.reason, false, false)
			}
			continue
		}
		total, bs, err := prices.line(ctx, in.segs[i], win)
		if err != nil {
			return err
		}
		line.USD = emisOneUSD(&total, u.reason, false, v.Reserves.LowerBound)
		line.USD.Approximate = approx && line.USD.AmountUSD != nil
		for j := range line.Points {
			p := &line.Points[j]
			p.USD = emisOneUSD(&bs[int(time.Time(p.T).Sub(win.from)/win.width)], u.reason, false, false)
			p.USD.Approximate = approx && p.USD.AmountUSD != nil
		}
	}
	return nil
}

// valueEmissionRows sets the claim and allocation streams' usd and their points'. The streams are layers of the same
// tokens: none is summed into another and no cross-stream USD total exists.
func valueEmissionRows(v *LendingEmissionsHistoryView, in emisUSDInput, u reserveUSDSeries, width time.Duration) {
	one := func(r emisRows, lowerBound bool, pts func(j int, usd LendingEmissionUSD)) LendingEmissionUSD {
		total, bs, bad := valueEmisBuckets[emisUSDOne](r.b, r.h, u, width)
		for j := range bs {
			usd := emisOneUSD(&bs[j], u.reason, bad[j], false)
			markOneUnread(&usd, r.c[j])
			pts(j, usd)
		}
		usd := emisOneUSD(&total, u.reason, anyTrue(bad), lowerBound)
		markOneUnread(&usd, r.total())
		return usd
	}
	pc := &v.PoolClaims
	pc.USD = one(in.pool, pc.LowerBound, func(j int, usd LendingEmissionUSD) { pc.Points[j].USD = usd })
	for i := range pc.ByReserve.Reserves {
		line := &pc.ByReserve.Reserves[i]
		r := emisRows{}
		if in.res[line.TokenID] != nil {
			r = *in.res[line.TokenID]
		}
		line.USD = one(r, pc.ByReserve.LowerBound, func(j int, usd LendingEmissionUSD) { line.Points[j].USD = usd })
	}
	for i := range v.BackstopClaims.Tiers {
		tier := &v.BackstopClaims.Tiers[i]
		r := emisRows{}
		if in.tier[tier.Tier] != nil {
			r = *in.tier[tier.Tier]
		}
		tier.USD = one(r, v.BackstopClaims.LowerBound, func(j int, usd LendingEmissionUSD) { tier.Points[j].USD = usd })
	}
	if ba := &v.BackstopAlloc; ba.Available {
		total, bs, bad := valueEmisBuckets[emisUSDTwo](in.alloc.b, in.alloc.h, u, width)
		for j := range ba.Points {
			ba.Points[j].USD = emisTwoUSD(&bs[j], u.reason, bad[j], false)
			markTwoUnread(&ba.Points[j].USD, in.alloc.c[j])
		}
		usd := emisTwoUSD(&total, u.reason, anyTrue(bad), ba.LowerBound)
		markTwoUnread(&usd, in.alloc.total())
		ba.USD = &usd
	}
}
