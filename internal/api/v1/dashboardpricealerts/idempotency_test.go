package dashboardpricealerts

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestMount_CreateIdempotencyKey_ReplaysInsteadOfRegistering proves
// T284: a client that retries POST /v1/dashboard/price-alerts with
// the same Idempotency-Key header (e.g. after a timed-out response)
// gets the ORIGINAL alert replayed, not a second alert registered.
// Routed through h.Mount so the idempotency middleware wired there is
// actually exercised, not bypassed by calling h.HandleCreate
// directly.
func TestMount_CreateIdempotencyKey_ReplaysInsteadOfRegistering(t *testing.T) {
	h, store, sc := newTestRig(t, nil)
	mux := http.NewServeMux()
	h.Mount(mux)

	body := validCreate()

	req1 := sessionReq(t, http.MethodPost, "/v1/dashboard/price-alerts", body, sc)
	req1.Host = "api.stellarindex.io"
	req1.Header.Set("X-Forwarded-Proto", "https")
	req1.Header.Set("Origin", "https://api.stellarindex.io")
	req1.Header.Set("Idempotency-Key", "retry-xyz-789")
	w1 := httptest.NewRecorder()
	mux.ServeHTTP(w1, req1)
	if w1.Code != http.StatusCreated {
		t.Fatalf("first request status = %d, body = %s", w1.Code, w1.Body.String())
	}
	var resp1 priceAlertDTO
	if err := json.Unmarshal(w1.Body.Bytes(), &resp1); err != nil {
		t.Fatalf("decode first response: %v", err)
	}

	req2 := sessionReq(t, http.MethodPost, "/v1/dashboard/price-alerts", body, sc)
	req2.Host = "api.stellarindex.io"
	req2.Header.Set("X-Forwarded-Proto", "https")
	req2.Header.Set("Origin", "https://api.stellarindex.io")
	req2.Header.Set("Idempotency-Key", "retry-xyz-789")
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusCreated {
		t.Fatalf("second request status = %d, body = %s", w2.Code, w2.Body.String())
	}
	var resp2 priceAlertDTO
	if err := json.Unmarshal(w2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode second response: %v", err)
	}

	if resp2.ID != resp1.ID {
		t.Errorf("replay registered a DIFFERENT alert: first id = %s, second id = %s", resp1.ID, resp2.ID)
	}

	store.mu.Lock()
	registered := len(store.alerts)
	store.mu.Unlock()
	if registered != 1 {
		t.Errorf("store registered %d alerts for two identical Idempotency-Key requests, want 1", registered)
	}
}
