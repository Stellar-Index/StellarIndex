package v1

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// /v1/anomalies (frozen_value), /v1/divergence and /v1/divergence/series
// (our_price) serve the aggregator's stored price for a market. They must
// apply the decision /v1/price serves under, on both legs, at read time.

type withholdingAnomalyReader struct{ rows []timescale.FreezeEventRow }

func (f *withholdingAnomalyReader) ListFreezeEvents(context.Context, bool, int) ([]timescale.FreezeEventRow, error) {
	return f.rows, nil
}

func (f *withholdingAnomalyReader) FreezeReasonCounts(context.Context, int) ([]timescale.FreezeReasonCount, error) {
	return nil, nil
}

func (f *withholdingAnomalyReader) FreezeDailyReasonCounts(context.Context, int) ([]timescale.FreezeDailyReasonCount, error) {
	return nil, nil
}

func (f *withholdingAnomalyReader) CountFiringFreezes(context.Context) (int64, error) {
	return int64(len(f.rows)), nil
}

type withholdingDivergenceReader struct {
	latest     []timescale.DivergenceRow
	points     []timescale.DivergenceSeriesPoint
	seriesRead bool
}

func (f *withholdingDivergenceReader) ListDivergenceLatest(context.Context, int, bool, int) ([]timescale.DivergenceRow, error) {
	return f.latest, nil
}

func (f *withholdingDivergenceReader) ListDivergenceSeries(context.Context, string, string, string, int) ([]timescale.DivergenceSeriesPoint, error) {
	f.seriesRead = true
	return f.points, nil
}

// withholdingScamGate flags the listed asset ids, on either leg.
type withholdingScamGate map[string]bool

func (g withholdingScamGate) Withheld(_ context.Context, base canonical.Asset, _ string) bool {
	return g[base.String()]
}

func (g withholdingScamGate) WithheldPair(_ context.Context, base, quote canonical.Asset, _ string) bool {
	return g[base.String()] || g[quote.String()]
}

// withholdingFlaggedAsset is a directory-flagged classic asset in this
// file's gate only.
const withholdingFlaggedAsset = "RIO-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"

func withholdingServer() *Server {
	return &Server{
		scam:   withholdingScamGate{withholdingFlaggedAsset: true},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestAnomalies_OmitsWithheldMarketsFrozenValue(t *testing.T) {
	s := withholdingServer()
	s.anomalies = &withholdingAnomalyReader{rows: []timescale.FreezeEventRow{
		{AssetID: withholdingFlaggedAsset, QuoteID: "native", FrozenAt: time.Unix(0, 0), Reason: "stale", FrozenValue: "0.42"},
		{AssetID: "native", QuoteID: withholdingFlaggedAsset, FrozenAt: time.Unix(0, 0), Reason: "stale", FrozenValue: "2.38"},
		{AssetID: "native", QuoteID: "fiat:USD", FrozenAt: time.Unix(0, 0), Reason: "stale", FrozenValue: "0.11"},
	}}
	rec := httptest.NewRecorder()
	s.handleAnomalies(rec, httptest.NewRequest(http.MethodGet, "/v1/anomalies", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var got struct {
		Data AnomaliesView `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Data.Events) != 1 || got.Data.Events[0].FrozenValue != "0.11" || got.Data.Events[0].AssetID != "native" {
		t.Fatalf("events = %+v, want only native/fiat:USD @ 0.11 — a flagged issuer's frozen_value (either leg) was served", got.Data.Events)
	}
	if got.Data.FiringCount != 3 {
		t.Errorf("firing_count = %d, want 3 — the count carries no price and is not filtered", got.Data.FiringCount)
	}
}

func TestDivergence_OmitsWithheldMarketsOurPrice(t *testing.T) {
	s := withholdingServer()
	s.divergences = &withholdingDivergenceReader{latest: []timescale.DivergenceRow{
		{AssetID: withholdingFlaggedAsset, QuoteID: "native", Reference: "coingecko", OurPrice: "0.42", RefPrice: "0.40", DeltaPct: "5"},
		{AssetID: "native", QuoteID: withholdingFlaggedAsset, Reference: "coingecko", OurPrice: "2.38", RefPrice: "2.5", DeltaPct: "-4.8"},
		{AssetID: "crypto:BTC", QuoteID: "fiat:USD", Reference: "coingecko", OurPrice: "100", RefPrice: "99", DeltaPct: "1"},
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
	if len(got.Data.Observations) != 1 || got.Data.Observations[0].OurPrice != "100" {
		t.Fatalf("observations = %+v, want only crypto:BTC/fiat:USD @ 100 — a flagged issuer's our_price (either leg) was served", got.Data.Observations)
	}
}

func TestDivergenceSeries_WithheldMarketIsWithheldProblem(t *testing.T) {
	for _, pair := range []string{withholdingFlaggedAsset + "~native", "native~" + withholdingFlaggedAsset} {
		t.Run(pair, func(t *testing.T) {
			s := withholdingServer()
			reader := &withholdingDivergenceReader{points: []timescale.DivergenceSeriesPoint{
				{Bucket: time.Unix(0, 0), DeltaPct: "5", OurPrice: "0.42", RefPrice: "0.40"},
			}}
			s.divergences = reader
			rec := httptest.NewRecorder()
			s.handleDivergenceSeries(rec, httptest.NewRequest(http.MethodGet,
				"/v1/divergence/series?pair="+pair+"&reference=coingecko", nil))
			if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "price-withheld") {
				t.Fatalf("status = %d body %s, want 404 price-withheld", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "0.42") {
				t.Errorf("withheld response leaked our_price: %s", rec.Body.String())
			}
			if reader.seriesRead {
				t.Error("series was read for a withheld market; the gate must be asked first")
			}
		})
	}
}

func TestDivergenceSeries_UnflaggedMarketStillServes(t *testing.T) {
	s := withholdingServer()
	s.divergences = &withholdingDivergenceReader{points: []timescale.DivergenceSeriesPoint{
		{Bucket: time.Unix(0, 0), DeltaPct: "1", OurPrice: "100", RefPrice: "99"},
	}}
	rec := httptest.NewRecorder()
	s.handleDivergenceSeries(rec, httptest.NewRequest(http.MethodGet,
		"/v1/divergence/series?pair=crypto:BTC~fiat:USD&reference=coingecko", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"our_price":"100"`) {
		t.Fatalf("status = %d body %s, want 200 with our_price 100", rec.Code, rec.Body.String())
	}
}
