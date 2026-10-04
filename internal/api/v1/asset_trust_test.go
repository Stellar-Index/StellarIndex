package v1

import (
	"encoding/json"
	"testing"
)

func trustFactorByID(t *testing.T, tr *AssetTrust, id string) TrustFactor {
	t.Helper()
	for _, f := range tr.Factors {
		if f.ID == id {
			return f
		}
	}
	t.Fatalf("factor %q missing", id)
	return TrustFactor{}
}

func TestBuildAssetTrust_NoEvidenceIsUnknownNotZero(t *testing.T) {
	tr := buildAssetTrust(&AssetDetail{Sep1Status: "not_fetched"}, false)
	if tr.Score != nil || tr.Band != trustBandUnknown || tr.CoveragePct != 0 {
		t.Fatalf("score=%v band=%s cov=%d; want null/unknown/0", tr.Score, tr.Band, tr.CoveragePct)
	}
	for _, f := range tr.Factors {
		if f.Points != nil || f.Band != trustBandUnknown || f.Source == "" {
			t.Errorf("factor %s: points=%v band=%s source=%q", f.ID, f.Points, f.Band, f.Source)
		}
	}
	b, _ := json.Marshal(tr)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if v, ok := m["score"]; !ok || v != nil {
		t.Errorf("score must serialise as explicit null, got %v (present=%v)", v, ok)
	}
}

func TestBuildAssetTrust_UnknownFactorsDoNotDragScore(t *testing.T) {
	d := &AssetDetail{Sep1Status: "verified"}
	tr := buildAssetTrust(d, false)
	if tr.Score == nil || *tr.Score != 100 || tr.Band != trustBandHigh {
		t.Fatalf("score=%v band=%s; want 100/high", tr.Score, tr.Band)
	}
	if tr.CoveragePct != 25 {
		t.Errorf("coverage = %d, want 25", tr.CoveragePct)
	}
	if tr.FormulaVersion != trustFormulaVersion {
		t.Errorf("formula version = %d", tr.FormulaVersion)
	}
}

func TestBuildAssetTrust_ScamFlagIsDecisive(t *testing.T) {
	d := &AssetDetail{
		Sep1Status:          "verified",
		IssuerDirectoryTags: []string{"malicious"},
	}
	tr := buildAssetTrust(d, false)
	if tr.Score == nil || *tr.Score != 0 || tr.Band != trustBandLow {
		t.Fatalf("score=%v band=%s; want 0/low", tr.Score, tr.Band)
	}
}

func TestBuildAssetTrust_Mixed(t *testing.T) {
	price := "1.00"
	basis := "current"
	d := &AssetDetail{
		Sep1Status:            "no_match",
		PriceUSD:              &price,
		SupplyBasis:           &basis,
		IssuerDirectoryDomain: "example.com",
	}
	tr := buildAssetTrust(d, false)
	// (35*90 + 25*40 + 15*100 + 15*100) / 90 = 79
	if tr.Score == nil || *tr.Score != 79 || tr.Band != trustBandHigh {
		t.Fatalf("score=%v band=%s; want 79/high", tr.Score, tr.Band)
	}
	if f := trustFactorByID(t, tr, trustFactorSep1); f.Band != trustBandMedium {
		t.Errorf("sep1 band = %s", f.Band)
	}
	if tr.CoveragePct != 90 {
		t.Errorf("coverage = %d, want 90", tr.CoveragePct)
	}
}

func TestBuildAssetTrust_TickerCollisionAndThinMarket(t *testing.T) {
	d := &AssetDetail{PriceWithheldReason: PriceWithheldSubstance}
	tr := buildAssetTrust(d, true)
	if f := trustFactorByID(t, tr, trustFactorTickerCollision); f.Points == nil || *f.Points != 0 {
		t.Errorf("collision factor = %+v", f)
	}
	if f := trustFactorByID(t, tr, trustFactorMarketSubstance); f.Band != trustBandLow {
		t.Errorf("market factor = %+v", f)
	}
	if tr.Band != trustBandLow {
		t.Errorf("band = %s, want low", tr.Band)
	}
}

func TestBuildAssetTrust_TickerCollisionNeverBandsHigh(t *testing.T) {
	price := "1.00"
	basis := "current"
	d := &AssetDetail{PriceUSD: &price, SupplyBasis: &basis, Sep1Status: "not_fetched"}
	tr := buildAssetTrust(d, true)
	if tr.Score == nil || *tr.Score >= trustMediumMin || tr.Band != trustBandLow {
		t.Fatalf("score=%v band=%s; want <%d/low", tr.Score, tr.Band, trustMediumMin)
	}
}

func TestBuildAssetTrust_DirectoryLookupFailureIsUnknown(t *testing.T) {
	d := &AssetDetail{issuerDirectoryUnchecked: true}
	tr := buildAssetTrust(d, false)
	if f := trustFactorByID(t, tr, trustFactorIssuerReputation); f.Points != nil || f.Observed != "lookup_failed" {
		t.Errorf("factor = %+v", f)
	}
}
