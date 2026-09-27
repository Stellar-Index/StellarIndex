package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestDivergence_ServesReferenceObservationTime — /v1/divergence carries
// each row's reference time beside the comparison time (GH-823), and a row
// recorded before it was stored says null rather than borrowing observed_at.
func TestDivergence_ServesReferenceObservationTime(t *testing.T) {
	comparedAt := time.Date(2026, 7, 3, 22, 37, 8, 0, time.UTC)
	refAt := time.Date(2026, 7, 3, 21, 47, 8, 500_000_000, time.FixedZone("UTC+2", 2*3600))
	s := withholdingServer()
	s.divergences = &withholdingDivergenceReader{latest: []timescale.DivergenceRow{
		{
			AssetID: "crypto:BTC", QuoteID: "fiat:USD", Reference: "redstone", ObservedAt: comparedAt,
			OurPrice: "100", RefPrice: "99", DeltaPct: "1", Status: "clear", RefObservedAt: &refAt,
		},
		{
			AssetID: "crypto:ETH", QuoteID: "fiat:USD", Reference: "coingecko", ObservedAt: comparedAt,
			OurPrice: "10", RefPrice: "10", DeltaPct: "0", Status: "clear",
		},
	}}
	rec := httptest.NewRecorder()
	s.handleDivergence(rec, httptest.NewRequest(http.MethodGet, "/v1/divergence", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var got struct {
		Data struct {
			Observations []map[string]json.RawMessage `json:"observations"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]string{
		"redstone":  `"2026-07-03T19:47:08.5Z"`,
		"coingecko": `null`,
	}
	if len(got.Data.Observations) != len(want) {
		t.Fatalf("observations = %d, want %d: %s", len(got.Data.Observations), len(want), rec.Body.String())
	}
	for _, o := range got.Data.Observations {
		var ref string
		_ = json.Unmarshal(o["reference"], &ref)
		raw, present := o["ref_observed_at"]
		if !present {
			t.Errorf("%s: ref_observed_at absent; want %s", ref, want[ref])
			continue
		}
		if string(raw) != want[ref] {
			t.Errorf("%s: ref_observed_at = %s, want %s", ref, raw, want[ref])
		}
	}
}
