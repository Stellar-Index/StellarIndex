package v1

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A CNAV share class with neither an oracle binding nor a listing price
// is valued at the NAV its prospectus fixes, under its own provenance;
// a listing price, when one exists, still wins; and a code alone does
// not bind.
func TestRWAApplyReference_ProspectusConstantNAV(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	supply := "566742721191613" // 56,674,272.1191613 tokens at 7 decimals
	mk := func(code, issuer string) *RWAAsset {
		return &RWAAsset{AssetID: code + "-" + issuer, Code: code, Issuer: issuer, CirculatingSupply: &supply, Decimals: 7}
	}
	snap := rwaReferences{available: true, byFeed: map[string]rwaReference{}, nonUSD: map[string]string{}}

	a := mk("gBENJI", "GD5J73EKK5IYL5XS3FBTHHX7CZIYRP7QXDL57XFWGC2WVYWT326OBXRP")
	rwaApplyReference(a, snap, nil, map[string]timescale.ListingEntry{}, now)
	if a.Reference == nil || a.Reference.Provenance != RWAReferenceProspectusCNAV || a.Reference.PriceUSD != "1.00" || a.Reference.Feed != "LU2900381208" {
		t.Fatalf("gBENJI reference = %+v, want the prospectus CNAV at 1.00 keyed by ISIN", a.Reference)
	}
	if a.ReferenceValuation.ValueUSD == nil || *a.ReferenceValuation.ValueUSD != "56674272.12" {
		t.Errorf("gBENJI reference valuation = %+v, want 56674272.12", a.ReferenceValuation)
	}
	if a.Premium.Status != RWAPremiumReferenceNotOracle {
		t.Errorf("premium = %+v, want the not-an-oracle status", a.Premium)
	}

	b := mk("gBENJI", "GD5J73EKK5IYL5XS3FBTHHX7CZIYRP7QXDL57XFWGC2WVYWT326OBXRP")
	rwaApplyReference(b, snap, nil, map[string]timescale.ListingEntry{b.AssetID: {PriceUSD: "0.99", PricedAt: now.Add(-time.Hour), Source: "listing", ListingID: "x"}}, now)
	if b.Reference == nil || b.Reference.Provenance != RWAReferenceListingPrice {
		t.Errorf("with a listing price: %+v, want the listing arm", b.Reference)
	}

	c := mk("gBENJI", "GAIMPOSTORIMPOSTORIMPOSTORIMPOSTORIMPOSTORIMPOSTORIMPOSTOR")
	rwaApplyReference(c, snap, nil, map[string]timescale.ListingEntry{}, now)
	if c.Reference != nil {
		t.Errorf("impostor got a reference: %+v", c.Reference)
	}
}
