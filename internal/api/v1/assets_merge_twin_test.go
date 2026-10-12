package v1

import (
	"slices"
	"testing"
)

// A catalogue row that inherits its classic twin's circulating supply must inherit the twin's scale too: the supply is a raw
// smallest-unit integer paired with `decimals`, and the catalogue's own SupplyDecimals is 0, so copying a 7dp stroop value onto
// it would render 10^7 too large.
func TestMergeTwinStats_CarriesDecimalsWithSupply(t *testing.T) {
	t.Parallel()

	supply := "342797138733487851"
	twin := AssetDetail{CirculatingSupply: &supply, Decimals: 7}

	// Catalogue row: no supply of its own, SupplyDecimals == 0.
	dst := AssetDetail{Type: "global", Decimals: 0}
	mergeTwinStats(&dst, twin)

	if dst.CirculatingSupply == nil || *dst.CirculatingSupply != supply {
		t.Fatalf("supply = %v, want the twin's", dst.CirculatingSupply)
	}
	if dst.Decimals != 7 {
		t.Errorf("decimals = %d, want 7 — a stroop-scale supply paired with decimals=0 "+
			"renders 10^7 too large in every consumer that scales by 10^decimals", dst.Decimals)
	}
}

// A row that already has its own supply keeps its own scale — the merge
// must not overwrite a self-consistent pair.
func TestMergeTwinStats_LeavesOwnSupplyAndDecimalsAlone(t *testing.T) {
	t.Parallel()

	own, twinSupply := "1000", "999999999"
	dst := AssetDetail{CirculatingSupply: &own, Decimals: 0}
	mergeTwinStats(&dst, AssetDetail{CirculatingSupply: &twinSupply, Decimals: 7})

	if *dst.CirculatingSupply != own {
		t.Errorf("supply overwritten: %s", *dst.CirculatingSupply)
	}
	if dst.Decimals != 0 {
		t.Errorf("decimals = %d, want its own 0", dst.Decimals)
	}
}

// TestMergeTwinStats_CarriesTheListingValuation: the valuation arm runs on the twin row (it holds the asset_id, price and gate
// outcome), so the catalogue row only sees it if the merge carries it; otherwise the /rwa stablecoin tile sum misses it.
func TestMergeTwinStats_CarriesTheListingValuation(t *testing.T) {
	value := "2581052.90"
	twin := AssetDetail{
		MarketCapLowLiquidity: true,
		ListingReference: &AssetListingReference{
			PriceUSD:    "1",
			Source:      "coingecko",
			ListingID:   "usdt0",
			Address:     "CBSJZEIO5C7KC2SF3MKSNXXJSW5G3VTNBX4ATMKUI3B2MR4JKM4R26YF",
			AddressForm: ListingAddressFormSAC,
			Provenance:  RWAReferenceListingPrice,
		},
		ListingValuation: &AssetListingValuation{
			Status:      ListingValuationPublished,
			ValueUSD:    &value,
			SupplyBasis: ListingSupplyBasisLakeFlows,
		},
	}
	var dst AssetDetail
	mergeTwinStats(&dst, twin)

	if dst.ListingValuation == nil || dst.ListingValuation.ValueUSD == nil {
		t.Fatalf("listing_valuation = %+v, want the twin's published figure", dst.ListingValuation)
	}
	if *dst.ListingValuation.ValueUSD != value {
		t.Errorf("value_usd = %q, want %q", *dst.ListingValuation.ValueUSD, value)
	}
	if dst.ListingReference == nil {
		t.Fatal("the reference was dropped — a figure with no traceable source")
	}
	if dst.ListingReference.Provenance != RWAReferenceListingPrice {
		t.Errorf("provenance = %q, want %q", dst.ListingReference.Provenance, RWAReferenceListingPrice)
	}
	// And the cap it stands beside is still absent.
	if dst.MarketCapUSD != nil {
		t.Errorf("market_cap_usd = %q, want none", *dst.MarketCapUSD)
	}
}

// TestMergeTwinStats_CarriesARefusalToo: a status with no figure must also cross, since a missing block reads as "arm does not
// apply" rather than "applies, but the directory could not be read".
func TestMergeTwinStats_CarriesARefusalToo(t *testing.T) {
	var dst AssetDetail
	mergeTwinStats(&dst, AssetDetail{
		ListingValuation: &AssetListingValuation{Status: ListingValuationNotListed},
	})
	if dst.ListingValuation == nil || dst.ListingValuation.Status != ListingValuationNotListed {
		t.Fatalf("listing_valuation = %+v, want status %q", dst.ListingValuation, ListingValuationNotListed)
	}
	if dst.ListingReference != nil {
		t.Error("a refusal must not carry a reference")
	}
}

// A catalogue row that takes its twin's volume takes the twin's flag with it;
// one with its own volume keeps its own.
func TestMergeTwinStats_CarriesLowerBoundWithVolume(t *testing.T) {
	t.Parallel()
	dst := AssetDetail{}
	mergeTwinStats(&dst, AssetDetail{VolumeUSD24h: strp("7"), VolumeLowerBound: true})
	if !dst.VolumeLowerBound {
		t.Error("borrowed twin volume lost its lower-bound flag")
	}
	own := AssetDetail{VolumeUSD24h: strp("9")}
	mergeTwinStats(&own, AssetDetail{VolumeUSD24h: strp("7"), VolumeLowerBound: true})
	if own.VolumeLowerBound {
		t.Error("row kept its own volume but took the twin's flag")
	}
}

// The catalogue row is priced on its own, so the twin's scam suppression must be carried, not just performed. Catalogue rows have
// no issuer (fillIssuerDirectoryTags skips them) and fillCataloguePricesForPage fills price and cap before mergeTwinStats, which
// only fills nils; a flagged currency would otherwise show a price while its classic row and detail serve null.
func TestMergeTwinStats_CarriesTheScamSuppressionOntoTheCatalogueRow(t *testing.T) {
	t.Parallel()

	ownPrice, ownCap, ownFDV := "1.07", "109504500.00", "200000000.00"
	change := "4.2"
	// The catalogue row as fillCataloguePricesForPage leaves it: priced
	// from the global tier, with no issuer and no directory verdict.
	dst := AssetDetail{
		Type:         assetTypeGlobal,
		AssetID:      "some-currency",
		Slug:         "some-currency",
		PriceUSD:     &ownPrice,
		MarketCapUSD: &ownCap,
		FDVUSD:       &ownFDV,
		Change24hPct: &change,
	}
	// The twin, after fillIssuerDirectoryTags stamped it and
	// suppressScamIssuerPricing emptied it.
	twin := AssetDetail{
		IssuerDirectoryTags:   []string{"malicious", "unsafe"},
		IssuerDirectoryDomain: "audrev-stellar.com",
		IssuerDirectoryName:   "AUD Revolution",
		IssuerScamReason:      "wash-inflated volume; issuer impersonates a regulated anchor",
	}

	mergeTwinStats(&dst, twin)

	if dst.PriceUSD != nil {
		t.Errorf("price_usd = %q, want withheld — the issuer carries a scam-class directory tag",
			*dst.PriceUSD)
	}
	if dst.MarketCapUSD != nil {
		t.Errorf("market_cap_usd = %q, want withheld — the same tag nulled it on the classic row "+
			"this catalogue row replaces", *dst.MarketCapUSD)
	}
	if dst.FDVUSD != nil {
		t.Errorf("fdv_usd = %q, want withheld", *dst.FDVUSD)
	}
	if dst.Change24hPct != nil {
		t.Errorf("change_24h_pct = %q, want withheld — it is the price over time", *dst.Change24hPct)
	}
	// The row goes silent WITH its reason, never without it: a null price
	// and no warning reads as "no data" rather than "refused".
	if !slices.Contains(dst.IssuerDirectoryTags, "malicious") {
		t.Errorf("issuer_directory_tags = %v, want the twin's verdict carried across",
			dst.IssuerDirectoryTags)
	}
	if dst.IssuerDirectoryDomain != twin.IssuerDirectoryDomain {
		t.Errorf("issuer_directory_domain = %q, want %q",
			dst.IssuerDirectoryDomain, twin.IssuerDirectoryDomain)
	}
	if dst.IssuerScamReason != twin.IssuerScamReason {
		t.Errorf("issuer_scam_reason = %q, want %q", dst.IssuerScamReason, twin.IssuerScamReason)
	}
}

// An issuer the directory merely labels keeps every figure: suppression is scoped to scam-class tags, and nulling an anchor's
// price would be worse than the bug being fixed.
func TestMergeTwinStats_AnUnflaggedIssuerKeepsItsFigures(t *testing.T) {
	t.Parallel()

	ownPrice, ownCap := "1.00", "5000000.00"
	dst := AssetDetail{
		Type:         assetTypeGlobal,
		Slug:         "some-currency",
		PriceUSD:     &ownPrice,
		MarketCapUSD: &ownCap,
	}
	twin := AssetDetail{
		IssuerDirectoryTags:   []string{"anchor", "issuer"},
		IssuerDirectoryDomain: "circle.com",
		IssuerDirectoryName:   "Circle",
	}

	mergeTwinStats(&dst, twin)

	if dst.PriceUSD == nil || *dst.PriceUSD != ownPrice {
		t.Errorf("price_usd = %v, want %q kept", dst.PriceUSD, ownPrice)
	}
	if dst.MarketCapUSD == nil || *dst.MarketCapUSD != ownCap {
		t.Errorf("market_cap_usd = %v, want %q kept", dst.MarketCapUSD, ownCap)
	}
	if !slices.Contains(dst.IssuerDirectoryTags, "anchor") {
		t.Errorf("issuer_directory_tags = %v, want the twin's labels", dst.IssuerDirectoryTags)
	}
}
