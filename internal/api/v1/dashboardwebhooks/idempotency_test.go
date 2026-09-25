package dashboardwebhooks

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// TestMount_CreateIdempotencyKey_ReplaysInsteadOfRegistering: a client
// that retries POST /v1/dashboard/webhooks with the same Idempotency-Key
// (after a timed-out response) gets the ORIGINAL webhook and signing
// secret replayed, not a second webhook registered with a secret the
// client never saw. Routed through h.Mount so the wired middleware is
// exercised.
func TestMount_CreateIdempotencyKey_ReplaysInsteadOfRegistering(t *testing.T) {
	h, store, sc := newTestRig(t)
	mux := http.NewServeMux()
	h.Mount(mux, middleware.NewPublicRoutes())

	body := createRequest{
		Name:   "ops-slack",
		URL:    "https://hooks.slack.example/services/T/B/X",
		Events: []string{string(platform.WebhookEventIncidentSEV1)},
	}
	send := func() createResponse {
		t.Helper()
		req := sessionReq(t, http.MethodPost, "/v1/dashboard/webhooks", body, sc)
		req.Host = "api.stellarindex.io"
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("Origin", "https://api.stellarindex.io")
		req.Header.Set("Idempotency-Key", "retry-wh-456")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
		var resp createResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return resp
	}

	first, second := send(), send()
	if second.Webhook.ID != first.Webhook.ID {
		t.Errorf("replay registered a DIFFERENT webhook: first id = %s, second id = %s",
			first.Webhook.ID, second.Webhook.ID)
	}
	if second.Secret != first.Secret {
		t.Error("replay returned a different signing secret than the original create")
	}

	store.mu.Lock()
	registered := len(store.webhooks)
	store.mu.Unlock()
	if registered != 1 {
		t.Errorf("store registered %d webhooks for two identical Idempotency-Key requests, want 1", registered)
	}
}
