// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

// Pins requireReason (Q151, audit-2026-09-02): the X-Reason contract
// shared by every unconditional operator-tier write — POST/DELETE
// /v1/admin/keys, PATCH /v1/admin/accounts/{id}, and the status-notice
// create/resolve routes — must be a single chokepoint, not five
// copy-pasted header reads. A duplicated-checks regression (reverting
// any one call site back to an inline read) would still pass an
// HTTP-level test with an identical response; this test targets the
// helper itself so the extraction is verified directly.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
