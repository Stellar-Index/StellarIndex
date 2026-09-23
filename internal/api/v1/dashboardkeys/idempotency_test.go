package dashboardkeys

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
)

// TestMount_CreateIdempotencyKey_ReplaysInsteadOfMinting proves T284:
// a client that retries POST /v1/dashboard/keys with the same
// Idempotency-Key header (e.g. after a timed-out response) gets the
// ORIGINAL key replayed, not a second key minted. Routed through
// h.Mount so the idempotency middleware wired there is actually
// exercised, not bypassed by calling h.HandleCreate directly.
func TestMount_CreateIdempotencyKey_ReplaysInsteadOfMinting(t *testing.T) {
	h, store, sc := newTestRig(t)
	mux := http.NewServeMux()
	h.Mount(mux, middleware.NewPublicRoutes())

	body := createRequest{Name: "production", RateLimitPerMin: 1000}

	req1 := sessionRequest(t, http.MethodPost, "/v1/dashboard/keys", body, sc)
	req1.Host = "api.stellarindex.io"
	req1.Header.Set("X-Forwarded-Proto", "https")
	req1.Header.Set("Origin", "https://api.stellarindex.io")
	req1.Header.Set("Idempotency-Key", "retry-abc-123")
	w1 := httptest.NewRecorder()
	mux.ServeHTTP(w1, req1)
	if w1.Code != http.StatusCreated {
		t.Fatalf("first request status = %d, body = %s", w1.Code, w1.Body.String())
	}
	var resp1 createResponse
	if err := json.Unmarshal(w1.Body.Bytes(), &resp1); err != nil {
		t.Fatalf("decode first response: %v", err)
	}

	req2 := sessionRequest(t, http.MethodPost, "/v1/dashboard/keys", body, sc)
	req2.Host = "api.stellarindex.io"
	req2.Header.Set("X-Forwarded-Proto", "https")
	req2.Header.Set("Origin", "https://api.stellarindex.io")
	req2.Header.Set("Idempotency-Key", "retry-abc-123")
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusCreated {
		t.Fatalf("second request status = %d, body = %s", w2.Code, w2.Body.String())
	}
	var resp2 createResponse
	if err := json.Unmarshal(w2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode second response: %v", err)
	}

	if resp2.Key.ID != resp1.Key.ID {
		t.Errorf("replay minted a DIFFERENT key: first id = %s, second id = %s", resp1.Key.ID, resp2.Key.ID)
	}
	if resp2.Plaintext != resp1.Plaintext {
		t.Errorf("replay returned a DIFFERENT plaintext than the original mint")
	}

	store.mu.Lock()
	minted := len(store.byID)
	store.mu.Unlock()
	if minted != 1 {
		t.Errorf("store minted %d keys for two identical Idempotency-Key requests, want 1", minted)
	}
}
