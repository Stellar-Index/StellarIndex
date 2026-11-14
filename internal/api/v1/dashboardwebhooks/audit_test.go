package dashboardwebhooks

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

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

func (s *recordingAudit) last(t *testing.T, wantRows int) (platform.AuditEntry, map[string]any) {
	t.Helper()
	all, _ := s.List(context.Background(), platform.AuditQuery{})
	if len(all) != wantRows {
		t.Fatalf("audit rows = %d, want %d (%+v)", len(all), wantRows, all)
	}
	e := all[len(all)-1]
	var meta map[string]any
	if err := json.Unmarshal(e.Metadata, &meta); err != nil {
		t.Fatalf("metadata: %v", err)
	}
	return e, meta
}

// TestWebhookChanges_WriteAuditRows: every committed change to where signed data is sent leaves a row naming the
// session, and no row carries the signing secret or the URL's path and query.
func TestWebhookChanges_WriteAuditRows(t *testing.T) {
	h, store, sc := newTestRig(t)
	sink := &recordingAudit{}
	h.cfg.Audit = sink

	w := call(t, h.HandleCreate, http.MethodPost, "", uuid.Nil,
		createBody("ops", "https://hooks.slack.example/services/T/B/tok3n", platform.WebhookEventIncidentSEV1), sc)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", w.Code, w.Body.String())
	}
	var created createResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id := uuid.MustParse(created.Webhook.ID)
	e, meta := sink.last(t, 1)
	if e.Action != "webhook.create" || e.ActorKind != platform.ActorUser || e.ActorUserID != sc.User.ID ||
		e.AccountID != sc.Account.ID || e.TargetKind != "customer_webhook" || e.TargetID != created.Webhook.ID {
		t.Fatalf("create row = %+v", e)
	}
	if meta["url_host"] != "hooks.slack.example" || meta["session_id"] != sc.Session.ID.String() {
		t.Fatalf("create metadata = %v", meta)
	}

	w = call(t, h.HandleUpdate, http.MethodPatch, "", id, updateRequest{URL: strPtr("https://evil.example/in?key=s3cret")}, sc)
	if w.Code != http.StatusOK {
		t.Fatalf("update status = %d, body=%s", w.Code, w.Body.String())
	}
	e, meta = sink.last(t, 2)
	if e.Action != "webhook.update" || e.TargetID != created.Webhook.ID {
		t.Fatalf("update row = %+v", e)
	}
	if meta["url_changed"] != true || meta["url_host"] != "evil.example" || meta["previous_url_host"] != "hooks.slack.example" {
		t.Fatalf("update metadata = %v", meta)
	}

	w = call(t, h.HandleRotateSecret, http.MethodPost, "/rotate-secret", id, nil, sc)
	if w.Code != http.StatusOK {
		t.Fatalf("rotate status = %d, body=%s", w.Code, w.Body.String())
	}
	var rotated rotateSecretResponse
	if err := json.Unmarshal(w.Body.Bytes(), &rotated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if e, _ = sink.last(t, 3); e.Action != "webhook.rotate_secret" || e.TargetID != created.Webhook.ID {
		t.Fatalf("rotate row = %+v", e)
	}

	w = call(t, h.HandleDelete, http.MethodDelete, "", id, nil, sc)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", w.Code)
	}
	if e, _ = sink.last(t, 4); e.Action != "webhook.delete" || e.TargetID != created.Webhook.ID {
		t.Fatalf("delete row = %+v", e)
	}

	all, _ := sink.List(context.Background(), platform.AuditQuery{})
	for _, row := range all {
		for _, secret := range []string{created.Secret, rotated.Secret, "tok3n", "s3cret"} {
			if strings.Contains(string(row.Metadata), secret) {
				t.Fatalf("%s metadata carries a secret: %s", row.Action, row.Metadata)
			}
		}
	}

	// A refused change records nothing.
	stranger := seedHook(store, uuid.New(), nil)
	if w = call(t, h.HandleDelete, http.MethodDelete, "", stranger, nil, sc); w.Code != http.StatusNotFound {
		t.Fatalf("cross-account delete status = %d, want 404", w.Code)
	}
	sink.last(t, 4)
}
