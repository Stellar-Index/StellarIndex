package v1

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// errStubSep1Scan stands in for a failing AllSep1Images scan.
var errStubSep1Scan = errors.New("stub: sep1 scan failed")

// stubSep1ImagesReader implements Sep1CachedReader (unused here) and the optional AllSep1Images capability; calls counts
// AllSep1Images invocations so the TTL/single-flight cache is verifiable.
type stubSep1ImagesReader struct {
	imgs  []timescale.Sep1Image
	err   error
	calls int
}

func (s *stubSep1ImagesReader) GetIssuerSep1Cached(context.Context, string) (*timescale.IssuerSep1Cached, error) {
	return nil, sql.ErrNoRows
}

func (s *stubSep1ImagesReader) AllSep1Images(context.Context) ([]timescale.Sep1Image, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.imgs, nil
}

func discardServer(sep1 Sep1CachedReader) *Server {
	return &Server{
		Options: Options{Sep1Cache: sep1},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

const (
	imgIssuerUSDC = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	imgIssuerAQUA = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7YLLES5NK4"
)

func ptr(s string) *string { return &s }

func TestFillImagesFromSep1_Overlay(t *testing.T) {
	stub := &stubSep1ImagesReader{imgs: []timescale.Sep1Image{
		{Code: "USDC", Issuer: imgIssuerUSDC, Image: "https://circle.com/usdc.svg"},
		{Code: "AQUA", Issuer: imgIssuerAQUA, Image: "https://aqua.network/aqua.png"},
		// Hostile scheme — must be filtered out, not surfaced.
		{Code: "EVIL", Issuer: imgIssuerUSDC, Image: "javascript:alert(1)"},
	}}
	s := discardServer(stub)

	preset := "https://preset.example/logo.png"
	rows := []AssetDetail{
		{AssetID: "USDC-" + imgIssuerUSDC, Type: "classic", Code: "USDC", Issuer: ptr(imgIssuerUSDC)},
		// Lower-case code must still match (case-insensitive code join).
		{AssetID: "aqua-" + imgIssuerAQUA, Type: "classic", Code: "aqua", Issuer: ptr(imgIssuerAQUA)},
		// Row already carrying an image must be left untouched.
		{AssetID: "USDC-x", Type: "classic", Code: "USDC", Issuer: ptr(imgIssuerUSDC), Image: ptr(preset)},
		// Native has no issuer → skipped.
		{AssetID: "native", Type: "native", Code: "XLM"},
		// Matching code, unknown issuer → no image.
		{AssetID: "USDC-other", Type: "classic", Code: "USDC", Issuer: ptr("GUNKNOWN")},
		// Hostile-scheme entry must not leak onto the row.
		{AssetID: "EVIL-" + imgIssuerUSDC, Type: "classic", Code: "EVIL", Issuer: ptr(imgIssuerUSDC)},
	}

	// The logo map is filled OFF the request path now (production does this
	// from main.go's prewarm loop), so warm it exactly as production does
	// before asserting on the overlay. A cold map serves no logos by
	// design — that is the trade that keeps the listing off the scan.
	s.PrewarmSep1Images(context.Background())
	s.fillImagesFromSep1(context.Background(), rows)

	if rows[0].Image == nil || *rows[0].Image != "https://circle.com/usdc.svg" {
		t.Errorf("row0 USDC image = %v, want circle logo", rows[0].Image)
	}
	if rows[1].Image == nil || *rows[1].Image != "https://aqua.network/aqua.png" {
		t.Errorf("row1 aqua (lowercase code) image = %v, want aqua logo", rows[1].Image)
	}
	if rows[2].Image == nil || *rows[2].Image != preset {
		t.Errorf("row2 preset image overwritten: %v", rows[2].Image)
	}
	if rows[3].Image != nil {
		t.Errorf("row3 native should have no image: %v", rows[3].Image)
	}
	if rows[4].Image != nil {
		t.Errorf("row4 unknown issuer should have no image: %v", rows[4].Image)
	}
	if rows[5].Image != nil {
		t.Errorf("row5 hostile-scheme image should be filtered: %v", rows[5].Image)
	}
}

// plainSep1Cache satisfies Sep1CachedReader WITHOUT AllSep1Images, so the
// capability type-assertion in cachedSep1Images fails (the overlay-disabled
// / test-stub path).
type plainSep1Cache struct{}

func (plainSep1Cache) GetIssuerSep1Cached(context.Context, string) (*timescale.IssuerSep1Cached, error) {
	return nil, sql.ErrNoRows
}

// TestProjectCatalogueRows_Sep1ImageOverlay: catalogue-sourced listing rows (asset_class=fiat|stablecoin|crypto,
// /v1/external/assets, catalogue phase of asset_class=all) must get the same SEP-1 logo overlay as classic_assets rows, via
// projectCatalogueRows.
func TestProjectCatalogueRows_Sep1ImageOverlay(t *testing.T) {
	stub := &stubSep1ImagesReader{imgs: []timescale.Sep1Image{
		{Code: "USDC", Issuer: imgIssuerUSDC, Image: "https://circle.com/usdc.svg"},
	}}
	s := discardServer(stub)

	matched := []*currency.VerifiedCurrency{
		{
			Ticker: "USDC",
			Slug:   "usdc",
			Name:   "USD Coin",
			Class:  currency.ClassStablecoin,
			Issuance: []currency.IssuanceEntry{
				{Network: "stellar", Code: "USDC", Issuer: imgIssuerUSDC, AssetID: "USDC-" + imgIssuerUSDC},
			},
		},
		{
			// Fiat entry: StellarEntry() == nil (no on-chain issuer to have
			// published a stellar.toml) — must stay imageless, not panic.
			Ticker: "USD",
			Slug:   "us-dollar",
			Name:   "US Dollar",
			Class:  currency.ClassFiat,
		},
		{
			// Stellar issuance present but no matching SEP-1 cache entry.
			Ticker: "AQUA",
			Slug:   "aqua",
			Name:   "Aquarius",
			Class:  currency.ClassCrypto,
			Issuance: []currency.IssuanceEntry{
				{Network: "stellar", Code: "AQUA", Issuer: "GUNVERIFIEDXXX", AssetID: "AQUA-GUNVERIFIEDXXX"},
			},
		},
	}
	caps := make([]string, len(matched))

	s.PrewarmSep1Images(context.Background())
	rows := s.projectCatalogueRows(context.Background(), matched, caps)

	if rows[0].Image == nil || *rows[0].Image != "https://circle.com/usdc.svg" {
		t.Errorf("catalogue USDC row image = %v, want circle logo", rows[0].Image)
	}
	if rows[1].Image != nil {
		t.Errorf("fiat row (no Stellar issuance) should have no image: %v", rows[1].Image)
	}
	if rows[2].Image != nil {
		t.Errorf("catalogue row with no SEP-1 cache match should have no image: %v", rows[2].Image)
	}
}

// TestHandleAssetListFromCatalogue_Sep1ImageOverlay: handler-level check that /v1/assets?asset_class=stablecoin serves USDC's
// real SEP-1 logo using the embedded catalogue, so a revert to the bare projectCatalogueRows call is caught.
func TestHandleAssetListFromCatalogue_Sep1ImageOverlay(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	stub := &stubSep1ImagesReader{imgs: []timescale.Sep1Image{
		{Code: "USDC", Issuer: imgIssuerUSDC, Image: "https://circle.com/usdc.svg"},
	}}
	s := discardServer(stub)
	s.VerifiedCurrencies = cat
	s.PrewarmSep1Images(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/v1/assets?asset_class=stablecoin", nil)
	w := httptest.NewRecorder()
	s.handleAssetListFromCatalogue(w, req, "stablecoin", assetListFilters{}, 100, "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var env struct {
		Data []AssetDetail `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, w.Body.String())
	}
	var found bool
	for _, row := range env.Data {
		if row.Slug != "usdc" {
			continue
		}
		found = true
		if row.Image == nil || *row.Image != "https://circle.com/usdc.svg" {
			t.Errorf("usdc listing row image = %v, want circle logo", row.Image)
		}
	}
	if !found {
		t.Fatal("usdc row not found in asset_class=stablecoin listing")
	}
}

// TestFillImagesFromSep1_CaseTwinsKeepTheirOwnLogo: asset codes are case-sensitive, so USDX and usdx each keep the logo their own
// [[CURRENCIES]] entry declares; a mis-cased row borrows a logo only when exactly one entry could be meant.
func TestFillImagesFromSep1_CaseTwinsKeepTheirOwnLogo(t *testing.T) {
	const upper, lower = "https://x.example/USDX.png", "https://x.example/usdx.png"
	s := discardServer(&stubSep1ImagesReader{imgs: []timescale.Sep1Image{
		{Code: "USDX", Issuer: imgIssuerUSDC, Image: upper},
		{Code: "usdx", Issuer: imgIssuerUSDC, Image: lower},
	}})
	s.PrewarmSep1Images(context.Background())
	rows := []AssetDetail{
		{AssetID: "USDX-" + imgIssuerUSDC, Type: "classic", Code: "USDX", Issuer: ptr(imgIssuerUSDC)},
		{AssetID: "usdx-" + imgIssuerUSDC, Type: "classic", Code: "usdx", Issuer: ptr(imgIssuerUSDC)},
		{AssetID: "Usdx-" + imgIssuerUSDC, Type: "classic", Code: "Usdx", Issuer: ptr(imgIssuerUSDC)},
	}
	s.fillImagesFromSep1(context.Background(), rows)
	if rows[0].Image == nil || *rows[0].Image != upper {
		t.Errorf("USDX image = %v, want %s", rows[0].Image, upper)
	}
	if rows[1].Image == nil || *rows[1].Image != lower {
		t.Errorf("usdx image = %v, want %s", rows[1].Image, lower)
	}
	if rows[2].Image != nil {
		t.Errorf("Usdx matches two entries case-insensitively and must get neither logo, got %s", *rows[2].Image)
	}
}

// TestIsSafeImageURL pins the image URL gate. `image` comes verbatim from an issuer-controlled stellar.toml, and a bare
// http(s):// prefix check would let attribute-breakout characters reach consumers rendering `<img src={data.image}>`.
func TestIsSafeImageURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		// Ordinary issuer icons — must keep working.
		{"https", "https://example.com/icon.png", true},
		{"http", "http://example.com/icon.png", true},
		{"query_and_fragment", "https://cdn.example.com/i.png?v=2#x", true},
		{"percent_encoded_quote", "https://example.com/a%22b.png", true},

		// Scheme allow-list (pre-existing behaviour, kept).
		{"javascript", "javascript:alert(1)", false},
		{"data", "data:image/svg+xml;base64,AAAA", false},
		{"file", "file:///etc/passwd", false},
		{"blob", "blob:https://example.com/uuid", false},
		{"protocol_relative", "//example.com/icon.png", false},
		{"empty", "", false},

		// Attribute breakout — the attribute-injection half. Each of these closes the
		// src attribute (or opens a tag) inside a naive non-escaping
		// template.
		{"double_quote", `https://example.com/a.png" onerror="alert(1)`, false},
		{"single_quote", "https://example.com/a.png' onerror='alert(1)", false},
		{"angle_open", "https://example.com/a.png<script>", false},
		{"angle_close", "https://example.com/a.png>", false},
		{"backtick", "https://example.com/a.png`x`", false},
		{"backslash", `https://example.com\a.png`, false},
		{"space", "https://example.com/a.png onerror=alert(1)", false},
		{"tab", "https://example.com/a.png\tonerror=x", false},
		{"newline", "https://example.com/a.png\nonerror=x", false},
		{"nul", "https://example.com/a.png\x00", false},
		{"del", "https://example.com/a.png\x7f", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSafeImageURL(tc.in); got != tc.want {
				t.Errorf("isSafeImageURL(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
