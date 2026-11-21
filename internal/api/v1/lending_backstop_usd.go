package v1

import (
	"context"
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// Reasons a backstop amount carries no USD value.
const (
	backstopUSDLPShare        = "lp_share_unvalued"
	backstopUSDUnpriced       = "unpriced"
	backstopUSDDecimals       = "decimals_unknown"
	backstopUSDScaleMismatch  = "price_scale_mismatch"
	backstopUSDNoBalance      = "balance_unavailable"
	backstopUSDTokenUnknown   = "token_unknown"
	backstopUSDNegativeAmount = "negative_amount"
	// backstopUSDPriceReadFailed degrades the response: the token may well have a price.
	backstopUSDPriceReadFailed = exclusionPriceReadFailed
	// backstopUSDWithheld: a gate refused the token's price, so it is not unpriced.
	backstopUSDWithheld = exclusionPriceWithheld
)

// tokenUSD values amount base units of token at its current price exactly as buildReserveView values a reserve: the reserve
// decimals and price path, refusing a price normalised on a different scale. reason is set when usd is nil.
func (s *Server) tokenUSD(ctx context.Context, token string, amount *big.Int) (usd *big.Rat, price string, stale bool, reason string) {
	dec, ok := s.reserveDecimals(ctx, clickhouse.BlendReserveState{Asset: token})
	if !ok {
		return nil, "", false, backstopUSDDecimals
	}
	rp := s.reservePriceUSD(ctx, token)
	if rp.price == "" {
		switch {
		case rp.unread:
			return nil, "", false, backstopUSDPriceReadFailed
		case rp.withheld:
			return nil, "", false, backstopUSDWithheld
		}
		return nil, "", false, backstopUSDUnpriced
	}
	price, stale = rp.price, rp.stale
	if rp.scale != dec {
		obs.NonstandardDecimalsLockstepMismatchTotal.WithLabelValues("lending_backstop", "").Inc()
		return nil, "", false, backstopUSDScaleMismatch
	}
	v, err := usdValueRat(amount, price, dec)
	if err != nil {
		return nil, "", false, backstopUSDUnpriced
	}
	if v.Sign() < 0 {
		return nil, "", false, backstopUSDNegativeAmount
	}
	return v, price, stale, ""
}

// backstopUnitUSD values a backstop amount in unit token. Only a BLNT tier of a plain token is valued: a Blend backstop and a BLNT
// BlntUsdc/BlntXlm tier hold Comet LP shares, and an asset kind not listed here is withheld, never assumed to be a token.
func (s *Server) backstopUnitUSD(ctx context.Context, protocol, asset, token string, amount *big.Int) (*big.Rat, string, bool, string) {
	if protocol != blnt.SourceName {
		return nil, "", false, backstopUSDLPShare
	}
	switch asset {
	case "Usdc", "Xlm":
		return s.tokenUSD(ctx, token, amount)
	case "BlntUsdc", "BlntXlm":
		return nil, "", false, backstopUSDLPShare
	default:
		return nil, "", false, backstopUSDTokenUnknown
	}
}

// valueBackstopView sets each tier's tokens_usd and the view's total_usd; the total is a lower bound when a tier is unvalued.
// degraded reports a tier left unvalued by a failed price read.
func (s *Server) valueBackstopView(ctx context.Context, view *LendingBackstopView, st clickhouse.LendingBackstopState) (anyStale, degraded bool) {
	protocol := ""
	if view.Protocol != nil {
		protocol = *view.Protocol
	}
	var vals []*big.Rat
	for i, t := range st.Tiers {
		tv := &view.Tiers[i]
		token := t.Token
		if token == "" {
			token = st.Token
		}
		if t.Balance == nil || token == "" {
			tv.USDReason = strRef(backstopUSDNoBalance)
			if t.Balance != nil {
				tv.USDReason = strRef(backstopUSDTokenUnknown)
			}
			vals = append(vals, nil)
			continue
		}
		usd, price, stale, reason := s.backstopUnitUSD(ctx, protocol, t.Asset, token, t.Balance.Tokens)
		vals = append(vals, usd)
		if usd == nil {
			tv.USDReason = strRef(reason)
			degraded = degraded || reason == backstopUSDPriceReadFailed
			continue
		}
		tv.TokensUSD, tv.PriceUSD, tv.PriceStale = strRef(usd.FloatString(2)), strRef(price), stale
		anyStale = anyStale || stale
	}
	view.TotalUSD, view.LowerBound = reservesTVLUSD(vals)
	return anyStale, degraded
}

// valueProtocolBackstop sets tokens_usd on each per-token backstop sum and the protocol's backstop_total_usd; the total is a lower
// bound when a token is unvalued or a pool's backstop was left out. degraded reports a token left unvalued by a failed price read.
func (s *Server) valueProtocolBackstop(ctx context.Context, st *LendingProtocolStats) (anyStale, degraded bool) {
	var vals []*big.Rat
	for i := range st.Backstop {
		b := &st.Backstop[i]
		amt, ok := new(big.Int).SetString(b.Tokens, 10)
		if !ok {
			b.USDReason = strRef(backstopUSDNoBalance)
			vals = append(vals, nil)
			continue
		}
		usd, _, stale, reason := s.backstopUnitUSD(ctx, st.Protocol, b.asset, b.BackstopToken, amt)
		vals = append(vals, usd)
		if usd == nil {
			b.USDReason = strRef(reason)
			degraded = degraded || reason == backstopUSDPriceReadFailed
			continue
		}
		b.TokensUSD, b.PriceStale = strRef(usd.FloatString(2)), stale
		anyStale = anyStale || stale
	}
	total, lower := reservesTVLUSD(vals)
	st.BackstopTotalUSD = total
	st.BackstopUSDLowerBound = total != nil && (lower || st.BackstopLowerBound)
	return anyStale, degraded
}
