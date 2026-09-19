package v1

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The scam-class refusal on the listing-valuation arm, from both sides:
// the ARM declining to build the pair once the directory has stamped the
// row, and the SUPPRESSION tearing it down if it was built anyway.
//
// Held apart from asset_listing_valuation_internal_test.go, which is
// about the arm's binding and its hole, so a reader looking for why a
// flagged issuer publishes nothing finds one file rather than a section.
// The stubs and helpers are that file's; same package.

// TestListingValuation_ScamSuppressionClearsTheListingPair — RLT-313.
//
// suppressScamIssuerPricing nulled price_usd, market_cap_usd, fdv_usd, the
// change_* set and the price series, and left the listing-sourced pair
// standing. `listing_reference.price_usd` is a dollar price and
// `listing_valuation.value_usd` is a dollar market cap, drawn from the same
// third-party directory, so a flagged row served exactly the two figures the
// tag exists to refuse — under different field names, beside the nulls.
//
// The pair is built by the arm above and then suppressed, which is the real
// shipped sequence on the catalogue-twin fan-out, so this exercises the
// production producer rather than a hand-built literal.
func TestListingValuation_ScamSuppressionClearsTheListingPair(t *testing.T) {
	srv := tlvServer(t, &stubAssetListingDirectory{rows: map[string]timescale.ListingEntry{
		tlvUSDT0SAC: tlvEntry(tlvUSDT0SAC, "usdt0", "1.0002", time.Hour),
	}}, map[string]string{tlvUSDT0SAC: tlvUSDT0LakeSupply})

	rows := tlvApply(t, srv, []AssetDetail{
		tlvDustSuppressedRow(tlvUSDT0Asset, "1.0001", tlvUSDT0TrustlineSupply),
	})
	row := &rows[0]
	if row.ListingReference == nil || row.ListingValuation == nil ||
		row.ListingValuation.ValueUSD == nil {
		t.Fatalf("precondition: the arm published no valuation to suppress "+
			"(reference=%+v valuation=%+v) — the rest of this test proves nothing",
			row.ListingReference, row.ListingValuation)
	}

	// The directory then labels the issuer malicious.
	row.IssuerDirectoryTags = []string{"malicious"}
	suppressScamIssuerPricing(row)

	if row.ListingReference != nil {
		t.Errorf("listing_reference = %+v, want nil — it carries a dollar price for an "+
			"issuer this row may publish no price for", row.ListingReference)
	}
	if row.ListingValuation != nil {
		t.Errorf("listing_valuation = %+v, want nil — it carries a dollar market cap for an "+
			"issuer this row may publish no market cap for", row.ListingValuation)
	}
	// The suppression's existing scope must not have moved.
	if row.PriceUSD != nil {
		t.Errorf("price_usd = %v, want nil", *row.PriceUSD)
	}
	if row.MarketCapUSD != nil {
		t.Errorf("market_cap_usd = %v, want nil", *row.MarketCapUSD)
	}
	// circulating_supply is a raw chain fact and stays.
	if row.CirculatingSupply == nil {
		t.Error("circulating_supply = nil, want the raw chain reading kept")
	}
}

// TestListingValuation_FlaggedIssuerGetsNoValuationWhenTagsArrivedFirst is
// the same refusal from the other side: once the directory has stamped the
// row — which is now what happens BEFORE this arm runs on every path — the
// arm declines to build the pair at all, silently, rather than building it
// for the suppression to tear down.
func TestListingValuation_FlaggedIssuerGetsNoValuationWhenTagsArrivedFirst(t *testing.T) {
	srv := tlvServer(t, &stubAssetListingDirectory{rows: map[string]timescale.ListingEntry{
		tlvUSDT0SAC: tlvEntry(tlvUSDT0SAC, "usdt0", "1.0002", time.Hour),
	}}, map[string]string{tlvUSDT0SAC: tlvUSDT0LakeSupply})

	row := tlvDustSuppressedRow(tlvUSDT0Asset, "1.0001", tlvUSDT0TrustlineSupply)
	row.IssuerDirectoryTags = []string{"malicious"}
	rows := tlvApply(t, srv, []AssetDetail{row})

	if rows[0].ListingReference != nil || rows[0].ListingValuation != nil {
		t.Errorf("flagged row got reference=%+v valuation=%+v, want neither",
			rows[0].ListingReference, rows[0].ListingValuation)
	}
}
