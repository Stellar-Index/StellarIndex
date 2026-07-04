// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"testing"

	"github.com/StellarIndex/stellar-index/internal/auth"
	"github.com/StellarIndex/stellar-index/internal/platform"
)

func TestRequiredScope(t *testing.T) {
	cases := map[string]string{
		"/v1/price":                 platform.KeyScopeRead,
		"/v1/chart":                 platform.KeyScopeRead,
		"/v1/ledgers/123":           platform.KeyScopeRead,
		"/v1/signup":                platform.KeyScopeRead,
		"/v1/account/me":            platform.KeyScopeAccount,
		"/v1/account/keys":          platform.KeyScopeAccount,
		"/v1/dashboard/keys":        platform.KeyScopeDashboard,
		"/v1/dashboard/webhooks/xy": platform.KeyScopeDashboard,
		"/v1/admin/keys":            platform.KeyScopeAdmin,
	}
	for path, want := range cases {
		if got := auth.RequiredScope(path); got != want {
			t.Errorf("RequiredScope(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestSubjectHasScope(t *testing.T) {
	// Empty scope list = full access (back-compat for pre-scopes keys).
	empty := auth.Subject{Tier: auth.TierAPIKey}
	if !empty.HasScope(platform.KeyScopeAdmin) {
		t.Errorf("empty Scopes must grant everything")
	}

	scoped := auth.Subject{Tier: auth.TierAPIKey, Scopes: []string{platform.KeyScopeRead}}
	if !scoped.HasScope(platform.KeyScopeRead) {
		t.Errorf("scoped key must grant its own scope")
	}
	if scoped.HasScope(platform.KeyScopeAccount) {
		t.Errorf("read-scoped key must not grant account")
	}

	wildcard := auth.Subject{Tier: auth.TierAPIKey, Scopes: []string{"*"}}
	if !wildcard.HasScope(platform.KeyScopeDashboard) {
		t.Errorf("wildcard scope must grant everything")
	}
}

func TestValidKeyScope(t *testing.T) {
	for _, s := range platform.KnownKeyScopes() {
		if !platform.ValidKeyScope(s) {
			t.Errorf("known scope %q rejected", s)
		}
	}
	for _, s := range []string{"", "*", "price:read", "admin:*", "READ"} {
		if platform.ValidKeyScope(s) {
			t.Errorf("scope %q should be rejected at mint time", s)
		}
	}
}
