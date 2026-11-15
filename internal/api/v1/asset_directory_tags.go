package v1

import (
	"context"
	"time"

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
// directoryAddress are skipped. A contract row that is the SAC of a classic asset is also judged on that asset's issuer:
// the SAC is the same token under another address, and a flag on the issuer must not be shed by listing the wrapper.
func (s *Server) fillIssuerDirectoryTags(ctx context.Context, rows []AssetDetail) {
	if s.Directory == nil || len(rows) == 0 {
		return
	}
	wrapped, wrappedUnread := s.sacWrappedIssuers(ctx, rows)
	seen := make(map[string]struct{}, len(rows))
	addrs := make([]string, 0, len(rows))
	for i := range rows {
		for _, addr := range []string{directoryAddress(&rows[i]), wrapped[i]} {
			if addr == "" {
				continue
			}
			if _, dup := seen[addr]; dup {
				continue
			}
			seen[addr] = struct{}{}
			addrs = append(addrs, addr)
		}
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
		stampListingRowIssuer(&rows[i], wrapped[i], found, err != nil, wrappedUnread[i])
	}
}

// stampListingRowIssuer applies the page's directory answer to one row. dirUnread: the batch read failed; wrapperUnread:
// the lake did not say whether the row is a SAC.
func stampListingRowIssuer(row *AssetDetail, wrappedIssuer string, found map[string]timescale.DirectoryEntry, dirUnread, wrapperUnread bool) {
	addr := directoryAddress(row)
	if addr == "" {
		return
	}
	e, ok := found[addr]
	if ok && !dirUnread {
		// Stamped even when the wrapper is unread: an unread wrapper hides no verdict the directory gave.
		stampIssuerDirectory(row, e)
	}
	if dirUnread || wrapperUnread {
		withholdUncheckedIssuerPricing(row)
		return
	}
	stampWrappedIssuerFlags(row, wrappedIssuer, found)
	// Every row, not only directory hits: the curated list flags an issuer the directory may not hold.
	suppressScamIssuerPricing(row)
}

// stampWrappedIssuerFlags carries a wrapped asset's issuer flags onto its SAC row: the curated scam reason, and the
// issuer's directory entry when the SAC has none of its own or the issuer's is the flagged one.
func stampWrappedIssuerFlags(row *AssetDetail, issuer string, found map[string]timescale.DirectoryEntry) {
	if issuer == "" {
		return
	}
	if row.IssuerScamReason == "" {
		row.IssuerScamReason = scamReason(issuer)
	}
	e, ok := found[issuer]
	if !ok {
		return
	}
	if len(row.IssuerDirectoryTags) == 0 || pricingguard.IsDirectoryScamFlagged(e.Tags) {
		stampIssuerDirectory(row, e)
	}
}

// sacWrappedIssuers returns, per row, the issuer of the classic asset a contract row is the SAC of ("" for any other row),
// and whether that lookup failed: an unread wrapper is not "not a SAC".
func (s *Server) sacWrappedIssuers(ctx context.Context, rows []AssetDetail) (issuers []string, unread []bool) {
	issuers, unread = make([]string, len(rows)), make([]bool, len(rows))
	if s.Explorer == nil {
		return issuers, unread
	}
	var lookup []int
	for i := range rows {
		if rows[i].Type == string(canonical.AssetSoroban) && canonical.IsContractID(rows[i].AssetID) {
			lookup = append(lookup, i)
		}
	}
	if len(lookup) == 0 {
		return issuers, unread
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	forEachBounded(s.logger, len(lookup), readFanoutConcurrency, func(j int) {
		i := lookup[j]
		// Unread until the lookup answers, so a panic inside it withholds the row.
		unread[i] = true
		id, ok, err := s.sacWrappedAssetIDErr(lookupCtx, rows[i].AssetID)
		if err != nil {
			return
		}
		unread[i] = false
		if !ok {
			return
		}
		if asset, perr := canonical.ParseAsset(id); perr == nil && asset.Type == canonical.AssetClassic {
			issuers[i] = asset.Issuer
		}
	})
	return issuers, unread
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
	return d.issuerDirectoryUnchecked || issuerScamFlagged(d)
}

// issuerScamFlagged reports a flag from either source: the curated list or a scam-class directory tag. The curated list
// exists so a verdict does not depend on the directory holding a row for the issuer.
func issuerScamFlagged(d *AssetDetail) bool {
	return d.IssuerScamReason != "" || pricingguard.IsDirectoryScamFlagged(d.IssuerDirectoryTags)
}

// suppressScamIssuerPricing nulls every dollar claim (price, market cap, FDV, change_*, price_history_*, listing valuation)
// when the issuer is flagged (curated list or scam-class directory tag) or its lookup failed (issuerDirectoryUnchecked); supply and warnings stay.
// Payload-side twin of pricingguard.ScamGate. Idempotent, nil-safe.
func suppressScamIssuerPricing(d *AssetDetail) {
	if d == nil || !issuerPricingWithheld(d) {
		return
	}
	flagged := issuerScamFlagged(d)
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
