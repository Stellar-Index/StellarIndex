package v1

import "testing"

func identityFixture() AssetDetail {
	s := func(v string) *string { return &v }
	return AssetDetail{
		OrgName: s("Brand Inc"), Name: s("USD Coin"), Description: s("d"), Image: s("https://brand.example/logo.png"),
		AnchorAsset: s("USD"), AnchorAssetType: s("fiat"), Conditions: s("c"), HomeDomain: s("brand.example"),
	}
}

func TestSuppressFlaggedIssuerIdentity_ClearsIssuerWrittenFields(t *testing.T) {
	for name, flag := range map[string]func(*AssetDetail){
		"curated":   func(d *AssetDetail) { d.IssuerScamReason = "Scam Assets" },
		"directory": func(d *AssetDetail) { d.IssuerDirectoryTags = []string{"malicious"} },
	} {
		d := identityFixture()
		d.Sep1Status = "no_match"
		flag(&d)
		suppressFlaggedIssuerIdentity(&d)
		if d.OrgName != nil || d.Name != nil || d.Description != nil || d.Image != nil ||
			d.AnchorAsset != nil || d.AnchorAssetType != nil || d.Conditions != nil || d.HomeDomain != nil {
			t.Errorf("%s flag: issuer-written identity still served: %+v", name, d)
		}
	}
}

func TestSuppressFlaggedIssuerIdentity_KeepsABoundDomainAndUnflaggedRows(t *testing.T) {
	d := identityFixture()
	d.Sep1Status = "verified"
	d.IssuerScamReason = "Scam Assets"
	suppressFlaggedIssuerIdentity(&d)
	if d.HomeDomain == nil || d.OrgName != nil || d.Image != nil {
		t.Errorf("flagged issuer with a bound domain: want domain kept, name and logo cleared: %+v", d)
	}

	clean := identityFixture()
	suppressFlaggedIssuerIdentity(&clean)
	if clean.OrgName == nil || clean.Image == nil || clean.HomeDomain == nil {
		t.Errorf("unflagged row lost its identity: %+v", clean)
	}

	catalogue := identityFixture()
	catalogue.Type = assetTypeGlobal
	catalogue.IssuerScamReason = "Scam Assets"
	suppressFlaggedIssuerIdentity(&catalogue)
	if catalogue.Name == nil {
		t.Error("catalogue row lost its curated name")
	}
}

func TestMergeTwinStats_WithheldTwinBlocksTheSeries(t *testing.T) {
	price := "1.0000"
	for name, twin := range map[string]AssetDetail{
		"withheld": {PriceWithheldReason: PriceWithheldSubstance},
		"thin":     {PriceUSD: &price, ThinMarket: true},
	} {
		row := AssetDetail{Type: assetTypeGlobal, PriceUSD: &price}
		mergeTwinStats(&row, twin)
		if priceSeriesPublishable(&row) {
			t.Errorf("%s twin: the catalogue row still publishes the twin's raw series", name)
		}
	}
	row := AssetDetail{Type: assetTypeGlobal, PriceUSD: &price}
	mergeTwinStats(&row, AssetDetail{PriceUSD: &price})
	if !priceSeriesPublishable(&row) {
		t.Error("a twin with a served price lost its series")
	}
}

func TestFreshnessStatusFor_MoreActiveThanTotalIsUnknown(t *testing.T) {
	active, total := 5, 4
	if got := freshnessStatusFor(StatusFreshness{ActiveSources: &active, TotalSources: &total}, nil); got != freshnessStatusUnknown {
		t.Errorf("active 5 of 4 = %q, want unknown", got)
	}
}
