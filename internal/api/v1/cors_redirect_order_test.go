// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
)

// TestTrailingSlashRedirectCarriesCORS: the trailing-slash 308 must carry Access-Control-Allow-Origin, otherwise browsers kill
// the cross-origin redirect and every trailing-slash API URL is as dead as the 404 it prevents.
func TestTrailingSlashRedirectCarriesCORS(t *testing.T) {
	s := New(Options{
		CORS: middleware.CORS(middleware.CORSOptions{
			AllowedOrigins: []string{"https://stellarindex.io"},
		}),
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/assets/native/", nil)
	req.Header.Set("Origin", "https://stellarindex.io")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusPermanentRedirect {
		t.Fatalf("status = %d, want 308", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://stellarindex.io" {
		t.Fatalf("Access-Control-Allow-Origin = %q on the 308 — browsers kill this redirect (S-009)", got)
	}
}
