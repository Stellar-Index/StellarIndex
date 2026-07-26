package dashboardkeys

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestMount_CrossSiteWriteBlockedBeforeHandler is the C3-031 / C3-057
// regression at the mount point rather than the middleware unit: a
// cross-site page must not be able to mint or revoke a logged-in
// customer's API keys with their session cookie.
//
// It asserts the CSRF `type` URI specifically, not just "403" — the
// handler's own role/session checks also produce 403s, and a test that
// accepted any 403 would pass even with the guard removed.
func TestMount_CrossSiteWriteBlockedBeforeHandler(t *testing.T) {
	h, _, sc := newTestRig(t)
	mux := http.NewServeMux()
	h.Mount(mux)

	for _, tc := range []struct {
		name, method, target string
	}{
		{"mint", http.MethodPost, "/v1/dashboard/keys"},
		// A non-existent id is deliberate: if the guard were removed
		// the handler would answer 404, which fails the 403 assertion
		// below — so this case can't pass without the guard.
		{"revoke", http.MethodDelete, "/v1/dashboard/keys/8f14e45f-ceea-467a-9575-1c1d1f6e0e5b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := sessionRequest(t, tc.method, tc.target, nil, sc)
			req.Host = "api.stellarindex.io"
			req.Header.Set("Origin", "https://evil.example")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body = %s", w.Code, w.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("body not JSON: %v", err)
			}
			if got := body["type"]; got != "https://api.stellarindex.io/errors/cross-site-request-blocked" {
				t.Fatalf("problem type = %v, want the cross-site guard's (handler reached)", got)
			}
		})
	}

	// Reads stay reachable — the guard must not have been hung on the
	// whole route table.
	req := sessionRequest(t, http.MethodGet, "/v1/dashboard/keys", nil, sc)
	req.Host = "api.stellarindex.io"
	req.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200 (reads are not gated); body = %s", w.Code, w.Body.String())
	}
}

// TestMount_SameSiteWriteReachesHandler proves the guard passes the
// legitimate same-origin dashboard write through to the handler.
func TestMount_SameSiteWriteReachesHandler(t *testing.T) {
	h, _, sc := newTestRig(t)
	mux := http.NewServeMux()
	h.Mount(mux)

	req := sessionRequest(t, http.MethodPost, "/v1/dashboard/keys", createRequest{
		Name:            "production",
		RateLimitPerMin: 1000,
	}, sc)
	req.Host = "api.stellarindex.io"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("Origin", "https://api.stellarindex.io")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", w.Code, w.Body.String())
	}
}
