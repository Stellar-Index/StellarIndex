package v1

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAnomalies_WindowDaysOutOfRangeRejects pins CA2-A01-correct-6:
// an out-of-range or unparseable ?window_days= used to silently fall
// back to the default (30) instead of clamping to [1, 365] as the doc
// comment claimed, or rejecting like the sibling ?days=/?limit= params
// do. A client asking for a one-year window got one month back with no
// signal it was ignored.
func TestAnomalies_WindowDaysOutOfRangeRejects(t *testing.T) {
	s := &Server{
		anomalies: &fakeAnomalyReader{firing: 1},
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
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
