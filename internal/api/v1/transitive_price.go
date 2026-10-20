package v1

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TransitivePricer supplies a one-hop USD price (candidates best-first, empty means no route) for assets the catalogue cannot price directly; nil leaves them unpriced.
type TransitivePricer interface {
	TransitiveUSDPriceCandidates(ctx context.Context, assetID string) ([]timescale.TransitivePrice, error)
}

// transitivePriceFor returns a USD price through ONE intermediate hop: the first ranked candidate whose asset and hop are not
// scam- withheld and whose two legs both clear the substance floors; a failing candidate falls through. Returns ("", false) when
// none may be served, errors included. Both legs are gated because a two-hop price inherits its weakest leg: a thin hop would
// otherwise reprice everything quoted against it, the manipulation the floors stop.
func (s *Server) transitivePriceFor(ctx context.Context, asset canonical.Asset, assetID string) (string, bool) {
	if s.TransitivePricer == nil {
		return "", false
	}
	candidates, err := s.TransitivePricer.TransitiveUSDPriceCandidates(ctx, assetID)
	if err != nil {
		if s.logger != nil {
			s.logger.Debug("transitive price lookup failed",
				"asset_id", assetID, "err", err)
		}
		return "", false
	}
	for _, tp := range candidates {
		if s.transitiveCandidateAllowed(ctx, asset, tp) {
			return s.normalizeTransitiveUSD(tp.PriceUSD, asset), true
		}
	}
	return "", false
}

// transitiveCandidateAllowed reports whether one candidate hop's price
// may be served: both legs scam-clear and substance-cleared.
func (s *Server) transitiveCandidateAllowed(ctx context.Context, asset canonical.Asset, tp timescale.TransitivePrice) bool {
	if tp.PriceUSD == "" {
		return false
	}
	hop, err := canonical.ParseAsset(tp.Hop)
	if err != nil {
		// An unparseable hop cannot be substance-gated, so it is never trusted.
		return false
	}

	// Near leg (the market converting asset into hop) through the package's one fold of both
	// gates. The scam half covers asset AND hop, since /v1/price refuses a flagged hop itself.
	if withheldBy(ctx, s.Substance, s.Scam, asset, hop, "transitive") != pricingguard.NotWithheld {
		return false
	}

	// With the substance gate not wired the fold above allowed the leg by default, and we must
	// NOT invent a price the gate never saw.
	if s.Substance == nil {
		return false
	}
	// Far leg: the hop must stand on its own against the SAME quote set the catalogue prices
	// through; reuse assetPriceAllowed, which encodes that policy. Counted as "transitive".
	return s.assetPriceAllowed(ctx, hop, "transitive")
}

// normalizeTransitiveUSD applies the dex-nonstandard-decimals normalisation to a chain of RAW prices_1m ratios. The hop's
// decimals telescope away, leaving an error of exactly 10^(asset decimals - terminal quote decimals), the terminal being a
// standard-scale USD proxy ([defaultPriceQuote]); otherwise a 9dp Soroban token reads 100x low.
func (s *Server) normalizeTransitiveUSD(priceUSD string, asset canonical.Asset) string {
	return s.normalizeRawRatioString(priceUSD, asset, defaultPriceQuote)
}
