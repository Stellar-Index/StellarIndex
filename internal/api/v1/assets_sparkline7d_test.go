package v1_test

// #355 — the 7d sparkline column on /assets was blank for exactly the
// eleven assets that matter (XLM, USDC, PYUSD, EURC, AQUA, yXLM, SHX,
// VELO, BLND, PHO, yUSDC) while the unverified long tail below charted
// fine, and `?include=sparkline7d` was a silent no-op on the plain
// listing. Those eleven are precisely the catalogue-projected rows,
// whose wire asset_id is the catalogue SLUG ("xlm", "aqua") — the batch
// series reader was asked for a series under an id that can never match
// a prices_1m row, and answered with its 7-bucket skeleton (all prices
// null), which looks exactly like "this asset never traded".
//
// The mirror-image defect these tests also pin: a row whose price we
// deliberately WITHHOLD (scam-flagged issuer, thin-market substance
// gate) must not publish the same number as a picture. Measured on r1
// 2026-08-29: the flagged JFKBANK2/RIO rows served price_usd null with
// a full 7-point chart, and their details served 24 hourly + 7 daily
// priced points.

import (
	"context"
	"database/sql"
	"net/http"
	"sync"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const (
	// The Stellar twins of the two catalogue entries used below.
	nativeAssetID = "native"
	aquaAssetID   = "AQUA-" + otherRealIssuer
)

// sparklineStub is an AssetsReader that records exactly which asset_ids
// the sparkline attach asked for, and answers the way the real batch
// query does: EVERY requested id gets its 7 daily buckets back, but only
// ids with a market get prices in them (the query's want × days CROSS
// JOIN). That fidelity is the point — a stub that returned nothing for
// an unknown id would hide the defect, which on the wire looks like a
// well-formed but priceless series.
type sparklineStub struct {
	*stubAssetsReaderExt
	classic []timescale.AssetRow
	byID    map[string]timescale.AssetRow
	series  map[string][]string

	mu        sync.Mutex
	requested []string
}

func (s *sparklineStub) ListAssetsExt(_ context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	if opts.Issuer == "" {
		return s.classic, nil
	}
	// lookupCatalogueTwin's exact-issuer filter.
	var out []timescale.AssetRow
	for _, row := range s.byID {
		if row.IssuerGStrkey == opts.Issuer {
			out = append(out, row)
		}
	}
	return out, nil
}

func (s *sparklineStub) GetAssetByAssetID(_ context.Context, assetID string) (timescale.AssetRow, error) {
	row, ok := s.byID[assetID]
	if !ok {
		return timescale.AssetRow{}, sql.ErrNoRows
	}
	return row, nil
}

func (s *sparklineStub) GetNativeAssetRow(ctx context.Context) (timescale.AssetRow, error) {
	return s.GetAssetByAssetID(ctx, nativeAssetID)
}

func (s *sparklineStub) GetAssetsPriceHistory7dBatch(_ context.Context, assetIDs []string) (map[string][]timescale.AssetPricePoint, error) {
	s.mu.Lock()
	s.requested = append(s.requested, assetIDs...)
	s.mu.Unlock()
	out := make(map[string][]timescale.AssetPricePoint, len(assetIDs))
	for _, id := range assetIDs {
		prices := s.series[id]
		pts := make([]timescale.AssetPricePoint, 0, 7)
		for i, day := range sparklineDays {
			var p *string
			if i < len(prices) {
				v := prices[i]
				p = &v
			}
			pts = append(pts, timescale.AssetPricePoint{T: day, P: p})
		}
		out[id] = pts
	}
	return out, nil
}

func (s *sparklineStub) requestedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requested...)
}

// sparklineDays is the bucket skeleton every series comes back with.
var sparklineDays = []string{
	"2026-08-23T00:00:00Z", "2026-08-24T00:00:00Z", "2026-08-25T00:00:00Z",
	"2026-08-26T00:00:00Z", "2026-08-27T00:00:00Z", "2026-08-28T00:00:00Z",
	"2026-08-29T00:00:00Z",
}

func sparklineSeries(prices ...string) []string { return prices }

// findRowBySlug / findRowByAssetID locate a listing row for assertions.
func findRowBySlug(rows []v1.AssetDetail, slug string) *v1.AssetDetail {
	for i := range rows {
		if rows[i].Slug == slug {
			return &rows[i]
		}
	}
	return nil
}

func findRowByAssetID(rows []v1.AssetDetail, assetID string) *v1.AssetDetail {
	for i := range rows {
		if rows[i].AssetID == assetID {
			return &rows[i]
		}
	}
	return nil
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// pricedPoints returns the non-null prices of a wire series.
func pricedPoints(pts []v1.AssetPricePoint) []string {
	out := make([]string, 0, len(pts))
	for _, pt := range pts {
		if pt.P != nil {
			out = append(out, *pt.P)
		}
	}
	return out
}

// TestAssetsListing_Sparkline7d_CatalogueRowsKeyOnStellarTwin — #355's
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

// TestAssetDetail_WithheldPrice_ServesNoPriceHistory — same invariant on
// the asset DETAIL payload, where the leak was widest: r1 served
// price_usd null beside 24 hourly and 7 daily priced points for every
// scam-flagged asset.
func TestAssetDetail_WithheldPrice_ServesNoPriceHistory(t *testing.T) {
	const scamID = "JFKBANK2-" + scamAUDIssuer
	hist24 := []timescale.AssetPricePoint{
		{T: "2026-08-29T10:00:00Z", P: sptr("0.42")},
		{T: "2026-08-29T11:00:00Z", P: sptr("0.43")},
	}
	hist7d := []timescale.AssetPricePoint{
		{T: "2026-08-28T00:00:00Z", P: sptr("0.44")},
		{T: "2026-08-29T00:00:00Z", P: sptr("0.42")},
	}
	issuer := scamAUDIssuer
	assetsReader := &stubAssetsReaderExt{
		row: timescale.AssetRow{
			Slug: "jfkbank2", AssetID: scamID, Code: "JFKBANK2",
			IssuerGStrkey: scamAUDIssuer, PriceUSD: sptr("0.42"),
		},
		hist24: hist24,
		hist7d: hist7d,
	}
	srv := v1.New(v1.Options{
		Assets: &stubAssetReader{byID: map[string]v1.AssetDetail{
			scamID: {AssetID: scamID, Type: "classic", Code: "JFKBANK2", Issuer: &issuer},
		}},
		AssetsReader: assetsReader,
		Directory:    scamAUDDirectoryStub(),
	})
	ts := httpTestServer(t, srv)

	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	mustDecode(t, mustGet(t, ts.URL+"/v1/assets/"+scamID), &env)

	if env.Data.PriceUSD != nil {
		t.Fatalf("fixture broken — price_usd = %q, want null (scam-flagged issuer)", *env.Data.PriceUSD)
	}
	if len(env.Data.PriceHistory24h) != 0 {
		t.Errorf("price_history_24h = %+v, want none — the last bucket IS the withheld price", env.Data.PriceHistory24h)
	}
	if len(env.Data.PriceHistory7d) != 0 {
		t.Errorf("price_history_7d = %+v, want none — the last bucket IS the withheld price", env.Data.PriceHistory7d)
	}
}
