// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// rwaSummaryReachableProvenances is every provenance a row in
// view.Assets can carry into RWAReferenceSummary. The curator provenance
// is absent on purpose: curated rows are served in CuratedAssets and
// never enter the verified total.
var rwaSummaryReachableProvenances = []string{
	RWAReferenceOracleNAV,
	RWAReferenceListingPrice,
	RWAReferenceProspectusCNAV,
}

// A prospectus-CNAV row contributes to the reference total, so the
// summary's basis has to name that claim, not stay silent about it or
// describe the total by the other kinds only.
func TestRWASummaryProvenance_NamesProspectusCNAV(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	supply := "566742721191613"
	a := RWAAsset{
		AssetID: "gBENJI-GD5J73EKK5IYL5XS3FBTHHX7CZIYRP7QXDL57XFWGC2WVYWT326OBXRP", Code: "gBENJI",
		Issuer: "GD5J73EKK5IYL5XS3FBTHHX7CZIYRP7QXDL57XFWGC2WVYWT326OBXRP", CirculatingSupply: &supply, Decimals: intPtr(7),
	}
	snap := rwaReferences{available: true, byFeed: map[string]rwaReference{}, nonUSD: map[string]string{}}
	rwaApplyReference(&a, snap, nil, map[string]timescale.ListingEntry{}, now)
	if a.Reference == nil || a.Reference.Provenance != RWAReferenceProspectusCNAV || a.ReferenceValuation.ValueUSD == nil {
		t.Fatalf("fixture did not produce a valued CNAV row: ref=%+v val=%+v", a.Reference, a.ReferenceValuation)
	}

	s := rwaSummariseReference([]RWAAsset{a})
	if strings.Join(s.Provenances, ",") != RWAReferenceProspectusCNAV {
		t.Fatalf("provenances = %v, want [%s]", s.Provenances, RWAReferenceProspectusCNAV)
	}
	if !strings.Contains(s.Basis, "`provenance: prospectus_constant_nav`") {
		t.Errorf("CNAV-only basis does not describe the prospectus NAV claim:\n%s", s.Basis)
	}
	if strings.Contains(s.Basis, "`provenance: oracle_instrument_nav`") || strings.Contains(s.Basis, "`provenance: listing_platform_price`") {
		t.Errorf("CNAV-only basis describes a provenance the total does not contain:\n%s", s.Basis)
	}
}

// Every reachable provenance has its own prose, and any mixture names
// each member and says it is a mixture.
func TestRWASummaryProvenance_ProseDescribesEveryReachableProvenance(t *testing.T) {
	for _, p := range rwaSummaryReachableProvenances {
		got := rwaReferenceProvenanceProse([]string{p})
		if !strings.Contains(got, "`provenance: "+p+"`") {
			t.Errorf("prose for [%s] does not name it: %q", p, got)
		}
		if strings.Contains(got, "MIXES") {
			t.Errorf("single-provenance prose for [%s] claims a mixture: %q", p, got)
		}
	}
	mixed := rwaReferenceProvenanceProse([]string{RWAReferenceOracleNAV, RWAReferenceProspectusCNAV})
	for _, p := range []string{RWAReferenceOracleNAV, RWAReferenceProspectusCNAV} {
		if !strings.Contains(mixed, "`provenance: "+p+"`") {
			t.Errorf("oracle+CNAV prose omits %s: %q", p, mixed)
		}
	}
	if !strings.HasPrefix(mixed, "The total MIXES TWO KINDS OF CLAIM") {
		t.Errorf("oracle+CNAV prose does not state the mixture: %q", mixed)
	}
	all := rwaReferenceProvenanceProse(rwaSummaryReachableProvenances)
	if !strings.HasPrefix(all, "The total MIXES THREE KINDS OF CLAIM") {
		t.Errorf("three-provenance prose does not state the mixture: %q", all)
	}
}

// The spec's RWAReferenceSummary.provenances enum is exactly the set a
// contributing row can carry.
func TestRWASummaryProvenance_SpecEnumIsReachableSet(t *testing.T) {
	schemas, _ := loadSpecDoc(t)["components"].(map[string]any)["schemas"].(map[string]any)
	summary, _ := schemas["RWAReferenceSummary"].(map[string]any)
	props, _ := summary["properties"].(map[string]any)
	prov, _ := props["provenances"].(map[string]any)
	items, _ := prov["items"].(map[string]any)
	raw, _ := items["enum"].([]any)
	got := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		got = append(got, s)
	}
	want := append([]string(nil), rwaSummaryReachableProvenances...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("openapi RWAReferenceSummary.provenances enum = %v, want %v "+
			"(edit the spec and regenerate docs/reference/api, examples/postman and web/explorer/src/api/types.ts)", got, want)
	}
}
