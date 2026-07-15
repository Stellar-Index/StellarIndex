// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// RequiredScope maps a request path to the capability scope a
// scoped key must carry to call it. Families are derived from the
// /v1 route table (see internal/api/v1/server.go mountRoutes):
//
//   - /v1/admin/*     → platform.KeyScopeAdmin
//   - /v1/account/*   → platform.KeyScopeAccount
//   - /v1/dashboard/* → platform.KeyScopeDashboard
//   - everything else → platform.KeyScopeRead (public data surfaces)
//
// The mapping is prefix-based rather than a per-route table so a
// new data endpoint is read-scoped by default and a new management
// endpoint under an existing family inherits the right scope with
// zero wiring. Enforcement lives in the KeyPolicy middleware and
// only applies to subjects whose Scopes list is non-empty
// (empty = full access, the pre-scopes posture).
func RequiredScope(path string) string {
	switch {
	case strings.HasPrefix(path, "/v1/admin/"):
		return platform.KeyScopeAdmin
	case strings.HasPrefix(path, "/v1/account/"):
		return platform.KeyScopeAccount
	case strings.HasPrefix(path, "/v1/dashboard/"):
		return platform.KeyScopeDashboard
	default:
		return platform.KeyScopeRead
	}
}

// HasScope reports whether the subject may exercise the given
// capability scope. An EMPTY scope list grants everything — that is
// the back-compat contract for every key minted before scopes
// shipped, and the documented meaning of "no scopes" at mint time.
// The "*" wildcard is honoured defensively for hand-seeded records
// even though the mint surfaces reject it.
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
