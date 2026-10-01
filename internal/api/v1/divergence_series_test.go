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
	gotAsset, gotQuote string
	gotDays            int
	seriesCalls        int
	points             []timescale.DivergenceSeriesPoint
}

func (f *fakeDivergenceReader) ListDivergenceLatest(context.Context, int, bool, int) ([]timescale.DivergenceRow, error) {
	return nil, nil
}

func (f *fakeDivergenceReader) ListDivergenceSeries(_ context.Context, assetID, quoteID string, sinceDays int) ([]timescale.DivergenceSeriesPoint, error) {
	f.gotAsset, f.gotQuote, f.gotDays = assetID, quoteID, sinceDays
	f.seriesCalls++
	return f.points, nil
}

func newSeriesServer(reader DivergenceReader, threshold float64) *Server {
	return &Server{
		divergences:            reader,
		divergenceThresholdPct: threshold,
		logger:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestDivergenceSeries_HappyPath pins the grouped shape: one point per
// bucket carrying our price beside every reference, with our_price from
// the bucket's newest observation.
func TestDivergenceSeries_HappyPath(t *testing.T) {
	b0 := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	b1 := b0.Add(30 * time.Minute)
	fake := &fakeDivergenceReader{points: []timescale.DivergenceSeriesPoint{
		{Bucket: b0, Reference: "chainlink", LastAt: b0.Add(20 * time.Minute), DeltaPct: "1.1", OurPrice: "100.6", RefPrice: "99.5"},
		{Bucket: b0, Reference: "coingecko", LastAt: b0.Add(25 * time.Minute), DeltaPct: "1.25", OurPrice: "100.5", RefPrice: "99.25"},
		{Bucket: b1, Reference: "chainlink", LastAt: b1.Add(5 * time.Minute), DeltaPct: "0.2", OurPrice: "106", RefPrice: "105.8"},
		{Bucket: b1, Reference: "coingecko", LastAt: b1.Add(5 * time.Minute), DeltaPct: "6.4", OurPrice: "106", RefPrice: "99.6", Firing: true},
	}}
	s := newSeriesServer(fake, 5.0)

	rec := httptest.NewRecorder()
	s.handleDivergenceSeries(rec, httptest.NewRequest(http.MethodGet,
		"/v1/divergence/series?pair=crypto:BTC~fiat:USD&days=7", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if fake.gotAsset != "crypto:BTC" || fake.gotQuote != "fiat:USD" || fake.gotDays != 7 || fake.seriesCalls != 1 {
		t.Errorf("reader args = (%q, %q, %d) ×%d, want one read of (crypto:BTC, fiat:USD, 7)",
			fake.gotAsset, fake.gotQuote, fake.gotDays, fake.seriesCalls)
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
		t.Fatalf("points = %d, want 2 (one per bucket)", len(got.Data.Points))
	}
	p0, p1 := got.Data.Points[0], got.Data.Points[1]
	if p0.OurPrice != "100.5" {
		t.Errorf("point[0].our_price = %q, want 100.5 (the bucket's newest observation)", p0.OurPrice)
	}
	if len(p0.References) != 2 || p0.References[0].Reference != "chainlink" || p0.References[1].Reference != "coingecko" {
		t.Errorf("point[0].references = %+v, want chainlink + coingecko", p0.References)
	}
	if len(p1.References) != 2 || p1.References[1].DeltaPct != "6.4" || !p1.References[1].Firing || p1.References[0].Firing {
		t.Errorf("point[1].references = %+v, want coingecko firing at 6.4 beside a clear chainlink", p1.References)
	}
}

// TestDivergenceSeries_NoSingleReferenceMode — one reference cannot be
// selected alone: ?reference= is refused, never silently honoured.
func TestDivergenceSeries_NoSingleReferenceMode(t *testing.T) {
	fake := &fakeDivergenceReader{}
	s := newSeriesServer(fake, 5.0)
	for _, ref := range []string{"coingecko", "chainlink", "band"} {
		rec := httptest.NewRecorder()
		s.handleDivergenceSeries(rec, httptest.NewRequest(http.MethodGet,
			"/v1/divergence/series?pair=crypto:BTC~fiat:USD&reference="+ref, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("reference=%s: status = %d, want 400 (body %s)", ref, rec.Code, rec.Body.String())
		}
	}
	if fake.seriesCalls != 0 {
		t.Errorf("reader called %d times for refused requests", fake.seriesCalls)
	}
}

func TestDivergenceSeries_ParamValidation(t *testing.T) {
	s := newSeriesServer(&fakeDivergenceReader{}, 5.0)
	cases := []struct {
		name, url string
	}{
		{"missing pair", "/v1/divergence/series"},
		{"pair without separator", "/v1/divergence/series?pair=crypto:BTC"},
		{"empty quote", "/v1/divergence/series?pair=crypto:BTC~"},
		{"non-whitelisted days", "/v1/divergence/series?pair=crypto:BTC~fiat:USD&days=90"},
		{"garbage days", "/v1/divergence/series?pair=crypto:BTC~fiat:USD&days=x"},
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
		"/v1/divergence/series?pair=crypto:BTC~fiat:USD", nil))
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
