package explorer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// GH-1067: a wedged lake (LakeWatermark reporting stale) must be reflected
// in flags.stale for every explorer handler, not just the handful that
// already consulted it (account_state.go, contracts.go, operations.go,
// movements.go). Before this fix LedgersList (and its siblings sharing the
// same hardcoded WriteJSON(w, out, false)) served a wedged lake as fresh.
func TestLedgersList_ReflectsLakeStaleness(t *testing.T) {
	probe := &deadlineProbe{}
	h := newProbeHandler(&capReader{probe: probe}, nil)
	h.LakeWatermark = func(_ context.Context) (uint32, bool, bool) { return 100, true, true }

	var gotStale bool
	var staleSet bool
	h.WriteJSON = func(w http.ResponseWriter, _ any, stale bool) {
		gotStale, staleSet = stale, true
		w.WriteHeader(http.StatusOK)
	}

	r := httptest.NewRequest(http.MethodGet, "/v1/ledgers", nil)
	rec := httptest.NewRecorder()
	h.LedgersList(rec, r)

	if !staleSet {
		t.Fatalf("WriteJSON was never called")
	}
	if !gotStale {
		t.Fatalf("LedgersList served flags.stale=false while LakeWatermark reported the lake stale")
	}
}
