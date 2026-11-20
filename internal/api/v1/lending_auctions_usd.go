package v1

import (
	"context"
	"math/big"
	"sort"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// LendingAuctionUSDView values an auction's bid and lot at today's prices and reserve rates, not those at fill.
type LendingAuctionUSDView struct {
	// LotUSD and BidUSD sum the side's valued legs, 2 dp: "0.00" for an empty side, null when no leg is valued. LowerBound is
	// true when a leg is left out; Unvalued names each, with its kind and why.
	LotUSD           *string                     `json:"lot_usd"`
	LotUSDLowerBound bool                        `json:"lot_usd_lower_bound"`
	LotUnvalued      []LendingAuctionUnvaluedLeg `json:"lot_unvalued"`
	BidUSD           *string                     `json:"bid_usd"`
	BidUSDLowerBound bool                        `json:"bid_usd_lower_bound"`
	BidUnvalued      []LendingAuctionUnvaluedLeg `json:"bid_unvalued"`
	ValuedAt         string                      `json:"valued_at"` // always "current"
}

// LendingAuctionUnvaluedLeg is one bid or lot entry left out of its side's USD sum.
type LendingAuctionUnvaluedLeg struct {
	Asset  string `json:"asset"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

// Auction leg kinds, per pool auctions/auction.rs (docs/operations/wasm-audits/blend.md "Auction type discriminants"): the map
// key is the reserve's underlying asset for b/dToken and underlying legs and the backstop token for a backstop leg.
const (
	auctionLegBToken     = "b_token"
	auctionLegDToken     = "d_token"
	auctionLegUnderlying = "underlying"
	auctionLegBackstop   = "backstop_token"
	auctionLegUnknown    = "unknown"
)

// Reasons an auction leg is unvalued, beyond the backstop and earnings ones.
const (
	auctionUSDKindUnknown   = "leg_kind_unknown"
	auctionUSDReserveUnread = "reserve_unread"
	auctionUSDRateUnread    = "rate_unread"
)

const auctionValuedAtCurrent = "current"

// auctionLegKinds is (bid kind, lot kind) per auction type; protocol_fee (BLNT type 3) is not established and stays unvalued.
func auctionLegKinds(auctionType uint32) (bid, lot string) {
	switch auctionType {
	case 0: // user liquidation: the filler repays the user's liabilities and takes their collateral
		return auctionLegDToken, auctionLegBToken
	case 1: // bad debt: the filler repays the debt and takes backstop tokens
		return auctionLegDToken, auctionLegBackstop
	case 2: // interest: the filler pays backstop tokens for the backstop's accrued underlying credit
		return auctionLegBackstop, auctionLegUnderlying
	}
	return auctionLegUnknown, auctionLegUnknown
}

// auctionValuer values one leg; usd is nil with reason when it cannot.
type auctionValuer struct {
	states   map[string]clickhouse.BlendReserveState
	views    map[string]ReserveView
	backstop func(token string, amount *big.Int) (*big.Rat, string)
}

func (av auctionValuer) leg(kind, asset string, amount *big.Int) (*big.Rat, string) {
	switch kind {
	case auctionLegBackstop:
		return av.backstop(asset, amount)
	case auctionLegUnknown:
		return nil, auctionUSDKindUnknown
	}
	st, ok := av.states[asset]
	rv, vok := av.views[asset]
	if !ok || !vok {
		return nil, auctionUSDReserveUnread
	}
	underlying := amount
	switch kind {
	case auctionLegBToken:
		if st.Data.BRate == nil {
			return nil, auctionUSDRateUnread
		}
		underlying = st.Data.BTokensToUnderlying(amount) // the contract's own rounding (floor)
	case auctionLegDToken:
		if st.Data.DRate == nil {
			return nil, auctionUSDRateUnread
		}
		underlying = st.Data.DTokensToUnderlying(amount) // rounded up, as the contract does
	}
	s := underlying.String()
	return legUSD(&s, rv)
}

// side sums one bid or lot map in key order.
func (av auctionValuer) side(kind string, legs map[string]string) (*string, bool, []LendingAuctionUnvaluedLeg) {
	assets := make([]string, 0, len(legs))
	for a := range legs {
		assets = append(assets, a)
	}
	sort.Strings(assets)
	sum, valued := new(big.Rat), 0
	unvalued := []LendingAuctionUnvaluedLeg{}
	for _, a := range assets {
		amount, ok := parseBig(legs[a])
		var usd *big.Rat
		reason := earningsUSDWithheld
		if ok {
			if amount.Sign() < 0 {
				reason = backstopUSDNegativeAmount
			} else {
				usd, reason = av.leg(kind, a, amount)
			}
		}
		if usd == nil {
			unvalued = append(unvalued, LendingAuctionUnvaluedLeg{Asset: a, Kind: kind, Reason: reason})
			continue
		}
		sum.Add(sum, usd)
		valued++
	}
	if valued == 0 && len(assets) > 0 {
		return nil, false, unvalued
	}
	return strRef(ratString(sum, 2)), len(unvalued) > 0, unvalued
}

func (av auctionValuer) value(v *LendingAuctionView) {
	bidKind, lotKind := auctionLegKinds(v.AuctionType)
	u := LendingAuctionUSDView{ValuedAt: auctionValuedAtCurrent}
	u.LotUSD, u.LotUSDLowerBound, u.LotUnvalued = av.side(lotKind, v.Lot)
	u.BidUSD, u.BidUSDLowerBound, u.BidUnvalued = av.side(bidKind, v.Bid)
	v.LendingAuctionUSDView = u
}

// valueAuctions values every auction on the page against the pool's current /reserves state; a failed reserve read leaves
// each reserve leg unvalued (reserve_unread) and marks the response degraded.
func (s *Server) valueAuctions(ctx context.Context, pool string, auctions []LendingAuctionView, flags *Flags) {
	if len(auctions) == 0 {
		return
	}
	protocol, _, err := s.backstopLineage(ctx, pool)
	if err != nil {
		s.logger.Warn("lending auctions: lineage read failed", "pool", pool, "err", err)
		flags.Degraded = true
	}
	var states map[string]clickhouse.BlendReserveState
	var views map[string]ReserveView
	if s.Explorer != nil { // without the lake reader no reserve is read: every reserve leg is reserve_unread
		var stale bool
		states, views, stale, err = s.earningsReserveStates(ctx, pool)
		if err != nil {
			s.logger.Warn("lending auctions: reserve read failed", "pool", pool, "err", err)
			flags.Degraded = true
		}
		flags.Stale = flags.Stale || stale
	}
	av := auctionValuer{states: states, views: views, backstop: func(token string, amount *big.Int) (*big.Rat, string) {
		if protocol == "" {
			return nil, backstopUSDLPShare
		}
		usd, _, st, reason := s.backstopUnitUSD(ctx, protocol, token, amount)
		flags.Stale = flags.Stale || (usd != nil && st)
		return usd, reason
	}}
	for i := range auctions {
		av.value(&auctions[i])
	}
}
