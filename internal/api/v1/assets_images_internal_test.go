package v1

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"

	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
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
