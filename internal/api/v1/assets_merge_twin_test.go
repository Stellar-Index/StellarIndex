package v1

import "testing"

// A catalogue row that inherits its classic twin's circulating supply
// must inherit the twin's SCALE with it.
//
// `circulating_supply` is documented and consumed as a raw integer in
// the asset's smallest unit, paired with `decimals`. A catalogue row's
// own Decimals is vc.SupplyDecimals, which is 0 for every
// Stellar-issued entry (the curated seed states fiat M2 in whole
// units). Copying a 7-decimal stroop value onto a decimals=0 row made
// the pair self-inconsistent, so every consumer scaling by 10^decimals
// rendered it 10^7 too large — measured live on r1 2026-08-04: XLM's
// /v1/assets row served decimals=0 with 342797138733487851, which the
// explorer displayed as 342,797,138,733,487,872 against the ~34.3B its
// own market_cap_usd/price_usd implies.
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

// TestMergeTwinStats_CarriesTheListingValuation — the listing-priced arm
// runs on the TWIN row (the one carrying the asset_id the directory's
// addresses derive from, the price, and the gate outcome), so the
// catalogue row it stands in for only sees the result if the merge
// carries it.
//
// This is the same failure the dust flag had: a value computed on the
// twin, correct on the classic row and the detail page, and absent from
// the catalogue-listing row the /rwa stablecoin tile actually sums.
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

// TestMergeTwinStats_CarriesARefusalToo — a status with no figure has to
// cross as well. A catalogue row silently missing the block reads as
// "this arm does not apply here", which is a different statement from
// "it applies and the directory could not be read".
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
