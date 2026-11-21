package v1

import (
	"context"
	"math/big"
	"sort"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
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

// auctionLegKindTable is {bid kind, lot kind} per protocol and auction type. A type not listed, or an unknown protocol, is
// leg_kind_unknown on both sides: a leg is valued only under a kind with evidence.
var auctionLegKindTable = map[string]map[uint32][2]string{
	// Blend pool auctions/auction.rs (docs/operations/wasm-audits/blend.md "Auction type discriminants").
	blend.SourceName: {
		0: {auctionLegDToken, auctionLegBToken},       // user liquidation: the filler repays liabilities and takes collateral
		1: {auctionLegDToken, auctionLegBackstop},     // bad debt: the filler repays the debt and takes backstop tokens
		2: {auctionLegBackstop, auctionLegUnderlying}, // interest: the filler pays backstop tokens for accrued credit
	},
	// BLNT V3 changes the backstop (tiers) and adds an auction type, so Blend's table is not assumed. Only interest is proven: a
	// mainnet new_auction (blnt testdata/decoded.golden.json, pool_65224843) bids a backstop tier token for reserve assets.
	blnt.SourceName: {
		2: {auctionLegBackstop, auctionLegUnderlying},
	},
}

// auctionLegKinds is the (bid kind, lot kind) of a protocol's auction type.
func auctionLegKinds(protocol string, auctionType uint32) (bid, lot string) {
	if k, ok := auctionLegKindTable[protocol][auctionType]; ok {
		return k[0], k[1]
	}
	return auctionLegUnknown, auctionLegUnknown
}

// auctionValuer values one leg; usd is nil with reason when it cannot.
type auctionValuer struct {
	protocol string
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
	bidKind, lotKind := auctionLegKinds(av.protocol, v.AuctionType)
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
		var stale, degraded bool
		states, views, stale, degraded, err = s.earningsReserveStates(ctx, pool)
		if err != nil {
			s.logger.Warn("lending auctions: reserve read failed", "pool", pool, "err", err)
			flags.Degraded = true
		}
		flags.Stale = flags.Stale || stale
		flags.Degraded = flags.Degraded || degraded
	}
	av := auctionValuer{protocol: protocol, states: states, views: views, backstop: s.auctionBackstopValuer(ctx, protocol, pool, flags)}
	for i := range auctions {
		av.value(&auctions[i])
	}
}

// auctionBackstopValuer values a backstop-token leg only when its token is a BLNT backstop tier's plain token: the key alone
// does not say whether it is a token or a Comet LP share, so the kind comes from the pool's own backstop configuration.
func (s *Server) auctionBackstopValuer(ctx context.Context, protocol, pool string, flags *Flags) func(string, *big.Int) (*big.Rat, string) {
	if protocol != blnt.SourceName {
		return func(string, *big.Int) (*big.Rat, string) { return nil, backstopUSDLPShare }
	}
	var tiers []clickhouse.BackstopTierState
	read := false
	return func(token string, amount *big.Int) (*big.Rat, string) {
		if !read {
			read = true
			tiers = s.auctionBackstopTiers(ctx, pool, flags)
		}
		asset, ok := backstopTierAsset(tiers, token)
		if !ok {
			return nil, auctionUSDKindUnknown
		}
		usd, _, st, reason := s.backstopUnitUSD(ctx, protocol, asset, token, amount)
		if reason == backstopUSDTokenUnknown {
			reason = auctionUSDKindUnknown
		}
		flags.Stale = flags.Stale || (usd != nil && st)
		flags.Degraded = flags.Degraded || reason == backstopUSDPriceReadFailed
		return usd, reason
	}
}

// auctionBackstopTiers reads the BLNT pool's backstop tiers; a failed read leaves every backstop leg leg_kind_unknown.
func (s *Server) auctionBackstopTiers(ctx context.Context, pool string, flags *Flags) []clickhouse.BackstopTierState {
	br, ok := s.Explorer.(lendingBackstopReader)
	if !ok {
		return nil
	}
	st, err := br.LendingBackstop(ctx, pool, true)
	if err != nil {
		s.logger.Warn("lending auctions: backstop read failed", "pool", pool, "err", err)
		flags.Degraded = true
		return nil
	}
	return st.Tiers
}

// backstopTierAsset is the asset kind of the tiers configured with token; ok is false when none is, or they disagree.
func backstopTierAsset(tiers []clickhouse.BackstopTierState, token string) (string, bool) {
	asset := ""
	for _, t := range tiers {
		if t.Token != token || t.Asset == "" {
			continue
		}
		if asset != "" && asset != t.Asset {
			return "", false
		}
		asset = t.Asset
	}
	return asset, asset != ""
}
