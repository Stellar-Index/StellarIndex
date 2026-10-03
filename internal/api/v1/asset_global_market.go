package v1

import (
	"context"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// priceBasisGlobalMarket marks a PriceUSD filled from the global
// cross-venue market of the ticker the verified catalogue binds to this
// exact (code, issuer), because no Stellar market price survived the gates.
const priceBasisGlobalMarket = "global_market"

// globalMarketMaxAge bounds the age of the aggregator row backing a
// global-market price; an older row is a stalled feed, not a market.
const globalMarketMaxAge = time.Hour

// globalMarketMaxSkew bounds how far ahead of the server clock a row may be
// stamped; a mis-stamped future row would otherwise win "newest" forever.
const globalMarketMaxSkew = 5 * time.Minute

// defaultStellarDivergenceThresholdPct mirrors divergence.threshold_pct's
// config default for a server built without it.
const defaultStellarDivergenceThresholdPct = 5.0

// AssetGlobalMarket is the global-market reference for a vetted
// same-asset token: the USD price of its global ticker across venues, and
// how far this asset's own Stellar market sits from it.
type AssetGlobalMarket struct {
	// Asset is the global ticker the catalogue binds to this issuance.
	Asset string `json:"asset"`
	// PriceUSD is the aggregator's published price, verbatim (ADR-0003).
	PriceUSD string   `json:"price_usd"`
	Source   string   `json:"source"`
	AsOf     WireTime `json:"as_of"`
	// StellarDivergencePct is (stellar − global) / global × 100, signed,
	// two decimals; absent when no Stellar market price is served.
	StellarDivergencePct *string `json:"stellar_divergence_pct,omitempty"`
	// DepegWarning is true when |StellarDivergencePct| exceeds the
	// divergence threshold.
	DepegWarning bool `json:"depeg_warning,omitempty"`
	// IssuerSignals names the issuer-behaviour risks present beside a
	// depeg warning ([AssetIssuerBehaviour]); set only with DepegWarning.
	IssuerSignals []string `json:"issuer_signals,omitempty"`
}

// globalMarketAsset returns the global ticker the verified catalogue binds
// to this exact classic asset_id, and the parsed asset itself. A lookalike
// sharing the code never matches: the lookup is on the full CODE-ISSUER id.
func (s *Server) globalMarketAsset(assetID string) (global, local canonical.Asset, ok bool) {
	vc, ok := s.verifiedCurrencies.LookupByStellarAssetID(assetID)
	if !ok || vc.Class == currency.ClassFiat || vc.CoinGeckoID == "" {
		return canonical.Asset{}, canonical.Asset{}, false
	}
	local, err := canonical.ParseAsset(assetID)
	if err != nil || local.Type != canonical.AssetClassic {
		return canonical.Asset{}, canonical.Asset{}, false
	}
	global, err = canonical.NewCryptoAsset(vc.Ticker)
	if err != nil {
		return canonical.Asset{}, canonical.Asset{}, false
	}
	return global, local, true
}

// globalMarketRefs reads the shared oracle-stream snapshot only when a
// row can use it, so pages without a vetted classic asset pay nothing.
func (s *Server) globalMarketRefs(ctx context.Context, rows []AssetDetail) map[string]rwaReference {
	if s.oracle == nil {
		return nil
	}
	for i := range rows {
		if _, _, ok := s.globalMarketAsset(rows[i].AssetID); ok {
			return s.cachedRWAReferences(ctx).globalUSD
		}
	}
	return nil
}

// addGlobalMarketRow keeps the newest aggregator USD row per global
// ticker, and reports whether u was a global-ticker row at all.
func addGlobalMarketRow(into map[string]rwaReference, u canonical.OracleUpdate) bool {
	if u.Asset.Type != canonical.AssetCrypto {
		return false
	}
	if external.Lookup(u.Source).Class != external.ClassAggregator || !isUSDQuote(u.Quote) ||
		u.Timestamp.After(time.Now().Add(globalMarketMaxSkew)) {
		return true
	}
	key := u.Asset.String()
	if prev, ok := into[key]; ok {
		if prev.asOf.After(u.Timestamp) ||
			(prev.asOf.Equal(u.Timestamp) && prev.source <= u.Source) {
			return true
		}
	}
	into[key] = rwaReference{
		priceUSD: ratFromScaledInt(u.Price.BigInt(), u.Decimals),
		wire:     scaledDecimalString(u.Price.BigInt(), u.Decimals),
		source:   u.Source,
		feed:     key,
		asOf:     u.Timestamp,
	}
	return true
}

// applyGlobalMarket attaches the global-market reference to a vetted row.
// Runs after the substance gate: a surviving Stellar market price is
// measured against the reference; a missing one is filled from it, ahead
// of any fixed declared peg, so a global depeg shows in price_usd.
func (s *Server) applyGlobalMarket(row *AssetDetail, refs map[string]rwaReference, now time.Time) {
	g, local, ok := s.globalMarketAsset(row.AssetID)
	if !ok {
		return
	}
	ref, ok := refs[g.String()]
	if !ok || ref.priceUSD == nil || ref.priceUSD.Sign() <= 0 ||
		now.Sub(ref.asOf) > globalMarketMaxAge || ref.asOf.After(now.Add(globalMarketMaxSkew)) {
		return
	}
	gm := &AssetGlobalMarket{Asset: g.String(), PriceUSD: ref.wire, Source: ref.source, AsOf: WireTime(ref.asOf)}
	switch {
	case row.PriceUSD == nil:
		p := ref.wire
		row.PriceUSD = &p
		row.PriceBasis = priceBasisGlobalMarket
		row.PriceWithheldReason = ""
	case row.PriceBasis != "" && row.PriceBasis != priceBasisTransitive:
	case s.isDeclaredUSDPeg(local):
		// A declared USD peg is the USD proxy itself: its Stellar USD price
		// is quoted through itself, so the gap would measure a global depeg.
	case row.ThinMarket:
		// A thin price is served under include_thin as-is; nothing is
		// derived from it, so no divergence, depeg warning or signals.
	default:
		s.stampStellarDivergence(gm, *row.PriceUSD, ref.priceUSD)
		if gm.DepegWarning {
			gm.IssuerSignals = row.IssuerBehaviour.riskSignals()
		}
	}
	row.GlobalMarket = gm
}

// stampStellarDivergence sets the signed divergence of the Stellar price
// from the global one and the depeg warning, in exact rationals.
func (s *Server) stampStellarDivergence(gm *AssetGlobalMarket, stellarPrice string, global *big.Rat) {
	stellar, ok := new(big.Rat).SetString(stellarPrice)
	if !ok {
		return
	}
	pct, err := pctChange(stellarPrice, gm.PriceUSD)
	if err != nil {
		return
	}
	gm.StellarDivergencePct = &pct
	dev := new(big.Rat).Sub(stellar, global)
	dev.Quo(dev, global).Mul(dev, big.NewRat(100, 1)).Abs(dev)
	th := s.divergenceThresholdPct
	if !(th > 0) {
		th = defaultStellarDivergenceThresholdPct
	}
	if thr := new(big.Rat).SetFloat64(th); thr != nil {
		gm.DepegWarning = dev.Cmp(thr) > 0
	}
}
