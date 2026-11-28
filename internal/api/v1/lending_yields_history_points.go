package v1

import (
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// LendingYieldHistoryPoint is one UTC day of a reserve side. Every null value carries its reason; nothing is interpolated
// or carried forward from another day.
type LendingYieldHistoryPoint struct {
	T WireTime `json:"t"`
	// RealisedFrom and RealisedTo are the last_time of the two stored states the day's rate is read between.
	RealisedFrom        *WireTime `json:"realised_from"`
	RealisedTo          *WireTime `json:"realised_to"`
	RealisedSpanSeconds *int64    `json:"realised_span_seconds"`
	RealisedAPR         *string   `json:"realised_apr"`
	RealisedAPRReason   *string   `json:"realised_apr_reason"`
	// RealisedAPY is display only (apy_convention realised).
	RealisedAPY *string `json:"realised_apy"`
	// Utilization is the state in force at the day's close, a fraction at 4 dp; UtilizationAt is when that state closed.
	Utilization       *string   `json:"utilization"`
	UtilizationAt     *WireTime `json:"utilization_at"`
	UtilizationReason *string   `json:"utilization_reason"`
	// EmissionsTokens is the emission token's base units the rollup scheduled to this side that day.
	EmissionsTokens           *string `json:"emissions_tokens"`
	EmissionsTokensLowerBound bool    `json:"emissions_tokens_lower_bound"`
	EmissionsTokensReason     *string `json:"emissions_tokens_reason"`
	// EmissionsUSD is those tokens as the rollup valued them, hour by hour at an unthinned price, under the source
	// exclusions in force at EmissionsValuedAt: a later exclusion change re-values the day on the rollup's next cycle.
	EmissionsUSD      *string   `json:"emissions_usd"`
	EmissionsValuedAt *WireTime `json:"emissions_valued_at"`
	// EmissionsAPR is emissions_usd x 365 over the side's USD at the day's close.
	EmissionsAPR         *string `json:"emissions_apr"`
	EmissionsAPRReason   *string `json:"emissions_apr_reason"`
	EmissionsLowerBound  bool    `json:"emissions_lower_bound"`
	EmissionsApproximate bool    `json:"emissions_approximate"`
}

// yieldHistoryEmissions is a pool's stored reserve_schedule days and what judges them; reason is every day's when set.
type yieldHistoryEmissions struct {
	reason   string
	cov      timescale.LendingEmissionsUSDCoverage
	rows     map[time.Time][]timescale.LendingEmissionsUSDDay
	verdicts map[string]string // emission token -> the read-time refusal of its stored USD
	history  historyVerdict
	degraded bool
}

// yieldSideUSD is a side's USD at a day's close, or why it has none.
type yieldSideUSD struct {
	usd    *big.Rat
	reason string
}

// bucketRateStates is the day opening at d as the window states /v1/lending/rates reads: the base is the previous day's
// last state when it closed within the slack, the end the last state closing by the day's end, and nothing earlier.
func bucketRateStates(days clickhouse.LendingRateDays, byDay map[time.Time]clickhouse.LendingRateDay, d time.Time) (clickhouse.LendingRateStates, bool) {
	var st clickhouse.LendingRateStates
	lo := d.Add(-clickhouse.LendingRateDaysSlack)
	prev, havePrev := byDay[d.Add(-day24)]
	cur, haveCur := byDay[d]
	if havePrev && prev.Last != nil && !prev.Last.CloseTime.Before(lo) {
		st.Base = prev.Last
	}
	st.Undecodable = (havePrev && prev.LastUndecodable) || (haveCur && (cur.FirstUndecodable || cur.LastUndecodable))
	if haveCur {
		st.First, st.End = cur.First, cur.Last
	} else {
		st.End = st.Base
	}
	earliest, known := earliestStateClose(days)
	st.Prior = days.Prior || (known && earliest.Before(lo))
	return st, haveCur || st.Base != nil || st.Undecodable || st.Prior
}

// earliestStateClose bounds from above the close of the series' first state: day d's Prior is whether it closed before
// d's slack, so each day is judged against the whole read, never only its neighbours, at no further query. Days ascend.
func earliestStateClose(days clickhouse.LendingRateDays) (time.Time, bool) {
	if len(days.Days) == 0 {
		return time.Time{}, false
	}
	switch e := days.Days[0]; {
	case e.First != nil:
		return e.First.CloseTime, true
	case e.Last != nil: // the first state did not decode; it closed no later than the last
		return e.Last.CloseTime, true
	default:
		return e.Day.Add(day24), true
	}
}

// yieldHistoryRealised fills a day's realised rate through realisedStatePair and realisedSide, /rates' own rules.
func yieldHistoryRealised(p *LendingYieldHistoryPoint, st clickhouse.LendingRateStates, ok bool, d time.Time, borrow bool, statesReason string) *clickhouse.LendingRateState {
	if statesReason != "" {
		p.RealisedAPRReason, p.UtilizationReason = strRef(statesReason), strRef(statesReason)
		return nil
	}
	base, end, reason := realisedStatePair(st, ok, d, day24)
	if reason == "" {
		from, to := WireTime(base.LastTime), WireTime(end.LastTime)
		secs := int64(end.LastTime.Sub(base.LastTime) / time.Second)
		p.RealisedFrom, p.RealisedTo, p.RealisedSpanSeconds = &from, &to, &secs
	}
	rr := realisedSide(base, end, reason, borrow)
	if rr.apr != nil {
		p.RealisedAPR, p.RealisedAPY = strRef(blend.FormatRate(rr.apr)), rr.apy
	} else {
		p.RealisedAPRReason = strRef(rr.reason)
	}
	return yieldHistoryUtilization(p, st, ok)
}

// yieldHistoryUtilization sets the utilisation of the state in force at the day's close, read only from the day or its
// base slack; it returns that state.
func yieldHistoryUtilization(p *LendingYieldHistoryPoint, st clickhouse.LendingRateStates, ok bool) *clickhouse.LendingRateState {
	end := st.End
	switch {
	case st.Undecodable:
		p.UtilizationReason = strRef(yieldReasonUndecodable)
		return nil
	case !ok || end == nil:
		p.UtilizationReason = strRef(yieldReasonNoAccrual)
		return nil
	case end.Removed:
		p.UtilizationReason = strRef(yieldReasonRemoved)
		return nil
	case end.BRate == nil || end.DRate == nil || end.BSupply == nil || end.DSupply == nil:
		p.UtilizationReason = strRef(yieldReasonUndecodable)
		return nil
	}
	u := new(big.Rat).SetFrac(rateStateData(end).Utilization(), big.NewInt(10_000_000))
	at := WireTime(end.CloseTime)
	p.Utilization, p.UtilizationAt = strRef(blend.FormatRate(u)), &at
	return end
}

// rateStateData is a decoded state as the reserve model reads it, at its protocol's rate scale.
func rateStateData(s *clickhouse.LendingRateState) blend.ReserveData {
	v := blend.PoolV2
	if s.Protocol == clickhouse.LendingBlendV1 {
		v = blend.PoolV1
	}
	return blend.ReserveData{BRate: s.BRate, DRate: s.DRate, BSupply: s.BSupply, DSupply: s.DSupply, Version: v}
}

// sideUSDAt values the side of the state in force at a day's close at that day's price, never a later one.
func sideUSDAt(end *clickhouse.LendingRateState, borrow bool, prices reserveUSDSeries, d time.Time) yieldSideUSD {
	if end == nil {
		return yieldSideUSD{reason: emissionAPRReserveUnread}
	}
	if prices.reason != "" {
		return yieldSideUSD{reason: prices.reason}
	}
	pt, ok := prices.priceAt(d)
	if !ok {
		return yieldSideUSD{reason: lendingUSDNoPriceInBound}
	}
	amount := rateStateData(end).SuppliedUnderlying()
	if borrow {
		amount = rateStateData(end).BorrowedUnderlying()
	}
	usd, err := usdValueRat(amount, pt.VWAP, prices.dec)
	switch {
	case err != nil:
		return yieldSideUSD{reason: emissionAPRSideUnpriced}
	case usd.Sign() <= 0:
		return yieldSideUSD{reason: emissionAPRSideEmpty}
	case usd.Cmp(emissionAPRMinSideUSD) < 0:
		return yieldSideUSD{reason: emissionAPRSideDust}
	}
	return yieldSideUSD{usd: usd}
}

// point fills a day's emissions from the stored rollup. A day with no row is zero only when the day is stored, the stream's
// whole day is known and its writer is verified back to its first ledger; otherwise the reason says why it is unknown.
func (e yieldHistoryEmissions) point(p *LendingYieldHistoryPoint, asset, side string, d time.Time, sideUSD yieldSideUSD) {
	reason := e.reason
	if reason == "" && (d.Before(e.cov.FloorDay) || d.After(e.cov.ThroughDay)) {
		reason = protocolRevenueDaysNotValued
	}
	if reason != "" {
		p.EmissionsTokensReason, p.EmissionsAPRReason = strRef(reason), strRef(reason)
		return
	}
	match, known := e.dayRows(d, asset, side)
	if known != "" {
		p.EmissionsTokensReason, p.EmissionsAPRReason = strRef(known), strRef(known)
		return
	}
	if len(match) == 0 {
		p.EmissionsTokens, p.EmissionsUSD = strRef("0"), strRef("0.00")
		if sideUSD.reason != "" { // a zero over a side with no value is no rate
			p.EmissionsAPRReason = strRef(sideUSD.reason)
			return
		}
		p.EmissionsAPR = strRef(blend.FormatRate(new(big.Rat)))
		return
	}
	e.valued(p, match, sideUSD)
}

// dayRows are the day's rows for this side, or why a missing row cannot be read as zero.
func (e yieldHistoryEmissions) dayRows(d time.Time, asset, side string) ([]timescale.LendingEmissionsUSDDay, string) {
	var match []timescale.LendingEmissionsUSDDay
	unmapped, streamLB := false, ""
	for _, r := range e.rows[d] {
		if r.StreamLowerBound && len(r.Excluded) > 0 {
			streamLB = r.Excluded[0]
		}
		switch {
		case r.Asset == nil || r.Side == nil:
			unmapped = true
		case *r.Asset == asset && *r.Side == side:
			match = append(match, r)
		}
	}
	switch {
	case len(match) > 0:
		return match, ""
	case unmapped:
		return nil, emissionStreamUnmapped
	case streamLB != "":
		return nil, streamLB
	}
	switch e.history {
	case historyVerified:
		return nil, ""
	case historyVerdictStale:
		return nil, earningsReasonHistoryVerdictStale
	case historyVerdictUnread:
		return nil, earningsReasonHistoryVerdictUnread
	case historyUnverified:
	}
	return nil, earningsReasonHistoryUnverified
}

// valued sums a day's rows for the side: tokens unless another revision stored them, USD unless a reason refuses it.
func (e yieldHistoryEmissions) valued(p *LendingYieldHistoryPoint, rows []timescale.LendingEmissionsUSDDay, sideUSD yieldSideUSD) {
	tokens, usd := new(big.Rat), new(big.Rat)
	usdReason := ""
	for _, r := range rows {
		if r.ValuationRev != lendingEmissionsValuationRev {
			p.EmissionsTokensReason, p.EmissionsAPRReason = strRef(protocolRevenueStale), strRef(protocolRevenueStale)
			return
		}
		if !emisAdd(&tokens, &r.Amount) {
			p.EmissionsTokensReason, p.EmissionsAPRReason = strRef(protocolEmissionsReadFailed), strRef(protocolEmissionsReadFailed)
			return
		}
		p.EmissionsTokensLowerBound = p.EmissionsTokensLowerBound || r.StreamLowerBound
		p.EmissionsLowerBound = p.EmissionsLowerBound || r.StreamLowerBound || r.LowerBound
		p.EmissionsApproximate = p.EmissionsApproximate || r.Approximate
		switch {
		case usdReason != "":
		case e.verdicts[strOrNone(r.EmissionToken)] != "":
			usdReason = e.verdicts[strOrNone(r.EmissionToken)]
		case r.USDReason != nil:
			usdReason = *r.USDReason
		case r.AmountUSD == nil: // a row with neither is unvalued, never a zero
			usdReason = protocolRevenueNotValued
		case !emisAdd(&usd, r.AmountUSD):
			usdReason = protocolEmissionsReadFailed
		}
	}
	if s, ok := ratExactDecimal(tokens); ok {
		p.EmissionsTokens = &s
	}
	if usdReason != "" {
		p.EmissionsAPRReason = strRef(usdReason)
		return
	}
	p.EmissionsUSD, p.EmissionsValuedAt = strRef(ratString(usd, 2)), latestValuedAt(rows)
	if sideUSD.reason != "" {
		p.EmissionsAPRReason = strRef(sideUSD.reason)
		return
	}
	apr := new(big.Rat).Quo(new(big.Rat).Mul(usd, big.NewRat(365, 1)), sideUSD.usd)
	p.EmissionsAPR = strRef(blend.FormatRate(apr))
}

// latestValuedAt is when the rollup last valued any of the day's rows; null when none records it.
func latestValuedAt(rows []timescale.LendingEmissionsUSDDay) *WireTime {
	var at time.Time
	for _, r := range rows {
		if r.ValuedAt.After(at) {
			at = r.ValuedAt
		}
	}
	if at.IsZero() {
		return nil
	}
	w := WireTime(at.UTC())
	return &w
}
