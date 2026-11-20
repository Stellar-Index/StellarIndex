// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package sep10_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// A SEP-10 token carries no per-endpoint policy, so both Subjects the validator issues must pass KeyPolicy: the
// closed posture (no permission entries) would 403 every request an authenticated wallet makes.
func TestSEP10Subjects_PassKeyPolicy(t *testing.T) {
	v, _, _ := newTestValidator(t)
	client, _ := keypair.Random()
	ch, _ := v.Challenge(context.Background(), client.Address())
	tok, err := v.Verify(context.Background(), signChallenge(t, ch.TransactionXDR, client))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	fromJWT, err := v.VerifyJWT(context.Background(), tok.JWT)
	if err != nil {
		t.Fatalf("VerifyJWT: %v", err)
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	for name, subject := range map[string]auth.Subject{"Verify": tok.Subject, "VerifyJWT": fromJWT} {
		req := httptest.NewRequest(http.MethodGet, "/v1/assets", nil)
		req = req.WithContext(auth.WithSubject(req.Context(), subject))
		rec := httptest.NewRecorder()
		middleware.KeyPolicy()(ok).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s subject through KeyPolicy: status %d, body %s; want 200", name, rec.Code, rec.Body.String())
		}
	}
}
