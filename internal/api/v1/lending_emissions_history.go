package v1

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend_backstop"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// lendingEmissionsHistoryReader is the event read behind GET /v1/lending/pools/{pool}/emissions/history.
type lendingEmissionsHistoryReader interface {
	LendingEmissionsHistory(ctx context.Context, pool string, isBLNT bool, q timescale.EmissionHistoryQuery) (timescale.LendingEmissionsHistory, error)
}

var (
	_ lendingEmissionsHistoryReader = (*timescale.Store)(nil)
	_ lendingEmissionsHistoryReader = (*CachedLendingReader)(nil)
)

// errNoEmissionsHistoryReader is a lending reader wired without the event history; empty streams would read as "nothing".
var errNoEmissionsHistoryReader = errors.New("lending: emissions history reader not wired")

// LendingEmissionsHistory is a pass-through: the window varies per request, so there is nothing to share.
func (c *CachedLendingReader) LendingEmissionsHistory(ctx context.Context, pool string, isBLNT bool, q timescale.EmissionHistoryQuery) (timescale.LendingEmissionsHistory, error) {
	if up, ok := c.upstream.(lendingEmissionsHistoryReader); ok {
		return up.LendingEmissionsHistory(ctx, pool, isBLNT, q)
	}
	return timescale.LendingEmissionsHistory{}, errNoEmissionsHistoryReader
}

// The emissions history route's reasons; an exclusion's reason sets lower_bound on the stream it names.
const (
	emisHistV1Schedule        = "v1_emission_schedule_unverified" // V1 eps is not stored the V2 way and its scale is unverified
	emisHistV1Backstop        = "v1_backstop_unaudited"
	emisHistNoPool            = "backstop_claim_names_no_pool" // Blend's backstop claim (LP tokens) names no pool
	emisHistTierSplit         = "tier_split_not_on_chain"
	emisHistEPSDrift          = "stored_eps_rewritten_without_event" // BLNT pool actions rewrite eps by dust with no event
	emisHistSeveral           = "claim_names_several_reserves"
	emisHistNone              = "claim_names_no_reserve"
	emisHistPastTip           = "window_past_lake_tip"
	emisHistTipUnknown        = "lake_tip_unknown"
	emisHistWriterBehind      = "writer_behind_lake"   // the stream's Postgres writer has not reached the lake's end
	emisHistWriterUnread      = "writer_cursor_unread" // the writer's cursor or its ledger's close time could not be read
	emisHistOrderUnknown      = "same_ledger_update_order_unknown"
	emisStreamReserves        = "reserves"
	emisStreamPoolClaims      = "pool_claims"
	emisStreamClaimsByReserve = "pool_claims_by_reserve"
	emisStreamBackstopAlloc   = "backstop_alloc"
	emisStreamBackstopClaims  = "backstop_claims"
	emisStreamWindow          = "window"
)

// LendingEmissionsHistoryView is the GET /v1/lending/pools/{pool}/emissions/history body: emission-token base units, each
// with its USD.
// The four streams are the same tokens at different stages (scheduled, allocated, claimed) and are never added together.
type LendingEmissionsHistoryView struct {
	Pool     string `json:"pool"`
	Protocol string `json:"protocol"` // blend | blnt
	Interval string `json:"interval"`
	// From and To bound the buckets: [from, to), to rounded down to its hour. ServedThrough is the earliest stream's
	// served_through: the earliest of to, now and the lake tip, and each stream's own writer.
	From                  WireTime                      `json:"from"`
	To                    WireTime                      `json:"to"`
	ServedThrough         WireTime                      `json:"served_through"`
	EmissionToken         *string                       `json:"emission_token"`
	EmissionTokenDecimals *int                          `json:"emission_token_decimals"`
	EPSDecimals           int                           `json:"eps_decimals"`
	PriceMaxAgeHours      int                           `json:"price_max_age_hours"`
	LowerBound            bool                          `json:"lower_bound"`
	Excluded              []LendingEmissionsExclusion   `json:"excluded"`
	Reserves              LendingEmissionScheduleView   `json:"reserves"`
	PoolClaims            LendingPoolClaimsHistoryView  `json:"pool_claims"`
	BackstopAlloc         LendingBackstopAllocView      `json:"backstop_alloc"`
	BackstopClaims        LendingBackstopClaimsHistView `json:"backstop_claims"`
}

// LendingEmissionsExclusion names what a stream's sums leave out or cannot vouch for.
type LendingEmissionsExclusion struct {
	Stream string  `json:"stream"`
	Reason string  `json:"reason"`
	Count  *int64  `json:"count"`
	Amount *string `json:"amount"` // emission-token base units left out, when known
}

// lendingEmissionStreamHead is every stream's availability and lower bound.
type lendingEmissionStreamHead struct {
	Available         bool    `json:"available"`
	UnavailableReason *string `json:"unavailable_reason"`
	LowerBound        bool    `json:"lower_bound"`
	// ServedThrough is where this stream's buckets end: the earliest of to, now, the lake tip and its own writer's cursor.
	ServedThrough *WireTime `json:"served_through"`
}

// LendingEmissionScheduleView is the scheduled emission per reserve token id, from its reserve_emission_update timeline.
type LendingEmissionScheduleView struct {
	lendingEmissionStreamHead
	Approximate       bool                          `json:"approximate"`
	ApproximateReason *string                       `json:"approximate_reason"`
	Streams           []LendingEmissionScheduleLine `json:"streams"`
}

// LendingEmissionScheduleLine is one token id's schedule; Points lists only buckets with tokens scheduled.
type LendingEmissionScheduleLine struct {
	TokenID uint32  `json:"token_id"`
	Asset   *string `json:"asset"`
	Side    string  `json:"side"`
	Tokens  string  `json:"tokens"`
	// Approximate: two transactions in one ledger updated this id and their apply order is not recorded.
	Approximate       bool                       `json:"approximate"`
	ApproximateReason *string                    `json:"approximate_reason"`
	USD               LendingEmissionUSD         `json:"usd"`
	Points            []LendingEmissionTokensPnt `json:"points"`
}

// LendingEmissionTokensPnt is one bucket's scheduled base units; Partial marks a bucket the window or the lake tip cuts.
type LendingEmissionTokensPnt struct {
	T       WireTime           `json:"t"`
	Partial bool               `json:"partial"`
	Tokens  string             `json:"tokens"`
	USD     LendingEmissionUSD `json:"usd"`
}

// LendingClaimSums is claims and the emission token claimed; Claimers is distinct within a bucket, so totals carry none.
type LendingClaimSums struct {
	Claims int64  `json:"claims"`
	Amount string `json:"amount"`
}

// LendingClaimsPoint is one bucket's claims.
type LendingClaimsPoint struct {
	T        WireTime `json:"t"`
	Partial  bool     `json:"partial"`
	Claimers int64    `json:"claimers"`
	LendingClaimSums
	USD LendingEmissionUSD `json:"usd"`
}

// LendingPoolClaimsHistoryView is the pool's claim events; ByReserve holds only claims naming exactly one token id.
type LendingPoolClaimsHistoryView struct {
	lendingEmissionStreamHead
	Totals    LendingClaimSums           `json:"totals"`
	USD       LendingEmissionUSD         `json:"usd"`
	Points    []LendingClaimsPoint       `json:"points"`
	ByReserve LendingClaimsByReserveView `json:"by_reserve"`
}

// LendingClaimsByReserveView is the single-reserve claims; it is a lower bound whenever any claim named several or none.
type LendingClaimsByReserveView struct {
	LowerBound bool                       `json:"lower_bound"`
	Reserves   []LendingClaimsReserveLine `json:"reserves"`
}

// LendingClaimsReserveLine is one token id's single-reserve claims.
type LendingClaimsReserveLine struct {
	TokenID uint32               `json:"token_id"`
	Asset   *string              `json:"asset"`
	Side    string               `json:"side"`
	Totals  LendingClaimSums     `json:"totals"`
	USD     LendingEmissionUSD   `json:"usd"`
	Points  []LendingClaimsPoint `json:"points"`
}

// LendingBackstopAllocView is the backstop's gulp_emissions naming the pool: what the backstop kept and passed to the pool.
type LendingBackstopAllocView struct {
	lendingEmissionStreamHead
	TierSplitAvailable bool                     `json:"tier_split_available"`
	TierSplitReason    *string                  `json:"tier_split_reason"`
	Totals             LendingAllocSums         `json:"totals"`
	USD                *LendingEmissionAllocUSD `json:"usd"` // null when the stream is unavailable
	Points             []LendingAllocPointView  `json:"points"`
}

// LendingAllocSums are emission-token base units.
type LendingAllocSums struct {
	Events            int64  `json:"events"`
	BackstopEmissions string `json:"backstop_emissions"`
	PoolEmissions     string `json:"pool_emissions"`
}

// LendingAllocPointView is one bucket's allocation.
type LendingAllocPointView struct {
	T       WireTime `json:"t"`
	Partial bool     `json:"partial"`
	LendingAllocSums
	USD LendingEmissionAllocUSD `json:"usd"`
}

// LendingBackstopClaimsHistView is BLNT's backstop claims per tier; Units names each amount's unit.
type LendingBackstopClaimsHistView struct {
	lendingEmissionStreamHead
	Units LendingBackstopClaimUnits     `json:"units"`
	Tiers []LendingBackstopClaimTierHis `json:"tiers"`
}

// LendingBackstopClaimUnits labels the three amounts of a BLNT backstop claim.
type LendingBackstopClaimUnits struct {
	BLNTAmount string `json:"blnt_amount"`
	LPAmount   string `json:"lp_amount"`
	Shares     string `json:"shares"`
}

var blntBackstopClaimUnits = LendingBackstopClaimUnits{
	BLNTAmount: "emission_token_base_units", LPAmount: "backstop_lp_token_base_units", Shares: "tier_shares",
}

// LendingBackstopClaimSums are one tier's claim amounts, each in its Units unit.
type LendingBackstopClaimSums struct {
	Claims     int64  `json:"claims"`
	BLNTAmount string `json:"blnt_amount"`
	LPAmount   string `json:"lp_amount"`
	Shares     string `json:"shares"`
}

// LendingBackstopClaimTierHis is one tier's claims.
type LendingBackstopClaimTierHis struct {
	Tier   string                   `json:"tier"`
	Totals LendingBackstopClaimSums `json:"totals"`
	// USD values blnt_amount only: lp_amount and shares are not the emission token.
	USD    LendingEmissionUSD           `json:"usd"`
	Points []LendingBackstopClaimPntHis `json:"points"`
}

// LendingBackstopClaimPntHis is one bucket of a tier's claims.
type LendingBackstopClaimPntHis struct {
	T        WireTime `json:"t"`
	Partial  bool     `json:"partial"`
	Claimers int64    `json:"claimers"`
	LendingBackstopClaimSums
	USD LendingEmissionUSD `json:"usd"`
}

// emissionsHistoryWindow is the request's bucket grid: buckets of width from from, served up to end (≤ to).
type emissionsHistoryWindow struct {
	from, to, end time.Time
	width         time.Duration
}

func (w emissionsHistoryWindow) partial(start time.Time) bool { return start.Add(w.width).After(w.end) }

// handleLendingPoolEmissionsHistory serves GET /v1/lending/pools/{pool}/emissions/history.
func (s *Server) handleLendingPoolEmissionsHistory(w http.ResponseWriter, r *http.Request) {
	pool := r.PathValue("pool")
	if !canonical.IsContractID(pool) {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-pool",
			"Invalid pool", http.StatusBadRequest, "pool must be a C-strkey contract id")
		return
	}
	q, interval, ok := parseLendingHistoryQuery(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), maxHandlerBudget)
	defer cancel()
	protocol, reason, ok := s.lendingHistoryLineage(ctx, w, r, pool)
	if !ok {
		return
	}
	hr, ok := s.Lending.(lendingEmissionsHistoryReader)
	if !ok {
		writeLendingHistoryNotReady(w, r)
		return
	}
	isBLNT, isV1 := protocol == blnt.SourceName, reason == backstopReasonV1
	view, win, flags := s.emissionsHistoryView(ctx, pool, protocol, interval, q)
	st, err := s.emissionsHistoryStorage(ctx, pool, isBLNT, isV1, &view)
	if err != nil {
		s.writeLendingHistoryError(ctx, w, r, "LendingEmissions", pool, err)
		return
	}
	b := s.emissionsWriterBounds(ctx, win, isBLNT, isV1, &flags)
	hq := timescale.EmissionHistoryQuery{
		From: win.from, To: b.pool.end, BackstopTo: b.backstop.end, Daily: q.Daily,
		Schedule: !isV1, BackstopAlloc: !isV1, BackstopClaim: isBLNT,
	}
	h, err := hr.LendingEmissionsHistory(ctx, pool, isBLNT, hq)
	if errors.Is(err, errNoEmissionsHistoryReader) {
		writeLendingHistoryNotReady(w, r)
		return
	}
	if err != nil {
		s.writeLendingHistoryError(ctx, w, r, "LendingEmissionsHistory", pool, err)
		return
	}
	fillEmissionsHistory(&view, h, b, st.ReserveList, isBLNT, isV1)
	b.apply(&view)
	s.applyEmissionsVerdicts(ctx, &view, isBLNT, &flags)
	finishEmissionsHistory(&view)
	if err := s.emissionsHistoryUSD(ctx, hr, pool, isBLNT, hq, &view, h, b, &flags); err != nil {
		s.writeLendingHistoryError(ctx, w, r, "LendingEmissionsHistory", pool, err)
		return
	}
	writeEnvelope(w, Envelope{Data: view, AsOf: WireTime(time.Now().UTC()), Flags: flags})
}

// emissionsHistoryView opens the view and clips the window at the lake tip, so hours not yet captured are not served as none.
func (s *Server) emissionsHistoryView(ctx context.Context, pool, protocol, interval string, q clickhouse.LendingHistoryQuery) (LendingEmissionsHistoryView, emissionsHistoryWindow, Flags) {
	win := emissionsHistoryWindow{from: q.From.UTC(), to: q.To.UTC().Truncate(time.Hour), width: time.Hour}
	if q.Daily {
		win.width = 24 * time.Hour
	}
	if win.to.Before(win.from) {
		win.to = win.from
	}
	// Nothing after now has happened: a schedule extended past it would serve announced tokens as emitted.
	win.end = minTime(win.to, time.Now().UTC().Truncate(time.Second))
	view := LendingEmissionsHistoryView{
		Pool: pool, Protocol: protocol, Interval: interval, From: WireTime(win.from), To: WireTime(win.to),
		EPSDecimals: clickhouse.EmissionEPSDecimals, Excluded: []LendingEmissionsExclusion{},
	}
	_, stale, ok := s.lakeWatermark(ctx)
	flags := Flags{Stale: stale}
	_, tip, _, _ := s.lakeWatermarkCached()
	switch {
	case !ok || tip.IsZero():
		view.Excluded = append(view.Excluded, LendingEmissionsExclusion{Stream: emisStreamWindow, Reason: emisHistTipUnknown})
	case tip.Before(win.end):
		win.end = tip.UTC().Truncate(time.Second)
		view.Excluded = append(view.Excluded, LendingEmissionsExclusion{Stream: emisStreamWindow, Reason: emisHistPastTip})
	}
	if win.end.Before(win.from) {
		win.end = win.from
	}
	view.ServedThrough = WireTime(win.end)
	return view, win, flags
}

// emissionsHistoryStorage reads the pool instance for the emission token and reserve list, which name each token id's
// reserve. A V1 pool's storage layout is unverified, so it is not read and its token ids carry no asset.
func (s *Server) emissionsHistoryStorage(ctx context.Context, pool string, isBLNT, isV1 bool, view *LendingEmissionsHistoryView) (clickhouse.LendingEmissionsState, error) {
	er, ok := s.Explorer.(lendingEmissionsReader)
	if isV1 || s.Explorer == nil || !ok {
		return clickhouse.LendingEmissionsState{}, nil
	}
	st, err := er.LendingEmissions(ctx, pool, isBLNT)
	if err != nil {
		return st, err
	}
	if st.EmissionToken != "" {
		view.EmissionToken = strRef(st.EmissionToken)
		if d, known, err := s.lakeTokenDecimals(ctx, st.EmissionToken); known && err == nil {
			view.EmissionTokenDecimals = &d
		}
	}
	return st, nil
}

func (v *LendingEmissionsHistoryView) exclude(stream, reason string, count *int64, amount *string) {
	v.Excluded = append(v.Excluded, LendingEmissionsExclusion{Stream: stream, Reason: reason, Count: count, Amount: amount})
}

func countRef(n int64) *int64 { return &n }

func unavailable(reason string) lendingEmissionStreamHead {
	return lendingEmissionStreamHead{UnavailableReason: strRef(reason)}
}

// fillEmissionsHistory maps the read into the four streams, with each version's nulls.
func fillEmissionsHistory(v *LendingEmissionsHistoryView, h timescale.LendingEmissionsHistory, b emissionsBounds, reserves []string, isBLNT, isV1 bool) {
	v.Reserves = LendingEmissionScheduleView{Streams: []LendingEmissionScheduleLine{}}
	v.BackstopAlloc = LendingBackstopAllocView{Points: []LendingAllocPointView{}, Totals: LendingAllocSums{BackstopEmissions: "0", PoolEmissions: "0"}}
	v.BackstopClaims = LendingBackstopClaimsHistView{Tiers: []LendingBackstopClaimTierHis{}}
	switch {
	case isV1:
		v.Reserves.lendingEmissionStreamHead = unavailable(emisHistV1Schedule)
		v.BackstopAlloc.lendingEmissionStreamHead = unavailable(emisHistV1Backstop)
	default:
		v.Reserves.Available = true
		v.Reserves.Streams = scheduleLines(h.Schedule, b.pool, reserves)
		fillBackstopAlloc(v, h.Alloc, b.backstop)
	}
	if isBLNT {
		v.Reserves.Approximate, v.Reserves.ApproximateReason = true, strRef(emisHistEPSDrift)
		v.BackstopAlloc.TierSplitReason = strRef(emisHistTierSplit)
		v.BackstopClaims.Available, v.BackstopClaims.Units = true, blntBackstopClaimUnits
		fillBackstopClaims(v, h.BackstopClaims, b.backstop)
	} else {
		v.BackstopClaims.lendingEmissionStreamHead = unavailable(emisHistNoPool)
	}
	fillPoolClaims(v, h.Claims, b.pool, reserves)
}

func scheduleLines(events []timescale.EmissionScheduleEvent, win emissionsHistoryWindow, reserves []string) []LendingEmissionScheduleLine {
	timelines, ids := emissionTimelines(events)
	out := make([]LendingEmissionScheduleLine, 0, len(ids))
	for _, id := range ids {
		tl := timelines[id]
		asset, side, _ := clickhouse.ReserveTokenSide(id, reserves)
		line := LendingEmissionScheduleLine{
			TokenID: id, Asset: nonEmptyPtr(asset), Side: side, Points: []LendingEmissionTokensPnt{},
			Tokens: scheduledTokensString(scheduledTokens(tl.segments, win.from, win.end)),
		}
		if tl.ambiguous {
			line.Approximate, line.ApproximateReason = true, strRef(emisHistOrderUnknown)
		}
		for i, t := range scheduledBuckets(tl.segments, win.from, win.end, win.width) {
			if t.Sign() != 0 {
				start := win.from.Add(time.Duration(i) * win.width)
				line.Points = append(line.Points, LendingEmissionTokensPnt{T: WireTime(start), Partial: win.partial(start), Tokens: scheduledTokensString(t)})
			}
		}
		out = append(out, line)
	}
	return out
}

func fillBackstopAlloc(v *LendingEmissionsHistoryView, rows []timescale.EmissionAllocBucket, win emissionsHistoryWindow) {
	v.BackstopAlloc.Available = true
	bk, pl := new(big.Int), new(big.Int)
	var unread int64
	for _, b := range rows {
		v.BackstopAlloc.Totals.Events += b.Events
		bk.Add(bk, b.BackstopShare.BigInt())
		pl.Add(pl, b.PoolShare.BigInt())
		unread += b.Unread
		v.BackstopAlloc.Points = append(v.BackstopAlloc.Points, LendingAllocPointView{T: WireTime(b.Start), Partial: win.partial(b.Start), LendingAllocSums: LendingAllocSums{
			Events: b.Events, BackstopEmissions: b.BackstopShare.String(), PoolEmissions: b.PoolShare.String(),
		}})
	}
	v.BackstopAlloc.Totals.BackstopEmissions, v.BackstopAlloc.Totals.PoolEmissions = bk.String(), pl.String()
	if unread > 0 {
		v.exclude(emisStreamBackstopAlloc, emissionsReasonAmountUnread, countRef(unread), nil)
	}
}

func fillBackstopClaims(v *LendingEmissionsHistoryView, rows []timescale.EmissionBackstopClaimBucket, win emissionsHistoryWindow) {
	type acc struct{ blnt, lp, shares *big.Int }
	idx, sums := map[string]int{}, []acc{}
	var unread int64
	for _, b := range rows {
		i, ok := idx[b.Tier]
		if !ok {
			i = len(v.BackstopClaims.Tiers)
			idx[b.Tier] = i
			v.BackstopClaims.Tiers = append(v.BackstopClaims.Tiers, LendingBackstopClaimTierHis{Tier: b.Tier, Points: []LendingBackstopClaimPntHis{}})
			sums = append(sums, acc{new(big.Int), new(big.Int), new(big.Int)})
		}
		t := &v.BackstopClaims.Tiers[i]
		t.Totals.Claims += b.Claims
		sums[i].blnt.Add(sums[i].blnt, b.BLNT.BigInt())
		sums[i].lp.Add(sums[i].lp, b.LP.BigInt())
		sums[i].shares.Add(sums[i].shares, b.Shares.BigInt())
		unread += b.Unread
		t.Points = append(t.Points, LendingBackstopClaimPntHis{
			T: WireTime(b.Start), Partial: win.partial(b.Start), Claimers: b.Claimers,
			LendingBackstopClaimSums: LendingBackstopClaimSums{Claims: b.Claims, BLNTAmount: b.BLNT.String(), LPAmount: b.LP.String(), Shares: b.Shares.String()},
		})
	}
	for i := range v.BackstopClaims.Tiers {
		tot := &v.BackstopClaims.Tiers[i].Totals
		tot.BLNTAmount, tot.LPAmount, tot.Shares = sums[i].blnt.String(), sums[i].lp.String(), sums[i].shares.String()
	}
	if unread > 0 {
		v.exclude(emisStreamBackstopClaims, emissionsReasonAmountUnread, countRef(unread), nil)
	}
}

// fillPoolClaims serves every claim in the pool total and only single-reserve claims per reserve: a claim naming several
// reserves carries one amount for all of them, which is never split.
func fillPoolClaims(v *LendingEmissionsHistoryView, rows []timescale.EmissionClaimBucket, win emissionsHistoryWindow, reserves []string) {
	pc := &v.PoolClaims
	pc.Available, pc.Points, pc.ByReserve.Reserves = true, []LendingClaimsPoint{}, []LendingClaimsReserveLine{}
	total := new(big.Int)
	left := map[string]*struct {
		n   int64
		amt *big.Int
	}{emisHistSeveral: {amt: new(big.Int)}, emisHistNone: {amt: new(big.Int)}}
	byID, idSums := map[uint32]int{}, []*big.Int{}
	var unread int64
	for _, b := range rows {
		pt := LendingClaimsPoint{
			T: WireTime(b.Start), Partial: win.partial(b.Start), Claimers: b.Claimers,
			LendingClaimSums: LendingClaimSums{Claims: b.Claims, Amount: b.Amount.String()},
		}
		switch {
		case b.Scope == timescale.EmissionClaimScopePool:
			pc.Totals.Claims += b.Claims
			total.Add(total, b.Amount.BigInt())
			unread += b.AmountUnread
			pc.Points = append(pc.Points, pt)
		case b.Scope == timescale.EmissionClaimScopeSingle && b.TokenID != nil:
			i, ok := byID[*b.TokenID]
			if !ok {
				i = len(pc.ByReserve.Reserves)
				byID[*b.TokenID] = i
				asset, side, _ := clickhouse.ReserveTokenSide(*b.TokenID, reserves)
				pc.ByReserve.Reserves = append(pc.ByReserve.Reserves, LendingClaimsReserveLine{
					TokenID: *b.TokenID, Asset: nonEmptyPtr(asset), Side: side, Points: []LendingClaimsPoint{},
				})
				idSums = append(idSums, new(big.Int))
			}
			line := &pc.ByReserve.Reserves[i]
			line.Totals.Claims += b.Claims
			idSums[i].Add(idSums[i], b.Amount.BigInt())
			line.Points = append(line.Points, pt)
		default:
			reason := emisHistNone
			if b.Scope == timescale.EmissionClaimScopeSeveral {
				reason = emisHistSeveral
			}
			left[reason].n += b.Claims
			left[reason].amt.Add(left[reason].amt, b.Amount.BigInt())
		}
	}
	pc.Totals.Amount = total.String()
	for i := range pc.ByReserve.Reserves {
		pc.ByReserve.Reserves[i].Totals.Amount = idSums[i].String()
	}
	if unread > 0 {
		v.exclude(emisStreamPoolClaims, emissionsReasonAmountUnread, countRef(unread), nil)
	}
	for _, reason := range []string{emisHistSeveral, emisHistNone} {
		if l := left[reason]; l.n > 0 {
			v.exclude(emisStreamClaimsByReserve, reason, countRef(l.n), strRef(l.amt.String()))
		}
	}
}

// applyEmissionsVerdicts withholds the "complete" claim from a stream whose writer's projection is not verified back to its
// first ledger, as /emissions does for claim totals.
func (s *Server) applyEmissionsVerdicts(ctx context.Context, v *LendingEmissionsHistoryView, isBLNT bool, flags *Flags) {
	poolSrc, bstopSrc := "blend", blend_backstop.SourceName
	if isBLNT {
		poolSrc, bstopSrc = blnt.SourceName, blnt.SourceName
	}
	verdicts := s.projectionHistoryVerdicts(ctx, poolSrc, bstopSrc)
	judge := func(stream, src string) {
		switch verdicts[src] {
		case historyVerified:
			return
		case historyVerdictStale:
			v.exclude(stream, earningsReasonHistoryVerdictStale, nil, nil)
			flags.Stale = true
		case historyVerdictUnread:
			v.exclude(stream, earningsReasonHistoryVerdictUnread, nil, nil)
			flags.Degraded = true
		case historyUnverified:
			v.exclude(stream, earningsReasonHistoryUnverified, nil, nil)
		}
	}
	if v.Reserves.Available {
		judge(emisStreamReserves, poolSrc)
	}
	judge(emisStreamPoolClaims, poolSrc)
	if v.BackstopAlloc.Available {
		judge(emisStreamBackstopAlloc, bstopSrc)
	}
	if v.BackstopClaims.Available {
		judge(emisStreamBackstopClaims, bstopSrc)
	}
}

// finishEmissionsHistory sets each stream's lower_bound from the exclusions naming it (or the window), and the view's from
// any. A pool-claims exclusion also bounds the per-reserve view; a per-reserve one never bounds the pool total.
func finishEmissionsHistory(v *LendingEmissionsHistoryView) {
	named := map[string]bool{}
	for _, e := range v.Excluded {
		named[e.Stream] = true
	}
	win := named[emisStreamWindow]
	v.Reserves.LowerBound = v.Reserves.Available && (win || named[emisStreamReserves])
	v.PoolClaims.LowerBound = win || named[emisStreamPoolClaims]
	v.PoolClaims.ByReserve.LowerBound = v.PoolClaims.LowerBound || named[emisStreamClaimsByReserve]
	v.BackstopAlloc.LowerBound = v.BackstopAlloc.Available && (win || named[emisStreamBackstopAlloc])
	v.BackstopClaims.LowerBound = v.BackstopClaims.Available && (win || named[emisStreamBackstopClaims])
	v.LowerBound = len(v.Excluded) > 0
}
