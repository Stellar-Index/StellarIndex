package v1

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Reasons a token-amount bespoke KPI carries no value_usd.
const (
	kpiUSDUnpriced        = "unpriced"
	kpiUSDDecimalsUnknown = "decimals_unknown"
	kpiUSDUnitUnpriceable = "unit_not_priceable"
)

// kpiTokenPrice is one protocol token's price and scale as /v1/assets/{id} serves them; ok=false when either is unavailable.
type kpiTokenPrice struct {
	price    string
	decimals int
	stale    bool
	reason   string
}

// valueBespokeKPIs sets value_usd on every KPI the store tagged with a token amount, at the token's current price through the
// /v1/lending/protocols token read. out.KPIs is parallel to blk.KPIs (bespokeFromStore appends one per KPI).
func (s *Server) valueBespokeKPIs(ctx context.Context, blk *timescale.BespokeBlock, out *ProtocolBespoke) {
	if blk == nil || out == nil || len(out.KPIs) != len(blk.KPIs) {
		return
	}
	prices := map[string]kpiTokenPrice{}
	for i, k := range blk.KPIs {
		if k.Token == "" || k.Amount == nil {
			continue
		}
		p, seen := prices[k.Token]
		if !seen {
			p = s.kpiTokenPrice(ctx, k.Token)
			prices[k.Token] = p
		}
		applyKPIUSD(&out.KPIs[i], k, p)
	}
}

func applyKPIUSD(dst *BespokeKPI, k timescale.BespokeKPI, p kpiTokenPrice) {
	if p.reason != "" {
		dst.USDReason = p.reason
		return
	}
	usd, err := usdMarketValue(k.Amount, p.price, p.decimals)
	if err != nil {
		dst.USDReason = kpiUSDUnpriced
		return
	}
	dst.ValueUSD, dst.PriceUSD, dst.PriceStale = &usd, &p.price, p.stale
}

// kpiTokenPrice prices ticker only when it is a lending protocol token: any other unit (a Comet LP share) has no valuation here.
func (s *Server) kpiTokenPrice(ctx context.Context, ticker string) kpiTokenPrice {
	known := false
	for _, pt := range lendingProtocols {
		known = known || pt.ticker == ticker
	}
	if !known || s.VerifiedCurrencies == nil {
		return kpiTokenPrice{reason: kpiUSDUnitUnpriceable}
	}
	var flags Flags
	tok, _ := s.readProtocolToken(ctx, ticker, &flags)
	switch {
	case tok == nil || tok.PriceUSD == nil:
		return kpiTokenPrice{reason: kpiUSDUnpriced}
	case tok.Decimals <= 0:
		return kpiTokenPrice{reason: kpiUSDDecimalsUnknown}
	}
	return kpiTokenPrice{price: *tok.PriceUSD, decimals: tok.Decimals, stale: flags.Stale}
}
