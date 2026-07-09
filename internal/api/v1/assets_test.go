package v1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/StellarIndex/stellar-index/internal/api/v1"
	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
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

func TestAssetList_EmptyWhenReaderNil(t *testing.T) {
	// Prove the default "reader not wired yet" path is 200 + empty.
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data []v1.AssetDetail `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 0 {
		t.Errorf("expected empty list, got %d rows", len(env.Data))
	}
}

func TestAssetList_NilSliceFromReaderMarshalsAsEmptyArray(t *testing.T) {
	// Regression: a reader returning (nil, "", nil) must not leak
	// "data": null onto the wire — OpenAPI's AssetListEnvelope.data
	// is `type: array`, which rejects null. The handler's nil guard
	// converts nil → [].
	reader := &stubAssetReader{page: nil, nextCur: ""}
	srv := v1.New(v1.Options{Assets: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := readAll(resp)
	// Don't decode through []T — that hides null. Assert on the raw
	// bytes that the field is an empty array.
	if !bytes.Contains([]byte(body), []byte(`"data":[]`)) {
		t.Errorf("expected \"data\":[] in body, got: %s", body)
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

func TestAssetGet_native(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/native")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data.AssetID != "native" || env.Data.Type != "native" {
		t.Errorf("wrong asset: %+v", env.Data)
	}
}

func TestAssetGet_classicEcho(t *testing.T) {
	srv := v1.New(v1.Options{}) // nil reader → canonical-echo path
	ts := httpTestServer(t, srv)

	url := ts.URL + "/v1/assets/USDC-" + testUSDCIssuer
	resp := mustGet(t, url)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data.Type != "classic" || env.Data.Code != "USDC" {
		t.Errorf("wrong decode: %+v", env.Data)
	}
	if env.Data.Issuer == nil || *env.Data.Issuer != testUSDCIssuer {
		t.Errorf("issuer missing: %+v", env.Data.Issuer)
	}
}

func TestAssetGet_fiatVariant(t *testing.T) {
	// ADR-0010: fiat:USD is a first-class asset.
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data.Type != "fiat" || env.Data.Code != "USD" {
		t.Errorf("fiat decode wrong: %+v", env.Data)
	}
}

func TestAssetGet_invalidIdReturns400(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/garbage-but-not-any-format")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("content-type = %q, want problem+json", ct)
	}
}

func TestAssetGet_notFound(t *testing.T) {
	reader := &stubAssetReader{byID: map[string]v1.AssetDetail{}}
	srv := v1.New(v1.Options{Assets: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/native")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, "asset-not-found") {
		t.Errorf("body missing error type: %s", body)
	}
}

func TestAssetGet_readerPopulatesSep1Status(t *testing.T) {
	// When the reader returns a detail, we use its fields verbatim
	// (vs canonical-echo which fills defaults).
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
			},
		},
	}
	srv := v1.New(v1.Options{Assets: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/USDC-"+testUSDCIssuer)
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data.Sep1Status != "verified" || env.Data.HomeDomain == nil {
		t.Fatalf("reader fields lost: %+v", env.Data)
	}
}

// TestAssetGet_Kind_SetForReaderPathAndSurvivesResponseCache proves
// the ADR-0042 LC-040 cache trap is closed. stubAssetReader — like
// the real storage.timescale AssetReader implementation — has no
// reason to know about the `kind` wire-shape discriminator, so its
// fixture row below deliberately carries a zero-value Kind, the same
// as a not-yet-updated storage layer would. handleAssetGet must stamp
// Kind AFTER resolveAssetDetail returns but BEFORE renderAssetDetailEnvelope
// caches the rendered bytes (assets.go's 30s assetDetailCache) — a fix
// applied only on the FIRST response would leave the cached bytes
// permanently missing `kind` for the remainder of the TTL window. This
// test issues the request twice: once to populate the cache, once to
// hit it, and requires `kind` on both.
func TestAssetGet_Kind_SetForReaderPathAndSurvivesResponseCache(t *testing.T) {
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			"native": {
				// Kind intentionally left zero-valued — simulates a
				// storage-layer AssetDetail that doesn't set it.
				AssetID:    "native",
				Type:       "native",
				Code:       "XLM",
				Decimals:   7,
				Sep1Status: "not_applicable",
			},
		},
	}
	srv := v1.New(v1.Options{Assets: reader})
	ts := httpTestServer(t, srv)

	for i, label := range []string{"first (uncached)", "second (cache hit)"} {
		resp := mustGet(t, ts.URL+"/v1/assets/native")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d", label, resp.StatusCode)
		}
		var env struct {
			Data v1.AssetDetail `json:"data"`
		}
		mustDecode(t, resp, &env)
		if env.Data.Kind != "stellar_asset" {
			t.Errorf("%s request (i=%d): kind = %q, want \"stellar_asset\" — the reader.GetAsset path must have Kind stamped before the response is cached, not left to the (Kind-unaware) storage layer", label, i, env.Data.Kind)
		}
	}
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

// TestAssetGet_BackfillsHomeDomainFromKnownIssuersMap exercises R-016.
// Prod live snapshot:
//
//	GET /v1/assets/USDC-G… → home_domain=null, sep1_status=not_applicable
//	GET /v1/issuers/G…     → home_domain="centre.io"
//
// Two surfaces disagreed on whether SEP-1 metadata existed for the
// same issuer. Root cause: the storage row for the asset doesn't
// carry a home_domain (the watched-set sep1-refresh worker
// populates it asynchronously and may not have run yet on a fresh
// deployment), so the SEP-1 overlay step short-circuited to
// "not_applicable". /v1/issuers, by contrast, runs every row
// through `enrichIssuer` which has a hand-curated fallback. The
// fix mirrors that policy on /v1/assets.
func TestAssetGet_BackfillsHomeDomainFromKnownIssuersMap(t *testing.T) {
	issuer := testUSDCIssuer
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			"USDC-" + testUSDCIssuer: {
				AssetID:    "USDC-" + testUSDCIssuer,
				Type:       "classic",
				Code:       "USDC",
				Issuer:     &issuer,
				HomeDomain: nil, // storage row didn't carry one
				Decimals:   7,
				// Sep1Status intentionally empty — we want the handler
				// path to compute it AFTER the known-issuers backfill.
			},
		},
	}
	srv := v1.New(v1.Options{Assets: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/USDC-"+testUSDCIssuer)
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.HomeDomain == nil || *env.Data.HomeDomain != "centre.io" {
		t.Errorf("HomeDomain = %v, want centre.io (from known_issuers map)", env.Data.HomeDomain)
	}
	// With no metadata resolver wired the status should advance from
	// the empty default to "not_fetched" — distinct from the
	// pre-fix "not_applicable" which incorrectly claimed the issuer
	// has no home-domain at all.
	if env.Data.Sep1Status != "not_fetched" {
		t.Errorf("Sep1Status = %q, want not_fetched (resolver not wired but home_domain known)", env.Data.Sep1Status)
	}
}

// TestAssetMetadata_BackfillsHomeDomainFromKnownIssuersMap is the
// /v1/assets/{id}/metadata variant of the same fix — the two
// surfaces share the same backfill so consumers see identical
// SEP-1 status across them.
func TestAssetMetadata_BackfillsHomeDomainFromKnownIssuersMap(t *testing.T) {
	issuer := testUSDCIssuer
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			"USDC-" + testUSDCIssuer: {
				AssetID: "USDC-" + testUSDCIssuer, Type: "classic", Code: "USDC",
				Issuer: &issuer, HomeDomain: nil, Decimals: 7,
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

	if env.Data.HomeDomain == nil || *env.Data.HomeDomain != "centre.io" {
		t.Errorf("HomeDomain = %v, want centre.io", env.Data.HomeDomain)
	}
	if env.Data.Sep1Status != "not_fetched" {
		t.Errorf("Sep1Status = %q, want not_fetched", env.Data.Sep1Status)
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

func TestAssetList_ReaderError500(t *testing.T) {
	reader := &stubAssetReader{err: errors.New("storage broke")}
	srv := v1.New(v1.Options{Assets: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

func TestAssetGet_ReaderError500(t *testing.T) {
	// Reader returning a non-NotFound error → 500 with the
	// internal error-type URL. Previously the only reader-returning
	// test path returned ErrAssetNotFound.
	reader := &stubAssetReader{err: errors.New("storage broke")}
	srv := v1.New(v1.Options{Assets: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/native")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
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
	assetsReader := &stubAssetsReaderExt{}
	// stubAssetsReaderExt.ListAssetsExt returns nil — override by
	// constructing a custom struct inline.
	listReader := &listingStub{rows: []timescale.AssetRow{assetRow}}
	srv := v1.New(v1.Options{AssetsReader: listReader, Assets: &stubAssetReader{}})
	_ = assetsReader
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

func TestAssetList_FromAssetsReader_IssuerFilter(t *testing.T) {
	// ?issuer=G should pass through to the AssetsReader's Issuer
	// option. Stub records what was passed.
	listReader := &listingStub{}
	srv := v1.New(v1.Options{AssetsReader: listReader, Assets: &stubAssetReader{}})
	ts := httpTestServer(t, srv)
	mustGet(t, ts.URL+"/v1/assets?issuer="+testUSDCIssuer)
	if listReader.lastOpts.Issuer != testUSDCIssuer {
		t.Errorf("ListAssetsExt called with Issuer=%q, want %q", listReader.lastOpts.Issuer, testUSDCIssuer)
	}
}

func TestAssetList_FromAssetsReader_CodeFilter(t *testing.T) {
	// ?code=USDC pushes down to the AssetsReader's Code option
	// (BACKLOG #54). Stub records what was passed.
	listReader := &listingStub{}
	srv := v1.New(v1.Options{AssetsReader: listReader, Assets: &stubAssetReader{}})
	ts := httpTestServer(t, srv)
	mustGet(t, ts.URL+"/v1/assets?code=USDC")
	if listReader.lastOpts.Code != "USDC" {
		t.Errorf("ListAssetsExt called with Code=%q, want %q", listReader.lastOpts.Code, "USDC")
	}
	if listReader.lastOpts.Issuer != "" {
		t.Errorf("Issuer must be empty when only code is set; got %q", listReader.lastOpts.Issuer)
	}
}

func TestAssetList_FromAssetsReader_IssuerAndCodeCombine(t *testing.T) {
	// ?issuer=G&code=USDC — both filters combine (the "pin one
	// classic asset" case). BACKLOG #54.
	listReader := &listingStub{}
	srv := v1.New(v1.Options{AssetsReader: listReader, Assets: &stubAssetReader{}})
	ts := httpTestServer(t, srv)
	mustGet(t, ts.URL+"/v1/assets?issuer="+testUSDCIssuer+"&code=USDC")
	if listReader.lastOpts.Issuer != testUSDCIssuer || listReader.lastOpts.Code != "USDC" {
		t.Errorf("ListAssetsExt opts = {Issuer:%q Code:%q}, want {%q USDC}",
			listReader.lastOpts.Issuer, listReader.lastOpts.Code, testUSDCIssuer)
	}
}

func TestAssetList_TypeClassic_PassesThrough(t *testing.T) {
	// type=classic is a no-op on the classic-only assetsReader listing:
	// the reader is still called and its rows are returned.
	listReader := &listingStub{rows: []timescale.AssetRow{{
		AssetID: "USDC-" + testUSDCIssuer, Code: "USDC", Slug: "usdc",
	}}}
	srv := v1.New(v1.Options{AssetsReader: listReader, Assets: &stubAssetReader{}})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/assets?type=classic")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var env struct {
		Data []v1.AssetDetail `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 1 {
		t.Fatalf("type=classic must pass through; got %d rows want 1", len(env.Data))
	}
}

func TestAssetList_TypeNative_ShortCircuitsEmpty(t *testing.T) {
	// type=native (or soroban/fiat) matches nothing on the classic-
	// only assetsReader listing → empty page WITHOUT hitting the reader
	// (BACKLOG #54 type fold). The stub is seeded with a row that
	// must NOT surface.
	listReader := &listingStub{rows: []timescale.AssetRow{{
		AssetID: "USDC-" + testUSDCIssuer, Code: "USDC", Slug: "usdc",
	}}}
	srv := v1.New(v1.Options{AssetsReader: listReader, Assets: &stubAssetReader{}})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/assets?type=native")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var env struct {
		Data []v1.AssetDetail `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 0 {
		t.Fatalf("type=native must short-circuit empty; got %d rows", len(env.Data))
	}
	// Reader must not have been called at all (lastOpts stays zero).
	if listReader.lastOpts.Limit != 0 {
		t.Errorf("type=native must NOT call ListAssetsExt; lastOpts.Limit=%d", listReader.lastOpts.Limit)
	}
}

func TestAssetList_InvalidFilters_400(t *testing.T) {
	// Malformed type / code / issuer 400 up front (BACKLOG #54),
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

func TestAssetList_TypeAny_NoFilter(t *testing.T) {
	// type=any is the documented "disable the filter" value — it must
	// pass through identically to omitting type.
	listReader := &listingStub{rows: []timescale.AssetRow{{
		AssetID: "USDC-" + testUSDCIssuer, Code: "USDC", Slug: "usdc",
	}}}
	srv := v1.New(v1.Options{AssetsReader: listReader, Assets: &stubAssetReader{}})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/assets?type=any")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var env struct {
		Data []v1.AssetDetail `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 1 {
		t.Fatalf("type=any must be a no-op; got %d rows want 1", len(env.Data))
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
