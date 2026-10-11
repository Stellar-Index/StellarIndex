package v1

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Issuer directory-label overlay for /v1/assets + /v1/assets/{id}.
//
// Joins each asset's issuer G-address (a contract token's own C-address)
// against account_directory and stamps issuer_directory_{tags,domain,name}
// onto AssetDetail. DISPLAY-ONLY, except for SCAM-CLASS tags
// (malicious/unsafe/fraud/scam/hack/phishing):
//
//  1. WITHHOLD every published dollar figure on the row (price, market cap,
//     listing reference/valuation pair) via suppressScamIssuerPricing and
//     pricingguard.ScamGate: a scam token must not lend itself legitimacy.
//  2. DEMOTE the asset in the /v1/assets listing (listingRankTierExpr); the row
//     and its warning fields stay.
//
// Labels are best-effort: a nil reader, unlisted issuer or failed lookup omits
// the fields. A failed lookup still withholds pricing (issuerDirectoryUnchecked).
// A nil reader prices normally; the SQL demotion fails open.

// stampIssuerDirectory copies one curated directory label onto the
// detail. Tags are set only when non-empty so an unlabelled entry
// doesn't emit an empty `[]`; domain/name are omitempty strings.
func stampIssuerDirectory(detail *AssetDetail, e timescale.DirectoryEntry) {
	if len(e.Tags) > 0 {
		detail.IssuerDirectoryTags = e.Tags
	}
	detail.IssuerDirectoryDomain = e.Domain
	detail.IssuerDirectoryName = e.Name
}

// directoryAddress is the account_directory key that labels the asset: a
// classic asset's issuer G-address, or a contract token's own C-address
// (it has no issuer account, and the directory labels contracts too).
// Empty for assets with neither (native, fiat, catalogue-global).
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

// fillIssuerDirectoryTags resolves the whole listing page's issuer set
// in ONE batch query (no N+1), then stamps each row. Rows with no
// directoryAddress (native / catalogue-global) are skipped.
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

// suppressScamIssuerPricing withholds an asset's published PRICE claim on
// the payload when its issuer carries a scam-class directory tag
// (malicious/unsafe/fraud/scam/hack/phishing — pricingguard classifier):
// price_usd, market_cap_usd, fdv_usd, the price-derived change_*, the
// price_history_* SERIES and the listing-sourced listing_reference /
// listing_valuation pair are nulled, while circulating_supply (a raw
// chain fact) and the scam warning fields are kept. This is the
// payload-side twin of the reader-seam pricingguard.ScamGate (which
// withholds /v1/price and every reader-backed surface); together they
// ensure a scam issuer publishes neither a price nor a market cap. An
// unlisted/untagged issuer is left untouched; one whose lookup failed is
// withheld (issuerDirectoryUnchecked). Idempotent + nil-safe.
func suppressScamIssuerPricing(d *AssetDetail) {
	if d == nil || !issuerPricingWithheld(d) {
		return
	}
	if d.PriceUSD != nil {
		d.PriceWithheldReason = PriceWithheldUnattributed
		if pricingguard.IsDirectoryScamFlagged(d.IssuerDirectoryTags) {
			d.PriceWithheldReason = PriceWithheldScamIssuer
		}
	}
	d.PriceUSD = nil
	d.MarketCapUSD = nil
	d.FDVUSD = nil
	d.Change1hPct = nil
	d.Change24hPct = nil
	d.Change7dPct = nil
	// The withheld number must not come back as a PICTURE of itself: the
	// last bucket of price_history_* IS the price we just refused to
	// publish. Measured on r1: the flagged JFKBANK2 and RIO
	// details served price_usd: null next to 24 hourly + 7 daily priced
	// points, and the flagged listing rows drew a full sparkline beside
	// their "—" price cell.
	//
	// ATH is nulled by the same call. An all-time HIGH is a USD price
	// claim drawn from the same USD-quoted CAGG as price_usd, so serving
	// `"ath": {"usd": "0.0091"}` next to `"price_usd": null` hands a
	// client a published dollar valuation for a token the platform
	// decided must publish none — and an anchor to expect value from,
	// which is the legitimacy transfer this gate exists to prevent.
	withholdPriceSeriesWhenUnpriced(d)
	// The listing-sourced second opinion is the same claim in a
	// different field name. `listing_reference.price_usd` is a dollar
	// price and `listing_valuation.value_usd` is a dollar market cap,
	// both drawn from the third-party listing directory, and serving
	// either beside the price_usd and market_cap_usd this function just
	// withheld republishes the exact figure the tag exists to refuse.
	//
	// The arm that fills them has its own refusal for a flagged issuer,
	// and it is not enough on its own: it reads IssuerDirectoryTags,
	// which only the caller of THIS function populates, so on any path
	// where the two run in the wrong order that refusal sees an empty
	// slice and the figure is published anyway. This makes the outcome
	// hold whatever the order — the tag is the authority, not the
	// sequence.
	//
	// The pair is cleared TOGETHER. A reference with no valuation is a
	// dollar price on the wire with no statement of what it was used
	// for, which is the same half-carried shape mergeTwinStats refuses
	// to produce.
	d.ListingReference = nil
	d.ListingValuation = nil
	// global_market.price_usd is a dollar price for this row too.
	d.GlobalMarket = nil
}

// withholdPriceSeriesWhenUnpriced drops every derived PRICE-OVER-TIME
// claim from a payload that may not publish a price series — whatever
// the reason (scam-issuer suppression, the thin-market substance gate,
// a declared peg, or simply having no price at all). A price series is
// the price over time and an all-time high is one point of it; serving
// either beside a null price_usd both leaks the withheld value and
// makes the payload self-contradictory.
//
// It shares [priceSeriesPublishable] with the listing's sparkline
// attach, rather than restating the rule. Two copies drift: testing
// only `PriceUSD != nil` here while [sparkline7dEligible] also excludes
// declared-peg rows would refuse a declared-peg asset a sparkline on
// /v1/assets and serve a full price_history_24h/7d on /v1/assets/{id} —
// the dust series the listing had just declined to draw. One predicate
// serves both callers, so they cannot drift.
func withholdPriceSeriesWhenUnpriced(d *AssetDetail) {
	if d == nil || priceSeriesPublishable(d) {
		return
	}
	d.PriceHistory24h = nil
	d.PriceHistory7d = nil
	d.ATH = nil
}
