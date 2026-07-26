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

func (f *fakeAnomalyReader) CountFiringFreezes(context.Context) (int64, error) {
	return f.firing, nil
}

// TestAnomalies_FiringCountIsNotPageCapped pins C1-051
// (audit-2026-07-23). firing_count was `len(ListFreezeEvents(ctx, true,
// 500))` — a LIMIT-capped page — so a freeze storm of ANY size above the
// cap reported exactly 500. The number saturates precisely when an
// operator most needs its magnitude.
func TestAnomalies_FiringCountIsNotPageCapped(t *testing.T) {
	const firing = 1337 // well past the 500 page cap
	s := &Server{
		anomalies: &fakeAnomalyReader{firing: firing},
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
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
