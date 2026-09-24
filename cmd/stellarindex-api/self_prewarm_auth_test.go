package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSelfPrewarmAdmitted_MatchesAuthMiddleware drives the real auth
// middleware with the exact request the self-prewarm sends (no credential,
// its own User-Agent) and requires the start gate to agree with it: the loop
// must never start in a mode where every warm-up 401s, and must still start
// where the request is served.
func TestSelfPrewarmAdmitted_MatchesAuthMiddleware(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mode         string
		wantAdmitted bool
	}{
		{"", true},
		{"none", true},
		{"apikey_optional", true},
		{"apikey", false},
		{"sep10", false},
		{"api-key", false}, // unknown mode: refuse rather than guess
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			t.Parallel()
			if got := selfPrewarmAdmitted(tc.mode); got != tc.wantAdmitted {
				t.Fatalf("selfPrewarmAdmitted(%q) = %v, want %v", tc.mode, got, tc.wantAdmitted)
			}
			var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			if mw := buildAuthMiddleware(tc.mode, authValidatorOptions{}, discardLogger()); mw != nil {
				h = mw(h)
			}
			req := httptest.NewRequest(http.MethodGet, "/v1/assets/native", nil)
			req.Header.Set("User-Agent", "stellarindex-prewarm/1")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if selfPrewarmAdmitted(tc.mode) && w.Code != http.StatusOK {
				t.Errorf("mode %q: self-prewarm would start but its request gets %d, "+
					"so every 60s pass is a no-op", tc.mode, w.Code)
			}
			if !tc.wantAdmitted && tc.mode != "api-key" && w.Code != http.StatusUnauthorized {
				t.Errorf("mode %q: credential-less request got %d, want 401 — the "+
					"premise for disabling the self-prewarm no longer holds", tc.mode, w.Code)
			}
		})
	}
}
