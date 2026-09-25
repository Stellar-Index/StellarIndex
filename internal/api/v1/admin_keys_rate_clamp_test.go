// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"net/http"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// GH-1147: POST /v1/admin/keys clamped scopes to the caller's but checked
// rate_limit_per_min only against the constant [0, 100000], so an
// operator key narrowed to "admin" could still mint 100,000/min keys.
func TestAdminKeysCreate_NarrowedOperatorCannotMintAboveItsRateLimit(t *testing.T) {
	store := &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "kid_minted"}, plain: "sip_x"}
	narrowed := auth.Subject{Identifier: "operator:staff-2", Tier: auth.TierOperator, KeyID: "kid_narrow", Scopes: []string{"admin"}}
	ts := newAdminTestServer(t, narrowed, store, nil)

	resp := postJSON(t, ts.URL+"/v1/admin/keys",
		`{"identifier":"acct:target","label":"comp","rate_limit_per_min":100000}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — a scoped operator on the default rate limit must not mint a 100000/min key", resp.StatusCode)
	}
	if store.calls != 0 {
		t.Errorf("store.Create called %d times for a refused mint", store.calls)
	}
}
