package v1

import (
	"context"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
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
	lendingEmissionUSDCoverage
}

// LendingEmissionAllocUSD is a backstop allocation's two shares valued as LendingEmissionUSD, null together.
type LendingEmissionAllocUSD struct {
	BackstopEmissionsUSD *string `json:"backstop_emissions_usd"`
	PoolEmissionsUSD     *string `json:"pool_emissions_usd"`
	lendingEmissionUSDCoverage
}

func emisUSDCoverage(w usdWire) lendingEmissionUSDCoverage {
	return lendingEmissionUSDCoverage{
		PricedHours: w.priced, UnpricedHours: w.unpriced, PriceCarriedHours: w.carried, PriceBasis: w.basis,
		PriceBasisHours: w.basisHours, LowerBound: w.lowerBound, USDReason: w.reason,
	}
}

// emisWire is t on the wire with the series' reason, except that a sum with nothing to value is zero: no tokens, no USD.
func emisWire[L any, P usdLegs[L]](t *usdSum[L, P], reason string, lowerBound bool) usdWire {
	if t.priced+t.unpriced == 0 {
		reason = ""
	}
	return t.wire(reason, lowerBound)
}

func emisOneUSD(t *emisOneSum, reason string, lowerBound bool) LendingEmissionUSD {
	w := emisWire(t, reason, lowerBound)
	out := LendingEmissionUSD{lendingEmissionUSDCoverage: emisUSDCoverage(w)}
	if w.legs != nil {
		out.AmountUSD = w.legs[0]
	}
	return out
}

func emisTwoUSD(t *emisTwoSum, reason string, lowerBound bool) LendingEmissionAllocUSD {
	w := emisWire(t, reason, lowerBound)
	out := LendingEmissionAllocUSD{lendingEmissionUSDCoverage: emisUSDCoverage(w)}
	if w.legs != nil {
		out.BackstopEmissionsUSD, out.PoolEmissionsUSD = w.legs[0], w.legs[1]
	}
	return out
}

// valueEmisBuckets values each bucket from the hours inside it, as revenue's reserves are: whatever of a bucket its
// hours do not account for counts as one unpriced hour. buckets and hours ascend.
func valueEmisBuckets[L any, P usdLegs[L]](buckets, hours []usdAmountHour, u reserveUSDSeries, width time.Duration) (usdSum[L, P], []usdSum[L, P]) {
	var total usdSum[L, P]
	out := make([]usdSum[L, P], len(buckets))
	h := 0
	for j, b := range buckets {
		out[j], h = valueUSDBucket[L, P](b.start, b.amounts, hours, h, u, width)
		total.add(&out[j])
	}
	return total, out
}

func oneAmount(start time.Time, a *big.Int) usdAmountHour {
	return usdAmountHour{start: start, amounts: []*big.Int{a}}
}

// emisUSDInput is every stream's amounts, per bucket and per hour; schedule amounts are at scheduleScale.
type emisUSDInput struct {
	schedB, schedH [][]usdAmountHour
	poolB, poolH   []usdAmountHour
	resB, resH     map[uint32][]usdAmountHour
	allocB, allocH []usdAmountHour
	tierB, tierH   map[string][]usdAmountHour
}

func claimHours(rows []timescale.EmissionClaimBucket) (pool []usdAmountHour, byID map[uint32][]usdAmountHour) {
	byID = map[uint32][]usdAmountHour{}
	for _, r := range rows {
		switch {
		case r.Scope == timescale.EmissionClaimScopePool:
			pool = append(pool, oneAmount(r.Start, r.Amount.BigInt()))
		case r.Scope == timescale.EmissionClaimScopeSingle && r.TokenID != nil:
			byID[*r.TokenID] = append(byID[*r.TokenID], oneAmount(r.Start, r.Amount.BigInt()))
		}
	}
	return pool, byID
}

func allocHours(rows []timescale.EmissionAllocBucket) []usdAmountHour {
	out := make([]usdAmountHour, len(rows))
	for i, r := range rows {
		out[i] = usdAmountHour{start: r.Start, amounts: []*big.Int{r.BackstopShare.BigInt(), r.PoolShare.BigInt()}}
	}
	return out
}

func tierHours(rows []timescale.EmissionBackstopClaimBucket) map[string][]usdAmountHour {
	out := map[string][]usdAmountHour{}
	for _, r := range rows {
		out[r.Tier] = append(out[r.Tier], oneAmount(r.Start, r.BLNT.BigInt()))
	}
	return out
}

// scheduleHours is each listed line's scheduled units per hour of the pool window, and per bucket as their sum.
func scheduleHours(events []timescale.EmissionScheduleEvent, win emissionsHistoryWindow) (buckets, hours [][]usdAmountHour) {
	timelines, ids := emissionTimelines(events)
	per := int(win.width / time.Hour)
	for _, id := range ids {
		units := scheduledBucketUnits(timelines[id].segments, win.from, win.end, time.Hour)
		var hs, bs []usdAmountHour
		for i, u := range units {
			start := win.from.Add(time.Duration(i) * time.Hour)
			hs = append(hs, oneAmount(start, u))
			if i%per == 0 {
				bs = append(bs, oneAmount(start, new(big.Int)))
			}
			b := bs[len(bs)-1].amounts[0]
			b.Add(b, u)
		}
		buckets, hours = append(buckets, bs), append(hours, hs)
	}
	return buckets, hours
}

// emisUSDInputs gathers what the view's available streams serve: buckets from h, hours from hourly (h itself at 1h).
func emisUSDInputs(v *LendingEmissionsHistoryView, h, hourly timescale.LendingEmissionsHistory, b emissionsBounds) emisUSDInput {
	var in emisUSDInput
	if v.Reserves.Available {
		in.schedB, in.schedH = scheduleHours(h.Schedule, b.pool)
	}
	in.poolB, in.resB = claimHours(h.Claims)
	in.poolH, in.resH = claimHours(hourly.Claims)
	if v.BackstopAlloc.Available {
		in.allocB, in.allocH = allocHours(h.Alloc), allocHours(hourly.Alloc)
	}
	if v.BackstopClaims.Available {
		in.tierB, in.tierH = tierHours(h.BackstopClaims), tierHours(hourly.BackstopClaims)
	}
	return in
}

// span is the first and last hour carrying an amount in any stream: the one price walk covers only those.
func (in emisUSDInput) span() (first, last time.Time, ok bool) {
	see := func(hs []usdAmountHour) {
		for _, h := range hs {
			if !amountsNonZero(h.amounts) {
				continue
			}
			if !ok || h.start.Before(first) {
				first = h.start
			}
			if !ok || h.start.After(last) {
				last = h.start
			}
			ok = true
		}
	}
	for _, hs := range in.schedH {
		see(hs)
	}
	see(in.poolH)
	see(in.allocH)
	for _, hs := range in.resH {
		see(hs)
	}
	for _, hs := range in.tierH {
		see(hs)
	}
	return first, last, ok
}

// emissionsHistoryUSD values every available stream in USD through revenue's walk: one read of the emission token's hourly
// series over the hours carrying an amount, after the token passes the protocol-token identity gate. At 1d the buckets
// are valued from a second, hourly read of the same streams and bounds, so each hour is priced at its own price.
func (s *Server) emissionsHistoryUSD(ctx context.Context, hr lendingEmissionsHistoryReader, pool string, isBLNT bool, hq timescale.EmissionHistoryQuery, v *LendingEmissionsHistoryView, h timescale.LendingEmissionsHistory, b emissionsBounds, flags *Flags) error {
	v.PriceMaxAgeHours = int(lendingUSDHourlyPriceMaxAge / time.Hour)
	token := ""
	if v.EmissionToken != nil {
		token = *v.EmissionToken
	}
	_, reason := s.emissionTokenIdentity(v.Protocol, token)
	hourly := h
	if hq.Daily {
		hourly = timescale.LendingEmissionsHistory{}
		if reason == "" {
			hq.Daily, hq.Schedule = false, false
			var err error
			if hourly, err = hr.LendingEmissionsHistory(ctx, pool, isBLNT, hq); err != nil {
				return err
			}
		}
	}
	in := emisUSDInputs(v, h, hourly, b)
	u := reserveUSDSeries{reason: reason}
	if first, last, ok := in.span(); ok && reason == "" {
		dec, known := 0, v.EmissionTokenDecimals != nil
		if known {
			dec = *v.EmissionTokenDecimals
		}
		u = s.reserveUSDWalk(ctx, token, dec, known, clickhouse.LendingHistoryQuery{From: first, To: last}, 0).series
		*flags = u.usdFlags(*flags)
	}
	valueEmissionStreams(v, in, u, b.pool.width)
	return nil
}

// valueEmissionStreams sets each stream's usd and its points'. The streams are layers of the same tokens: none is summed
// into another and no cross-stream USD total exists.
func valueEmissionStreams(v *LendingEmissionsHistoryView, in emisUSDInput, u reserveUSDSeries, width time.Duration) {
	sched := u
	sched.dec += scheduleScale
	for i := range v.Reserves.Streams {
		line := &v.Reserves.Streams[i]
		total, bs := valueEmisBuckets[emisUSDOne](in.schedB[i], in.schedH[i], sched, width)
		line.USD = emisOneUSD(&total, u.reason, v.Reserves.LowerBound)
		for j := range line.Points {
			k := int(time.Time(line.Points[j].T).Sub(in.schedB[i][0].start) / width)
			line.Points[j].USD = emisOneUSD(&bs[k], u.reason, false)
		}
	}
	pc := &v.PoolClaims
	total, bs := valueEmisBuckets[emisUSDOne](in.poolB, in.poolH, u, width)
	pc.USD = emisOneUSD(&total, u.reason, pc.LowerBound)
	for j := range pc.Points {
		pc.Points[j].USD = emisOneUSD(&bs[j], u.reason, false)
	}
	for i := range pc.ByReserve.Reserves {
		line := &pc.ByReserve.Reserves[i]
		total, bs := valueEmisBuckets[emisUSDOne](in.resB[line.TokenID], in.resH[line.TokenID], u, width)
		line.USD = emisOneUSD(&total, u.reason, pc.ByReserve.LowerBound)
		for j := range line.Points {
			line.Points[j].USD = emisOneUSD(&bs[j], u.reason, false)
		}
	}
	if ba := &v.BackstopAlloc; ba.Available {
		total, bs := valueEmisBuckets[emisUSDTwo](in.allocB, in.allocH, u, width)
		usd := emisTwoUSD(&total, u.reason, ba.LowerBound)
		ba.USD = &usd
		for j := range ba.Points {
			ba.Points[j].USD = emisTwoUSD(&bs[j], u.reason, false)
		}
	}
	for i := range v.BackstopClaims.Tiers {
		tier := &v.BackstopClaims.Tiers[i]
		total, bs := valueEmisBuckets[emisUSDOne](in.tierB[tier.Tier], in.tierH[tier.Tier], u, width)
		tier.USD = emisOneUSD(&total, u.reason, v.BackstopClaims.LowerBound)
		for j := range tier.Points {
			tier.Points[j].USD = emisOneUSD(&bs[j], u.reason, false)
		}
	}
}
