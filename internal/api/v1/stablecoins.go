package v1

import (
	"context"
	"math/big"
	"net/http"
	"sort"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// GET /v1/stablecoins — the hand-vetted stablecoin set with its supply and USD
// value. Membership is the catalogue's `class: stablecoin` entries that carry
// a Stellar issuer; nothing is derived from an aggregator. Methodology:
// docs/methodology/stablecoins.md.

const (
	stablecoinBasis = "Circulating supply of the catalogue's hand-vetted Stellar stablecoins, valued at the " +
		"served USD price; only USD-pegged, non-yield-bearing members are summed"

	stablecoinPublished         = "published"
	stablecoinSupplyUnavailable = "supply_unavailable"
	stablecoinPriceUnavailable  = "price_unavailable"
	stablecoinWithheld          = "withheld"
	stablecoinNotObserved       = "not_observed"

	stablecoinReasonNoIssuer   = "no_stellar_issuer"
	stablecoinReasonNonUSD     = "non_usd_peg"
	stablecoinReasonYield      = "yield_bearing_wrapper"
	stablecoinReasonPegUnknown = "peg_unmapped"
)

// stablecoinPegs is the peg each catalogue ticker tracks. The catalogue holds
// no peg field; a ticker absent here is served with peg "unknown" and never
// summed, so a new seed entry cannot silently enter the USD total.
var stablecoinPegs = map[string]string{
	"USDC": "USD", "PYUSD": "USD", "USDT0": "USD", "YUSDC": "USD", "EURC": "EUR",
}

// stablecoinYieldBearing marks wrappers whose price is not meant to sit at the peg.
var stablecoinYieldBearing = map[string]bool{"YUSDC": true}

// StablecoinsView is the GET /v1/stablecoins data object.
type StablecoinsView struct {
	Total    StablecoinTotal       `json:"total"`
	ByPeg    []StablecoinPegTotal  `json:"by_peg"`
	Excluded []StablecoinExclusion `json:"excluded"`
	Assets   []StablecoinAsset     `json:"assets"`
}

// StablecoinTotal is the headline. SupplyUSD is absent, never "0.00", when no
// summed member is valued; LowerBound is true whenever the figure is less than
// the value of every stablecoin on Stellar.
type StablecoinTotal struct {
	SupplyUSD *string `json:"supply_usd,omitempty"`
	// Assets counts the set; AssetsValued and AssetsUnvalued split it.
	Assets         int `json:"assets"`
	AssetsValued   int `json:"assets_valued"`
	AssetsUnvalued int `json:"assets_unvalued"`
	// NotSummed names set members left out of supply_usd and why.
	NotSummed  []StablecoinExclusion `json:"not_summed"`
	LowerBound bool                  `json:"lower_bound"`
	Basis      string                `json:"basis"`
}

// StablecoinPegTotal is the per-peg subtotal over the valued members.
type StablecoinPegTotal struct {
	Peg       string  `json:"peg"`
	SupplyUSD *string `json:"supply_usd,omitempty"`
	Assets    int     `json:"assets"`
}

// StablecoinExclusion names a ticker and the reason it is left out.
type StablecoinExclusion struct {
	Ticker string `json:"ticker"`
	Reason string `json:"reason"`
}

// StablecoinAsset is one member, keyed by (code, issuer). The price is the
// real served pair price, never coerced to the peg, so a depeg shows.
type StablecoinAsset struct {
	AssetID                     string  `json:"asset_id"`
	Code                        string  `json:"code"`
	Issuer                      string  `json:"issuer"`
	ContractID                  string  `json:"contract_id,omitempty"`
	Ticker                      string  `json:"ticker"`
	Peg                         string  `json:"peg"`
	YieldBearing                bool    `json:"yield_bearing,omitempty"`
	CirculatingSupply           *string `json:"circulating_supply,omitempty"`
	Decimals                    int     `json:"decimals"`
	SupplyBasis                 *string `json:"supply_basis,omitempty"`
	CirculatingSupplyLowerBound bool    `json:"circulating_supply_lower_bound,omitempty"`
	PriceUSD                    *string `json:"price_usd,omitempty"`
	SupplyUSD                   *string `json:"supply_usd,omitempty"`
	ValuationStatus             string  `json:"valuation_status"`
}

type stablecoinMember struct {
	ticker, code, issuer string
}

// stablecoinPrewarmSet keys the members to their SACs for the lake-supply
// prewarm, reduced by the request path's own [classicLakeSupplyCandidates] so
// it cannot warm an asset the handler would not look up.
func (s *Server) stablecoinPrewarmSet() map[string]string {
	members, _ := stablecoinMembership(s.verifiedCurrencies)
	details := make([]AssetDetail, 0, len(members))
	for _, m := range members {
		details = append(details, AssetDetail{AssetID: m.code + "-" + m.issuer})
	}
	return classicLakeSupplyCandidates(details)
}

// stablecoinMembership reads the set off the catalogue: class stablecoin with
// a Stellar issuance, plus the stablecoin entries without one, as exclusions.
func stablecoinMembership(cat *currency.Catalogue) ([]stablecoinMember, []StablecoinExclusion) {
	members := []stablecoinMember{}
	excluded := []StablecoinExclusion{}
	for _, vc := range cat.ByClass(currency.ClassStablecoin) {
		se := vc.StellarEntry()
		if se == nil || se.Code == "" || se.Issuer == "" {
			excluded = append(excluded, StablecoinExclusion{Ticker: vc.Ticker, Reason: stablecoinReasonNoIssuer})
			continue
		}
		members = append(members, stablecoinMember{ticker: vc.Ticker, code: se.Code, issuer: se.Issuer})
	}
	return members, excluded
}

// stablecoinSupplyUSD is supply × price ÷ 10^decimals to cents, in big.Rat so
// no figure passes through a float. Empty when an input does not parse.
func stablecoinSupplyUSD(circ, price string, decimals int) (string, bool) {
	c, ok := new(big.Int).SetString(circ, 10)
	if !ok || c.Sign() < 0 || decimals < 0 {
		return "", false
	}
	p, ok := new(big.Rat).SetString(price)
	if !ok || p.Sign() < 0 {
		return "", false
	}
	v := new(big.Rat).SetInt(c)
	v.Mul(v, p)
	v.Quo(v, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)))
	return v.FloatString(2), true
}

// stablecoinAliasSupply finds a supply reading for an asset under any of its
// alias forms, so a reader keyed on a SAC or alternate spelling is not missed.
func stablecoinAliasSupply(
	assetID string, precise map[string]timescale.SupplyObservation, lake, broad map[string]string,
) (string, supply.Basis) {
	asset, err := canonical.ParseAsset(assetID)
	if err != nil {
		return classicSupplyReading(assetID, precise, lake, broad)
	}
	for _, id := range canonical.AssetAliasStrings(asset) {
		if circ, basis := classicSupplyReading(id, precise, lake, broad); circ != "" {
			return circ, basis
		}
	}
	return "", ""
}

// stablecoinFillAliasSupply attaches supply, under any alias form, to the
// rows the listing pipeline left without one. Supply is a chain fact; this
// moves no price and no gate.
func (s *Server) stablecoinFillAliasSupply(ctx context.Context, rows map[string]AssetDetail) {
	var missing []AssetDetail
	for _, d := range rows {
		if d.CirculatingSupply == nil {
			missing = append(missing, d)
		}
	}
	if len(missing) == 0 {
		return
	}
	precise := s.latestPreciseSupply(ctx)
	broad := s.cachedClassicSupply(ctx)
	lake := s.classicLakeSupply(ctx, missing)
	for key, d := range rows {
		if d.CirculatingSupply != nil {
			continue
		}
		circ, basis := stablecoinAliasSupply(d.AssetID, precise, lake, broad)
		stampCirculatingSupply(&d, circ, basis)
		rows[key] = d
	}
}

func stablecoinAssetOf(m stablecoinMember, d AssetDetail, found bool) StablecoinAsset {
	peg := stablecoinPegs[strings.ToUpper(m.ticker)]
	if peg == "" {
		peg = "unknown"
	}
	a := StablecoinAsset{
		AssetID: m.code + "-" + m.issuer, Code: m.code, Issuer: m.issuer, Ticker: m.ticker, Peg: peg,
		YieldBearing: stablecoinYieldBearing[strings.ToUpper(m.ticker)], Decimals: 7,
		ValuationStatus: stablecoinNotObserved,
	}
	if sac, ok := classicSACContractID(a.AssetID); ok {
		a.ContractID = sac
	}
	if !found {
		return a
	}
	a.Decimals = d.Decimals
	a.CirculatingSupply = d.CirculatingSupply
	a.PriceUSD = d.PriceUSD
	if d.CirculatingSupply != nil && d.SupplyBasis != nil {
		b := supply.Basis(*d.SupplyBasis)
		bs := b.String()
		a.SupplyBasis, a.CirculatingSupplyLowerBound = &bs, b.LowerBound()
	}
	withheld := d.MarketCapLowLiquidity || d.MarketCapDecimalsMismatch || d.DecimalsUnresolved || d.UnverifiedTickerCollision
	switch {
	case d.CirculatingSupply == nil:
		a.ValuationStatus = stablecoinSupplyUnavailable
	case withheld:
		a.ValuationStatus = stablecoinWithheld
	case d.PriceUSD == nil:
		a.ValuationStatus = stablecoinPriceUnavailable
	default:
		if v, ok := stablecoinSupplyUSD(*d.CirculatingSupply, *d.PriceUSD, d.Decimals); ok {
			a.SupplyUSD = &v
			a.ValuationStatus = stablecoinPublished
		} else {
			a.ValuationStatus = stablecoinPriceUnavailable
		}
	}
	return a
}

// stablecoinSummarise totals the rows over their PUBLISHED per-asset strings,
// so a reader re-adding the rows reaches the same figure.
func stablecoinSummarise(assets []StablecoinAsset, excluded []StablecoinExclusion, truncated bool) (StablecoinTotal, []StablecoinPegTotal) {
	t := StablecoinTotal{Assets: len(assets), NotSummed: []StablecoinExclusion{}, Basis: stablecoinBasis}
	sum := new(big.Rat)
	summed := false
	pegSum := map[string]*big.Rat{}
	pegN := map[string]int{}
	floor := false
	for _, a := range assets {
		pegN[a.Peg]++
		if a.SupplyUSD == nil {
			t.AssetsUnvalued++
			continue
		}
		t.AssetsValued++
		v, _ := new(big.Rat).SetString(*a.SupplyUSD)
		if pegSum[a.Peg] == nil {
			pegSum[a.Peg] = new(big.Rat)
		}
		pegSum[a.Peg].Add(pegSum[a.Peg], v)
		switch {
		case a.Peg == "unknown":
			t.NotSummed = append(t.NotSummed, StablecoinExclusion{Ticker: a.Ticker, Reason: stablecoinReasonPegUnknown})
		case a.Peg != "USD":
			t.NotSummed = append(t.NotSummed, StablecoinExclusion{Ticker: a.Ticker, Reason: stablecoinReasonNonUSD})
		case a.YieldBearing:
			t.NotSummed = append(t.NotSummed, StablecoinExclusion{Ticker: a.Ticker, Reason: stablecoinReasonYield})
		default:
			sum.Add(sum, v)
			summed = true
			floor = floor || a.CirculatingSupplyLowerBound
		}
	}
	if summed {
		s := sum.FloatString(2)
		t.SupplyUSD = &s
	}
	// An unvalued member is unvalued, not zero: it is why the total is a floor.
	t.LowerBound = len(excluded) > 0 || t.AssetsUnvalued > 0 || len(t.NotSummed) > 0 || floor || truncated
	pegs := make([]StablecoinPegTotal, 0, len(pegN))
	for peg, n := range pegN {
		p := StablecoinPegTotal{Peg: peg, Assets: n}
		if r := pegSum[peg]; r != nil {
			s := r.FloatString(2)
			p.SupplyUSD = &s
		}
		pegs = append(pegs, p)
	}
	sort.Slice(pegs, func(i, j int) bool { return pegs[i].Peg < pegs[j].Peg })
	return t, pegs
}

func stablecoinSortAssets(assets []StablecoinAsset) {
	val := func(a StablecoinAsset) *big.Rat {
		if a.SupplyUSD == nil {
			return nil
		}
		r, _ := new(big.Rat).SetString(*a.SupplyUSD)
		return r
	}
	sort.SliceStable(assets, func(i, j int) bool {
		vi, vj := val(assets[i]), val(assets[j])
		switch {
		case vi != nil && vj != nil && vi.Cmp(vj) != 0:
			return vi.Cmp(vj) > 0
		case (vi == nil) != (vj == nil):
			return vi != nil
		}
		return assets[i].AssetID < assets[j].AssetID
	})
}

// handleStablecoins serves GET /v1/stablecoins. No query parameters: the set
// is small and fixed by the catalogue.
func (s *Server) handleStablecoins(w http.ResponseWriter, r *http.Request) {
	view := StablecoinsView{
		Total:  StablecoinTotal{NotSummed: []StablecoinExclusion{}, Basis: stablecoinBasis},
		ByPeg:  []StablecoinPegTotal{},
		Assets: []StablecoinAsset{}, Excluded: []StablecoinExclusion{},
	}
	if s.assetsReader == nil || s.verifiedCurrencies == nil {
		view.Total.LowerBound = true
		writeEnvelope(w, Envelope{Data: view, Flags: Flags{}})
		return
	}
	members, excluded := stablecoinMembership(s.verifiedCurrencies)
	view.Excluded = excluded

	rm := rwaMembership{members: make([]rwaMember, 0, len(members))}
	for _, m := range members {
		rm.members = append(rm.members, rwaMember{code: m.code, issuer: m.issuer})
	}
	if !middleware.ChargeRateLimit(w, r, rwaAssetsCost(rm)) {
		return
	}
	rows, join, err := s.rwaListingRows(r.Context(), rm)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("stablecoins listing read failed", "err", err)
		writeProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	s.stablecoinFillAliasSupply(r.Context(), rows)

	for _, m := range members {
		d, ok := rows[rwaKey(m.code, m.issuer)]
		view.Assets = append(view.Assets, stablecoinAssetOf(m, d, ok))
	}
	stablecoinSortAssets(view.Assets)
	view.Total, view.ByPeg = stablecoinSummarise(view.Assets, view.Excluded, join.pagesTruncated > 0)
	writeEnvelope(w, Envelope{Data: view, Flags: Flags{}})
}
