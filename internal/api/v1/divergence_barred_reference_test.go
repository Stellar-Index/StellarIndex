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

// TestDivergenceSeries_RejectsRedistributionBarredReference: the series route serves the same reference prices, so the same
// bar applies; a bucket whose only reference is barred is dropped rather than served as our_price beside nothing.
func TestDivergenceSeries_RejectsRedistributionBarredReference(t *testing.T) {
	at := time.Date(2026, 7, 3, 22, 0, 0, 0, time.UTC)
	later := at.Add(time.Hour)
	s := withholdingServer()
	s.Divergences = &withholdingDivergenceReader{points: []timescale.DivergenceSeriesPoint{
		{Bucket: at, Reference: "redstone", LastAt: at, OurPrice: "100", RefPrice: "99", DeltaPct: "1"},
		{Bucket: at, Reference: "coinmarketcap", LastAt: at, OurPrice: "100", RefPrice: "98.765", DeltaPct: "1.2"},
		{Bucket: later, Reference: "coinmarketcap", LastAt: later, OurPrice: "101.5", RefPrice: "97.531", DeltaPct: "4.1", Firing: true},
	}}
	rec := httptest.NewRecorder()
	s.handleDivergenceSeries(rec, httptest.NewRequest(http.MethodGet, "/v1/divergence/series?pair=crypto:BTC~fiat:USD", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, leaked := range []string{"coinmarketcap", "98.765", "97.531", "101.5"} {
		if strings.Contains(body, leaked) {
			t.Errorf("barred reference %q served: %s", leaked, body)
		}
	}
	if !strings.Contains(body, `"redstone"`) || !strings.Contains(body, `"99"`) {
		t.Errorf("unbarred reference dropped: %s", body)
	}
}

// TestDivergence_FullPageIsTruncated: `limit` caps pairs in SQL before barred references are dropped, so a page that hit the
// cap says its length is a lower bound; a page under the cap is the whole list.
func TestDivergence_FullPageIsTruncated(t *testing.T) {
	at := time.Date(2026, 7, 3, 22, 37, 8, 0, time.UTC)
	rows := []timescale.DivergenceRow{
		{AssetID: "crypto:BTC", QuoteID: "fiat:USD", Reference: "redstone", ObservedAt: at, OurPrice: "100", RefPrice: "99", DeltaPct: "1", Status: "clear"},
		{AssetID: "crypto:ETH", QuoteID: "fiat:USD", Reference: "coinmarketcap", ObservedAt: at, OurPrice: "10", RefPrice: "9", DeltaPct: "11", Status: "firing"},
	}
	for _, tc := range []struct {
		limit string
		want  bool
	}{{"2", true}, {"3", false}} {
		s := withholdingServer()
		s.Divergences = &withholdingDivergenceReader{latest: rows}
		rec := httptest.NewRecorder()
		s.handleDivergence(rec, httptest.NewRequest(http.MethodGet, "/v1/divergence?limit="+tc.limit, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("limit=%s: status = %d (body %s)", tc.limit, rec.Code, rec.Body.String())
		}
		if got := strings.Contains(rec.Body.String(), `"truncated":true`); got != tc.want {
			t.Errorf("limit=%s: truncated = %v, want %v (body %s)", tc.limit, got, tc.want, rec.Body.String())
		}
	}
}
