package v1_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
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
	"github.com/Stellar-Index/StellarIndex/internal/supply"
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

// ctxDeadCountingReader answers GetAsset with a distinct Code per call, independent of context state, like the real AssetReader
// implementations which don't observe cancellation themselves.
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

// TestAssetMetadata_ReturnsOnlyOverlayFields checks /v1/assets/{id}/metadata returns the SEP-1
// slice without the canonical core (Code, Decimals, Issuer / ContractID). It shares the overlay
// path of /v1/assets/{id}; the status field carries the resolution outcome.
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

// A storage row without a home_domain is backfilled from the known-issuers map on /v1/assets/{id} and /metadata, agreeing with
// /v1/issuers. With no metadata resolver wired the status is "not_fetched", not "not_applicable" (which would claim no home
// domain).
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

// TestAssetMetadata_ProjectsSEP1IssuanceFields confirms the four SEP-1 issuance declarations
// (conditions / fixed_number / max_number / is_unlimited) round-trip from AssetDetail through the
// metadata projection. AssetDetail is pre-populated as if `applySep1Overlay` had run.
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
	// The ?network= param is ignored: any value (stellar, ethereum, potato) yields the same
	// Stellar listing from the default reader path, never a 400 and never external rows.
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

// type=classic and type=any are no-ops on the classic-only spine; type=native and type=fiat return an empty page without calling
// the reader; type=soroban is answerable by the spine (traded contracts) and must reach the store.
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

// listingStub is a small AssetsReader for the listing tests; it returns configured values and records the latest ListAssetsExt
// opts so tests can assert the filter the handler passed through.
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

// TestAssetList_SubstanceUnmeasured_WithholdsPriceAndStampsStale: on both listing paths, when the substance gate cannot measure a
// row the ungated catalogue price must not be published (so it cannot back a market cap) and the page must be flagged degraded.
// The control shows a measured withhold stays unflagged.
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

// paginatingAssetsReader overrides only ListAssetsExt, honouring opts.Limit with min(Limit, total) rows so the handler's
// overfetch-by-one logic runs as the real store drives it.
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

// TestAssetList_AssetsPaginationEmitsCursor pins that when the assetsReader catalogue holds more
// than `limit` rows, /v1/assets must emit a next cursor. The handler must pass limit+1 to the
// store so the overfetch sentinel appears; passing `limit` leaves the listing stuck on page one.
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

// TestAssetList_RejectsMalformedCursor: a malformed cursor on the default listing and the unified classic phase must be rejected,
// else it falls into the keyset predicate's degenerate (0, "") case and reads as a quiet end of pagination (empty 200).
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

// TestAssetList_ChargesByThePlanSelected: /v1/assets is one route with several query plans, so charging one token would sell the
// expensive volume-ranked plan and the cache-defeating `q` scan at the cheapest price. Each case runs in a fresh 100-token window
// and asserts the exact post-charge remainder.
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

// TestAssetList_DeniedPlanDoesNoRead: the surcharge lands before the read, so a caller with 2 tokens cannot buy a 10-token plan
// and the store is not touched. The budget is 13 because the remainder probe spends one of the 3 the plan leaves.
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

// TestAssetList_QueryLengthIsBounded: `q` goes verbatim into the cache key and three LIKE patterns, so it is bounded at 100 bytes
// (above the longest matchable 69-byte classic id); one byte more is a 400 on every path and reads nothing.
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

// TestAssetList_BootSeededPageIsLabelledStale: a boot-seeded page-set was observed by a previous process, so it must serve
// `flags.stale: true` with as_of equal to the seed's real observation time, not the request time (as /v1/markets does).
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

// TestAssetList_FreshCachedPageIsNotLabelledStale: the stale label must discriminate; a page-set inside the TTL is not stale, and
// flagging every cached response would make the flag worthless.
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

// TestAssetList_UncachedReaderStillFresh: a reader with no cache answers live, so it must not acquire a stale flag or a back-
// dated as_of.
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

// The listing's ORDER BY has a leading rank-tier key (flagged / unpriced rows sort below rankable
// ones). The keyset cursor must carry that key: a cursor that encodes fewer keys than the ORDER BY
// ranks on resumes in the wrong place and drops whole tiers of rows. These pin the handler's half:
// it emits the store's encoding verbatim (tier first) on both /v1/assets paths, and the emitted
// cursor is accepted back by the same handler's validator.

// The reported row, as the store would hand it to the handler.
// Named constants, not inline literals: a G-strkey next to a field whose name ends in "Key" trips
// gitleaks' generic-api-key rule (it is a public issuer address, not a secret).
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
	// The contract-arm read behind the off-page SAC merge is not the page read under test.
	if opts.Type != "soroban" {
		r.lastCursor = opts.Cursor
	}
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

const pegHandlerTestAUDD = "AUDD-GDC7X2MXTYSAKUUGAIQ7J7RPEIM7GXSAIWFYWWH4GLNFECQVJJLB2EEU"

// pegHandlerTestServer wires the full handleAssetGet chain for declared-peg detail tests: a blanket-verdict substance gate, a
// catalogue overlay row with a dust or real market price and change pills, a fresh AUD FX point, and AUDD peg-configured.
func pegHandlerTestServer(t *testing.T, allow bool, row timescale.AssetRow) *v1.Server {
	t.Helper()
	aud, err := canonical.NewFiatAsset("AUD")
	if err != nil {
		t.Fatal(err)
	}
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			row.AssetID: {AssetID: row.AssetID, Type: "classic", Code: row.Code},
		},
	}
	return v1.New(v1.Options{
		Assets:       reader,
		AssetsReader: &stubAssetsReaderExt{row: row},
		Substance:    &stubSubstanceGate{allow: allow},
		FXHistory: &stubFXHistoryReader{points: []v1.FXQuotePoint{
			{Bucket: time.Now().UTC().Add(-24 * time.Hour), RateUSDText: "1.5267", InverseUSDText: "0.655"},
		}},
		FiatPeggedClassics: map[string]canonical.Asset{pegHandlerTestAUDD: aud},
	})
}

// derefOrNil renders a *string for failure messages.
func derefOrNil(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func getPegAssetDetail(t *testing.T, srv *v1.Server, assetID string) v1.AssetDetail {
	t.Helper()
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/assets/"+assetID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return env.Data
}

// decStub is a canned v1.TokenDecimalsReader recording which contract (if
// any) the handler consulted.
type decStub struct {
	d           uint32
	found       bool
	gotContract string
}

func (s *decStub) TokenDecimals(_ context.Context, contractID string) (uint32, bool, error) {
	s.gotContract = contractID
	return s.d, s.found, nil
}

const decTestContract = "CDB2WMKQQNVZMEBY7Q7GZ5C7E7IAFSNMZ7GGVD6WKTCEWK7XOIAVZSAP"

func getAssetDetail(t *testing.T, base, assetID string) (v1.AssetDetail, int) {
	t.Helper()
	resp := mustGet(t, base+"/v1/assets/"+assetID)
	if resp.StatusCode != http.StatusOK {
		return v1.AssetDetail{}, resp.StatusCode
	}
	var body struct {
		Data v1.AssetDetail `json:"data"`
	}
	mustDecode(t, resp, &body)
	return body.Data, resp.StatusCode
}

// TestAssetDetail_SorobanDecimalsOverlay: a Soroban token whose instance
// METADATA is captured serves its REAL on-chain decimals, not the 7 default.
func TestAssetDetail_SorobanDecimalsOverlay(t *testing.T) {
	stub := &decStub{d: 18, found: true}
	srv := v1.New(v1.Options{TokenDecimals: stub})
	base := httpTestServer(t, srv).URL

	detail, code := getAssetDetail(t, base, decTestContract)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if detail.Decimals != 18 {
		t.Errorf("decimals = %d, want 18 (overlaid from instance metadata)", detail.Decimals)
	}
	if stub.gotContract != decTestContract {
		t.Errorf("consulted contract = %q, want %q", stub.gotContract, decTestContract)
	}
}

// TestAssetDetail_SorobanDecimalsNotDerivable: no captured metadata → the
// documented default 7 stays.
func TestAssetDetail_SorobanDecimalsNotDerivable(t *testing.T) {
	srv := v1.New(v1.Options{TokenDecimals: &decStub{found: false}})
	base := httpTestServer(t, srv).URL

	detail, code := getAssetDetail(t, base, decTestContract)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if detail.Decimals != 7 {
		t.Errorf("decimals = %d, want default 7 when metadata is not derivable", detail.Decimals)
	}
}

// TestAssetDetail_ClassicNeverConsultsDecimals: classic assets ARE 7 by
// protocol — the reader must not even be consulted (a wrong overlay here
// would corrupt every unit computation downstream).
func TestAssetDetail_ClassicNeverConsultsDecimals(t *testing.T) {
	stub := &decStub{d: 18, found: true} // would lie if consulted
	srv := v1.New(v1.Options{TokenDecimals: stub})
	base := httpTestServer(t, srv).URL

	detail, code := getAssetDetail(t, base, "USDC-"+testG)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if detail.Decimals != 7 {
		t.Errorf("classic decimals = %d, want 7 (protocol-fixed)", detail.Decimals)
	}
	if stub.gotContract != "" {
		t.Errorf("classic asset consulted the decimals reader (contract %q)", stub.gotContract)
	}
}

// supplySnapAt is lockstepSupply's 1000-token (9 dp) circulating figure,
// observed at `at` and ledger 4242.
func supplySnapAt(at time.Time) *stubSupplyLooker {
	circ := new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(9), nil))
	return &stubSupplyLooker{hit: true, snap: supply.Supply{
		CirculatingSupply: circ, ObservedAt: at, LedgerSequence: 4242,
	}}
}

// /v1/assets/{id} carries the supply's vintage, and an observation older than
// the listing's 6 h precise-arm bound is not multiplied by today's price: the
// supply serves with its as-of, the cap is withheld, the body is stale.
func TestAssetDetail_SupplyAgeBoundsTheCap(t *testing.T) {
	cases := []struct {
		name      string
		age       time.Duration
		wantCap   bool
		wantStale bool
	}{
		{"fresh observation", time.Hour, true, false},
		{"observer stopped a day ago", 25 * time.Hour, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Now().UTC().Add(-tc.age).Truncate(time.Second)
			srv := v1.New(v1.Options{Prices: lockstepPrices(), Supply: supplySnapAt(at)})
			body := lockstepGet(t, srv)

			if want := `"supply_as_of":"` + at.Format(time.RFC3339); !strings.Contains(body, want) {
				t.Errorf("body missing %s: %s", want, body)
			}
			if !strings.Contains(body, `"supply_as_of_ledger":4242`) {
				t.Errorf("body missing supply_as_of_ledger 4242: %s", body)
			}
			if got := strings.Contains(body, `"market_cap_usd":"`); got != tc.wantCap {
				t.Errorf("market_cap_usd present = %v, want %v: %s", got, tc.wantCap, body)
			}
			if got := strings.Contains(body, `"stale":true`); got != tc.wantStale {
				t.Errorf("flags.stale = %v, want %v: %s", got, tc.wantStale, body)
			}
		})
	}
}

const (
	aliasUSDCIssuer  = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	aliasUSDCClassic = "USDC-" + aliasUSDCIssuer
	aliasUSDCSAC     = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
)

// TestAssetDetail_SACFormResolvesCanonical: GET /v1/assets/{USDC-SAC} must serve the canonical classic USDC detail (same
// asset_id), resolved from the config AliasRegistry with no lake round-trip, so a SAC link lands on the real page. Without
// handleAssetGet canonicalization the SAC address echoes back as its own asset_id.
func TestAssetDetail_SACFormResolvesCanonical(t *testing.T) {
	// Process-global registry — must not run parallel; reset on cleanup.
	reg, err := canonical.NewAliasRegistry(canonical.PubnetPassphrase, map[string]string{aliasUSDCSAC: "USDC:" + aliasUSDCIssuer})
	if err != nil {
		t.Fatalf("NewAliasRegistry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })

	srv := v1.New(v1.Options{})
	base := httpTestServer(t, srv).URL

	detail, code := getAssetDetail(t, base, aliasUSDCSAC)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if detail.AssetID != aliasUSDCClassic {
		t.Errorf("SAC-form detail asset_id = %q, want the canonical classic %q — a SAC link must resolve to the real asset page",
			detail.AssetID, aliasUSDCClassic)
	}
}

// TestAssetDetail_WithheldPrice_ServesNoPriceHistory: the same invariant on the detail payload, where the leak was widest; a
// scam-flagged asset with price_usd null must not carry priced hourly or daily history points.
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

// stubSep1Cache implements v1.Sep1CachedReader in memory, mirroring Store.GetIssuerSep1Cached: payload for known issuers, (nil,
// nil) for a row with no payload yet, (nil, sql.ErrNoRows) for unknown issuers, or a configured error.
type stubSep1Cache struct {
	byIssuer map[string]*timescale.IssuerSep1Cached
	err      error
}

func (s *stubSep1Cache) GetIssuerSep1Cached(_ context.Context, gStrkey string) (*timescale.IssuerSep1Cached, error) {
	if s.err != nil {
		return nil, s.err
	}
	if payload, ok := s.byIssuer[gStrkey]; ok {
		return payload, nil
	}
	return nil, sql.ErrNoRows
}

func currencyAuthorityFixture(t *testing.T, cur timescale.IssuerSep1Currency) string {
	t.Helper()
	issuer := testUSDCIssuer
	domain := "issuer.example.com"
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			"USDC-" + testUSDCIssuer: {
				AssetID: "USDC-" + testUSDCIssuer, Type: "classic",
				Code: "USDC", Issuer: &issuer, HomeDomain: &domain, Decimals: 7,
			},
		},
	}
	cur.Code, cur.Issuer = "USDC", testUSDCIssuer
	sep1 := &stubSep1Cache{byIssuer: map[string]*timescale.IssuerSep1Cached{
		testUSDCIssuer: {Currencies: []timescale.IssuerSep1Currency{cur}},
	}}
	return httpTestServer(t, v1.New(v1.Options{Assets: reader, Sep1Cache: sep1})).URL
}

// /v1/assets/{id}: no projection row and a failed lake read → the cap and
// FDV (supply ÷ 10^decimals × price) are refused, the body is flagged
// stale, and it is NOT cached — the next request reads again.
func TestAssetDetail_DecimalsReadFailed_RefusesCapStaleUncached(t *testing.T) {
	dec := &countingDecStub{err: errDecimalsRead}
	srv := v1.New(v1.Options{
		Prices:        lockstepPrices(),
		Supply:        lockstepSupply(),
		TokenDecimals: dec,
	})
	body := lockstepGet(t, srv)

	for _, field := range []string{`"market_cap_usd":"`, `"fdv_usd":"`} {
		if strings.Contains(body, field) {
			t.Errorf("%s published on a guessed scale of 7: %s", field, body)
		}
	}
	if !strings.Contains(body, `"stale":true`) {
		t.Errorf("flags.stale must be set on an unresolved scale: %s", body)
	}
	first := dec.calls.Load()
	_ = lockstepGet(t, srv)
	if dec.calls.Load() == first {
		t.Error("degraded body was cached: the second request never re-read decimals")
	}
}

// TestAssetList_IssuerDirectoryTags_BatchedNoN1 — the listing joins the
// page's issuer set in ONE batch query (not N+1) and stamps the tags onto
// the row.
func TestAssetList_IssuerDirectoryTags_BatchedNoN1(t *testing.T) {
	aud, err := canonical.NewClassicAsset("AUD", scamAUDIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	other, err := canonical.NewClassicAsset("USDC", testUSDCIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	assetsReader := &listStub{
		stubAssetsReaderExt: &stubAssetsReaderExt{},
		rows: []timescale.AssetRow{
			{AssetID: aud.String(), Code: "AUD", IssuerGStrkey: scamAUDIssuer},
			{AssetID: other.String(), Code: "USDC", IssuerGStrkey: testUSDCIssuer},
		},
	}
	dir := &countingDirectoryReader{entries: scamAUDDirectoryStub().entries}
	srv := v1.New(v1.Options{Assets: &stubAssetReader{}, AssetsReader: assetsReader, Directory: dir})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data []v1.AssetDetail `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var audRow *v1.AssetDetail
	for i := range env.Data {
		if env.Data[i].Code == "AUD" {
			audRow = &env.Data[i]
		}
	}
	if audRow == nil {
		t.Fatalf("AUD row missing from listing: %+v", env.Data)
	}
	if got := audRow.IssuerDirectoryTags; len(got) != 2 || got[0] != "malicious" {
		t.Errorf("listing AUD issuer_directory_tags = %v, want [malicious unsafe]", got)
	}
	if audRow.IssuerDirectoryDomain != scamAUDDomain {
		t.Errorf("listing AUD issuer_directory_domain = %q, want %q", audRow.IssuerDirectoryDomain, scamAUDDomain)
	}
	if n := atomic.LoadInt64(&dir.batchCalls); n != 1 {
		t.Errorf("directory batch calls = %d, want exactly 1 (page issuer set resolved in one query)", n)
	}
	if n := atomic.LoadInt64(&dir.oneCalls); n != 0 {
		t.Errorf("directory single-lookup calls = %d, want 0 (must not N+1 per row)", n)
	}
}

// TestAssetList_DirectoryReadFailure_WithholdsPrice — listing
// side. The batch read failing must not skip the stamp+suppress loop for
// the whole page; every issuer-bearing row must be withheld instead.
func TestAssetList_DirectoryReadFailure_WithholdsPrice(t *testing.T) {
	aud, err := canonical.NewClassicAsset("AUD", scamAUDIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	other, err := canonical.NewClassicAsset("USDC", testUSDCIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	list := func(t *testing.T, dir *stubDirectoryReader) []v1.AssetDetail {
		t.Helper()
		assetsReader := &listStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			rows: []timescale.AssetRow{
				{AssetID: aud.String(), Code: "AUD", IssuerGStrkey: scamAUDIssuer, PriceUSD: sptr("0.65")},
				{AssetID: other.String(), Code: "USDC", IssuerGStrkey: testUSDCIssuer, PriceUSD: sptr("1.00")},
			},
		}
		srv := v1.New(v1.Options{Assets: &stubAssetReader{}, AssetsReader: assetsReader, Directory: dir})
		resp := mustGet(t, httpTestServer(t, srv).URL+"/v1/assets")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		var env struct {
			Data []v1.AssetDetail `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(env.Data) != 2 {
			t.Fatalf("rows = %d, want 2", len(env.Data))
		}
		return env.Data
	}

	// Instrument check: an answering directory with no entries leaves
	// both rows priced, so a nil below is the refusal, not the fixture.
	for _, r := range list(t, &stubDirectoryReader{}) {
		if r.PriceUSD == nil {
			t.Fatalf("precondition: %s unpriced with a healthy, empty directory", r.Code)
		}
	}
	for _, r := range list(t, &stubDirectoryReader{err: errors.New("account_directory: connection refused")}) {
		if r.PriceUSD != nil {
			t.Errorf("%s price_usd = %q, want withheld — the page's scam check did not run", r.Code, *r.PriceUSD)
		}
	}
}
