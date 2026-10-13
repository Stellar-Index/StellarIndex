// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// Pins requireReason: the X-Reason contract shared by every unconditional operator-tier write
// (admin keys, admin accounts, status-notice routes) must be a single chokepoint, not
// copy-pasted header reads. Reverting one call site to an inline read would still pass an
// HTTP-level test with an identical response, so this test targets the helper itself.

func TestRequireReason_MissingHeaderWrites400MissingReasonProblem(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/keys", nil)
	rec := httptest.NewRecorder()
	s := &Server{}

	reason, ok := s.requireReason(rec, req)

	if ok {
		t.Fatalf("ok = true with no X-Reason header, want false")
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty on rejection", reason)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if p.Type != "https://api.stellarindex.io/errors/missing-reason" {
		t.Errorf("problem type = %q, want the missing-reason type", p.Type)
	}
	if p.Status != http.StatusBadRequest {
		t.Errorf("problem status = %d, want 400", p.Status)
	}
}

func TestRequireReason_PresentHeaderPassesThroughUnwritten(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/keys", nil)
	req.Header.Set("X-Reason", "abuse report #4412")
	rec := httptest.NewRecorder()
	s := &Server{}

	reason, ok := s.requireReason(rec, req)

	if !ok {
		t.Fatalf("ok = false with X-Reason set, want true")
	}
	if reason != "abuse report #4412" {
		t.Errorf("reason = %q, want the header value verbatim", reason)
	}
	if rec.Code != 200 {
		t.Errorf("status = %d, want no write on the success path (default 200)", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want no problem body written on success", rec.Body.String())
	}
}

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
