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

// fakeAnomalyReader is a store double whose firing set is larger than any
// page cap, so the handler's firing_count can be checked against the true
// population rather than against a page length.
type fakeAnomalyReader struct {
	firing int64
	daily  []timescale.FreezeDailyReasonCount
}

func (f *fakeAnomalyReader) ListFreezeEvents(_ context.Context, firingOnly bool, limit int) ([]timescale.FreezeEventRow, error) {
	// Mirrors the store: newest-first, LIMIT-capped. That cap is exactly
	// what made the old len()-derived count wrong.
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	n := int(f.firing)
	if !firingOnly {
		n = int(f.firing)
	}
	if n > limit {
		n = limit
	}
	out := make([]timescale.FreezeEventRow, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, timescale.FreezeEventRow{
			AssetID: "AST:GISSUER", QuoteID: "USD",
			FrozenAt: time.Unix(0, 0).UTC(), FrozenAtLedger: int64(i),
			Reason: "stale", FrozenValue: "1",
		})
	}
	return out, nil
}

func (f *fakeAnomalyReader) FreezeReasonCounts(context.Context, int) ([]timescale.FreezeReasonCount, error) {
	return nil, nil
}

func (f *fakeAnomalyReader) FreezeDailyReasonCounts(context.Context, int) ([]timescale.FreezeDailyReasonCount, error) {
	return f.daily, nil
}

func (f *fakeAnomalyReader) CountFiringFreezes(context.Context) (int64, error) {
	return f.firing, nil
}

// TestAnomalies_FiringCountIsNotPageCapped pins that firing_count is a true count, not a
// LIMIT-capped page (`len(ListFreezeEvents(ctx, true, 500))`), which would
// report exactly 500 for a freeze storm of ANY size above the cap, saturating
// precisely when an operator most needs the magnitude.
func TestAnomalies_FiringCountIsNotPageCapped(t *testing.T) {
	const firing = 1337 // well past the 500 page cap
	s := &Server{
		Options: Options{Anomalies: &fakeAnomalyReader{firing: firing}},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	rec := httptest.NewRecorder()
	s.handleAnomalies(rec, httptest.NewRequest(http.MethodGet, "/v1/anomalies", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	var got struct {
		Data AnomaliesView `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if got.Data.FiringCount != firing {
		t.Errorf("firing_count = %d, want %d — the count must be the true population, "+
			"not the length of a capped page (500 would mean it saturated)",
			got.Data.FiringCount, firing)
	}
	// The event PAGE is still capped — that half is correct and must stay.
	if len(got.Data.Events) != 100 {
		t.Errorf("events page = %d, want 100 (the default limit) — the page cap is not the bug", len(got.Data.Events))
	}
}

// TestAnomalies_WindowDaysOutOfRangeRejects pins that
// an out-of-range or unparseable ?window_days= must not silently fall
// back to the default (30) instead of clamping to [1, 365] as the doc
// comment claimed, or rejecting like the sibling ?days=/?limit= params
// do. A client asking for a one-year window got one month back with no
// signal it was ignored.
func TestAnomalies_WindowDaysOutOfRangeRejects(t *testing.T) {
	s := &Server{
		Options: Options{Anomalies: &fakeAnomalyReader{firing: 1}},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	for _, raw := range []string{"400", "0", "abc"} {
		rec := httptest.NewRecorder()
		s.handleAnomalies(rec, httptest.NewRequest(http.MethodGet, "/v1/anomalies?window_days="+raw, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("window_days=%s: status = %d, want 400 (body %s)", raw, rec.Code, rec.Body.String())
		}
	}

	// A valid boundary value must still work.
	rec := httptest.NewRecorder()
	s.handleAnomalies(rec, httptest.NewRequest(http.MethodGet, "/v1/anomalies?window_days=365", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("window_days=365: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestAnomalies_OmitsWithheldMarketsFrozenValue(t *testing.T) {
	s := withholdingServer()
	s.Anomalies = &withholdingAnomalyReader{rows: []timescale.FreezeEventRow{
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

// TestAnomalies_IncludeDaily pins the daily block contract: absent
// (JSON null) unless requested; [] when requested with no freezes —
// a client must be able to tell "not served" from "zero freezes".
func TestAnomalies_IncludeDaily(t *testing.T) {
	s := &Server{
		Options: Options{Anomalies: &fakeAnomalyReader{daily: []timescale.FreezeDailyReasonCount{
			{Day: time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC), Reason: "divergence", Count: 3},
		}}},
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
