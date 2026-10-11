// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// RequiredScope maps a request path to the scope a scoped key must carry: admin families (/v1/admin/*, /v1/account/admin/*, /v1/dashboard/admin/*) need KeyScopeAdmin, /v1/account/* Account, /v1/dashboard/* Dashboard, everything else Read.
// Prefix-based so new endpoints are read-scoped by default; admin arms match first so staff surfaces nested in a self-service family (account/admin/lookup returns PII) do not inherit its weaker scope.
// Enforced by the KeyPolicy middleware only for subjects with a non-empty Scopes list.
func RequiredScope(path string) string {
	switch {
	case strings.HasPrefix(path, "/v1/admin/"),
		strings.HasPrefix(path, "/v1/account/admin/"),
		strings.HasPrefix(path, "/v1/dashboard/admin/"):
		return platform.KeyScopeAdmin
	case strings.HasPrefix(path, "/v1/account/"):
		return platform.KeyScopeAccount
	case strings.HasPrefix(path, "/v1/dashboard/"):
		return platform.KeyScopeDashboard
	default:
		return platform.KeyScopeRead
	}
}

// HasScope reports whether the subject may exercise scope; an EMPTY scope list grants everything (keys minted without scopes keep working).
// The "*" wildcard is honoured for hand-seeded records although mint surfaces reject it.
func (s Subject) HasScope(scope string) bool {
	if len(s.Scopes) == 0 {
		return true
	}
	for _, have := range s.Scopes {
		if have == scope || have == "*" {
			return true
		}
	}
	return false
}
