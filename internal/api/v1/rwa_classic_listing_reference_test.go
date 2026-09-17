// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The independent listing directory held 33 recognised CLASSIC rows on
// 2026-09-16, every one of them priced, against 17 contract rows — and
// only the contract arm read it. A classic member the directory priced
// was published as `reference_not_bound` beside the contract rows the
// same directory was pricing, which is one source answering one arm of
// a surface and not the other.
func TestClassicRowTakesItsListingPrice(t *testing.T) {
	const id = "WTGX-GDMBNMFJ3TRFLASJ6UGETFME3PJPNKPU24C7KFDBEBPQFG2CI6UC3JG6"
	now := time.Now().UTC()
	listings := map[string]timescale.ListingEntry{
		id: {
			Address:   id,
			ListingID: "wisdomtree-treasury-money-market-digital-fund",
			Symbol:    "wtgxx",
			PriceUSD:  "1.0",
			PricedAt:  now.Add(-time.Hour),
			Source:    "coingecko",
		},
	}

	a := RWAAsset{
		AssetID:           id,
		Code:              "WTGX",
		Issuer:            "GDMBNMFJ3TRFLASJ6UGETFME3PJPNKPU24C7KFDBEBPQFG2CI6UC3JG6",
		CirculatingSupply: supplyOf("22987932026782"),
		Decimals:          7,
	}
	rwaApplyReference(&a, rwaReferenceSnapshotFrom(nil), nil, listings, now)

	if a.Reference == nil {
		t.Fatalf("no reference attached; premium status = %q", a.Premium.Status)
	}
	if a.Reference.Provenance != RWAReferenceListingPrice {
		t.Errorf("provenance = %q, want %q", a.Reference.Provenance, RWAReferenceListingPrice)
	}
	if a.ReferenceValuation.ValueUSD == nil {
		t.Fatalf("reference attached but no valuation computed; status = %q", a.ReferenceValuation.Status)
	}
	// 2,298,793.2027 tokens at $1.00.
	if got := *a.ReferenceValuation.ValueUSD; got != "2298793.20" {
		t.Errorf("reference valuation = %s, want 2298793.20", got)
	}
	// R-C: the premium is refused even though the valuation is published,
	// because a premium against an aggregate of the same markets our own
	// price samples is the market compared with itself.
	if a.Premium.Status != RWAPremiumReferenceNotOracle {
		t.Errorf("premium status = %q, want %q", a.Premium.Status, RWAPremiumReferenceNotOracle)
	}
}

// The key is the asset's own CODE-GISSUER and never the code. This
// network carries twenty-six assets coded BENJI and one of them is
// Franklin Templeton's; a code-keyed lookup would hand the real
// instrument's price to whichever account minted the ticker.
func TestImpersonatorDoesNotInheritAListingPrice(t *testing.T) {
	const genuine = "BENJI-GBHNGLLIE3KWGKCHIKMHJ5HVZHYIK7WTBE4QF5PLAKL4CJGSEU7HZIW5"
	const fake = "BENJI-GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	now := time.Now().UTC()
	listings := map[string]timescale.ListingEntry{
		genuine: {Address: genuine, ListingID: "franklin-templeton-benji", PriceUSD: "1.0", PricedAt: now, Source: "coingecko"},
	}

	a := RWAAsset{
		AssetID:           fake,
		Code:              "BENJI",
		Issuer:            "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		CirculatingSupply: supplyOf("10000000000"),
		Decimals:          7,
	}
	rwaApplyReference(&a, rwaReferenceSnapshotFrom(nil), nil, listings, now)

	if a.Reference != nil {
		t.Fatalf("an impersonator was handed the real instrument's listing price: %+v", a.Reference)
	}
	if a.Premium.Status != RWAPremiumNotBound {
		t.Errorf("premium status = %q, want %q", a.Premium.Status, RWAPremiumNotBound)
	}
}

// The directory publishes each asset under ONE address form with no
// pattern — some by `CODE-GISSUER`, some by the Stellar Asset Contract
// address alone (issue #514). /v1/assets already tries both; this arm
// keyed on the classic id only, so a SAC-listed classic member was
// refused as `reference_not_bound` while the snapshot in hand named
// its address. Both surfaces now resolve through [listingEntryIn].
func TestClassicRowTakesItsListingPriceUnderTheSACForm(t *testing.T) {
	const id = "WTGX-GDMBNMFJ3TRFLASJ6UGETFME3PJPNKPU24C7KFDBEBPQFG2CI6UC3JG6"
	classic, sac := listingAddressesFor(id)
	if classic != id || sac == "" {
		t.Fatalf("listingAddressesFor(%s) = (%q, %q), want the classic id and a derived SAC", id, classic, sac)
	}
	now := time.Now().UTC()
	listings := map[string]timescale.ListingEntry{
		sac: {
			Address:   sac,
			ListingID: "wisdomtree-treasury-money-market-digital-fund",
			Symbol:    "wtgxx",
			PriceUSD:  "1.0",
			PricedAt:  now.Add(-time.Hour),
			Source:    "coingecko",
		},
	}

	a := RWAAsset{
		AssetID:           id,
		Code:              "WTGX",
		Issuer:            "GDMBNMFJ3TRFLASJ6UGETFME3PJPNKPU24C7KFDBEBPQFG2CI6UC3JG6",
		CirculatingSupply: supplyOf("22987932026782"),
		Decimals:          7,
	}
	rwaApplyReference(&a, rwaReferenceSnapshotFrom(nil), nil, listings, now)

	if a.Reference == nil {
		t.Fatalf("SAC-listed classic member got no reference; premium status = %q", a.Premium.Status)
	}
	if a.Reference.Provenance != RWAReferenceListingPrice || a.Reference.Feed != "wisdomtree-treasury-money-market-digital-fund" {
		t.Errorf("reference = %+v, want the listing row published under the SAC", a.Reference)
	}
	if a.ReferenceValuation.ValueUSD == nil || *a.ReferenceValuation.ValueUSD != "2298793.20" {
		t.Errorf("reference valuation = %v, want 2298793.20", a.ReferenceValuation.ValueUSD)
	}

	// Same rule on the CNAV pre-check: a listing price is an observation
	// and the prospectus NAV is a rule, so a SAC-listed CNAV share class
	// takes the listing. Before the fix the classic-only key missed the
	// SAC row and the prospectus figure was published over a live price.
	const gBENJI = "gBENJI-GD5J73EKK5IYL5XS3FBTHHX7CZIYRP7QXDL57XFWGC2WVYWT326OBXRP"
	if _, ok := rwa.ConstantNAV("gBENJI", "GD5J73EKK5IYL5XS3FBTHHX7CZIYRP7QXDL57XFWGC2WVYWT326OBXRP"); !ok {
		t.Fatal("fixture drift: gBENJI is no longer a CNAV-bound pair; pick another")
	}
	_, gSAC := listingAddressesFor(gBENJI)
	cnav := RWAAsset{
		AssetID:           gBENJI,
		Code:              "gBENJI",
		Issuer:            "GD5J73EKK5IYL5XS3FBTHHX7CZIYRP7QXDL57XFWGC2WVYWT326OBXRP",
		CirculatingSupply: supplyOf("10000000000"),
		Decimals:          7,
	}
	rwaApplyReference(&cnav, rwaReferenceSnapshotFrom(nil), nil, map[string]timescale.ListingEntry{
		gSAC: {Address: gSAC, ListingID: "franklin-onchain-us-government-money-fund", PriceUSD: "1.0", PricedAt: now, Source: "coingecko"},
	}, now)
	if cnav.Reference == nil || cnav.Reference.Provenance != RWAReferenceListingPrice {
		t.Errorf("CNAV pair with a SAC-listed price: reference = %+v, want the listing observation over the prospectus rule", cnav.Reference)
	}
}

// A listing price answers "nobody bound this pair to an oracle". It must
// NOT overturn a finding about a feed that IS bound — an expired one, a
// non-USD one, a non-positive net asset value. Publishing a figure while
// suppressing the finding that refused it is the failure this guards.
func TestListingDoesNotOverturnAnOracleFinding(t *testing.T) {
	const id = "USDY-GAJMPX5NBOG6TQFPQGRABJEEB2YE7RFRLUKJDZAZGAD5GFX4J7TADAZ6"
	now := time.Now().UTC()
	listings := map[string]timescale.ListingEntry{
		id: {Address: id, ListingID: "ondo-us-dollar-yield", PriceUSD: "1.15", PricedAt: now, Source: "coingecko"},
	}

	// A bound pair whose feed carries a non-positive value.
	a := RWAAsset{
		AssetID:           id,
		Code:              "USDY",
		Issuer:            "GAJMPX5NBOG6TQFPQGRABJEEB2YE7RFRLUKJDZAZGAD5GFX4J7TADAZ6",
		CirculatingSupply: supplyOf("10000000000"),
		Decimals:          7,
	}
	rwaApplyReference(&a, rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "redstone", "rwa:USDY", "fiat:USD", "0", 8, now),
	}), nil, listings, now)

	if a.Reference != nil && a.Reference.Provenance == RWAReferenceListingPrice {
		t.Fatalf("a listing price overturned an oracle finding; premium = %q", a.Premium.Status)
	}
}

// supplyOf is local to this file: `strPtr` is already taken in this
// package by a helper that goes the other way (*string -> string).
func supplyOf(s string) *string { return &s }
