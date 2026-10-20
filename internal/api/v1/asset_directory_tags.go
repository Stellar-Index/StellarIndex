package v1

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Issuer directory-label overlay for /v1/assets + /v1/assets/{id}.
//
// Stamps issuer_directory_{tags,domain,name} onto AssetDetail. Display-only, except scam-class tags
// (malicious/unsafe/fraud/scam/hack/phishing), which:
//
//  1. WITHHOLD every dollar figure (suppressScamIssuerPricing, pricingguard.ScamGate).
//  2. DEMOTE the asset in the listing (listingRankTierExpr); the row and its warnings stay.
//
// A failed lookup still withholds pricing (issuerDirectoryUnchecked); a nil reader prices normally
// and the SQL demotion fails open.

// stampIssuerDirectory copies one curated directory label onto the detail; tags are set only when non-empty so an unlabelled
// entry never emits `[]`.
func stampIssuerDirectory(detail *AssetDetail, e timescale.DirectoryEntry) {
	if len(e.Tags) > 0 {
		detail.IssuerDirectoryTags = e.Tags
	}
	detail.IssuerDirectoryDomain = e.Domain
	detail.IssuerDirectoryName = e.Name
}

// directoryAddress is the account_directory key for the asset: a classic issuer G-address or a contract token's own C-address;
// empty for native, fiat and catalogue-global.
func directoryAddress(d *AssetDetail) string {
	if d.Issuer != nil && *d.Issuer != "" {
		return *d.Issuer
	}
	if d.Type == string(canonical.AssetSoroban) {
		return d.AssetID
	}
	return ""
}

// applyIssuerDirectoryTags resolves the single detail-page issuer.
func (s *Server) applyIssuerDirectoryTags(ctx context.Context, detail *AssetDetail) {
	if s.Directory == nil || detail == nil {
		return
	}
	addr := directoryAddress(detail)
	if addr == "" {
		return
	}
	e, ok, err := s.Directory.DirectoryEntryByAddress(ctx, addr)
	if err != nil {
		s.logger.Warn("asset issuer directory lookup failed — withholding pricing",
			"address", addr, "err", err)
		detail.issuerDirectoryUnchecked = true
		return
	}
	if !ok {
		return
	}
	stampIssuerDirectory(detail, e)
}

// fillIssuerDirectoryTags resolves the page's issuer set in ONE batch query (no N+1) and stamps each row; rows with no
// directoryAddress are skipped.
func (s *Server) fillIssuerDirectoryTags(ctx context.Context, rows []AssetDetail) {
	if s.Directory == nil || len(rows) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(rows))
	addrs := make([]string, 0, len(rows))
	for i := range rows {
		addr := directoryAddress(&rows[i])
		if addr == "" {
			continue
		}
		if _, dup := seen[addr]; dup {
			continue
		}
		seen[addr] = struct{}{}
		addrs = append(addrs, addr)
	}
	if len(addrs) == 0 {
		return
	}
	found, err := s.Directory.DirectoryEntriesByAddresses(ctx, addrs)
	if err != nil {
		s.logger.Warn("asset listing directory batch lookup failed — withholding pricing",
			"n", len(addrs), "err", err)
	}
	for i := range rows {
		addr := directoryAddress(&rows[i])
		if addr == "" {
			continue
		}
		if err != nil {
			withholdUncheckedIssuerPricing(&rows[i])
			continue
		}
		if e, ok := found[addr]; ok {
			stampIssuerDirectory(&rows[i], e)
			suppressScamIssuerPricing(&rows[i])
		}
	}
}

// anyDirectoryUnchecked reports whether any row's pricing was withheld because
// its directory read failed, not on a verdict.
func anyDirectoryUnchecked(rows []AssetDetail) bool {
	for i := range rows {
		if rows[i].issuerDirectoryUnchecked {
			return true
		}
	}
	return false
}

// withholdUncheckedIssuerPricing marks a row whose directory lookup failed
// and withholds its dollar figures: an unanswered read is not "no tags".
func withholdUncheckedIssuerPricing(d *AssetDetail) {
	d.issuerDirectoryUnchecked = true
	suppressScamIssuerPricing(d)
}

// issuerPricingWithheld reports whether the row may publish no dollar
// figure because its issuer is scam-flagged or could not be checked.
func issuerPricingWithheld(d *AssetDetail) bool {
	return d.issuerDirectoryUnchecked || pricingguard.IsDirectoryScamFlagged(d.IssuerDirectoryTags)
}

// suppressScamIssuerPricing nulls every dollar claim (price, market cap, FDV, change_*, price_history_*, listing valuation)
// when the issuer has a scam-class directory tag or its lookup failed (issuerDirectoryUnchecked); supply and warnings stay.
// Payload-side twin of pricingguard.ScamGate. Idempotent, nil-safe.
func suppressScamIssuerPricing(d *AssetDetail) {
	if d == nil || !issuerPricingWithheld(d) {
		return
	}
	flagged := pricingguard.IsDirectoryScamFlagged(d.IssuerDirectoryTags)
	switch {
	case flagged && (d.PriceUSD != nil || d.PriceWithheldReason != ""):
		// The flag is this asset's own cause and overrides what a reader-side walk said: a
		// fiat-quoted read misses the scam gate and folds the flag into `upstream_leg`.
		d.PriceWithheldReason = PriceWithheldScamIssuer
	case d.PriceUSD != nil:
		d.PriceWithheldReason = PriceWithheldUnattributed
	}
	d.PriceUSD = nil
	d.MarketCapUSD = nil
	d.FDVUSD = nil
	d.Change1hPct = nil
	d.Change24hPct = nil
	d.Change7dPct = nil
	// Provenance belongs to the price: a withheld row credits no vendor and stamps no time for a number it refuses.
	d.PriceAuthority = ""
	d.PriceSources = nil
	d.PriceAsOf = nil
	d.PriceCredit = ""
	// The last bucket of price_history_* IS the withheld price, and an all-time high is a USD
	// price claim from the same CAGG, so both go with it; otherwise a null price_usd would
	// sit beside a priced sparkline or an "ath" figure.
	withholdPriceSeriesWhenUnpriced(d)
	// listing_reference and listing_valuation are the same dollar claims under other names.
	// The arm that fills them reads IssuerDirectoryTags, which only this function's caller
	// populates, so the tag must be enforced here whatever the call order. The pair is
	// cleared together: a reference without a valuation is a half-carried shape that
	// mergeTwinStats also refuses to produce.
	d.ListingReference = nil
	d.ListingValuation = nil
	// global_market.price_usd is a dollar price for this row too.
	d.GlobalMarket = nil
}

// withholdPriceSeriesWhenUnpriced drops price_history_* and the ATH from a payload that may not publish a price, since serving
// them beside a null price_usd leaks the withheld value. Shares [priceSeriesPublishable] with the sparkline attach so the
// surfaces cannot disagree.
func withholdPriceSeriesWhenUnpriced(d *AssetDetail) {
	if d == nil || priceSeriesPublishable(d) {
		return
	}
	d.PriceHistory24h = nil
	d.PriceHistory7d = nil
	d.ATH = nil
}
