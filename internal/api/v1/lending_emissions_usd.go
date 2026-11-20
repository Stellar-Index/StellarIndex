package v1

import (
	"context"
	"math/big"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// LendingEmissionRateView is one emission stream's current rate. An ended stream (live=false) emits nothing: tokens_per_day
// "0" and usd_per_day "0.00", never its last eps annualised.
type LendingEmissionRateView struct {
	// TokensPerDay is eps x 86400 / 10^eps_decimals: emission-token base units per day, exact (at most 5 fractional digits).
	TokensPerDay string `json:"tokens_per_day"`
	// USDPerDay is tokens_per_day at emission_token_price_usd, 2 dp; null with USDReason when the token is unpriced, its
	// decimals are unknown, or the pool's emission token is not the protocol's verified token.
	USDPerDay *string `json:"usd_per_day"`
	USDReason *string `json:"usd_reason"`
}

// Reasons an emission stream has no usd_per_day or emissions_apr, beyond the bespoke KPI ones.
const (
	emissionUSDTokenUnread     = "emission_token_unread"
	emissionUSDTokenUnverified = "emission_token_unverified" // not the SAC of the protocol token in the verified catalogue
	emissionUSDScaleMismatch   = "price_scale_mismatch"
	emissionAPRReserveUnread   = "reserve_unread"
	emissionAPRSideUnpriced    = "side_unpriced"
	emissionAPRSideEmpty       = "side_empty"
)

const (
	secondsPerDay  = 86_400
	secondsPerYear = 365 * secondsPerDay
)

// emissionTokenPrice prices the pool's emission token at the /v1/lending/protocols token read, only when the token is the
// SAC of that protocol's verified-catalogue asset: a pool names its own emission token, so a code match is not identity.
func (s *Server) emissionTokenPrice(ctx context.Context, protocol, token string, lakeDecimals *int) kpiTokenPrice {
	if token == "" {
		return kpiTokenPrice{reason: emissionUSDTokenUnread}
	}
	ticker := ""
	for _, pt := range lendingProtocols {
		if pt.protocol == protocol {
			ticker = pt.ticker
		}
	}
	if ticker == "" || s.VerifiedCurrencies == nil || s.NetworkPassphrase == "" {
		return kpiTokenPrice{reason: emissionUSDTokenUnverified}
	}
	var flags Flags
	tok, _ := s.readProtocolToken(ctx, ticker, &flags)
	if tok == nil {
		return kpiTokenPrice{reason: emissionUSDTokenUnverified}
	}
	if sac, ok := xdrjson.SACContractID(tok.AssetID, s.NetworkPassphrase); !ok || sac != token {
		return kpiTokenPrice{reason: emissionUSDTokenUnverified}
	}
	switch {
	case tok.PriceUSD == nil:
		return kpiTokenPrice{reason: kpiUSDUnpriced}
	case tok.Decimals <= 0:
		return kpiTokenPrice{reason: kpiUSDDecimalsUnknown}
	case lakeDecimals != nil && *lakeDecimals != tok.Decimals:
		return kpiTokenPrice{reason: emissionUSDScaleMismatch}
	}
	return kpiTokenPrice{price: *tok.PriceUSD, decimals: tok.Decimals, stale: flags.Stale}
}

// valueEmissions sets every stream's current rate and each reserve stream's emissions_apr against the pool's /reserves views.
func (s *Server) valueEmissions(ctx context.Context, view *LendingEmissionsView, flags *Flags) {
	token := ""
	if view.EmissionToken != nil {
		token = *view.EmissionToken
	}
	p := s.emissionTokenPrice(ctx, view.Protocol, token, view.EmissionTokenDecimals)
	if p.reason == "" {
		view.EmissionTokenPriceUSD = strRef(p.price)
		flags.Stale = flags.Stale || p.stale
	}
	var reserves map[string]ReserveView
	if len(view.Reserves) > 0 {
		_, rvs, stale, err := s.earningsReserveStates(ctx, view.Pool)
		if err != nil {
			s.logger.Warn("lending emissions: reserve read failed", "pool", view.Pool, "err", err)
			flags.Degraded = true
		} else {
			reserves = rvs
			flags.Stale = flags.Stale || stale
		}
	}
	for i := range view.Reserves {
		r := &view.Reserves[i]
		eps := streamEPS(r.LendingEmissionStreamView)
		r.LendingEmissionRateView = emissionRate(eps, r.Live, p)
		rv, found := ReserveView{}, false
		if r.Asset != nil {
			rv, found = reserves[*r.Asset]
		}
		r.EmissionsAPR, r.EmissionsAPRReason = emissionAPR(eps, r.Live, r.Side, rv, found, p)
	}
	for i := range view.Backstop {
		b := &view.Backstop[i]
		b.LendingEmissionRateView = emissionRate(streamEPS(b.LendingEmissionStreamView), b.Live, p)
	}
}

func streamEPS(v LendingEmissionStreamView) *big.Int {
	n, ok := new(big.Int).SetString(v.EPS, 10)
	if !ok {
		return new(big.Int)
	}
	return n
}

// emissionRate is a stream's per-day rate; an ended stream's is zero whatever its price.
func emissionRate(eps *big.Int, live bool, p kpiTokenPrice) LendingEmissionRateView {
	if !live || eps.Sign() == 0 {
		return LendingEmissionRateView{TokensPerDay: "0", USDPerDay: strRef("0.00")}
	}
	perDay := new(big.Int).Mul(eps, big.NewInt(secondsPerDay))
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(clickhouse.EmissionEPSDecimals), nil)
	out := LendingEmissionRateView{TokensPerDay: exactDecimal(new(big.Rat).SetFrac(perDay, scale), clickhouse.EmissionEPSDecimals)}
	if p.reason != "" {
		out.USDReason = strRef(p.reason)
		return out
	}
	usd, err := usdValueRat(perDay, p.price, p.decimals+clickhouse.EmissionEPSDecimals)
	if err != nil {
		out.USDReason = strRef(kpiUSDUnpriced)
		return out
	}
	out.USDPerDay = strRef(ratString(usd, 2))
	return out
}

// emissionAPR is the stream's annual USD emissions over its side's current USD size, both exact until the final 4 dp.
func emissionAPR(eps *big.Int, live bool, side string, rv ReserveView, found bool, p kpiTokenPrice) (*string, *string) {
	if !live || eps.Sign() == 0 {
		return strRef(ratString(new(big.Rat), netAPRDecimals)), nil
	}
	if p.reason != "" {
		return nil, strRef(p.reason)
	}
	if !found {
		return nil, strRef(emissionAPRReserveUnread)
	}
	amount := rv.Supplied
	if side == clickhouse.EmissionSideBorrow {
		amount = rv.Borrowed
	}
	size, reason := legUSD(&amount, rv)
	switch {
	case reason == earningsUSDWithheld:
		return nil, strRef(emissionAPRReserveUnread)
	case reason != "":
		return nil, strRef(emissionAPRSideUnpriced)
	case size.Sign() <= 0:
		return nil, strRef(emissionAPRSideEmpty)
	}
	annual, err := usdValueRat(new(big.Int).Mul(eps, big.NewInt(secondsPerYear)), p.price, p.decimals+clickhouse.EmissionEPSDecimals)
	if err != nil {
		return nil, strRef(kpiUSDUnpriced)
	}
	return strRef(ratString(annual.Quo(annual, size), netAPRDecimals)), nil
}

// exactDecimal renders r, whose denominator divides 10^maxScale, with no trailing fractional zeros.
func exactDecimal(r *big.Rat, maxScale int) string {
	s := r.FloatString(maxScale)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}
