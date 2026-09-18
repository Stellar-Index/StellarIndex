// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

// F017, the ?include=sparkline7d leg. The listing's price_usd comes from
// asset_price_snapshot, which its writer normalises for a confirmed
// non-7-decimals token. The 7d series attached beside it comes from the
// batch price-history reader, which returns RAW prices_1m ratios — and
// was put on the wire verbatim, so a 9-decimals token listed at 2.50 USD
// over a chart that ran along 0.025.

import (
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func sparklineFor(t *testing.T, stub *sparklineStub, cache *v1.NonstandardDecimalsCache, assetID string) []v1.AssetPricePoint {
	t.Helper()
	srv := v1.New(v1.Options{AssetsReader: stub, NonstandardDecimals: cache})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/assets?limit=10&include=sparkline7d")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data []v1.AssetDetail `json:"data"`
	}
	mustDecode(t, resp, &env)
	row := findRowByAssetID(env.Data, assetID)
	if row == nil {
		t.Fatalf("row %s missing from the listing: %+v", assetID, env.Data)
	}
	if row.PriceUSD == nil {
		t.Fatalf("row %s has no price_usd — its chart would be withheld for that reason; fix the fixture", assetID)
	}
	return row.PriceHistory7d
}

// A flagged 9-decimals token's series is corrected by the same 10^2 its
// listing price already carries, point for point, and keeps its grid.
func TestAssetsListing_Sparkline7d_NormalisesNonstandardDecimals(t *testing.T) {
	stub := &sparklineStub{
		stubAssetsReaderExt: &stubAssetsReaderExt{},
		classic: []timescale.AssetRow{
			// Already corrected: the rollup's writer normalises it.
			{AssetID: flaggedAsset, Slug: flaggedAsset, PriceUSD: sptr("2.5000000000")},
		},
		series: map[string][]string{
			// Rounded on the RAW scale (10 places), as a reader that has
			// not widened its rounding returns it.
			flaggedAsset: sparklineSeries("0.0125000000", "0.0250000000"),
		},
	}
	got := sparklineFor(t, stub, nonstandardDecimalsCacheWith(t, flaggedAsset, 9), flaggedAsset)
	if len(got) != len(sparklineDays) {
		t.Fatalf("series has %d buckets, want the %d-day grid", len(got), len(sparklineDays))
	}
	want := []string{"1.2500000000", "2.5000000000"}
	priced := pricedPoints(got)
	if len(priced) != len(want) {
		t.Fatalf("priced points = %v, want %v", priced, want)
	}
	for i := range want {
		if priced[i] != want[i] {
			t.Errorf("price_history_7d[%d] = %s, want %s (the raw ratio is 100x low for a 9-decimals token)",
				i, priced[i], want[i])
		}
	}
}

// An 18-decimals token, factor 10^11. A point rounded on the RAW scale
// has been crushed (14 USD reads as 0.0000000001) and is withheld as a
// gap; the same point rounded to 10+11 places is exact and is served.
// Both keep their bucket.
func TestAssetsListing_Sparkline7d_EighteenDecimals_WideRoundingServedNarrowWithheld(t *testing.T) {
	stub := &sparklineStub{
		stubAssetsReaderExt: &stubAssetsReaderExt{},
		classic: []timescale.AssetRow{
			{AssetID: flaggedAsset, Slug: flaggedAsset, PriceUSD: sptr("14.0000000000")},
		},
		series: map[string][]string{
			flaggedAsset: sparklineSeries("0.0000000001", "0.000000000140000000000"),
		},
	}
	got := sparklineFor(t, stub, nonstandardDecimalsCacheWith(t, flaggedAsset, 18), flaggedAsset)
	if len(got) != len(sparklineDays) {
		t.Fatalf("series has %d buckets, want the %d-day grid", len(got), len(sparklineDays))
	}
	if got[0].P != nil {
		t.Errorf("bucket 0 = %s, want a gap: a ratio rounded to 10 places before an 11-place "+
			"scale-up is not a price (it would read as exactly 10 USD)", *got[0].P)
	}
	if got[1].P == nil {
		t.Error("bucket 1 is a gap, want 14.0000000000")
	} else if *got[1].P != "14.0000000000" {
		t.Errorf("bucket 1 = %s, want 14.0000000000", *got[1].P)
	}
}

// No confirmed row: the series is the reader's strings, byte for byte.
func TestAssetsListing_Sparkline7d_UnflaggedSeriesIsByteIdentical(t *testing.T) {
	series := sparklineSeries("0.0125000000", "0.025", "not-a-number")
	stub := &sparklineStub{
		stubAssetsReaderExt: &stubAssetsReaderExt{},
		classic: []timescale.AssetRow{
			{AssetID: flaggedAsset, Slug: flaggedAsset, PriceUSD: sptr("0.0250000000")},
		},
		series: map[string][]string{flaggedAsset: series},
	}
	// A populated cache that flags a DIFFERENT contract.
	const otherContract = "CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J"
	got := pricedPoints(sparklineFor(t, stub, nonstandardDecimalsCacheWith(t, otherContract, 9), flaggedAsset))
	if len(got) != len(series) {
		t.Fatalf("priced points = %v, want %v", got, series)
	}
	for i := range series {
		if got[i] != series[i] {
			t.Errorf("price_history_7d[%d] = %q, want it untouched: %q", i, got[i], series[i])
		}
	}
}
