package v1

import (
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Why a bad-debt part has no d-token amount; its usd_reason repeats it.
const (
	badDebtReasonBidNotRecorded         = "bid_not_recorded"
	badDebtReasonAmountNotRecorded      = "amount_not_recorded"
	badDebtReasonAssetNotRecorded       = "asset_not_recorded"
	badDebtReasonBackstopUnresolved     = "backstop_unresolved"
	badDebtReasonBackstopRoleUnverified = "backstop_role_unverified"
	badDebtReasonLakeUnavailable        = "lake_unavailable"
	badDebtReasonPositionUnread         = "position_unread"
	badDebtReasonCurrentRate            = "current_rate_unavailable"
)

// Why a reserve's identity is not checked.
const (
	badDebtReconcilePartMissing = "part_not_derivable"
	badDebtReconcileAsOf        = "as_of_mismatch"
)

// badDebtInputs is everything one pool's bad-debt view is folded from.
type badDebtInputs struct {
	assets []string // the pool's reserves, in pool order
	events []timescale.LendingBadDebtEvent
	// backstopReason is why the backstop's position (repaid, outstanding) is not read; "" when position is set.
	backstopReason string
	position       clickhouse.LendingAccountPositions
	current        map[string]blend.ReserveData // each reserve's current state
	marks          map[string]*liqMarks         // prices for every asset, rates at every event's transaction
	now            time.Time
	// projectedThrough is the last ledger every writer of the pool's event tables has fully written; 0 when unknown.
	projectedThrough uint32
	// coveredBidKind is the protocol's proven bad-debt bid kind; fills are covered d-tokens only when it is auctionLegDToken.
	coveredBidKind string
}

// badDebtPart accumulates one part of one reserve.
type badDebtPart struct {
	d, clipped *big.Int // nil when reason is set; clipped sums only events at or before the position's ledger
	reason     string
	events     int
	// under and usd are each nil, with their own first-cause reason, once any event lacks that field's input.
	under       *big.Int
	underReason string
	usd         *big.Rat
	usdReason   string
}

// badDebtValue is one event's (or the position's) conversion: underlying and USD, each nil with its reason when not derivable.
type badDebtValue struct {
	under       *big.Int
	underReason string
	usd         *big.Rat
	usdReason   string
}

func newBadDebtPart() *badDebtPart {
	return &badDebtPart{d: new(big.Int), clipped: new(big.Int), under: new(big.Int), usd: new(big.Rat)}
}

func (p *badDebtPart) refuse(reason string) {
	if p.reason == "" {
		p.reason, p.d, p.clipped, p.under, p.usd = reason, nil, nil, nil, nil
	}
}

// add books d d-tokens and their conversion v.
func (p *badDebtPart) add(d *big.Int, clip bool, v badDebtValue) {
	if p.reason != "" {
		return
	}
	p.d.Add(p.d, d)
	if clip {
		p.clipped.Add(p.clipped, d)
	}
	switch {
	case p.under == nil:
	case v.under == nil:
		p.under, p.underReason = nil, v.underReason
	default:
		p.under.Add(p.under, v.under)
	}
	switch {
	case p.usd == nil:
	case v.usd == nil:
		p.usd, p.usdReason = nil, v.usdReason
	default:
		p.usd.Add(p.usd, v.usd)
	}
}

func (p *badDebtPart) view() LendingBadDebtPart {
	v := LendingBadDebtPart{Events: p.events}
	if p.reason != "" {
		v.DTokensReason, v.UnderlyingReason, v.USDReason = strRef(p.reason), strRef(p.reason), strRef(p.reason)
		return v
	}
	v.DTokens = strRef(p.d.String())
	if p.under != nil {
		v.Underlying = strRef(p.under.String())
	} else {
		v.UnderlyingReason = strRef(p.underReason)
	}
	if p.usd != nil {
		v.USD = strRef(ratString(p.usd, 2))
	} else {
		v.USDReason = strRef(p.usdReason)
	}
	return v
}

// badDebtReserve is one reserve's five parts.
type badDebtReserve struct {
	moved, covered, socialised, repaid, outstanding *badDebtPart
}

func (r *badDebtReserve) parts() []*badDebtPart {
	return []*badDebtPart{r.moved, r.covered, r.socialised, r.repaid, r.outstanding}
}

// value converts one event's d-tokens at the reserve state its transaction stored and the price at or before it.
func (in badDebtInputs) value(asset string, d *big.Int, ledger uint32, tx string, at time.Time) badDebtValue {
	if d.Sign() == 0 {
		return badDebtValue{under: new(big.Int), usd: new(big.Rat)}
	}
	m := in.marks[asset]
	under, reason := m.underlying(auctionLegDToken, clickhouse.LendingTxRef{Ledger: ledger, TxHash: strings.ToLower(tx)}, d)
	if under == nil {
		return badDebtValue{underReason: reason, usdReason: reason}
	}
	usd, reason := in.price(m, under, at)
	return badDebtValue{under: under, usd: usd, usdReason: reason}
}

func (in badDebtInputs) price(m *liqMarks, under *big.Int, at time.Time) (*big.Rat, string) {
	p, reason := m.priceAt(at)
	if reason != "" {
		return nil, reason
	}
	usd, err := usdValueRat(under, p.price, m.scale)
	if err != nil {
		return nil, liqReasonNoPrice
	}
	return usd, ""
}

// foldBadDebt folds a pool's bad-debt events and its backstop's position into the per-reserve view and the pool's USD totals.
// Identity per reserve, in d-tokens over the events at or before the position's ledger:
// moved = covered + socialised + repaid + outstanding.
func foldBadDebt(in badDebtInputs) ([]LendingBadDebtReserve, LendingBadDebtTotals, badDebtPoolFacts) {
	res, order := badDebtReserves(in)
	facts := in.book(res)
	for _, a := range order {
		in.outstanding(a, res[a])
	}
	reserves := make([]LendingBadDebtReserve, 0, len(order))
	for _, a := range order {
		reserves = append(reserves, in.reserveView(a, res[a], facts))
	}
	return reserves, badDebtTotals(order, res), facts
}

// badDebtReserves is a zeroed reserve per pool reserve (in pool order), then per other asset an event or the position names.
func badDebtReserves(in badDebtInputs) (map[string]*badDebtReserve, []string) {
	res := map[string]*badDebtReserve{}
	add := func(a string) bool {
		if _, ok := res[a]; ok || a == "" {
			return false
		}
		res[a] = &badDebtReserve{newBadDebtPart(), newBadDebtPart(), newBadDebtPart(), newBadDebtPart(), newBadDebtPart()}
		return true
	}
	order := make([]string, 0, len(in.assets))
	for _, a := range in.assets {
		if add(a) {
			order = append(order, a)
		}
	}
	var extra []string
	for _, e := range in.events {
		if add(e.Asset) {
			extra = append(extra, e.Asset)
		}
	}
	if in.backstopReason == "" {
		for a := range in.position.Borrow {
			if add(a) {
				extra = append(extra, a)
			}
		}
	}
	sort.Strings(extra)
	return res, append(order, extra...)
}

// book books every event into its reserve's part; a fill with no bid, or an event with no asset, nulls that part everywhere.
func (in badDebtInputs) book(res map[string]*badDebtReserve) badDebtPoolFacts {
	var facts badDebtPoolFacts
	poison := map[string]string{}
	for _, e := range in.events {
		facts.observe(e, in.position.Ledger)
		if reason := in.bookEvent(res, e); reason != "" {
			poison[e.Kind] = reason
		}
	}
	for kind, reason := range poison {
		pick := badDebtPartOf(kind)
		for _, r := range res {
			pick(r).refuse(reason)
		}
	}
	return facts
}

// bookEvent books one event, or returns why its part cannot be attributed to any one reserve.
func (in badDebtInputs) bookEvent(res map[string]*badDebtReserve, e timescale.LendingBadDebtEvent) string {
	pick := badDebtPartOf(e.Kind)
	switch {
	case isBadDebtFill(e.Kind) && in.coveredBidKind != auctionLegDToken:
		return auctionUSDKindUnknown
	case e.Kind == timescale.BadDebtKindFillBidUnrecorded:
		return badDebtReasonBidNotRecorded
	case pick == nil || e.Kind == timescale.BadDebtKindFillZeroBid:
		return ""
	case e.Asset == "":
		return badDebtReasonAssetNotRecorded
	}
	part := pick(res[e.Asset])
	part.events++
	d, ok := parseBig(e.DTokens)
	if !ok || d.Sign() < 0 {
		part.refuse(badDebtReasonAmountNotRecorded)
		return ""
	}
	part.add(d, in.position.Ledger == 0 || e.Ledger <= in.position.Ledger, in.value(e.Asset, d, e.Ledger, e.TxHash, e.Ts))
	return ""
}

// isBadDebtFill is every fill kind; their bids are covered d-tokens only where the protocol's bad-debt bid kind is proven.
func isBadDebtFill(kind string) bool {
	return kind == timescale.BadDebtKindFill || kind == timescale.BadDebtKindFillZeroBid || kind == timescale.BadDebtKindFillBidUnrecorded
}

func (f *badDebtPoolFacts) observe(e timescale.LendingBadDebtEvent, positionLedger uint32) {
	switch {
	case positionLedger > 0 && e.Ledger > positionLedger:
		f.eventsAfterPosition++
	case e.Ledger == positionLedger:
		f.eventAtPosition = true
	}
	switch e.Kind {
	case timescale.BadDebtKindFillZeroBid:
		f.zeroBidFills++
	case timescale.BadDebtKindFillBidUnrecorded:
		f.unrecordedBidFills++
	}
}

// badDebtPoolFacts are the pool-level counts the identity's as-of check reads.
type badDebtPoolFacts struct {
	zeroBidFills, unrecordedBidFills, eventsAfterPosition int
	eventAtPosition                                       bool
}

// badDebtPartOf is the part an event kind books into; repay names the backstop's own repays.
func badDebtPartOf(kind string) func(*badDebtReserve) *badDebtPart {
	switch kind {
	case timescale.BadDebtKindMoved:
		return func(r *badDebtReserve) *badDebtPart { return r.moved }
	case timescale.BadDebtKindFill, timescale.BadDebtKindFillBidUnrecorded:
		return func(r *badDebtReserve) *badDebtPart { return r.covered }
	case timescale.BadDebtKindDefaulted:
		return func(r *badDebtReserve) *badDebtPart { return r.socialised }
	case timescale.BadDebtKindRepay:
		return func(r *badDebtReserve) *badDebtPart { return r.repaid }
	}
	return nil
}

// outstanding books the backstop's current d-tokens of asset at the current d_rate and the latest price.
func (in badDebtInputs) outstanding(asset string, r *badDebtReserve) {
	if in.backstopReason != "" {
		r.repaid.refuse(in.backstopReason)
		r.outstanding.refuse(in.backstopReason)
		return
	}
	d := in.position.Borrow[asset]
	if d == nil {
		d = new(big.Int)
	}
	if d.Sign() == 0 {
		r.outstanding.add(d, true, badDebtValue{under: new(big.Int), usd: new(big.Rat)})
		return
	}
	cur, ok := in.current[asset]
	if !ok || cur.DRate == nil {
		r.outstanding.add(d, true, badDebtValue{underReason: badDebtReasonCurrentRate, usdReason: badDebtReasonCurrentRate})
		return
	}
	under := cur.DTokensToUnderlying(d)
	usd, reason := in.price(in.marks[asset], under, in.now)
	r.outstanding.add(d, true, badDebtValue{under: under, usd: usd, usdReason: reason})
}

func (in badDebtInputs) reserveView(asset string, r *badDebtReserve, f badDebtPoolFacts) LendingBadDebtReserve {
	v := LendingBadDebtReserve{
		Asset: asset, MovedToBackstop: r.moved.view(), CoveredByBackstop: r.covered.view(),
		SocialisedToSuppliers: r.socialised.view(), RepaidByBackstop: r.repaid.view(), Outstanding: r.outstanding.view(),
	}
	for _, p := range r.parts() {
		if p.reason != "" {
			v.ReconcileReason = strRef(badDebtReconcilePartMissing)
			return v
		}
	}
	// Every event of the position's ledger must be in Postgres: the writers commit their cursors per cycle, not per ledger, so a
	// ledger past the last fully written one may hold only some of its events. Then the position was last written by one of
	// these events, or never written when there are none.
	if (in.position.Ledger > 0 && in.projectedThrough < in.position.Ledger) || (len(in.events) > 0 && !f.eventAtPosition) {
		v.ReconcileReason = strRef(badDebtReconcileAsOf)
		return v
	}
	residual := new(big.Int).Set(r.moved.clipped)
	for _, p := range []*badDebtPart{r.covered, r.socialised, r.repaid, r.outstanding} {
		residual.Sub(residual, p.clipped)
	}
	ok := residual.Sign() == 0
	v.Reconciled, v.ResidualDTokens = &ok, strRef(residual.String())
	return v
}

// badDebtTotals sums each part's USD over the reserves; a reserve whose part is not valued is excluded by name.
func badDebtTotals(order []string, res map[string]*badDebtReserve) LendingBadDebtTotals {
	total := func(pick func(*badDebtReserve) *badDebtPart) LendingBadDebtTotal {
		t := LendingBadDebtTotal{Excluded: []LendingBadDebtExclusion{}}
		var sum *big.Rat
		for _, a := range order {
			p := pick(res[a])
			if p.usd == nil {
				reason := p.reason
				if reason == "" {
					reason = p.usdReason
				}
				t.Excluded = append(t.Excluded, LendingBadDebtExclusion{Asset: a, Reason: reason})
				continue
			}
			if sum == nil {
				sum = new(big.Rat)
			}
			sum.Add(sum, p.usd)
		}
		switch {
		case sum == nil && len(t.Excluded) > 0:
		case sum == nil:
			t.USD = strRef(ratString(new(big.Rat), 2))
		default:
			t.USD = strRef(ratString(sum, 2))
		}
		t.LowerBound = len(t.Excluded) > 0 && sum != nil
		return t
	}
	return LendingBadDebtTotals{
		MovedToBackstop:       total(func(r *badDebtReserve) *badDebtPart { return r.moved }),
		CoveredByBackstop:     total(func(r *badDebtReserve) *badDebtPart { return r.covered }),
		SocialisedToSuppliers: total(func(r *badDebtReserve) *badDebtPart { return r.socialised }),
		RepaidByBackstop:      total(func(r *badDebtReserve) *badDebtPart { return r.repaid }),
		Outstanding:           total(func(r *badDebtReserve) *badDebtPart { return r.outstanding }),
	}
}
