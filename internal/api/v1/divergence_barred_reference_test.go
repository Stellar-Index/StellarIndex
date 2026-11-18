package v1

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestDivergence_RejectsRedistributionBarredReference: a barred vendor's ref_price must not reach /v1/divergence.
func TestDivergence_RejectsRedistributionBarredReference(t *testing.T) {
	if !external.RedistributionBarred("coinmarketcap") {
		t.Fatal("fixture assumes coinmarketcap is redistribution-barred")
	}
	at := time.Date(2026, 7, 3, 22, 37, 8, 0, time.UTC)
	s := withholdingServer()
	s.Divergences = &withholdingDivergenceReader{latest: []timescale.DivergenceRow{
		{AssetID: "crypto:BTC", QuoteID: "fiat:USD", Reference: "redstone", ObservedAt: at, OurPrice: "100", RefPrice: "99", DeltaPct: "1", Status: "clear"},
		{AssetID: "crypto:BTC", QuoteID: "fiat:USD", Reference: "coinmarketcap", ObservedAt: at, OurPrice: "100", RefPrice: "98.765", DeltaPct: "1.2", Status: "clear"},
	}}
	rec := httptest.NewRecorder()
	s.handleDivergence(rec, httptest.NewRequest(http.MethodGet, "/v1/divergence", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "coinmarketcap") || strings.Contains(body, "98.765") {
		t.Errorf("barred reference served: %s", body)
	}
	if !strings.Contains(body, `"redstone"`) {
		t.Errorf("unbarred reference dropped: %s", body)
	}
}
