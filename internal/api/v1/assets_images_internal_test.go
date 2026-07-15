package v1

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubSep1ImagesReader implements both Sep1CachedReader (unused here) and
// the optional AllSep1Images capability cachedSep1Images type-asserts for.
// calls counts AllSep1Images invocations so the TTL/single-flight cache is
// verifiable.
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
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		sep1Cache: sep1,
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

func TestCachedSep1Images_TTLSingleQuery(t *testing.T) {
	stub := &stubSep1ImagesReader{imgs: []timescale.Sep1Image{
		{Code: "USDC", Issuer: imgIssuerUSDC, Image: "https://circle.com/usdc.svg"},
	}}
	s := discardServer(stub)

	for i := 0; i < 3; i++ {
		m := s.cachedSep1Images(context.Background())
		if got := m[sep1ImageKey("USDC", imgIssuerUSDC)]; got != "https://circle.com/usdc.svg" {
			t.Fatalf("call %d: image = %q", i, got)
		}
	}
	if stub.calls != 1 {
		t.Errorf("AllSep1Images called %d times; TTL cache should query once", stub.calls)
	}
}

func TestCachedSep1Images_NoReaderIsNoOp(t *testing.T) {
	// sep1Cache that lacks AllSep1Images (plain Sep1CachedReader) → nil map,
	// and fillImagesFromSep1 leaves rows untouched.
	s := discardServer(&plainSep1Cache{})
	if m := s.cachedSep1Images(context.Background()); m != nil {
		t.Errorf("expected nil map when reader lacks AllSep1Images, got %v", m)
	}
	rows := []AssetDetail{{AssetID: "USDC-" + imgIssuerUSDC, Type: "classic", Code: "USDC", Issuer: ptr(imgIssuerUSDC)}}
	s.fillImagesFromSep1(context.Background(), rows)
	if rows[0].Image != nil {
		t.Errorf("row should be untouched when no image reader wired: %v", rows[0].Image)
	}
}

// plainSep1Cache satisfies Sep1CachedReader WITHOUT AllSep1Images, so the
// capability type-assertion in cachedSep1Images fails (the overlay-disabled
// / test-stub path).
type plainSep1Cache struct{}

func (plainSep1Cache) GetIssuerSep1Cached(context.Context, string) (*timescale.IssuerSep1Cached, error) {
	return nil, sql.ErrNoRows
}

// TestProjectCatalogueRows_Sep1ImageOverlay pins BACKLOG #37b: catalogue-
// sourced listing rows (asset_class=fiat|stablecoin|crypto,
// /v1/external/assets, and the catalogue phase of asset_class=all) must
// gain the same SEP-1 logo overlay the classic_assets-backed listing rows
// already got in b8d817f0. Before this change projectCatalogueRows never
// touched Image at all, regardless of what the SEP-1 cache held.
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

// TestHandleAssetListFromCatalogue_Sep1ImageOverlay is the handler-level
// regression: /v1/assets?asset_class=stablecoin must serve USDC's real
// SEP-1 logo end to end, using the actual embedded verified-currency
// catalogue (same pattern as TestCatalogueStatsUseListingReader) so a
// future wiring regression (e.g. someone reverting to the bare
// projectCatalogueRows(matched, caps) call) is caught by CI.
func TestHandleAssetListFromCatalogue_Sep1ImageOverlay(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	stub := &stubSep1ImagesReader{imgs: []timescale.Sep1Image{
		{Code: "USDC", Issuer: imgIssuerUSDC, Image: "https://circle.com/usdc.svg"},
	}}
	s := discardServer(stub)
	s.verifiedCurrencies = cat

	req := httptest.NewRequest(http.MethodGet, "/v1/assets?asset_class=stablecoin", nil)
	w := httptest.NewRecorder()
	s.handleAssetListFromCatalogue(w, req, "stablecoin", 100, "")

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
