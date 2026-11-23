package explorer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A ?asset= SAC id whose lookup errored cannot be matched to the archive's stored spelling, so every archive row for it is
// missing: the page is a retryable 503, never a short 200 whose absent next_cursor ends the walk.
func TestAccountMovements_AssetFilterLookupFailureIs503(t *testing.T) {
	wm := timescale.MovementsFloor() + 1000
	reader := &sacLookupErrReader{&movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}, wm: wm}}
	h := newProbeHandler(reader, nil)
	h.SEP41Movements = &stubSEP41Tail{}
	h.WriteJSONAt = func(w http.ResponseWriter, _ any, _, _ bool, _ time.Time) { w.WriteHeader(http.StatusOK) }
	r := httptest.NewRequest(http.MethodGet, "/v1/accounts/"+validTestAccount+"/movements?asset="+validTestContract, nil)
	r.SetPathValue("g_strkey", validTestAccount)
	rec := httptest.NewRecorder()
	h.AccountMovements(rec, r)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status = %d, Retry-After %q; want a retryable 503 when the ?asset= SAC lookup failed", rec.Code, rec.Header().Get("Retry-After"))
	}
}

// The cap67 watermark proves P23..wm only; nothing records how far the pre-P23 classic backfill ran, so the note must
// neither claim all history through wm nor imply pre-P23 is fully served.
func TestMovementsNotes_DoNotClaimPreP23Archive(t *testing.T) {
	floor := timescale.MovementsFloor()
	notes := map[string]string{
		"archive":     movementsCoverageNote(floor+1000, "", supplyRange{}),
		"create kind": movementsKindNote("create_account", supplyRange{}),
		"clawback":    movementsKindNote("clawback", supplyRange{}),
	}
	for name, note := range notes {
		if strings.Contains(note, "all assets through ledger") {
			t.Errorf("%s note claims all history through the watermark: %q", name, note)
		}
		if !strings.Contains(note, "classic-movements-backfill") {
			t.Errorf("%s note does not disclose that pre-P23 history is only as complete as the classic backfill: %q", name, note)
		}
	}
}
