// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// RequiredScope maps a request path to the scope a scoped key must carry (admin families need KeyScopeAdmin, then Account, Dashboard, else Read); admin arms match first so nested staff surfaces never inherit a weaker scope.
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

// HasScope reports whether the subject may exercise scope; an EMPTY scope list grants everything and "*" is honoured for hand-seeded records.
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
