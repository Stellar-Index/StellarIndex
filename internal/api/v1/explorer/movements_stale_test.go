package explorer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
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
