package v1

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fakeDivergenceReader records the args the handler passed down so the
// tests can pin the param plumbing (pair split, whitelisted days).
type fakeDivergenceReader struct {
	gotAsset, gotQuote, gotRef string
	gotDays                    int
	points                     []timescale.DivergenceSeriesPoint
}

func (f *fakeDivergenceReader) ListDivergenceLatest(context.Context, int, bool, int) ([]timescale.DivergenceRow, error) {
	return nil, nil
}

func (f *fakeDivergenceReader) ListDivergenceSeries(_ context.Context, assetID, quoteID, reference string, sinceDays int) ([]timescale.DivergenceSeriesPoint, error) {
	f.gotAsset, f.gotQuote, f.gotRef, f.gotDays = assetID, quoteID, reference, sinceDays
	return f.points, nil
}

func newSeriesServer(reader DivergenceReader, threshold float64) *Server {
	return &Server{
		divergences:            reader,
		divergenceThresholdPct: threshold,
		logger:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestDivergenceSeries_HappyPath(t *testing.T) {
	fake := &fakeDivergenceReader{points: []timescale.DivergenceSeriesPoint{
		{Bucket: time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC), DeltaPct: "1.25", OurPrice: "100.5", RefPrice: "99.25", Firing: false},
		{Bucket: time.Date(2026, 7, 30, 12, 30, 0, 0, time.UTC), DeltaPct: "6.4", OurPrice: "106", RefPrice: "99.6", Firing: true},
	}}
	s := newSeriesServer(fake, 5.0)

	rec := httptest.NewRecorder()
	s.handleDivergenceSeries(rec, httptest.NewRequest(http.MethodGet,
		"/v1/divergence/series?pair=crypto:BTC~fiat:USD&reference=coingecko&days=7", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if fake.gotAsset != "crypto:BTC" || fake.gotQuote != "fiat:USD" || fake.gotRef != "coingecko" || fake.gotDays != 7 {
		t.Errorf("reader args = (%q, %q, %q, %d), want (crypto:BTC, fiat:USD, coingecko, 7)",
			fake.gotAsset, fake.gotQuote, fake.gotRef, fake.gotDays)
	}

	var got struct {
		Data DivergenceSeriesView `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if got.Data.ThresholdPct != 5.0 {
		t.Errorf("threshold_pct = %v, want 5.0 (the configured alert band)", got.Data.ThresholdPct)
	}
	if got.Data.BucketSeconds != int(timescale.DivergenceSeriesBucket(7).Seconds()) {
		t.Errorf("bucket_seconds = %d, want the 7d bucket width", got.Data.BucketSeconds)
	}
	if len(got.Data.Points) != 2 {
		t.Fatalf("points = %d, want 2", len(got.Data.Points))
	}
	if got.Data.Points[1].DeltaPct != "6.4" || !got.Data.Points[1].Firing {
		t.Errorf("point[1] = %+v, want delta 6.4 + firing", got.Data.Points[1])
	}
	// ADR-0003: deltas/prices must arrive as JSON strings, not numbers.
	if raw := rec.Body.String(); !json.Valid([]byte(raw)) {
		t.Fatalf("invalid JSON: %s", raw)
	}
}

func TestDivergenceSeries_ParamValidation(t *testing.T) {
	s := newSeriesServer(&fakeDivergenceReader{}, 5.0)
	cases := []struct {
		name, url string
	}{
		{"missing pair", "/v1/divergence/series?reference=coingecko"},
		{"pair without separator", "/v1/divergence/series?pair=crypto:BTC&reference=coingecko"},
		{"empty quote", "/v1/divergence/series?pair=crypto:BTC~&reference=coingecko"},
		{"unknown reference", "/v1/divergence/series?pair=crypto:BTC~fiat:USD&reference=bloomberg"},
		{"missing reference", "/v1/divergence/series?pair=crypto:BTC~fiat:USD"},
		{"non-whitelisted days", "/v1/divergence/series?pair=crypto:BTC~fiat:USD&reference=coingecko&days=90"},
		{"garbage days", "/v1/divergence/series?pair=crypto:BTC~fiat:USD&reference=coingecko&days=x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.handleDivergenceSeries(rec, httptest.NewRequest(http.MethodGet, tc.url, nil))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			// Problem responses must never be cached (cachecontrol.go invariant).
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store on a problem response", cc)
			}
		})
	}
}

func TestDivergenceSeries_NilReaderAndZeroThreshold(t *testing.T) {
	// Nil reader → 200 + empty points (feature-gated like /v1/divergence);
	// zero threshold → threshold_pct omitted, never a fabricated band.
	s := newSeriesServer(nil, 0)
	rec := httptest.NewRecorder()
	s.handleDivergenceSeries(rec, httptest.NewRequest(http.MethodGet,
		"/v1/divergence/series?pair=crypto:BTC~fiat:USD&reference=band", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var raw struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := raw.Data["threshold_pct"]; present {
		t.Error("threshold_pct present with zero config — must be omitted (no invented band)")
	}
	if string(raw.Data["points"]) != "[]" {
		t.Errorf("points = %s, want [] (empty, not null)", raw.Data["points"])
	}
	if string(raw.Data["days"]) != "7" {
		t.Errorf("days = %s, want default 7", raw.Data["days"])
	}
}

// TestAnomalies_IncludeDaily pins the daily block contract: absent
// (JSON null) unless requested; [] when requested with no freezes —
// a client must be able to tell "not served" from "zero freezes".
func TestAnomalies_IncludeDaily(t *testing.T) {
	s := &Server{
		anomalies: &fakeAnomalyReader{daily: []timescale.FreezeDailyReasonCount{
			{Day: time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC), Reason: "divergence", Count: 3},
		}},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// Without include=daily → daily is null.
	rec := httptest.NewRecorder()
	s.handleAnomalies(rec, httptest.NewRequest(http.MethodGet, "/v1/anomalies", nil))
	var got struct {
		Data struct {
			Daily json.RawMessage `json:"daily"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(got.Data.Daily) != "null" {
		t.Errorf("daily without include = %s, want null (not requested)", got.Data.Daily)
	}

	// With include=daily → the tally rows, day formatted YYYY-MM-DD.
	rec = httptest.NewRecorder()
	s.handleAnomalies(rec, httptest.NewRequest(http.MethodGet, "/v1/anomalies?include=daily", nil))
	var got2 struct {
		Data AnomaliesView `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got2.Data.Daily) != 1 || got2.Data.Daily[0].Day != "2026-07-29" ||
		got2.Data.Daily[0].Reason != "divergence" || got2.Data.Daily[0].Count != 3 {
		t.Errorf("daily = %+v, want one 2026-07-29/divergence/3 cell", got2.Data.Daily)
	}
}
