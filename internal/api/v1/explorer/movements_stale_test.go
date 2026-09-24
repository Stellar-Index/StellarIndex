package explorer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// movementsEnvelope runs AccountMovements with the lake tip at lakeTip and
// the cap67 archive watermark at wm, returning the coverage note and the
// envelope's stale flag.
func movementsEnvelope(t *testing.T, wm, lakeTip uint32) (string, bool) {
	t.Helper()
	reader := &movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}, wm: wm}
	h := newProbeHandler(reader, nil)
	h.SEP41Movements = &stubSEP41Tail{}
	h.LakeWatermark = func(context.Context) (uint32, bool, bool) { return lakeTip, false, true }
	var note string
	var stale, wrote bool
	h.WriteJSON = func(w http.ResponseWriter, v any, s bool) {
		note, stale, wrote = v.(AccountMovementsView).CoverageNote, s, true
		w.WriteHeader(http.StatusOK)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/accounts/"+validTestAccount+"/movements", nil)
	r.SetPathValue("g_strkey", validTestAccount)
	h.AccountMovements(httptest.NewRecorder(), r)
	if !wrote {
		t.Fatal("AccountMovements wrote no envelope")
	}
	return note, stale
}

// TestAccountMovements_StaleWhenArchiveTrailsLake pins #1299: the feed's
// all-assets boundary is the cap67 derive watermark, so a derive that
// stopped (or is parked at a lake hole) while the lake moved on must raise
// flags.stale — it was hardcoded false.
func TestAccountMovements_StaleWhenArchiveTrailsLake(t *testing.T) {
	wm := timescale.SEP41MovementsFloorLedger + 10_000
	if _, stale := movementsEnvelope(t, wm, wm+10*cap67MovementsStaleLedgers); !stale {
		t.Error("flags.stale = false with the archive 600 ledgers behind the lake tip, want true")
	}
	if _, stale := movementsEnvelope(t, wm, wm+5); stale {
		t.Error("flags.stale = true with the archive 5 ledgers behind the lake tip (a live follow daemon)")
	}
}

// TestMovementsCoverageNote_DoesNotAssertUnverifiedCompleteness pins the
// prose half of #1299: account_movements has no completeness verdict, so
// the note may name the archive boundary but must not call it complete.
func TestMovementsCoverageNote_DoesNotAssertUnverifiedCompleteness(t *testing.T) {
	note := movementsCoverageNote(64_400_000, "")
	if strings.Contains(note, "complete for") {
		t.Errorf("note %q asserts completeness for an archive no reconcile verifies", note)
	}
	if !strings.Contains(note, "ledger 64400000") || !strings.Contains(note, "not a verified completeness verdict") {
		t.Errorf("note %q must name the boundary ledger and say it is not a verdict", note)
	}
}
