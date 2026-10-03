package v1

import (
	"context"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// pegDeviationBand is the |price − 1| beyond which a declared USD peg's
// own market is reported as off-peg (2%).
var pegDeviationBand = big.NewRat(2, 100)

// pegOffBand reports whether price (a decimal USD price for a declared
// peg) sits outside [pegDeviationBand] of 1. An unparseable or
// non-positive price is "unknown", never off-band.
func pegOffBand(price string) bool {
	r := ratFromDecimal(price)
	if r == nil || r.Sign() <= 0 {
		return false
	}
	dev := new(big.Rat).Sub(r, big.NewRat(1, 1))
	return dev.Abs(dev).Cmp(pegDeviationBand) > 0
}

// pegMarketOffBand prices the declared peg from its own XLM book
// ([Server.crossDeclaredPegThroughXLM]) and reports whether that market
// is off the 1:1 declaration. The pivot is XLM, not another peg, so the
// check never compares the declaration with itself. No market, a refused
// leg or a failed read leave it false: absence of evidence is not a flag.
func (s *Server) pegMarketOffBand(ctx context.Context, peg, quote canonical.Asset) bool {
	if s.prices == nil {
		return false
	}
	snap, _, verdict := s.crossDeclaredPegThroughXLM(ctx, peg, quote)
	return verdict == pegXLMLegPriced && pegOffBand(snap.Price)
}

// pegMarketOffBandAt is [Server.pegMarketOffBand] at a past instant, from
// the closed buckets at-or-before ts: peg/XLM × XLM/quote.
func (s *Server) pegMarketOffBandAt(ctx context.Context, peg, quote canonical.Asset, ts time.Time) bool {
	xlm := canonical.NativeAsset()
	if sameAsset(peg, xlm) {
		return false
	}
	pegLeg, found, _, err := s.lookupPriceAt(ctx, peg, xlm, ts)
	if err != nil || !found {
		return false
	}
	xlmLeg, found, _, err := s.lookupPriceAt(ctx, xlm, quote, ts)
	if err != nil || !found {
		return false
	}
	price, ok := crossThroughPivot(pegLeg.Price, xlmLeg.Price)
	return ok && pegOffBand(price)
}

// fiatProxyFlags is the envelope flags of a raw-trade point answer
// (/v1/vwap, /v1/twap, single-bar /v1/ohlc). A triangulated fiat:USD answer
// is a blend of the declared pegs' prints, so it carries proxy_deviation when
// any declared peg's market is off its declaration. Other fiat quotes are
// not priced in USD terms and are never flagged.
//
// The peg is judged at the window end, never from the live market: a depeg
// that ended before now (or began after the window) must not colour the
// window. Without a point-in-time reader only a window ending within
// [pegLiveHorizon] of now can be judged from the live book; older windows
// stay unflagged.
func (s *Server) fiatProxyFlags(ctx context.Context, quote canonical.Asset, triangulated bool, to time.Time) Flags {
	flags := Flags{Triangulated: triangulated}
	if !triangulated || quote.Type != canonical.AssetFiat || quote.Code != "USD" {
		return flags
	}
	for _, peg := range s.usdPeggedClassics {
		if s.windowPegOffBand(ctx, peg, quote, to) {
			flags.ProxyDeviation = true
			break
		}
	}
	return flags
}

// pegLiveHorizon bounds how stale a window end may be for the live peg
// market to stand in for it.
const pegLiveHorizon = time.Hour

func (s *Server) windowPegOffBand(ctx context.Context, peg, quote canonical.Asset, to time.Time) bool {
	if s.priceAt != nil {
		return s.pegMarketOffBandAt(ctx, peg, quote, to)
	}
	if time.Since(to) > pegLiveHorizon {
		return false
	}
	return s.pegMarketOffBand(ctx, peg, quote)
}
