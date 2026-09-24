package v1

import (
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/rwa"
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
		return &RWAAsset{AssetID: code + "-" + issuer, Code: code, Issuer: issuer, CirculatingSupply: &supply, Decimals: intPtr(7)}
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
	if a.Premium.Status != RWAPremiumReferenceNotOracleCNAV {
		t.Errorf("premium = %+v, want the not-an-oracle-CNAV status", a.Premium)
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

// A prospectus constant NAV is a rule, but the READING of it is bounded
// like every other reference: through the binding's review deadline the
// row is served at par unlabelled; from the first instant after it the
// same figure is served `stale: true` and Source says the binding is
// due for re-verification. The figure itself is never withheld on this
// bound — the label is the finding.
func TestRWAApplyReference_ProspectusConstantNAV_StaleAfterReviewBy(t *testing.T) {
	const issuer = "GD5J73EKK5IYL5XS3FBTHHX7CZIYRP7QXDL57XFWGC2WVYWT326OBXRP"
	binding, ok := rwa.ConstantNAV("gBENJI", issuer)
	if !ok {
		t.Fatal("gBENJI binding missing")
	}
	deadline := binding.ReviewDeadline()
	if deadline.IsZero() || binding.ReviewBy == "" {
		t.Fatalf("gBENJI binding carries no review bound: %+v", binding)
	}
	supply := "566742721191613"
	snap := rwaReferences{available: true, byFeed: map[string]rwaReference{}, nonUSD: map[string]string{}}
	serve := func(now time.Time) *RWAAsset {
		a := &RWAAsset{AssetID: "gBENJI-" + issuer, Code: "gBENJI", Issuer: issuer, CirculatingSupply: &supply, Decimals: intPtr(7)}
		rwaApplyReference(a, snap, nil, map[string]timescale.ListingEntry{}, now)
		if a.Reference == nil || a.Reference.Provenance != RWAReferenceProspectusCNAV || a.Reference.PriceUSD != "1.00" {
			t.Fatalf("at %s: reference = %+v, want the prospectus CNAV at 1.00", now.Format(time.RFC3339), a.Reference)
		}
		if a.ReferenceValuation.ValueUSD == nil || *a.ReferenceValuation.ValueUSD != "56674272.12" {
			t.Errorf("at %s: valuation = %+v, want 56674272.12 — the bound labels, it does not withhold", now.Format(time.RFC3339), a.ReferenceValuation)
		}
		if a.Premium.Status != RWAPremiumReferenceNotOracleCNAV {
			t.Errorf("at %s: premium = %+v, want the not-an-oracle-CNAV status", now.Format(time.RFC3339), a.Premium)
		}
		return a
	}

	for _, now := range []time.Time{deadline.Add(-24 * time.Hour), deadline.Add(-time.Second), deadline} {
		a := serve(now)
		if a.Reference.Stale {
			t.Errorf("at %s (on or before the review deadline %s): stale = true, want false", now.Format(time.RFC3339), binding.ReviewBy)
		}
		if !strings.Contains(a.Reference.Source, "review by "+binding.ReviewBy) || strings.Contains(a.Reference.Source, "due for re-verification") {
			t.Errorf("at %s: source %q should name the review date and not claim the review is due", now.Format(time.RFC3339), a.Reference.Source)
		}
	}
	for _, now := range []time.Time{deadline.Add(time.Second), deadline.Add(24 * time.Hour), deadline.AddDate(1, 0, 0)} {
		a := serve(now)
		if !a.Reference.Stale {
			t.Errorf("at %s (after the review deadline %s): stale = false, want true", now.Format(time.RFC3339), binding.ReviewBy)
		}
		if !strings.Contains(a.Reference.Source, "due for re-verification") || !strings.Contains(a.Reference.Source, binding.ReviewBy) || !strings.Contains(a.Reference.Source, binding.VerifiedOn) {
			t.Errorf("at %s: source %q should say the binding is due for re-verification and name both dates", now.Format(time.RFC3339), a.Reference.Source)
		}
		if !strings.Contains(a.Reference.Source, binding.ISIN) {
			t.Errorf("at %s: source %q should cite the page for the binding's own ISIN", now.Format(time.RFC3339), a.Reference.Source)
		}
	}
}

// A listing row that is not a usable valuation — zero, negative,
// unparseable or past the reference bound — must not suppress the
// prospectus NAV: precedence asks whether the listing IS a valuation, not
// whether the directory published a string. A pair the prospectus does
// not bind still reports the listing arm's specific refusal.
func TestRWAApplyReference_UnusableListingPriceFallsToConstantNAV(t *testing.T) {
	const issuer = "GD5J73EKK5IYL5XS3FBTHHX7CZIYRP7QXDL57XFWGC2WVYWT326OBXRP"
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	supply := "566742721191613"
	snap := rwaReferences{available: true, byFeed: map[string]rwaReference{}, nonUSD: map[string]string{}}
	fresh := now.Add(-time.Hour)
	cases := []struct {
		name      string
		entry     timescale.ListingEntry
		unboundAs string
	}{
		{"zero", timescale.ListingEntry{PriceUSD: "0", PricedAt: fresh}, RWAPremiumReferenceNotPositive},
		{"zero decimal", timescale.ListingEntry{PriceUSD: "0.000", PricedAt: fresh}, RWAPremiumReferenceNotPositive},
		{"negative", timescale.ListingEntry{PriceUSD: "-1", PricedAt: fresh}, RWAPremiumReferenceNotPositive},
		{"unparseable", timescale.ListingEntry{PriceUSD: "n/a", PricedAt: fresh}, RWAPremiumReferenceNotPositive},
		{"expired", timescale.ListingEntry{PriceUSD: "0.99", PricedAt: now.Add(-rwaReferenceMaxAge - time.Second)}, RWAPremiumReferenceExpired},
		{"undated", timescale.ListingEntry{PriceUSD: "0.99"}, RWAPremiumReferenceExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := tc.entry
			entry.Source, entry.ListingID = "listing", "x"

			a := &RWAAsset{AssetID: "gBENJI-" + issuer, Code: "gBENJI", Issuer: issuer, CirculatingSupply: &supply, Decimals: intPtr(7)}
			rwaApplyReference(a, snap, nil, map[string]timescale.ListingEntry{a.AssetID: entry}, now)
			if a.Reference == nil || a.Reference.Provenance != RWAReferenceProspectusCNAV || a.Reference.PriceUSD != "1.00" {
				t.Fatalf("reference = %+v (valuation status %q), want the prospectus CNAV at 1.00", a.Reference, a.ReferenceValuation.Status)
			}
			if a.ReferenceValuation.ValueUSD == nil || *a.ReferenceValuation.ValueUSD != "56674272.12" {
				t.Errorf("reference valuation = %+v, want 56674272.12", a.ReferenceValuation)
			}
			if a.Premium.Status != RWAPremiumReferenceNotOracleCNAV {
				t.Errorf("premium = %+v, want the not-an-oracle-CNAV status", a.Premium)
			}

			const other = "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
			u := &RWAAsset{AssetID: "gBENJI-" + other, Code: "gBENJI", Issuer: other, CirculatingSupply: &supply, Decimals: intPtr(7)}
			rwaApplyReference(u, snap, nil, map[string]timescale.ListingEntry{u.AssetID: entry}, now)
			if u.Reference != nil || u.ReferenceValuation.Status != tc.unboundAs || u.Premium.Status != tc.unboundAs {
				t.Errorf("unbound pair: reference = %+v, valuation %q, premium %q; want no reference and %q", u.Reference, u.ReferenceValuation.Status, u.Premium.Status, tc.unboundAs)
			}
		})
	}
}
