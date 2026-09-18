// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The catalogue SQL rounds a RAW ratio to 10 places before the decimals
// normalisation can run, and scaling UP promotes that rounding error by
// the same power of ten. An 18-decimals token (factor 10^11) worth $1 has
// a raw ratio of 1e-11 — the SQL hands back zero — and one worth $14
// comes back as exactly $10. Those must become gaps, not precise-looking
// fiction; a value that kept enough quanta is corrected exactly; and the
// un-rounded ATH is never floored at all.
func TestNormalizeCatalogueUSD_PrecisionFloor(t *testing.T) {
	const sorobanContract = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"
	token, err := canonical.ParseAsset(sorobanContract)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		decimals int
		value    string
		rounded  bool
		want     string
		wantOK   bool
	}{
		{"18dp: SQL rounded the ratio to zero", 18, "0.0000000000", true, "", false},
		{"18dp: one quantum left — $14 would read as $10", 18, "0.0000000001", true, "", false},
		{"18dp: 999 quanta — just under the floor", 18, "0.0000000999", true, "", false},
		{"18dp: 1000 quanta — corrected exactly", 18, "0.0000001000", true, "10000.0000000000", true},
		{"18dp: un-rounded ATH is exact, never floored", 18, "0.00000000001432", false, "1.4320000000", true},
		{"9dp: ordinary value", 9, "41.3200000000", true, "4132.0000000000", true},
		{"6dp: scaling DOWN needs no floor", 6, "0.0000000010", true, "0.0000000001", true},
		{"flagged: unparseable cannot be corrected, so it is withheld", 9, "NaN", true, "", false},
		{"flagged: a negative price is not a price", 9, "-1.5", false, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{nonstandardDecimals: decimalsCacheFlagging(t, map[string]int{sorobanContract: tc.decimals})}
			got, ok := s.normalizeCatalogueUSD(tc.value, token, tc.rounded)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("normalizeCatalogueUSD(%q) = (%q, %v), want (%q, %v)", tc.value, got, ok, tc.want, tc.wantOK)
			}
		})
	}

	// No confirmed row: whatever the reader produced passes through
	// untouched — including text this function could not have parsed.
	s := &Server{nonstandardDecimals: decimalsCacheFlagging(t, map[string]int{})}
	for _, v := range []string{"0.0000000000", "0.0000000001", "41.32", "not-a-number"} {
		if got, ok := s.normalizeCatalogueUSD(v, token, true); !ok || got != v {
			t.Errorf("unflagged %q = (%q, %v), want it byte-identical", v, got, ok)
		}
	}
}

// A withheld point keeps its bucket: the series grid the client draws
// against must not lose a slot because one value could not be corrected.
func TestNormalizedAssetPointsToWire_WithheldPointBecomesGap(t *testing.T) {
	const sorobanContract = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"
	token, err := canonical.ParseAsset(sorobanContract)
	if err != nil {
		t.Fatal(err)
	}
	crushed, fine := "0.0000000001", "0.0000001000"
	s := &Server{nonstandardDecimals: decimalsCacheFlagging(t, map[string]int{sorobanContract: 18})}
	in := []timescale.AssetPricePoint{
		{T: "2026-09-17T00:00:00Z", P: &crushed},
		{T: "2026-09-17T01:00:00Z", P: &fine},
		{T: "2026-09-17T02:00:00Z", P: nil},
	}
	out := s.normalizedAssetPointsToWire(in, token)
	if len(out) != 3 {
		t.Fatalf("got %d points, want 3", len(out))
	}
	if out[0].P != nil {
		t.Errorf("point 0 = %q, want a null gap", *out[0].P)
	}
	if out[1].P == nil || *out[1].P != "10000.0000000000" {
		t.Errorf("point 1 = %v, want 10000.0000000000", out[1].P)
	}
	if out[2].P != nil {
		t.Errorf("point 2 = %q, want the existing gap preserved", *out[2].P)
	}
	if crushed != "0.0000000001" || fine != "0.0000001000" {
		t.Error("the reader's points were mutated in place — they may be a shared cache entry")
	}
}
