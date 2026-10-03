package dashboardwebhooks

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/dashboardauth"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

func rotateViaMount(t *testing.T, mux *http.ServeMux, sc dashboardauth.SessionContext, id uuid.UUID, idemKey string) *httptest.ResponseRecorder {
	t.Helper()
	req := sessionReq(t, http.MethodPost, "/v1/dashboard/webhooks/"+id.String()+"/rotate-secret", nil, sc)
	req.Host = "api.stellarindex.io"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("Origin", "https://api.stellarindex.io")
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// TestMount_RotateSecret_KeepsWebhookAndQueue: rotation replaces the key
// on the SAME webhook, keeps the old key for the overlap window, and
// leaves the queued deliveries in place — the delete + recreate it
// replaces lost all three.
func TestMount_RotateSecret_KeepsWebhookAndQueue(t *testing.T) {
	h, store, sc := newTestRig(t)
	mux := http.NewServeMux()
	h.Mount(mux, middleware.NewPublicRoutes())

	id := uuid.New()
	oldKey := []byte("wsec_old")
	store.webhooks[id] = platform.CustomerWebhook{
		ID: id, AccountID: sc.Account.ID, SecretHash: oldKey,
		URL: "https://ok.example", Events: []string{"incident.sev1"}, Enabled: true,
	}
	store.deliveries[id] = []platform.WebhookDelivery{{ID: uuid.New(), WebhookID: id, EventType: "incident.sev1"}}

	w := rotateViaMount(t, mux, sc, id, "rotate-1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp rotateSecretResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.WebhookID != id.String() {
		t.Errorf("webhook_id = %q, want %s", resp.WebhookID, id)
	}
	wantExpiry := h.cfg.Now().Add(previousSecretOverlap)
	if !time.Time(resp.PreviousSecretExpiresAt).Equal(wantExpiry) {
		t.Errorf("previous_secret_expires_at = %v, want %v", time.Time(resp.PreviousSecretExpiresAt), wantExpiry)
	}

	got := store.webhooks[id]
	if string(got.SecretHash) != resp.Secret || resp.Secret == string(oldKey) || len(resp.Secret) < 10 {
		t.Errorf("stored key %q, returned %q: want the fresh returned key stored", got.SecretHash, resp.Secret)
	}
	if string(got.PreviousSecret) != string(oldKey) || !got.PreviousSecretExpiresAt.Equal(wantExpiry) {
		t.Errorf("previous = (%q, %v), want the old key until %v", got.PreviousSecret, got.PreviousSecretExpiresAt, wantExpiry)
	}
	if len(store.deliveries[id]) != 1 {
		t.Errorf("queued deliveries = %d after rotate, want the 1 queued before it", len(store.deliveries[id]))
	}

	// A retried rotate must not rotate again: that would push the key the
	// receiver still holds out of the overlap.
	replay := rotateViaMount(t, mux, sc, id, "rotate-1")
	var again rotateSecretResponse
	if err := json.Unmarshal(replay.Body.Bytes(), &again); err != nil {
		t.Fatalf("decode replay: %v", err)
	}
	if again.Secret != resp.Secret || string(store.webhooks[id].PreviousSecret) != string(oldKey) {
		t.Error("idempotent replay rotated the key a second time")
	}
}

func TestHandleRotateSecret_CrossAccountAndRole(t *testing.T) {
	h, store, sc := newTestRig(t)
	stranger := uuid.New()
	strangerKey := []byte("wsec_stranger")
	store.webhooks[stranger] = platform.CustomerWebhook{
		ID: stranger, AccountID: uuid.New(), SecretHash: strangerKey,
		URL: "https://y.example", Events: []string{"incident.sev1"}, Enabled: true,
	}
	req := sessionReq(t, http.MethodPost, "/v1/dashboard/webhooks/"+stranger.String()+"/rotate-secret", nil, sc)
	req.SetPathValue("id", stranger.String())
	w := httptest.NewRecorder()
	h.HandleRotateSecret(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("cross-account rotate: status = %d, want 404", w.Code)
	}
	if string(store.webhooks[stranger].SecretHash) != string(strangerKey) {
		t.Error("cross-account rotate changed the other account's key")
	}

	viewer := sc
	viewer.User.Role = platform.RoleViewer
	req = sessionReq(t, http.MethodPost, "/v1/dashboard/webhooks/"+stranger.String()+"/rotate-secret", nil, viewer)
	req.SetPathValue("id", stranger.String())
	w = httptest.NewRecorder()
	h.HandleRotateSecret(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("viewer rotate: status = %d, want 403", w.Code)
	}
}
