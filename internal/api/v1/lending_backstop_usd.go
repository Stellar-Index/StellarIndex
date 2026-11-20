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
	backstopUSDNegativeAmount = "negative_amount"
)

// tokenUSD values amount base units of token at its current price exactly as buildReserveView values a reserve: the reserve
// decimals and price path, refusing a price normalised on a different scale. reason is set when usd is nil.
func (s *Server) tokenUSD(ctx context.Context, token string, amount *big.Int) (usd *big.Rat, price string, stale bool, reason string) {
	dec, ok := s.reserveDecimals(ctx, clickhouse.BlendReserveState{Asset: token})
	if !ok {
		return nil, "", false, backstopUSDDecimals
	}
	price, scale, stale, ok := s.reservePriceUSD(ctx, token)
	if !ok {
		return nil, "", false, backstopUSDUnpriced
	}
	if scale != dec {
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

// backstopUnitUSD values a backstop amount in unit token; a Blend backstop is a Comet BLND:USDC LP share, which nothing here values.
func (s *Server) backstopUnitUSD(ctx context.Context, protocol, token string, amount *big.Int) (*big.Rat, string, bool, string) {
	if protocol != blnt.SourceName {
		return nil, "", false, backstopUSDLPShare
	}
	return s.tokenUSD(ctx, token, amount)
}

// valueBackstopView sets each tier's tokens_usd and the view's total_usd; the total is a lower bound when a tier is unvalued.
func (s *Server) valueBackstopView(ctx context.Context, view *LendingBackstopView, st clickhouse.LendingBackstopState) bool {
	protocol := ""
	if view.Protocol != nil {
		protocol = *view.Protocol
	}
	var vals []*big.Rat
	anyStale := false
	for i, t := range st.Tiers {
		tv := &view.Tiers[i]
		token := t.Token
		if token == "" {
			token = st.Token
		}
		if t.Balance == nil || token == "" {
			tv.USDReason = strRef(backstopUSDNoBalance)
			vals = append(vals, nil)
			continue
		}
		usd, price, stale, reason := s.backstopUnitUSD(ctx, protocol, token, t.Balance.Tokens)
		vals = append(vals, usd)
		if usd == nil {
			tv.USDReason = strRef(reason)
			continue
		}
		tv.TokensUSD, tv.PriceUSD, tv.PriceStale = strRef(usd.FloatString(2)), strRef(price), stale
		anyStale = anyStale || stale
	}
	view.TotalUSD, view.LowerBound = reservesTVLUSD(vals)
	return anyStale
}

// valueProtocolBackstop sets tokens_usd on each per-token backstop sum and the protocol's backstop_total_usd; the total is a lower
// bound when a token is unvalued or a pool's backstop was left out.
func (s *Server) valueProtocolBackstop(ctx context.Context, st *LendingProtocolStats) bool {
	var vals []*big.Rat
	anyStale := false
	for i := range st.Backstop {
		b := &st.Backstop[i]
		amt, ok := new(big.Int).SetString(b.Tokens, 10)
		if !ok {
			b.USDReason = strRef(backstopUSDNoBalance)
			vals = append(vals, nil)
			continue
		}
		usd, _, stale, reason := s.backstopUnitUSD(ctx, st.Protocol, b.BackstopToken, amt)
		vals = append(vals, usd)
		if usd == nil {
			b.USDReason = strRef(reason)
			continue
		}
		b.TokensUSD, b.PriceStale = strRef(usd.FloatString(2)), stale
		anyStale = anyStale || stale
	}
	total, lower := reservesTVLUSD(vals)
	st.BackstopTotalUSD = total
	st.BackstopUSDLowerBound = total != nil && (lower || st.BackstopLowerBound)
	return anyStale
}
