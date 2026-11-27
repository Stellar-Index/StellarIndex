package v1

import (
	"math/big"
	"slices"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// emissionSegment is one reserve_emission_update: eps (at 10^EmissionEPSDecimals base units per second) from start until
// the next update or exp, whichever comes first.
type emissionSegment struct {
	start time.Time
	eps   *big.Int
	exp   time.Time
}

// scheduleDenom turns eps x nanoseconds into base units: 10^eps_decimals x 10^9.
var scheduleDenom = new(big.Int).Exp(big.NewInt(10), big.NewInt(clickhouse.EmissionEPSDecimals+9), nil)

// scheduledTokens is Σ eps_i x |[t_i, min(t_{i+1}, exp_i)) ∩ [from, to)| / 10^7: the emission-token base units the
// timeline (sorted by start) announces over [from, to), exact.
func scheduledTokens(timeline []emissionSegment, from, to time.Time) *big.Rat {
	sum := new(big.Rat)
	for i, seg := range timeline {
		end := seg.exp
		if i+1 < len(timeline) && timeline[i+1].start.Before(end) {
			end = timeline[i+1].start
		}
		lo, hi := seg.start, end
		if from.After(lo) {
			lo = from
		}
		if to.Before(hi) {
			hi = to
		}
		if !hi.After(lo) {
			continue
		}
		units := new(big.Int).Mul(seg.eps, big.NewInt(hi.Sub(lo).Nanoseconds()))
		sum.Add(sum, new(big.Rat).SetFrac(units, scheduleDenom))
	}
	return sum
}

// scheduledTokensString renders scheduledTokens exactly: its denominator divides 10^(eps_decimals+9), and is at most 10^7
// when every boundary is a whole second, as ledger close times and bucket edges are.
func scheduledTokensString(r *big.Rat) string {
	return exactDecimal(r, clickhouse.EmissionEPSDecimals+9)
}

// scheduledBuckets is scheduledTokens for every bucket [from + i*width, …) of [from, end) in one sweep: each segment's
// active span is split over only the buckets it touches, so the cost is buckets + segments, not their product.
func scheduledBuckets(timeline []emissionSegment, from, end time.Time, width time.Duration) []*big.Rat {
	if !end.After(from) {
		return nil
	}
	n := int((end.Sub(from) + width - 1) / width)
	units := make([]*big.Int, n)
	for i := range units {
		units[i] = new(big.Int)
	}
	for i, seg := range timeline {
		segEnd := seg.exp
		if i+1 < len(timeline) && timeline[i+1].start.Before(segEnd) {
			segEnd = timeline[i+1].start
		}
		lo, hi := maxTime(seg.start, from), minTime(segEnd, end)
		for b := int(lo.Sub(from) / width); hi.After(lo) && b < n; b++ {
			bEnd := minTime(from.Add(time.Duration(b+1)*width), hi)
			units[b].Add(units[b], new(big.Int).Mul(seg.eps, big.NewInt(bEnd.Sub(lo).Nanoseconds())))
			lo = bEnd
		}
	}
	out := make([]*big.Rat, n)
	for i, u := range units {
		out[i] = new(big.Rat).SetFrac(u, scheduleDenom)
	}
	return out
}

// emissionTimeline is one token id's updates in apply order; ambiguous when the order within a ledger is not recorded.
type emissionTimeline struct {
	segments  []emissionSegment
	ambiguous bool
}

// emissionTimelines groups the stored updates by token id, each in the reader's apply order.
func emissionTimelines(events []timescale.EmissionScheduleEvent) (map[uint32]*emissionTimeline, []uint32) {
	out := map[uint32]*emissionTimeline{}
	var ids []uint32
	for _, e := range events {
		tl, seen := out[e.TokenID]
		if !seen {
			tl = &emissionTimeline{}
			out[e.TokenID] = tl
			ids = append(ids, e.TokenID)
		}
		tl.segments = append(tl.segments, emissionSegment{start: e.At, eps: e.EPS.BigInt(), exp: amountUnixSeconds(e.Expiration)})
		tl.ambiguous = tl.ambiguous || e.OrderAmbiguous
	}
	slices.Sort(ids)
	return out, ids
}

// maxScheduleUnix is 9999-12-31T23:59:59Z: a later expiration (u64 max means "never") is clamped to it, because
// time.Unix past it overflows time.Time's internal seconds and would order before every window.
const maxScheduleUnix = 253_402_300_799

// amountUnixSeconds is a u64 contract timestamp held as an Amount.
func amountUnixSeconds(a canonical.Amount) time.Time {
	b := a.BigInt()
	switch {
	case b.Sign() < 0:
		return time.Unix(0, 0).UTC()
	case b.Cmp(big.NewInt(maxScheduleUnix)) > 0:
		return time.Unix(maxScheduleUnix, 0).UTC()
	}
	return time.Unix(b.Int64(), 0).UTC() // i128:ok a contract timestamp, bounded by maxScheduleUnix above
}
