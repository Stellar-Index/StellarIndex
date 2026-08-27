package v1

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TransitivePricer supplies a one-hop USD price for assets the catalogue
// cannot price directly. Optional seam: nil leaves every asset exactly as
// it is today, so wiring this on is the whole behaviour change.
//
// Production wiring is timescale.Store. See
// [timescale.Store.TransitiveUSDPrice] for why the hop is returned
// alongside the price rather than hidden.
type TransitivePricer interface {
	TransitiveUSDPrice(ctx context.Context, assetID string) (timescale.TransitivePrice, bool, error)
}

// transitivePriceFor returns a USD price derived through ONE intermediate
// hop, but ONLY when both legs independently clear the substance floors.
// Returns ("", false) whenever the price must not be served — including
// every error path, because a price we cannot fully verify is worse than
// no price.
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
	if s.transitive == nil {
		return "", false
	}
	tp, ok, err := s.transitive.TransitiveUSDPrice(ctx, assetID)
	if err != nil || !ok || tp.PriceUSD == "" {
		if err != nil && s.logger != nil {
			s.logger.Debug("transitive price lookup failed",
				"asset_id", assetID, "err", err)
		}
		return "", false
	}

	hop, err := canonical.ParseAsset(tp.Hop)
	if err != nil {
		// An unparseable hop cannot be substance-gated, so it cannot be
		// trusted. Never serve on the strength of a hop we can't name.
		return "", false
	}

	// The substance gate is the whole safety property here — with it not
	// wired we must NOT invent a price the gate never saw.
	if s.substance == nil {
		return "", false
	}

	// Near leg: the market converting asset into hop.
	if !s.substance.Allowed(ctx, asset, hop, "transitive") {
		return "", false
	}
	// Far leg: the hop must stand on its own against the SAME quote set
	// the catalogue prices through — native/XLM-SAC, fiat:USD, or an
	// operator-declared USD peg. listingPriceAllowed already encodes
	// exactly that policy, so reuse it rather than restate it and risk
	// the two drifting.
	if !s.listingPriceAllowed(ctx, hop) {
		return "", false
	}
	return tp.PriceUSD, true
}
