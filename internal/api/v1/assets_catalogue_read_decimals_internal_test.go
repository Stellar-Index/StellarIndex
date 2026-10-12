// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// normalizeCatalogueReadUSD decides from the value whether the SQL rounded before or after the decimals correction: ROUND(x,
// n)::text has exactly n fraction places, and n >= 10+k for a 10^k scale-up means the corrected price was rounded to 10. A
// shorter string was rounded on the raw scale and keeps the fail-closed precision floor.
func TestNormalizeCatalogueReadUSD_RoundingScaleDecidesTheFloor(t *testing.T) {
	const sorobanContract = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"
	token, err := canonical.ParseAsset(sorobanContract)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		decimals int
		value    string
		want     string
		wantOK   bool
	}{
		{"18dp, 21 places: 14 USD survives exactly", 18, "0.000000000140000000000", "14.0000000000", true},
		{"18dp, 21 places: 1 USD is no longer zero", 18, "0.000000000010000000000", "1.0000000000", true},
		{"18dp, 21 places: sub-cent price, no floor needed", 18, "0.000000000000012340000", "0.0012340000", true},
		{"18dp, 10 places: crushed on the raw scale, withheld", 18, "0.0000000001", "", false},
		{"18dp, 20 places: one short of 10+11, floor still applies", 18, "0.00000000014000000000", "", false},
		{"9dp, 12 places", 9, "0.025000000000", "2.5000000000", true},
		{"9dp, 10 places: enough quanta, corrected", 9, "0.0250000000", "2.5000000000", true},
		{"9dp, 10 places: too few quanta, withheld", 9, "0.0000000999", "", false},
		{"5dp scales DOWN: never floored", 5, "200.0000000000", "2.0000000000", true},
		{"flagged and unparseable: withheld", 9, "NaN", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{Options: Options{NonstandardDecimals: decimalsCacheFlagging(t, map[string]int{sorobanContract: tc.decimals})}}
			got, ok := s.normalizeCatalogueReadUSD(tc.value, token)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("normalizeCatalogueReadUSD(%q) = (%q, %v), want (%q, %v)", tc.value, got, ok, tc.want, tc.wantOK)
			}
			// The by-id form is the same function behind a parse.
			gotID, okID := s.normalizeCatalogueReadUSDByID(tc.value, sorobanContract)
			if okID != ok || gotID != got {
				t.Errorf("ByID(%q) = (%q, %v), diverges from (%q, %v)", tc.value, gotID, okID, got, ok)
			}
		})
	}

	// No confirmed row, or no cache at all: byte-identical, unparsed.
	for _, s := range []*Server{
		{Options: Options{NonstandardDecimals: decimalsCacheFlagging(t, map[string]int{})}},
		{},
	} {
		for _, v := range []string{"0.0000000001", "41.32", "not-a-number"} {
			if got, ok := s.normalizeCatalogueReadUSDByID(v, sorobanContract); !ok || got != v {
				t.Errorf("unflagged %q = (%q, %v), want it byte-identical", v, got, ok)
			}
		}
	}
}

// An id that does not parse cannot be corrected. Unflagged — every real
// case — it passes through; on record, it is withheld rather than raw.
func TestNormalizeCatalogueReadUSDByID_UnparseableID(t *testing.T) {
	const bogus = "not-an-asset-id"
	s := &Server{Options: Options{NonstandardDecimals: decimalsCacheFlagging(t, map[string]int{})}}
	if got, ok := s.normalizeCatalogueReadUSDByID("1.5", bogus); !ok || got != "1.5" {
		t.Errorf("unflagged unparseable id = (%q, %v), want (\"1.5\", true)", got, ok)
	}
	s = &Server{Options: Options{NonstandardDecimals: decimalsCacheFlagging(t, map[string]int{bogus: 9})}}
	if got, ok := s.normalizeCatalogueReadUSDByID("1.5", bogus); ok {
		t.Errorf("flagged unparseable id = (%q, true), want it withheld", got)
	}
}

// onChainPriceReader serves one per-asset row; every other AssetsReader
// method is unreachable from onChainListingPriceUSD.
type onChainPriceReader struct {
	AssetsReader
	row timescale.AssetRow
}

func (r onChainPriceReader) GetAssetByAssetID(context.Context, string) (timescale.AssetRow, error) {
	return r.row, nil
}

// onChainListingPriceUSD feeds the global asset view's on-chain price
// fallback from the PER-ASSET reader, whose price is a raw prices_1m
// ratio. It must not hand that string straight back.
func TestOnChainListingPriceUSD_NormalisesNonstandardDecimals(t *testing.T) {
	const sorobanContract = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"
	raw := "0.0250000000"
	reader := onChainPriceReader{row: timescale.AssetRow{AssetID: sorobanContract, PriceUSD: &raw}}

	s := &Server{Options: Options{AssetsReader: reader, NonstandardDecimals: decimalsCacheFlagging(t, map[string]int{sorobanContract: 9})}}
	got := s.onChainListingPriceUSD(context.Background(), sorobanContract)
	if got == nil {
		t.Fatal("flagged 9dp price = nil, want 2.5000000000")
	}
	if *got != "2.5000000000" {
		t.Errorf("flagged 9dp price = %s, want 2.5000000000", *got)
	}

	// 18dp and rounded on the raw scale: unpriced, not 0.0000000001.
	crushed := "0.0000000001"
	s = &Server{
		Options: Options{AssetsReader: onChainPriceReader{row: timescale.AssetRow{AssetID: sorobanContract, PriceUSD: &crushed}}, NonstandardDecimals: decimalsCacheFlagging(t, map[string]int{sorobanContract: 18})},
	}
	if got := s.onChainListingPriceUSD(context.Background(), sorobanContract); got != nil {
		t.Errorf("crushed 18dp price = %s, want nil", *got)
	}

	// Unflagged: the reader's own pointer target, byte for byte.
	s = &Server{Options: Options{AssetsReader: reader, NonstandardDecimals: decimalsCacheFlagging(t, map[string]int{})}}
	got = s.onChainListingPriceUSD(context.Background(), sorobanContract)
	if got == nil {
		t.Fatalf("unflagged price = nil, want %s", raw)
	}
	if *got != raw {
		t.Errorf("unflagged price = %s, want %s", *got, raw)
	}
	if raw != "0.0250000000" {
		t.Error("the reader's row was mutated — it may be a shared cache entry")
	}
}

// The catalogue SQL rounds a raw ratio to 10 places before decimals normalisation, and scaling up amplifies that error: an
// 18-decimals token at $1 returns zero and at $14 exactly $10. Those must become gaps, not fiction; values that kept enough
// quanta are corrected exactly; the un-rounded ATH is never floored.
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
			s := &Server{Options: Options{NonstandardDecimals: decimalsCacheFlagging(t, map[string]int{sorobanContract: tc.decimals})}}
			got, ok := s.normalizeCatalogueUSD(tc.value, token, tc.rounded)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("normalizeCatalogueUSD(%q) = (%q, %v), want (%q, %v)", tc.value, got, ok, tc.want, tc.wantOK)
			}
		})
	}

	// No confirmed row: whatever the reader produced passes through
	// untouched — including text this function could not have parsed.
	s := &Server{Options: Options{NonstandardDecimals: decimalsCacheFlagging(t, map[string]int{})}}
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
	s := &Server{Options: Options{NonstandardDecimals: decimalsCacheFlagging(t, map[string]int{sorobanContract: 18})}}
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
