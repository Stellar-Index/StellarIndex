package v1

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TransitivePricer supplies a one-hop USD price for assets the catalogue
// cannot price directly. Optional seam: nil leaves every asset exactly as
// it is today, so wiring this on is the whole behaviour change.
//
// Production wiring is timescale.Store. See
// [timescale.Store.TransitiveUSDPriceCandidates] for why the hop is
// returned alongside the price rather than hidden. Candidates arrive
// best-first; an empty slice means no route.
type TransitivePricer interface {
	TransitiveUSDPriceCandidates(ctx context.Context, assetID string) ([]timescale.TransitivePrice, error)
}

// transitivePriceFor returns a USD price derived through ONE intermediate
// hop: the first ranked candidate whose asset and hop are not
// scam-withheld and whose two legs both clear the substance floors. A
// candidate that fails a gate falls through to the next, so a deep near
// leg into a thin hop cannot hide a sound route behind it. Returns
// ("", false) whenever no candidate may be served, error paths included.
//
// # WHY BOTH LEGS
//
// A two-hop price inherits the weakness of its weakest leg. Gating only
// the near leg (asset→hop) would let a thin INTERMEDIATE market set the
// price of everything quoted against it: move the hop's price in a quiet
// moment and every downstream asset reprices with it. That is precisely
// the manipulation the substance floors exist to stop, one hop removed —
// so the hop is gated against the same proxy quotes the catalogue prices
// through, exactly as if it were being served on its own.
//
// The near leg is gated as (asset, hop) rather than against a proxy,
// because that IS the market being trusted to convert one into the other.
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
		// An unparseable hop cannot be substance-gated, so it cannot be
		// trusted. Never serve on the strength of a hop we can't name.
		return false
	}

	// Near leg, the market converting asset into hop, through the
	// package's one fold of both gates. The scam half covers asset AND
	// hop: a price derived through a flagged issuer's market is that
	// market's price; /v1/price refuses it for the hop itself, so it must
	// not reappear here one conversion removed.
	if withheldBy(ctx, s.Substance, s.Scam, asset, hop, "transitive") != pricingguard.NotWithheld {
		return false
	}

	// With the substance gate not wired the fold above allowed the leg by
	// default, and we must NOT invent a price the gate never saw.
	if s.Substance == nil {
		return false
	}
	// Far leg: the hop must stand on its own against the SAME quote set
	// the catalogue prices through — native/XLM-SAC, fiat:USD, or an
	// operator-declared USD peg. listingPriceAllowed already encodes
	// exactly that policy, so reuse it rather than restate it and risk
	// the two drifting — counted as "transitive", since no listing row
	// was served.
	return s.assetPriceAllowed(ctx, hop, "transitive")
}

// normalizeTransitiveUSD applies the dex-nonstandard-decimals forward
// normalisation to the resolver's product, which is a chain of RAW
// prices_1m ratios: asset→hop, then hop→USD-proxy (or hop→XLM→USD-proxy).
//
// Each raw leg is off by 10^(its base's decimals − its quote's decimals)
// — inverted legs included, since inverting the ratio inverts the raw
// factor with it — so the chain's factors TELESCOPE: the hop's (and
// XLM's) decimals cancel and the whole product is off by exactly
// 10^(asset decimals − terminal quote decimals). The terminal quote is
// always one of the resolver's USD proxies (classic USDC, its SAC,
// fiat:USD), every one of them on the standard scale, which is what
// resolving [defaultPriceQuote] yields. One exact multiply therefore
// corrects the product no matter which hop or arm produced it, and the
// hop never needs to be resolved at all.
//
// Byte-identical for an asset with no confirmed non-7-decimals row (see
// [Server.normalizeRawRatioString]). Without it the raw product would read
// 100x low for a 9dp token, on exactly the assets this fill exists for —
// Soroban-native contracts, the only class that can be non-7dp.
func (s *Server) normalizeTransitiveUSD(priceUSD string, asset canonical.Asset) string {
	return s.normalizeRawRatioString(priceUSD, asset, defaultPriceQuote)
}
