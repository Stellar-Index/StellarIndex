package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const testUSDCIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// stubAssetReader implements v1.AssetReader in-memory. Each test
// instantiates one with the fixture data it needs.
type stubAssetReader struct {
	byID    map[string]v1.AssetDetail
	page    []v1.AssetDetail
	nextCur string
	err     error // non-nil → both methods return this; for the 500-path tests
}

func (r *stubAssetReader) GetAsset(_ context.Context, a canonical.Asset) (v1.AssetDetail, error) {
	if r.err != nil {
		return v1.AssetDetail{}, r.err
	}
	d, ok := r.byID[a.String()]
	if !ok {
		return v1.AssetDetail{}, v1.ErrAssetNotFound
	}
	return d, nil
}

func (r *stubAssetReader) ListAssets(_ context.Context, cursor string, limit int) ([]v1.AssetDetail, string, error) {
	if r.err != nil {
		return nil, "", r.err
	}
	return r.page, r.nextCur, nil
}

// ─── /v1/assets (list) ────────────────────────────────────────────

// A nil reader (not wired) and a reader returning a nil slice must both put
// "data":[] on the wire; OpenAPI's AssetListEnvelope.data is `type: array`
// and rejects null. Asserted on raw bytes because decoding hides null.
func TestAssetList_EmptyDataMarshalsAsEmptyArray(t *testing.T) {
	cases := []struct {
		name string
		opts v1.Options
	}{
		{"reader nil", v1.Options{}},
		{"reader returns nil slice", v1.Options{Assets: &stubAssetReader{page: nil, nextCur: ""}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httpTestServer(t, v1.New(tc.opts))
			resp := mustGet(t, ts.URL+"/v1/assets")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			body, _ := readAll(resp)
			if !strings.Contains(body, `"data":[]`) {
				t.Errorf("expected \"data\":[] in body, got: %s", body)
			}
		})
	}
}

func TestAssetList_ReturnsFixtureWithPagination(t *testing.T) {
	native := v1.AssetDetail{
		AssetID: "native", Type: "native", Code: "XLM",
		Decimals: 7, Sep1Status: "not_applicable",
	}
	reader := &stubAssetReader{
		page:    []v1.AssetDetail{native},
		nextCur: "opaque-next",
	}
	srv := v1.New(v1.Options{Assets: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets?limit=50")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data       []v1.AssetDetail `json:"data"`
		Pagination struct {
			Next string `json:"next"`
		} `json:"pagination"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 1 || env.Data[0].AssetID != "native" {
		t.Fatalf("data wrong: %+v", env.Data)
	}
	if env.Pagination.Next != "opaque-next" {
		t.Errorf("pagination next = %q", env.Pagination.Next)
	}
}

func TestAssetList_InvalidLimitRejected(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)

	for _, raw := range []string{"0", "501", "abc", "-1"} {
		t.Run("limit="+raw, func(t *testing.T) {
			resp := mustGet(t, ts.URL+"/v1/assets?limit="+raw)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
			body, _ := readAll(resp)
			if !strings.Contains(body, "invalid-limit") {
				t.Errorf("error type missing: %s", body)
			}
		})
	}
}

// ─── /v1/assets/{asset_id} (single) ───────────────────────────────

// ctxDeadCountingReader answers GetAsset with a distinct Code each
// call, independent of the request's context state (mirrors the real
// AssetReader implementations, which don't observe cancellation
// themselves).
type ctxDeadCountingReader struct {
	calls int
}

func (r *ctxDeadCountingReader) GetAsset(_ context.Context, a canonical.Asset) (v1.AssetDetail, error) {
	r.calls++
	return v1.AssetDetail{
		AssetID:    a.String(),
		Type:       "native",
		Code:       fmt.Sprintf("CALL%d", r.calls),
		Decimals:   7,
		Sep1Status: "not_applicable",
	}, nil
}

func (r *ctxDeadCountingReader) ListAssets(_ context.Context, _ string, _ int) ([]v1.AssetDetail, string, error) {
	return nil, "", nil
}

// TestAssetMetadata_ReturnsOnlyOverlayFields checks the
// /v1/assets/{id}/metadata endpoint returns the SEP-1 slice
// without the canonical core (Code, Decimals, Issuer / ContractID).
// Same overlay path as /v1/assets/{id}; status field carries the
// resolution outcome.
func TestAssetMetadata_ReturnsOnlyOverlayFields(t *testing.T) {
	issuer := testUSDCIssuer
	domain := "circle.com"
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			"USDC-" + testUSDCIssuer: {
				AssetID:    "USDC-" + testUSDCIssuer,
				Type:       "classic",
				Code:       "USDC",
				Issuer:     &issuer,
				HomeDomain: &domain,
				Decimals:   7,
				Sep1Status: "verified",
				// Pre-populate name/desc to simulate the post-overlay state.
				Name:        ptr("USD Coin"),
				Description: ptr("Centre-issued USDC stablecoin"),
			},
		},
	}
	srv := v1.New(v1.Options{Assets: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/USDC-"+testUSDCIssuer+"/metadata")
	var env struct {
		Data v1.AssetMetadata `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.AssetID != "USDC-"+testUSDCIssuer {
		t.Errorf("asset_id = %q, want USDC-%s", env.Data.AssetID, testUSDCIssuer)
	}
	if env.Data.Sep1Status != "verified" {
		t.Errorf("sep1_status = %q, want verified", env.Data.Sep1Status)
	}
	if env.Data.HomeDomain == nil || *env.Data.HomeDomain != "circle.com" {
		t.Errorf("home_domain mismatch: %+v", env.Data.HomeDomain)
	}
	if env.Data.Name == nil || *env.Data.Name != "USD Coin" {
		t.Errorf("name not populated: %+v", env.Data.Name)
	}
	if env.Data.Description == nil || *env.Data.Description != "Centre-issued USDC stablecoin" {
		t.Errorf("description not populated: %+v", env.Data.Description)
	}
}

// A storage row without a home_domain is backfilled from the known-issuers
// map on both /v1/assets/{id} and its /metadata variant, so the two surfaces
// agree with /v1/issuers. With no metadata resolver wired the status advances
// to "not_fetched", not "not_applicable" (which would claim no home domain).
func TestAsset_BackfillsHomeDomainFromKnownIssuersMap(t *testing.T) {
	issuer := testUSDCIssuer
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			"USDC-" + testUSDCIssuer: {
				AssetID: "USDC-" + testUSDCIssuer, Type: "classic", Code: "USDC",
				Issuer: &issuer, HomeDomain: nil, Decimals: 7,
				// Sep1Status left empty: the handler computes it after the backfill.
			},
		},
	}
	ts := httpTestServer(t, v1.New(v1.Options{Assets: reader}))

	for _, suffix := range []string{"", "/metadata"} {
		t.Run("asset"+suffix, func(t *testing.T) {
			resp := mustGet(t, ts.URL+"/v1/assets/USDC-"+testUSDCIssuer+suffix)
			var body struct {
				Data struct {
					HomeDomain *string `json:"home_domain"`
					Sep1Status string  `json:"sep1_status"`
				} `json:"data"`
			}
			mustDecode(t, resp, &body)
			if body.Data.HomeDomain == nil || *body.Data.HomeDomain != "circle.com" {
				t.Errorf("HomeDomain = %v, want circle.com (from known_issuers map)", body.Data.HomeDomain)
			}
			if body.Data.Sep1Status != "not_fetched" {
				t.Errorf("Sep1Status = %q, want not_fetched", body.Data.Sep1Status)
			}
		})
	}
}

// TestAssetMetadata_ProjectsSEP1IssuanceFields confirms the four
// SEP-1 issuance declarations (conditions / fixed_number / max_number
// / is_unlimited) round-trip from AssetDetail through the metadata
// projection. Pre-overlay state — pretends `applySep1Overlay` has
// already populated AssetDetail; here we just exercise the
// projection.
func TestAssetMetadata_ProjectsSEP1IssuanceFields(t *testing.T) {
	issuer := testUSDCIssuer
	domain := "circle.com"
	yes := false // issuer asserts a bounded supply
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			"USDC-" + testUSDCIssuer: {
				AssetID:     "USDC-" + testUSDCIssuer,
				Type:        "classic",
				Code:        "USDC",
				Issuer:      &issuer,
				HomeDomain:  &domain,
				Decimals:    7,
				Sep1Status:  "verified",
				Conditions:  ptr("Issuer terms of service: https://centre.io/terms"),
				FixedNumber: ptr("100000000000000"),
				MaxNumber:   ptr("100000000000000"),
				IsUnlimited: &yes,
			},
		},
	}
	srv := v1.New(v1.Options{Assets: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/USDC-"+testUSDCIssuer+"/metadata")
	var env struct {
		Data v1.AssetMetadata `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.Conditions == nil || *env.Data.Conditions == "" {
		t.Errorf("conditions not projected: %+v", env.Data.Conditions)
	}
	if env.Data.FixedNumber == nil || *env.Data.FixedNumber != "100000000000000" {
		t.Errorf("fixed_number not projected: %+v", env.Data.FixedNumber)
	}
	if env.Data.MaxNumber == nil || *env.Data.MaxNumber != "100000000000000" {
		t.Errorf("max_number not projected: %+v", env.Data.MaxNumber)
	}
	if env.Data.IsUnlimited == nil {
		t.Errorf("is_unlimited not projected (nil)")
	} else if *env.Data.IsUnlimited != false {
		t.Errorf("is_unlimited = %v, want false", *env.Data.IsUnlimited)
	}
}

// TestAssetMetadata_NotFoundOn404 confirms that an unknown asset
// surfaces as 404 even on the metadata endpoint — same shape as
// /v1/assets/{id}, not a 200-with-empty-overlay.
func TestAssetMetadata_NotFoundOn404(t *testing.T) {
	reader := &stubAssetReader{byID: map[string]v1.AssetDetail{}} // empty
	srv := v1.New(v1.Options{Assets: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/USDC-"+testUSDCIssuer+"/metadata")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for unknown asset", resp.StatusCode)
	}
}

func ptr[T any](v T) *T { return &v }

// ─── helpers ──────────────────────────────────────────────────────

func httpTestServer(t *testing.T, srv *v1.Server) *testServer {
	t.Helper()
	ts := newTestServerFromHandler(t, srv.Handler())
	return ts
}

// Tiny wrapper around httptest.NewServer for readable test code.
type testServer = testServerImpl

func newTestServerFromHandler(t *testing.T, h http.Handler) *testServerImpl {
	t.Helper()
	return startHTTPTest(t, h)
}

func mustGet(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func mustDecode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// ─── 500 error paths ─────────────────────────────────────────

// A reader error that is not ErrAssetNotFound surfaces as 500.
func TestAsset_ReaderError500(t *testing.T) {
	reader := &stubAssetReader{err: errors.New("storage broke")}
	ts := httpTestServer(t, v1.New(v1.Options{Assets: reader}))

	for _, path := range []string{"/v1/assets", "/v1/assets/native"} {
		t.Run(path, func(t *testing.T) {
			resp := mustGet(t, ts.URL+path)
			if resp.StatusCode != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500", resp.StatusCode)
			}
		})
	}
}

func TestAssetList_NetworkParamIgnored_DefaultPath(t *testing.T) {
	// The cross-chain ?network= browse surface was removed (Stellar-
	// focus refactor): the param is now ignored and the listing always
	// returns the default reader path. Any ?network= value (stellar,
	// ethereum, potato) yields the same Stellar listing — never a 400
	// and never external rows.
	xlm := v1.AssetDetail{AssetID: "native", Type: "native", Code: "XLM"}
	reader := &stubAssetReader{page: []v1.AssetDetail{xlm}, nextCur: ""}
	srv := v1.New(v1.Options{Assets: reader, VerifiedCurrencies: newTestCatalogue(t)})
	ts := httpTestServer(t, srv)
	for _, net := range []string{"stellar", "ethereum", "potato"} {
		resp := mustGet(t, ts.URL+"/v1/assets?network="+net+"&limit=10")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("network=%s: status=%d want 200", net, resp.StatusCode)
		}
		var env struct {
			Data []v1.AssetDetail `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("network=%s: decode: %v", net, err)
		}
		if len(env.Data) != 1 || env.Data[0].AssetID != "native" {
			t.Errorf("network=%s: expected xlm row, got %+v", net, env.Data)
		}
	}
}

func TestAssetList_FromAssetsReader_IncludesPrice(t *testing.T) {
	// When an AssetsReader is wired, the listing endpoint sources from
	// ListAssetsExt and projects each AssetRow into an AssetDetail
	// with the asset-catalogue overlay fields populated.
	price := "1.0008"
	vol := "1131827.32"
	assetRow := timescale.AssetRow{
		Slug:             "USDC",
		AssetID:          "USDC-" + testUSDCIssuer,
		Code:             "USDC",
		IssuerGStrkey:    testUSDCIssuer,
		ObservationCount: 41610630,
		FirstSeenLedger:  50457424,
		LastSeenLedger:   62523839,
		PriceUSD:         &price,
		Volume24hUSD:     &vol,
	}
	listReader := &listingStub{rows: []timescale.AssetRow{assetRow}}
	srv := v1.New(v1.Options{AssetsReader: listReader, Assets: &stubAssetReader{}})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/assets?limit=10")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var env struct {
		Data []v1.AssetDetail `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data) != 1 {
		t.Fatalf("got %d rows, want 1", len(env.Data))
	}
	d := env.Data[0]
	if d.Slug != "USDC" {
		t.Errorf("slug=%q want USDC", d.Slug)
	}
	if d.PriceUSD == nil || *d.PriceUSD != "1.0008" {
		t.Errorf("price_usd=%v want 1.0008", d.PriceUSD)
	}
	if d.VolumeUSD24h == nil || *d.VolumeUSD24h != "1131827.32" {
		t.Errorf("volume_24h_usd=%v", d.VolumeUSD24h)
	}
	if d.ObservationCount == nil || *d.ObservationCount != 41610630 {
		t.Errorf("observation_count=%v", d.ObservationCount)
	}
}

// Filters pass through to the AssetsReader's ListAssetsExt options.
func TestAssetList_FromAssetsReader_FilterPassThrough(t *testing.T) {
	cases := []struct {
		name, query, wantIssuer, wantCode string
	}{
		{"issuer", "issuer=" + testUSDCIssuer, testUSDCIssuer, ""},
		{"code", "code=USDC", "", "USDC"},
		{"issuer and code combine", "issuer=" + testUSDCIssuer + "&code=USDC", testUSDCIssuer, "USDC"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			listReader := &listingStub{}
			srv := v1.New(v1.Options{AssetsReader: listReader, Assets: &stubAssetReader{}})
			mustGet(t, httpTestServer(t, srv).URL+"/v1/assets?"+tc.query)
			if listReader.lastOpts.Issuer != tc.wantIssuer || listReader.lastOpts.Code != tc.wantCode {
				t.Errorf("ListAssetsExt opts = {Issuer:%q Code:%q}, want {%q %q}",
					listReader.lastOpts.Issuer, listReader.lastOpts.Code, tc.wantIssuer, tc.wantCode)
			}
		})
	}
}

// type=classic and type=any are no-ops on the classic-only listing spine.
// type=native and type=fiat match nothing there, so they return an empty page
// without calling the reader. type=soroban is answerable by the spine (traded
// Soroban contracts) and must reach the store.
func TestAssetList_TypeFilter(t *testing.T) {
	usdc := timescale.AssetRow{AssetID: "USDC-" + testUSDCIssuer, Code: "USDC", Slug: "usdc"}
	soroban := timescale.AssetRow{
		AssetID: "CAUP7QFDIVYY4HYPHEUCVIVAUKY7CBHFVBQ2WWDGFME7RGVQ5SUKAAAA",
		Slug:    "caup7qfd",
	}
	cases := []struct {
		name, query string
		row         timescale.AssetRow
		wantRows    int
		wantCalled  bool
		wantType    string
	}{
		{"classic", "type=classic", usdc, 1, true, "classic"},
		{"any", "type=any", usdc, 1, true, ""},
		{"native", "type=native", usdc, 0, false, ""},
		{"fiat", "type=fiat", usdc, 0, false, ""},
		{"soroban", "type=soroban", soroban, 1, true, "soroban"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			listReader := &listingStub{rows: []timescale.AssetRow{tc.row}}
			srv := v1.New(v1.Options{AssetsReader: listReader, Assets: &stubAssetReader{}})
			resp := mustGet(t, httpTestServer(t, srv).URL+"/v1/assets?"+tc.query)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d", resp.StatusCode)
			}
			var body struct {
				Data []v1.AssetDetail `json:"data"`
			}
			mustDecode(t, resp, &body)
			if len(body.Data) != tc.wantRows {
				t.Fatalf("%s: got %d rows want %d", tc.query, len(body.Data), tc.wantRows)
			}
			if called := listReader.lastOpts.Limit != 0; called != tc.wantCalled {
				t.Errorf("%s: ListAssetsExt called = %v, want %v", tc.query, called, tc.wantCalled)
			}
			if tc.wantCalled && listReader.lastOpts.Type != tc.wantType {
				t.Errorf("%s: ListAssetsExt saw Type=%q, want %q", tc.query, listReader.lastOpts.Type, tc.wantType)
			}
		})
	}
}

func TestAssetList_InvalidFilters_400(t *testing.T) {
	// Malformed type / code / issuer 400 up front,
	// before any backing reader is consulted.
	listReader := &listingStub{}
	srv := v1.New(v1.Options{AssetsReader: listReader, Assets: &stubAssetReader{}})
	ts := httpTestServer(t, srv)
	cases := []struct {
		name  string
		query string
	}{
		{"bad type", "type=payment"},
		{"code too long", "code=THIRTEENCHARS"},
		{"code non-alnum", "code=US-DC"},
		{"bad issuer", "issuer=not-a-strkey"},
		{"issuer wrong prefix", "issuer=CA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := mustGet(t, ts.URL+"/v1/assets?"+tc.query)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("query %q: status=%d want 400", tc.query, resp.StatusCode)
			}
		})
	}
}

// listingStub is a tiny AssetsReader implementation tailored to the
// listing-endpoint tests. Each method returns the configured value;
// recording the most-recent ListAssetsExt opts lets tests assert
// what filter the handler passed through.
type listingStub struct {
	rows     []timescale.AssetRow
	lastOpts timescale.ListAssetsOptions
}

func (s *listingStub) ListAssetsExt(_ context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	s.lastOpts = opts
	return s.rows, nil
}

func (s *listingStub) GetAssetBySlug(_ context.Context, _ string) (timescale.AssetRow, error) {
	return timescale.AssetRow{}, nil
}

func (s *listingStub) GetAssetByAssetID(_ context.Context, _ string) (timescale.AssetRow, error) {
	return timescale.AssetRow{}, nil
}

func (s *listingStub) GetNativeAssetRow(_ context.Context) (timescale.AssetRow, error) {
	return timescale.AssetRow{}, nil
}

func (s *listingStub) GetAssetTopMarkets(_ context.Context, _ string, _ int) ([]timescale.AssetTopMarket, error) {
	return nil, nil
}

func (s *listingStub) GetAssetPriceHistory24h(_ context.Context, _ string) ([]timescale.AssetPricePoint, error) {
	return nil, nil
}

func (s *listingStub) GetAssetPriceHistory7d(_ context.Context, _ string) ([]timescale.AssetPricePoint, error) {
	return nil, nil
}

func (s *listingStub) GetAssetsPriceHistory24hBatch(_ context.Context, _ []string) (map[string][]timescale.AssetPricePoint, error) {
	return nil, nil
}

func (s *listingStub) GetAssetsPriceHistory7dBatch(_ context.Context, _ []string) (map[string][]timescale.AssetPricePoint, error) {
	return nil, nil
}

func (s *listingStub) GetAssetMarketsCount(_ context.Context, _ string) (int64, error) {
	return 0, nil
}

func (s *listingStub) GetAssetATH(_ context.Context, _ string) (*timescale.AssetATH, error) {
	return nil, nil
}

func (s *listingStub) GetAssetsATHBatch(_ context.Context, _ []string) (map[string]timescale.AssetATH, error) {
	return nil, nil
}

func (s *listingStub) GetAssetTradeCount24h(_ context.Context, _ string) (int64, error) {
	return 0, nil
}

// slugStubAssetReader wraps stubAssetReader with the optional
// classicSlugResolver capability (migration-0134 slug URLs).
type slugStubAssetReader struct {
	v1.AssetReader
	code, issuer string
}

func (r slugStubAssetReader) ClassicAssetBySlug(_ context.Context, slug string) (string, string, bool, error) {
	if slug == "usdt-gcqtgzqq" {
		return r.code, r.issuer, true, nil
	}
	return "", "", false, nil
}

// unmeasuredSubstanceGate is a substance gate whose store cannot answer
// (error or request deadline): Allowed fails open as the production gate
// does, Probe reports that no verdict was reached.
type unmeasuredSubstanceGate struct{}

func (unmeasuredSubstanceGate) Allowed(context.Context, canonical.Asset, canonical.Asset, string) bool {
	return true
}

func (unmeasuredSubstanceGate) Probe(context.Context, canonical.Asset, canonical.Asset) (allowed, measured bool, floor pricingguard.SubstanceFloor) {
	return false, false, pricingguard.FloorNone
}

// TestAssetList_SubstanceUnmeasured_WithholdsPriceAndStampsStale pins
// the rule on both /v1/assets listing paths: when the substance gate cannot
// measure a row, the ungated catalogue price must not be published (so it
// cannot back a market cap — ADR-0018) and the page must say it is
// degraded. The control shows a MEASURED withhold stays unflagged.
func TestAssetList_SubstanceUnmeasured_WithholdsPriceAndStampsStale(t *testing.T) {
	price := "0.42"
	row := timescale.AssetRow{
		Slug:             "DUST",
		AssetID:          "DUST-" + testUSDCIssuer,
		Code:             "DUST",
		IssuerGStrkey:    testUSDCIssuer,
		ObservationCount: 10,
		PriceUSD:         &price,
	}
	cases := []struct {
		name      string
		gate      v1.PriceSubstanceGate
		wantStale bool
	}{
		{"unmeasured", unmeasuredSubstanceGate{}, true},
		{"measured_thin", &stubSubstanceGate{allow: false}, false},
	}
	for _, path := range []string{"/v1/assets?limit=10", "/v1/assets?asset_class=all&limit=10"} {
		for _, tc := range cases {
			t.Run(tc.name+" "+path, func(t *testing.T) {
				srv := v1.New(v1.Options{
					AssetsReader: &listingStub{rows: []timescale.AssetRow{row}},
					Assets:       &stubAssetReader{},
					Substance:    tc.gate,
				})
				resp := mustGet(t, httpTestServer(t, srv).URL+path)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status=%d", resp.StatusCode)
				}
				var env struct {
					Data  []v1.AssetDetail `json:"data"`
					Flags v1.Flags         `json:"flags"`
				}
				mustDecode(t, resp, &env)
				if len(env.Data) != 1 {
					t.Fatalf("got %d rows, want 1", len(env.Data))
				}
				if p := env.Data[0].PriceUSD; p != nil {
					t.Errorf("price_usd = %q, want withheld", *p)
				}
				if env.Flags.Stale != tc.wantStale {
					t.Errorf("flags.stale = %v, want %v", env.Flags.Stale, tc.wantStale)
				}
			})
		}
	}
}

// paginatingAssetsReader embeds the full stub and overrides only
// ListAssetsExt, honouring opts.Limit by returning min(Limit, total)
// rows so the handler's overfetch-by-one logic is exercised exactly as
// the real store would drive it.
type paginatingAssetsReader struct {
	stubAssetsReaderExt
	total int
}

func (p *paginatingAssetsReader) ListAssetsExt(_ context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	n := opts.Limit
	if n > p.total {
		n = p.total
	}
	rows := make([]timescale.AssetRow, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, timescale.AssetRow{
			AssetID:          "USDC-GAAA",
			Slug:             "usdc",
			Code:             "USDC",
			ObservationCount: int64(i + 1),
		})
	}
	return rows, nil
}

// TestAssetList_AssetsPaginationEmitsCursor pins the case when the assetsReader
// catalogue holds more than `limit` rows, /v1/assets MUST emit a next
// cursor. The previous handler passed `limit` (not limit+1) to the
// store, so the overfetch sentinel never appeared and the listing was
// stuck on its first page over a ~199K-asset directory.
func TestAssetList_AssetsPaginationEmitsCursor(t *testing.T) {
	srv := v1.New(v1.Options{AssetsReader: &paginatingAssetsReader{total: 1000}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets?limit=50")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data       []v1.AssetDetail `json:"data"`
		Pagination *struct {
			Next string `json:"next"`
		} `json:"pagination"`
	}
	mustDecode(t, resp, &env)

	if len(env.Data) != 50 {
		t.Fatalf("returned %d rows, want exactly the page size 50 (overfetch row must be trimmed)", len(env.Data))
	}
	if env.Pagination == nil || env.Pagination.Next == "" {
		t.Fatalf("no next cursor emitted despite 1000 > 50 rows available (F-1326)")
	}
}

// TestAssetList_RejectsMalformedCursor guards cursor validation on both the
// default listing and the unified (asset_class=all) classic phase: without it
// a malformed cursor falls through to the keyset predicate's degenerate
// (0, "") case and reads as a quiet end-of-pagination (empty page, 200 OK).
func TestAssetList_RejectsMalformedCursor(t *testing.T) {
	ts := httpTestServer(t, v1.New(v1.Options{AssetsReader: &paginatingAssetsReader{total: 1000}}))
	for _, q := range []string{
		"limit=50&cursor=not-a-valid-cursor",
		"asset_class=all&limit=50&cursor=classic:not-a-valid-cursor",
	} {
		t.Run(q, func(t *testing.T) {
			resp := mustGet(t, ts.URL+"/v1/assets?"+q)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for a malformed cursor", resp.StatusCode)
			}
		})
	}
}

// TestAssetList_AssetsPaginationLastPageNoCursor confirms the tail page
// (rows ≤ limit) correctly omits the cursor.
func TestAssetList_AssetsPaginationLastPageNoCursor(t *testing.T) {
	srv := v1.New(v1.Options{AssetsReader: &paginatingAssetsReader{total: 30}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets?limit=50")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data       []v1.AssetDetail `json:"data"`
		Pagination *struct {
			Next string `json:"next"`
		} `json:"pagination"`
	}
	mustDecode(t, resp, &env)

	if len(env.Data) != 30 {
		t.Fatalf("returned %d rows, want 30", len(env.Data))
	}
	if env.Pagination != nil && env.Pagination.Next != "" {
		t.Fatalf("unexpected next cursor on the final page: %q", env.Pagination.Next)
	}
}

// listCountingAssetsReader counts the listing reads that reach the
// store, so a test can assert a rate-limit denial stopped the read
// rather than merely relabelling its response.
type listCountingAssetsReader struct {
	paginatingAssetsReader
	lists atomic.Int64
}

func (r *listCountingAssetsReader) ListAssetsExt(ctx context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	r.lists.Add(1)
	return r.paginatingAssetsReader.ListAssetsExt(ctx, opts)
}

// newAssetsLimitedServer wires /v1/assets behind the production limiter
// constructor over a Redis-backed anonymous bucket. withStore selects
// whether an AssetsReader is wired.
func newAssetsLimitedServer(t *testing.T, anonLimit int, withStore bool) (*testServerImpl, *listCountingAssetsReader) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	opts := v1.Options{
		RateLimit: middleware.RateLimitBySubject(
			ratelimit.New(rdb, anonLimit, time.Minute, pinnedWindow), nil, middleware.SkipHealthAndMetrics, nil),
	}
	reader := &listCountingAssetsReader{paginatingAssetsReader: paginatingAssetsReader{total: 3}}
	if withStore {
		opts.AssetsReader = reader
	}
	return startHTTPTest(t, v1.New(opts).Handler()), reader
}

// TestAssetList_ChargesByThePlanSelected is the regression for the
// second surface. /v1/assets is one route and several query plans, the
// query string picks the plan, and the limiter charged all of them one
// token — so the volume-ranked plan (measured 18x the default) and the
// cache-defeating `q` scan were bought at the cheapest plan's price.
//
// Each case is one request into a fresh 100-token window; the assertion
// is the exact post-charge remainder.
func TestAssetList_ChargesByThePlanSelected(t *testing.T) {
	cases := []struct {
		name, query   string
		wantStatus    int
		wantRemaining string
	}{
		{"default listing", "?limit=5", 200, "99"},
		{"explicit default order", "?order_by=observation_count_desc", 200, "99"},
		{"volume-ranked plan", "?order_by=volume_24h_usd_desc", 200, "90"},
		{"search", "?q=usd", 200, "95"},
		{"search on the volume-ranked plan", "?q=usd&order_by=volume_24h_usd_desc", 200, "86"},
		// asset_class=all reaches the SAME volume-ranked store read
		// without naming order_by; pricing only the named parameter would
		// leave this as the way around the charge.
		{"unified listing", "?asset_class=all", 200, "90"},
		{"unified listing + search", "?asset_class=all&q=usd", 200, "86"},
		// The class-scoped listings filter a few dozen curated rows
		// in-process and ignore q: no plan selected, base token.
		{"class-scoped listing ignores q", "?asset_class=stablecoin&q=usd", 200, "99"},
		// Rejected before any plan is chosen: base token only.
		{"invalid order_by", "?order_by=bogus", 400, "99"},
		{"order_by with asset_class", "?asset_class=all&order_by=volume_24h_usd_desc", 400, "99"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := newAssetsLimitedServer(t, 100, true)
			resp := mustGet(t, ts.URL+"/v1/assets"+tc.query)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			got := resp.Header.Get("X-RateLimit-Remaining")
			if resp.StatusCode == http.StatusOK {
				got = strconv.Itoa(remainingBeforeProbe(t, resp, ts.URL+assetsProbe))
			}
			if got != tc.wantRemaining {
				t.Fatalf("X-RateLimit-Remaining = %q, want %q", got, tc.wantRemaining)
			}
		})
	}
}

// assetsProbe is rejected before any plan is chosen (the "invalid
// order_by" case above pins it at the base token) and answered no-store.
const assetsProbe = "/v1/assets?order_by=bogus"

// TestAssetList_DeniedPlanDoesNoRead: the surcharge lands before the
// read. A caller with 2 tokens left cannot buy a 10-token plan, and the
// store is not touched finding that out. The budget is 13 because the
// probe that reads the remainder spends one of the 3 the plan leaves.
func TestAssetList_DeniedPlanDoesNoRead(t *testing.T) {
	ts, reader := newAssetsLimitedServer(t, 13, true)
	url := ts.URL + "/v1/assets?order_by=volume_24h_usd_desc&limit=2"

	resp := mustGet(t, url)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", resp.StatusCode)
	}
	if got := remainingBeforeProbe(t, resp, ts.URL+assetsProbe); got != 3 {
		t.Fatalf("X-RateLimit-Remaining = %d, want 3", got)
	}

	before := reader.lists.Load()
	resp = mustGet(t, url+"&cursor=")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("429 must carry Retry-After")
	}
	if got := reader.lists.Load(); got != before {
		t.Fatalf("a denied request still ran %d listing read(s)", got-before)
	}
}

// TestAssetList_NoStoreNoSurcharge: with no AssetsReader wired the
// volume-ranked plan does not exist to be selected, and the request
// costs the base token.
func TestAssetList_NoStoreNoSurcharge(t *testing.T) {
	ts, _ := newAssetsLimitedServer(t, 100, false)
	resp := mustGet(t, ts.URL+"/v1/assets?order_by=volume_24h_usd_desc&q=usd")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := remainingBeforeProbe(t, resp, ts.URL+assetsProbe); got != 99 {
		t.Fatalf("X-RateLimit-Remaining = %d, want 99", got)
	}
}

// TestAssetList_QueryLengthIsBounded: `q` was bounded only by the
// server's header limit while being carried verbatim into the listing
// cache key and three LIKE patterns. 100 bytes clears the longest value
// that can match a row (a 69-byte classic asset id); one byte more is a
// 400 on every path, class-scoped listings included, and reads nothing.
func TestAssetList_QueryLengthIsBounded(t *testing.T) {
	ts, reader := newAssetsLimitedServer(t, 100, true)

	resp := mustGet(t, ts.URL+"/v1/assets?q="+strings.Repeat("a", 100))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("100-byte q: status = %d, want 200", resp.StatusCode)
	}

	before := reader.lists.Load()
	for _, path := range []string{"/v1/assets?q=", "/v1/assets?asset_class=all&q=", "/v1/assets?asset_class=fiat&q="} {
		resp = mustGet(t, ts.URL+path+strings.Repeat("a", 101))
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s<101 bytes>: status = %d, want 400", path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("%s: Content-Type = %q, want application/problem+json", path, ct)
		}
	}
	if got := reader.lists.Load(); got != before {
		t.Fatalf("an over-long q still ran %d listing read(s)", got-before)
	}

	// Surrounding whitespace is trimmed before the bound applies.
	resp = mustGet(t, ts.URL+"/v1/assets?q=%20"+strings.Repeat("a", 100)+"%20")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("padded 100-byte q: status = %d, want 200", resp.StatusCode)
	}
}

// TestAssetList_BootSeededPageIsLabelledStale is the honesty half of the boot seed.
//
// A boot-seeded page-set is real data, but it was observed by a
// PREVIOUS process and is stale by construction. Serving it is the
// fix; serving it under `flags.stale: false` with `as_of` stamped to
// the moment of the request would be a new lie — precisely the one
// /v1/markets was corrected for (markets.go: "never now() over rows a
// failing refresh has let age past the TTL").
//
// The assertions are on the CORRECTED VALUES: stale must be true, and
// as_of must equal the seed's real observation time to the second, not
// merely be non-zero.
func TestAssetList_BootSeededPageIsLabelledStale(t *testing.T) {
	const ttl = time.Minute
	reader := v1.NewCachedAssetsReader(&paginatingAssetsReader{total: 1000}, ttl)

	// The key the handler will look up for `?limit=2`: it overfetches
	// by one, so Limit is 3. Built through the same SeedListing seam
	// the boot path uses.
	observedAt := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)
	opts := timescale.ListAssetsOptions{Limit: 3}
	seeded := []timescale.AssetRow{
		{AssetID: "SEEDED1-GAAA", Slug: "seeded1", Code: "SEEDED1", ObservationCount: 9},
		{AssetID: "SEEDED2-GAAA", Slug: "seeded2", Code: "SEEDED2", ObservationCount: 8},
	}
	if !reader.SeedListing(opts, seeded, observedAt) {
		t.Fatal("SeedListing refused the boot seed")
	}

	srv := v1.New(v1.Options{AssetsReader: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets?limit=2")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data  []v1.AssetDetail `json:"data"`
		AsOf  time.Time        `json:"as_of"`
		Flags struct {
			Stale bool `json:"stale"`
		} `json:"flags"`
	}
	mustDecode(t, resp, &env)

	if len(env.Data) != 2 || env.Data[0].AssetID != "SEEDED1-GAAA" {
		t.Fatalf("data = %+v, want the seeded rows served straight from the boot seed", env.Data)
	}
	if !env.Flags.Stale {
		t.Error("flags.stale = false over a page-set observed 30 minutes ago — a boot seed must be labelled, not passed off as fresh")
	}
	if !env.AsOf.Equal(observedAt) {
		t.Errorf("as_of = %s, want the served rows' real observation time %s",
			env.AsOf.Format(time.RFC3339Nano), observedAt.Format(time.RFC3339Nano))
	}
}

// TestAssetList_FreshCachedPageIsNotLabelledStale is the other side of
// the same contract: the honest label must discriminate. A page-set
// inside the TTL is not stale, and stamping `stale: true` on every
// cached response would make the flag worthless.
func TestAssetList_FreshCachedPageIsNotLabelledStale(t *testing.T) {
	reader := v1.NewCachedAssetsReader(&paginatingAssetsReader{total: 1000}, time.Minute)
	observedAt := time.Now().Add(-2 * time.Second).UTC().Truncate(time.Second)
	if !reader.SeedListing(timescale.ListAssetsOptions{Limit: 3}, []timescale.AssetRow{
		{AssetID: "SEEDED1-GAAA", Slug: "seeded1", Code: "SEEDED1"},
	}, observedAt) {
		t.Fatal("SeedListing refused the seed")
	}

	ts := httpTestServer(t, v1.New(v1.Options{AssetsReader: reader}))
	resp := mustGet(t, ts.URL+"/v1/assets?limit=2")
	var env struct {
		AsOf  time.Time `json:"as_of"`
		Flags struct {
			Stale bool `json:"stale"`
		} `json:"flags"`
	}
	mustDecode(t, resp, &env)

	if env.Flags.Stale {
		t.Error("flags.stale = true over a page-set observed 2 seconds ago under a 1-minute TTL")
	}
	if !env.AsOf.Equal(observedAt) {
		t.Errorf("as_of = %s, want the served rows' observation time %s",
			env.AsOf.Format(time.RFC3339Nano), observedAt.Format(time.RFC3339Nano))
	}
}

// TestAssetList_UncachedReaderStillFresh — the degraded path. A wired
// reader with no cache (every test double in this package, and any
// deployment with the listing cache off) answers live, so it must not
// acquire a stale flag or a back-dated as_of from this change.
func TestAssetList_UncachedReaderStillFresh(t *testing.T) {
	ts := httpTestServer(t, v1.New(v1.Options{AssetsReader: &paginatingAssetsReader{total: 10}}))
	before := time.Now().UTC().Add(-time.Second)

	resp := mustGet(t, ts.URL+"/v1/assets?limit=2")
	var env struct {
		Data  []v1.AssetDetail `json:"data"`
		AsOf  time.Time        `json:"as_of"`
		Flags struct {
			Stale bool `json:"stale"`
		} `json:"flags"`
	}
	mustDecode(t, resp, &env)

	if len(env.Data) != 2 {
		t.Fatalf("data = %+v", env.Data)
	}
	if env.Flags.Stale {
		t.Error("flags.stale = true on a live uncached read")
	}
	if env.AsOf.Before(before) {
		t.Errorf("as_of = %s is back-dated; a live read stamps now", env.AsOf.Format(time.RFC3339Nano))
	}
}

// The listing's ORDER BY has a LEADING rank-tier key (flagged /
// unpriced rows sort below rankable ones). The keyset cursor MUST carry
// that key — a cursor that encodes fewer keys than the ORDER BY ranks on
// resumes at the wrong place and drops whole tiers of rows. These pin the
// handler's half of that contract: it emits the store's encoding verbatim
// (tier first) on BOTH /v1/assets paths, and the emitted cursor is
// accepted back by the same handler's validator.

// The reported row, as the store would hand it to the handler.
// Named constants rather than inline literals — a G-strkey spelled out
// next to a field whose name ends in "Key" trips gitleaks' generic-api-key
// rule (it is a public issuer address, not a secret), and the sibling
// directory tests already use this shape.
const (
	jfkBankIssuer  = "GB7KFNUR5IAIN5NTYM2BUWWUTM6QMUBXF7NHXXKAMRPFLFWR7KL5BANK"
	jfkBankAssetID = "JFKBANK2-" + jfkBankIssuer
)

// rankTierAssetsReader returns one demoted row carrying the sort keys the
// store's listing query would have produced.
type rankTierAssetsReader struct {
	stubAssetsReaderExt
	lastCursor string
}

func (r *rankTierAssetsReader) ListAssetsExt(_ context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	r.lastCursor = opts.Cursor
	tier := 2
	sortVol := "62341.98422258"
	// Two rows so the handler's overfetch-by-one sees a next page at limit=1.
	rows := make([]timescale.AssetRow, 0, 2)
	for i := 0; i < 2; i++ {
		rows = append(rows, timescale.AssetRow{
			AssetID:          jfkBankAssetID,
			Slug:             jfkBankAssetID,
			Code:             "JFKBANK2",
			IssuerGStrkey:    jfkBankIssuer,
			ObservationCount: int64(1779 - i),
			SortVolume24hUSD: &sortVol,
			RankTier:         &tier,
		})
	}
	return rows, nil
}

func nextCursorOf(t *testing.T, ts, path string) string {
	t.Helper()
	resp := mustGet(t, ts+path)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d", path, resp.StatusCode)
	}
	var env struct {
		Pagination *struct {
			Next string `json:"next"`
		} `json:"pagination"`
	}
	mustDecode(t, resp, &env)
	if env.Pagination == nil || env.Pagination.Next == "" {
		t.Fatalf("GET %s emitted no next cursor", path)
	}
	return env.Pagination.Next
}

func TestAssetList_NextCursorCarriesTheRankTier(t *testing.T) {
	reader := &rankTierAssetsReader{}
	srv := v1.New(v1.Options{AssetsReader: reader})
	ts := httpTestServer(t, srv)

	// Observation-count listing: <rank_tier>:<observation_count>:<asset_id>.
	obs := nextCursorOf(t, ts.URL, "/v1/assets?limit=1")
	wantObs := "2:1779:" + jfkBankAssetID
	if obs != wantObs {
		t.Fatalf("observation-count next cursor = %q, want %q", obs, wantObs)
	}

	// Unified listing's classic phase:
	// classic:<rank_tier>:<sort_volume>:<asset_id>. The sort volume stays
	// the §4-B adjusted key, not the raw payload volume.
	cls := nextCursorOf(t, ts.URL, "/v1/assets?asset_class=all&limit=1")
	wantCls := "classic:2:62341.98422258:" + jfkBankAssetID
	if cls != wantCls {
		t.Fatalf("classic-phase next cursor = %q, want %q", cls, wantCls)
	}

	// Both must be accepted back by the handler that minted them (which
	// validates cursors at the boundary), and reach the store intact.
	for _, tc := range []struct{ path, wantInner string }{
		{"/v1/assets?limit=1&cursor=" + url.QueryEscape(obs), obs},
		{"/v1/assets?asset_class=all&limit=1&cursor=" + url.QueryEscape(cls), strings.TrimPrefix(cls, "classic:")},
	} {
		resp := mustGet(t, ts.URL+tc.path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("replaying own cursor on %s: status = %d, want 200", tc.path, resp.StatusCode)
		}
		_ = resp.Body.Close()
		if reader.lastCursor != tc.wantInner {
			t.Errorf("store received cursor %q, want %q", reader.lastCursor, tc.wantInner)
		}
	}
}
