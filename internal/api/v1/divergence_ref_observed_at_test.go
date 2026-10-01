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
// each reference's own time beside the comparison time (GH-823), and a row
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
			AssetID: "crypto:BTC", QuoteID: "fiat:USD", Reference: "coingecko", ObservedAt: comparedAt,
			OurPrice: "100", RefPrice: "100", DeltaPct: "0", Status: "clear",
		},
	}}
	rec := httptest.NewRecorder()
	s.handleDivergence(rec, httptest.NewRequest(http.MethodGet, "/v1/divergence", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var got struct {
		Data struct {
			Pairs []struct {
				References []map[string]json.RawMessage `json:"references"`
			} `json:"pairs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]string{
		"redstone":  `"2026-07-03T19:47:08.5Z"`,
		"coingecko": `null`,
	}
	if len(got.Data.Pairs) != 1 || len(got.Data.Pairs[0].References) != len(want) {
		t.Fatalf("want 1 pair with %d references: %s", len(want), rec.Body.String())
	}
	for _, o := range got.Data.Pairs[0].References {
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

// TestDivergence_GroupsReferencesPerPair — a reference's price is served
// only inside its pair, beside our price and the other references; the
// pair-level our_price/observed_at come from the newest comparison and
// the reader's pair order is kept.
func TestDivergence_GroupsReferencesPerPair(t *testing.T) {
	newer := time.Date(2026, 7, 3, 22, 0, 0, 0, time.UTC)
	older := newer.Add(-10 * time.Minute)
	s := withholdingServer()
	s.divergences = &withholdingDivergenceReader{latest: []timescale.DivergenceRow{
		{
			AssetID: "crypto:BTC", QuoteID: "fiat:USD", Reference: "coingecko", ObservedAt: older, ObservedAtLedger: 10,
			OurPrice: "99", RefPrice: "92", DeltaPct: "7.6", Status: "firing",
		},
		{
			AssetID: "crypto:BTC", QuoteID: "fiat:USD", Reference: "chainlink", ObservedAt: newer, ObservedAtLedger: 20,
			OurPrice: "100", RefPrice: "99.5", DeltaPct: "0.5", Status: "clear",
		},
		{
			AssetID: "crypto:ETH", QuoteID: "fiat:USD", Reference: "band", ObservedAt: newer, ObservedAtLedger: 20,
			OurPrice: "10", RefPrice: "10.1", DeltaPct: "-1", Status: "clear",
		},
	}}
	rec := httptest.NewRecorder()
	s.handleDivergence(rec, httptest.NewRequest(http.MethodGet, "/v1/divergence", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var got struct {
		Data DivergenceView `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Data.Pairs) != 2 || got.Data.Pairs[0].AssetID != "crypto:BTC" || got.Data.Pairs[1].AssetID != "crypto:ETH" {
		t.Fatalf("pairs = %+v, want BTC then ETH", got.Data.Pairs)
	}
	btc := got.Data.Pairs[0]
	if btc.OurPrice != "100" || btc.ObservedAtLedger != 20 || btc.ObservedAt != "2026-07-03T22:00:00Z" {
		t.Errorf("BTC pair = (%s, %d, %s), want the newest comparison (100, 20, 22:00Z)", btc.OurPrice, btc.ObservedAtLedger, btc.ObservedAt)
	}
	if len(btc.References) != 2 {
		t.Fatalf("BTC references = %+v, want coingecko + chainlink", btc.References)
	}
	cg := btc.References[0]
	if cg.Reference != "coingecko" || cg.RefPrice != "92" || cg.Status != "firing" || cg.ObservedAt != "2026-07-03T21:50:00Z" {
		t.Errorf("coingecko = %+v, want its own older comparison time", cg)
	}
	if btc.References[1].Reference != "chainlink" || btc.References[1].DeltaPct != "0.5" {
		t.Errorf("chainlink = %+v", btc.References[1])
	}
}
