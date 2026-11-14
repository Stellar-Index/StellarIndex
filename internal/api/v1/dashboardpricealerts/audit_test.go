package dashboardpricealerts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

type recordingAudit struct {
	mu      sync.Mutex
	entries []platform.AuditEntry
}

func (s *recordingAudit) Append(_ context.Context, e platform.AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, e)
	return nil
}

func (s *recordingAudit) AppendBatch(ctx context.Context, entries []platform.AuditEntry) error {
	for _, e := range entries {
		_ = s.Append(ctx, e)
	}
	return nil
}

func (s *recordingAudit) List(context.Context, platform.AuditQuery) ([]platform.AuditEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]platform.AuditEntry(nil), s.entries...), nil
}

// TestPriceAlertChanges_WriteAuditRows: create, update and delete each leave one row by the session user.
func TestPriceAlertChanges_WriteAuditRows(t *testing.T) {
	h, _, sc := newTestRig(t, nil)
	sink := &recordingAudit{}
	h.cfg.Audit = sink

	w := httptest.NewRecorder()
	h.HandleCreate(w, sessionReq(t, http.MethodPost, "/v1/dashboard/price-alerts", validCreate(), sc))
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", w.Code, w.Body.String())
	}
	var created priceAlertDTO
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	req := sessionReq(t, http.MethodPatch, "/v1/dashboard/price-alerts/"+created.ID, updateRequest{Threshold: ptr("0.99")}, sc)
	req.SetPathValue("id", created.ID)
	w = httptest.NewRecorder()
	h.HandleUpdate(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("update status = %d, body=%s", w.Code, w.Body.String())
	}

	req = sessionReq(t, http.MethodDelete, "/v1/dashboard/price-alerts/"+created.ID, nil, sc)
	req.SetPathValue("id", created.ID)
	w = httptest.NewRecorder()
	h.HandleDelete(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", w.Code)
	}

	rows, _ := sink.List(context.Background(), platform.AuditQuery{})
	want := []string{"price_alert.create", "price_alert.update", "price_alert.delete"}
	if len(rows) != len(want) {
		t.Fatalf("audit rows = %d, want %d (%+v)", len(rows), len(want), rows)
	}
	for i, e := range rows {
		if e.Action != want[i] || e.ActorKind != platform.ActorUser || e.ActorUserID != sc.User.ID ||
			e.AccountID != sc.Account.ID || e.TargetKind != "price_alert" || e.TargetID != created.ID {
			t.Fatalf("row %d = %+v, want %s on %s by the session user", i, e, want[i], created.ID)
		}
	}
	var meta map[string]any
	if err := json.Unmarshal(rows[1].Metadata, &meta); err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if meta["threshold"] != "0.99" || meta["session_id"] != sc.Session.ID.String() {
		t.Fatalf("update metadata = %v", meta)
	}
}
