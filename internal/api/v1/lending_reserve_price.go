package v1

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// reservePrice is [Server.reservePriceUSD]'s verdict: a price, or why there is none. scale is the decimals the price was normalised
// with (nonstandard-decimals projection, else 7): only an amount divided by that same exponent can be multiplied by it.
type reservePrice struct {
	price string
	// basis is the price's [AssetDetail.PriceBasis]: priceBasisTransitive for the one-hop fill, "" for a market price.
	basis string
	scale int
	stale bool
	// withheld: a gate refused the price, so the token is not unpriced.
	withheld bool
	// unread: a read on the chain failed, so a missing price is not "no market".
	unread bool
}

// exclusion is the reserve exclusion reason for a price-less verdict; unread outranks withheld, as neither verdict was complete.
func (p reservePrice) exclusion() string {
	switch {
	case p.unread:
		return exclusionPriceReadFailed
	case p.withheld:
		return exclusionPriceWithheld
	}
	return exclusionNoUSDValue
}

// reservePriceUSD prices a reserve's token through the /v1/assets/{id} chain under its default gates: unwrap a SAC, the canonical USD
// read, then the catalogue row's price, then a one-hop transitive price, then the issuer scam check. It takes neither the thin
// opt-in nor the declared-peg fill: the page derives no valuation from either.
func (s *Server) reservePriceUSD(ctx context.Context, assetC string) reservePrice {
	asset, ok, unread := s.reserveTokenAsset(ctx, assetC)
	if !ok || unread {
		// An unanswered SAC check leaves the wrapped issuer unchecked, which the page withholds pricing for.
		return reservePrice{unread: unread}
	}
	var out reservePrice
	l := s.lookupUSDPriceWithSources(ctx, asset)
	if l.price == "" && l.withheld == "" && !l.unread {
		// A feed keyed by the wrapper's own contract id still prices it.
		if raw, err := canonical.ParseAsset(assetC); err == nil && !raw.Equal(asset) {
			l = s.lookupUSDPriceWithSources(ctx, raw)
		}
	}
	if l.price != "" {
		out = reservePrice{price: l.price, stale: l.stale}
	} else {
		switch {
		case l.withheld != "":
			// Withheld beats every fallback, as on /v1/price.
			return reservePrice{withheld: true}
		case l.unread:
			return reservePrice{unread: true}
		}
		if out = s.reserveFallbackPrice(ctx, asset); out.price == "" {
			return out
		}
	}
	if flagged, unread := s.reserveIssuerFlagged(ctx, asset); flagged || unread {
		return reservePrice{withheld: flagged, unread: unread}
	}
	out.scale = aggregate.ResolveDecimals(s.NonstandardDecimals, asset)
	return out
}

// reserveTokenAsset resolves a reserve's C-strkey to the asset its prices are keyed by: a verified-catalogue SAC from the cached map,
// else the lake's SAC check ([Server.unwrapSAC]); ok=false when the id does not parse.
func (s *Server) reserveTokenAsset(ctx context.Context, assetC string) (asset canonical.Asset, ok, unread bool) {
	if id, found := s.resolveSACAsset(assetC); found {
		if a, err := canonical.ParseAsset(id); err == nil {
			return a, true, false
		}
	}
	a, err := canonical.ParseAsset(assetC)
	if err != nil {
		return canonical.Asset{}, false, false
	}
	a, unread = s.unwrapSAC(ctx, canonical.CanonicalAsset(a))
	return a, true, unread
}

// reserveFallbackPrice is the page's fill after the canonical read found nothing ([Server.applyAssetRowToDetail]): the catalogue
// row's price under its substance verdict, else the gated transitive price. withheld when the row's price was refused and no
// transitive price replaced it.
func (s *Server) reserveFallbackPrice(ctx context.Context, asset canonical.Asset) reservePrice {
	if s.AssetsReader == nil || asset.Type == canonical.AssetFiat {
		return reservePrice{}
	}
	ctx, rep := withReadReport(ctx)
	assetID := asset.String()
	refused := false
	row, err := s.lookupAssetRow(ctx, asset, assetID)
	switch {
	case err == nil:
		allowed, unread := s.catalogueRowPriceAllowed(ctx, asset)
		if unread {
			return reservePrice{unread: true}
		}
		refused = !allowed
		if allowed && row.PriceUSD != nil {
			if p, ok := s.normalizeCatalogueReadUSD(*row.PriceUSD, asset); ok {
				return reservePrice{price: p, stale: rep.stale.Load()}
			}
			refused = true
		}
	case !errors.Is(err, sql.ErrNoRows):
		return reservePrice{unread: true}
	}
	p, ok, unread := s.transitivePriceFor(ctx, asset, assetID)
	switch {
	case unread:
		return reservePrice{unread: true}
	case !ok:
		return reservePrice{withheld: refused}
	}
	return reservePrice{price: p, basis: priceBasisTransitive, stale: rep.stale.Load()}
}

// reserveIssuerFlagged is the page's issuer scam check ([suppressScamIssuerPricing]): the curated list, then the directory's
// scam-class tags; a failed directory read is unread, which withholds the price as it does on the page.
func (s *Server) reserveIssuerFlagged(ctx context.Context, asset canonical.Asset) (flagged, unread bool) {
	if scamReason(asset.Issuer) != "" {
		return true, false
	}
	addr := asset.Issuer
	if addr == "" && asset.Type == canonical.AssetSoroban {
		addr = asset.ContractID
	}
	if s.Directory == nil || addr == "" {
		return false, false
	}
	e, found, err := s.Directory.DirectoryEntryByAddress(ctx, addr)
	if err != nil {
		return false, true
	}
	return found && pricingguard.IsDirectoryScamFlagged(e.Tags), false
}
