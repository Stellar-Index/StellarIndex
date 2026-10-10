// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

// The ?include=sparkline7d leg. The listing's price_usd comes from
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

// TestAssetsListing_Sparkline7d_CatalogueRowsKeyOnStellarTwin — the
// core: a catalogue row's series must be read under its Stellar twin's
// asset_id (the id its price and its change_7d_pct already come from),
// never under the catalogue slug the row carries on the wire.
func TestAssetsListing_Sparkline7d_CatalogueRowsKeyOnStellarTwin(t *testing.T) {
	stub := &sparklineStub{
		stubAssetsReaderExt: &stubAssetsReaderExt{},
		byID: map[string]timescale.AssetRow{
			nativeAssetID: {AssetID: nativeAssetID, Code: "XLM", Slug: "xlm", PriceUSD: sptr("0.1790411226")},
			aquaAssetID: {
				AssetID: aquaAssetID, Code: "AQUA", Slug: "aqua",
				IssuerGStrkey: otherRealIssuer, PriceUSD: sptr("0.0003433943"),
			},
		},
		series: map[string][]string{
			nativeAssetID: sparklineSeries("0.1980", "0.1930", "0.1900", "0.1870", "0.1850", "0.1810", "0.1790"),
			aquaAssetID:   sparklineSeries("0.00037", "0.00037", "0.00036", "0.00036", "0.00035", "0.00035", "0.00034"),
		},
	}
	srv := v1.New(v1.Options{AssetsReader: stub, VerifiedCurrencies: newTestCatalogue(t)})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets?asset_class=all&limit=11&include=sparkline7d")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data []v1.AssetDetail `json:"data"`
	}
	mustDecode(t, resp, &env)

	ids := stub.requestedIDs()
	for _, slug := range []string{"xlm", "aqua"} {
		if containsID(ids, slug) {
			t.Errorf("series requested under the catalogue SLUG %q — slugs never match a prices_1m row; "+
				"the row's Stellar twin asset_id is what its price and change_7d_pct key on. requested=%v", slug, ids)
		}
	}
	for _, want := range []string{nativeAssetID, aquaAssetID} {
		if !containsID(ids, want) {
			t.Errorf("series never requested for %q; requested=%v", want, ids)
		}
	}

	for _, tc := range []struct {
		slug string
		want []string
	}{
		{"xlm", stub.series[nativeAssetID]},
		{"aqua", stub.series[aquaAssetID]},
	} {
		row := findRowBySlug(env.Data, tc.slug)
		if row == nil {
			t.Fatalf("%s row missing from the listing page", tc.slug)
		}
		if row.PriceUSD == nil {
			t.Fatalf("%s has no price_usd — the honesty gate would legitimately withhold its chart; fix the fixture", tc.slug)
		}
		got := pricedPoints(row.PriceHistory7d)
		if len(got) != len(tc.want) {
			t.Fatalf("%s price_history_7d has %d priced points, want %d (%v) — a priced row must never render an empty chart",
				tc.slug, len(got), len(tc.want), row.PriceHistory7d)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s price_history_7d[%d] = %q, want %q", tc.slug, i, got[i], tc.want[i])
			}
		}
	}
}

// TestAssetsListing_Sparkline7d_DefaultListingHonoursInclude — the
// listing served WITHOUT asset_class (the /v1/assets shape the issue
// reproduced with, and the one every SDK consumer gets) ignored
// `include=sparkline7d` entirely: the response was byte-identical with
// and without it.
func TestAssetsListing_Sparkline7d_DefaultListingHonoursInclude(t *testing.T) {
	const xrp = "XRP-" + otherRealIssuer
	stub := &sparklineStub{
		stubAssetsReaderExt: &stubAssetsReaderExt{},
		classic: []timescale.AssetRow{
			{AssetID: xrp, Code: "XRP", Slug: "xrp", IssuerGStrkey: otherRealIssuer, PriceUSD: sptr("1.39")},
		},
		series: map[string][]string{
			xrp: sparklineSeries("1.51", "1.48", "1.46", "1.44", "1.42", "1.40", "1.39"),
		},
	}
	srv := v1.New(v1.Options{AssetsReader: stub})
	ts := httpTestServer(t, srv)

	var env struct {
		Data []v1.AssetDetail `json:"data"`
	}
	mustDecode(t, mustGet(t, ts.URL+"/v1/assets?limit=10&include=sparkline7d"), &env)
	row := findRowByAssetID(env.Data, xrp)
	if row == nil {
		t.Fatalf("XRP row missing: %+v", env.Data)
	}
	got := pricedPoints(row.PriceHistory7d)
	want := stub.series[xrp]
	if len(got) != len(want) {
		t.Fatalf("price_history_7d has %d priced points, want %d — ?include=sparkline7d must be honoured on the default listing (%+v)",
			len(got), len(want), row.PriceHistory7d)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("price_history_7d[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// …and stays opt-in: no include, no series, no batch read.
	var plain struct {
		Data []v1.AssetDetail `json:"data"`
	}
	mustDecode(t, mustGet(t, ts.URL+"/v1/assets?limit=10"), &plain)
	if row := findRowByAssetID(plain.Data, xrp); row == nil || len(row.PriceHistory7d) != 0 {
		t.Errorf("price_history_7d served without ?include=sparkline7d: %+v", plain.Data)
	}
}

// TestAssetsListing_Sparkline7d_WithheldPriceRendersNoChart — the
// honesty rule in the other direction. A row whose price we withhold
// (scam-flagged issuer) or that has no price at all must render NO
// chart, and must not even be looked up: the last point of the series
// IS the number we refused to publish.
func TestAssetsListing_Sparkline7d_WithheldPriceRendersNoChart(t *testing.T) {
	const (
		scamID  = "JFKBANK2-" + scamAUDIssuer
		plainID = "MJQ-" + otherRealIssuer
		okID    = "XRP-" + testUSDCIssuer
	)
	stub := &sparklineStub{
		stubAssetsReaderExt: &stubAssetsReaderExt{},
		classic: []timescale.AssetRow{
			// Priced by the listing query, then withheld by the scam gate.
			{AssetID: scamID, Code: "JFKBANK2", Slug: "jfkbank2", IssuerGStrkey: scamAUDIssuer, PriceUSD: sptr("0.42")},
			// Never priced (thin market / no USD leg).
			{AssetID: plainID, Code: "MJQ", Slug: "mjq", IssuerGStrkey: otherRealIssuer},
			// Control: a published price keeps its chart.
			{AssetID: okID, Code: "XRP", Slug: "xrp", IssuerGStrkey: testUSDCIssuer, PriceUSD: sptr("1.39")},
		},
		series: map[string][]string{
			scamID:  sparklineSeries("0.51", "0.48", "0.46", "0.44", "0.42", "0.43", "0.42"),
			plainID: sparklineSeries("9.10", "9.20", "9.30", "9.40", "9.50", "9.60", "9.70"),
			okID:    sparklineSeries("1.51", "1.48", "1.46", "1.44", "1.42", "1.40", "1.39"),
		},
	}
	srv := v1.New(v1.Options{
		AssetsReader:       stub,
		VerifiedCurrencies: newTestCatalogue(t),
		Directory:          scamAUDDirectoryStub(),
	})
	ts := httpTestServer(t, srv)

	// The classic phase of the unified listing (the shape the explorer's
	// /assets directory renders).
	var env struct {
		Data []v1.AssetDetail `json:"data"`
	}
	mustDecode(t, mustGet(t, ts.URL+"/v1/assets?asset_class=all&cursor=classic:&limit=10&include=sparkline7d"), &env)

	for _, tc := range []struct {
		name    string
		assetID string
	}{
		{"scam-flagged issuer (price withheld)", scamID},
		{"no published price", plainID},
	} {
		row := findRowByAssetID(env.Data, tc.assetID)
		if row == nil {
			t.Fatalf("%s: row %s missing from listing", tc.name, tc.assetID)
		}
		if row.PriceUSD != nil {
			t.Fatalf("%s: fixture broken — price_usd = %q, want null", tc.name, *row.PriceUSD)
		}
		if len(row.PriceHistory7d) != 0 {
			t.Errorf("%s: price_history_7d = %+v, want none — a withheld price must not be republished as a picture of itself",
				tc.name, row.PriceHistory7d)
		}
		if containsID(stub.requestedIDs(), tc.assetID) {
			t.Errorf("%s: series requested for %s; an unpriced row must not even be looked up", tc.name, tc.assetID)
		}
	}

	row := findRowByAssetID(env.Data, okID)
	if row == nil {
		t.Fatalf("control row %s missing", okID)
	}
	if got := pricedPoints(row.PriceHistory7d); len(got) != 7 {
		t.Errorf("control row price_history_7d has %d priced points, want 7 (%+v) — gating must not cost a priced row its chart",
			len(got), row.PriceHistory7d)
	}
}
