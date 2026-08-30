package v1

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Issuer directory-label overlay for /v1/assets + /v1/assets/{id}.
//
// Joins each asset's issuer G-address against the curated third-party
// account_directory table (migration 0136; synced from the MIT-licensed
// stellar-expert/public-directory) and stamps the additive
// issuer_directory_{tags,domain,name} fields onto AssetDetail.
//
// DISPLAY-ONLY invariant — with TWO deliberate exceptions, both scoped to
// the SCAM-CLASS tags (malicious/unsafe/fraud/scam/hack/phishing). The
// tags remain third-party attribution that never affects the verified
// status or the substance/decimals gates.
//
//  1. 2026-08-25 — a scam-class tag WITHHOLDS the published price +
//     market cap, via suppressScamIssuerPricing below and the reader-seam
//     pricingguard.ScamGate: a scam token must not publish a
//     price/market-cap that lends it legitimacy, even when its market
//     clears the substance floor (RIO-GBNLJIYH… did).
//  2. 2026-08-29 (#356) — a scam-class tag DEMOTES the asset in the
//     /v1/assets listing: timescale's listingRankTierExpr ranks a flagged
//     issuer's assets below every unflagged one whatever the sort key.
//     Withholding the numbers while still ranking the token on raw volume
//     put a `malicious`/`unsafe` asset at #12 on the flagship /assets
//     page. The row and its warning fields stay — we refuse to rank a
//     flagged asset, we never hide it.
//
// Raw trade surfaces stay visible.
//
// Best-effort: a nil reader, an unlisted issuer (the common case), or a
// lookup failure just leaves the fields omitted and never fails the asset
// response — a directory outage must not take the asset surface down with
// it (the suppression fails OPEN too, and so does the ranking demotion).

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

// applyIssuerDirectoryTags resolves the single detail-page issuer.
func (s *Server) applyIssuerDirectoryTags(ctx context.Context, detail *AssetDetail) {
	if s.directory == nil || detail == nil || detail.Issuer == nil || *detail.Issuer == "" {
		return
	}
	e, ok, err := s.directory.DirectoryEntryByAddress(ctx, *detail.Issuer)
	if err != nil {
		s.logger.Warn("asset issuer directory lookup failed", "issuer", *detail.Issuer, "err", err)
		return
	}
	if !ok {
		return
	}
	stampIssuerDirectory(detail, e)
}

// fillIssuerDirectoryTags resolves the whole listing page's issuer set
// in ONE batch query (no N+1), then stamps each row. Rows without an
// issuer (native / catalogue-global) are skipped.
func (s *Server) fillIssuerDirectoryTags(ctx context.Context, rows []AssetDetail) {
	if s.directory == nil || len(rows) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(rows))
	addrs := make([]string, 0, len(rows))
	for i := range rows {
		iss := rows[i].Issuer
		if iss == nil || *iss == "" {
			continue
		}
		if _, dup := seen[*iss]; dup {
			continue
		}
		seen[*iss] = struct{}{}
		addrs = append(addrs, *iss)
	}
	if len(addrs) == 0 {
		return
	}
	found, err := s.directory.DirectoryEntriesByAddresses(ctx, addrs)
	if err != nil {
		s.logger.Warn("asset listing directory batch lookup failed", "n", len(addrs), "err", err)
		return
	}
	for i := range rows {
		iss := rows[i].Issuer
		if iss == nil {
			continue
		}
		if e, ok := found[*iss]; ok {
			stampIssuerDirectory(&rows[i], e)
			suppressScamIssuerPricing(&rows[i])
		}
	}
}

// suppressScamIssuerPricing withholds an asset's published PRICE claim on
// the payload when its issuer carries a scam-class directory tag
// (malicious/unsafe/fraud/scam/hack/phishing — pricingguard classifier):
// price_usd, market_cap_usd, fdv_usd, the price-derived change_*, and the
// price_history_* SERIES are nulled, while circulating_supply (a raw
// chain fact) and the scam warning fields are kept. This is the
// payload-side twin of the reader-seam pricingguard.ScamGate (which
// withholds /v1/price and every reader-backed surface); together they
// ensure a scam issuer publishes neither a price nor a market cap. Fails
// OPEN — an unlisted/untagged issuer is left untouched. Idempotent +
// nil-safe.
func suppressScamIssuerPricing(d *AssetDetail) {
	if d == nil || !pricingguard.IsDirectoryScamFlagged(d.IssuerDirectoryTags) {
		return
	}
	d.PriceUSD = nil
	d.MarketCapUSD = nil
	d.FDVUSD = nil
	d.Change1hPct = nil
	d.Change24hPct = nil
	d.Change7dPct = nil
	// The withheld number must not come back as a PICTURE of itself: the
	// last bucket of price_history_* IS the price we just refused to
	// publish. Measured on r1 2026-08-29 — the flagged JFKBANK2 and RIO
	// details served price_usd: null next to 24 hourly + 7 daily priced
	// points, and the flagged listing rows drew a full sparkline beside
	// their "—" price cell.
	withholdPriceSeriesWhenUnpriced(d)
}

// withholdPriceSeriesWhenUnpriced drops the price_history_* series from a
// payload whose headline price is NOT published — whatever withheld it
// (scam-issuer suppression, the thin-market substance gate) or however
// it came to be missing. A price series is the price over time; serving
// one beside a null price_usd both leaks the withheld value and makes
// the payload self-contradictory.
//
// The listing's own sparkline attach applies the same rule before the
// read ([sparkline7dEligible]) so a gated row is never even looked up;
// this is the detail-path (and post-suppression) enforcement.
func withholdPriceSeriesWhenUnpriced(d *AssetDetail) {
	if d == nil || d.PriceUSD != nil {
		return
	}
	d.PriceHistory24h = nil
	d.PriceHistory7d = nil
}
