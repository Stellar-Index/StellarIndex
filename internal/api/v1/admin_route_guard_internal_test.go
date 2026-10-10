// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// TestHandleAdmin_GatesBeforeTheHandler pins the mount-level guard with a
// handler that does no checks of its own, as a new admin route might.
func TestHandleAdmin_GatesBeforeTheHandler(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		subject *auth.Subject
		reason  string
		want    int
	}{
		{"anonymous", http.MethodGet, nil, "", http.StatusUnauthorized},
		{"apikey tier read", http.MethodGet, &auth.Subject{Identifier: "c", Tier: auth.TierAPIKey}, "", http.StatusForbidden},
		{"apikey tier write with reason", http.MethodPost, &auth.Subject{Identifier: "c", Tier: auth.TierAPIKey}, "r", http.StatusForbidden},
		{"operator write without reason", http.MethodPost, &auth.Subject{Identifier: "op", Tier: auth.TierOperator}, "", http.StatusBadRequest},
		{"operator write with reason", http.MethodPost, &auth.Subject{Identifier: "op", Tier: auth.TierOperator}, "r", http.StatusTeapot},
		{"operator read without reason", http.MethodGet, &auth.Subject{Identifier: "op", Tier: auth.TierOperator}, "", http.StatusTeapot},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{mux: http.NewServeMux()}
			s.handleAdmin(tc.method+" /v1/admin/probe", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusTeapot)
			})
			req := httptest.NewRequest(tc.method, "/v1/admin/probe", nil)
			if tc.subject != nil {
				req = req.WithContext(auth.WithSubject(req.Context(), *tc.subject))
			}
			if tc.reason != "" {
				req.Header.Set("X-Reason", tc.reason)
			}
			rec := httptest.NewRecorder()
			s.mux.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
